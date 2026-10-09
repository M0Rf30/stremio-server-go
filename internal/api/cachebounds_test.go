// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMustIncludeCacheReusesCompiledRegexp(t *testing.T) {
	a := compileMustInclude([]string{"/S01E0[1-3]\\.mkv$/i"})
	b := compileMustInclude([]string{"/S01E0[1-3]\\.mkv$/i"})
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("len = %d,%d, want 1,1", len(a), len(b))
	}
	if a[0] != b[0] {
		t.Error("second compile did not reuse the cached *regexp.Regexp")
	}
	if !a[0].MatchString("x.s01e02.mkv") {
		t.Error("cached /…/i pattern lost its case-insensitive flag")
	}
}

func TestMustIncludeCacheBounded(t *testing.T) {
	for i := range 3 * mustIncludeCacheMax {
		compileMustInclude([]string{fmt.Sprintf("bound-test-%d.mkv", i)})
	}
	mustIncludeCache.mu.Lock()
	n, m := mustIncludeCache.ll.Len(), len(mustIncludeCache.idx)
	mustIncludeCache.mu.Unlock()
	if n > mustIncludeCacheMax || m > mustIncludeCacheMax {
		t.Errorf("cache size list=%d map=%d, want <= %d", n, m, mustIncludeCacheMax)
	}
}

func TestMustIncludeCacheSkipsOversized(t *testing.T) {
	long := make([]byte, mustIncludeCacheMaxLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if out := compileMustInclude([]string{string(long)}); len(out) != 1 {
		t.Fatalf("oversized pattern: got %d regexps, want 1", len(out))
	}
	mustIncludeCache.mu.Lock()
	_, cached := mustIncludeCache.idx[string(long)]
	mustIncludeCache.mu.Unlock()
	if cached {
		t.Error("oversized pattern was cached")
	}
}

// heavyMustIncludePatterns are f= values (all within mustIncludeCacheMaxLen)
// whose compiled programs are large although the pattern TEXT is small: huge
// instruction counts from repetition, or few instructions each carrying a
// distinct multi-KiB Unicode rune table. Caching 128 of them would pin
// hundreds of MB to GB. match is a cheap input the pattern matches; empty
// skips that check for the programs whose NFA scan is slow under -race.
var heavyMustIncludePatterns = []struct{ pat, match string }{
	{`/(?:\pL\pN\pL\pN\pL\pN\pL\pN){1000}/`, ""},
	{`/(?:\pL\pN\pL\pN\pL\pN\pL\pN){999}/`, ""},
	{`/(?:\pL\pN\pL\pN\pL\pN\pL\pN){998}/i`, ""},
	{`/(?:\pL\pN){1000}/`, strings.Repeat("é1", 1000)},
	// ~160 instructions but ~1.3 MB of rune tables (150 distinct \pL nodes).
	{`/` + strings.Repeat(`\pL`, 150) + `/i`, strings.Repeat("é", 150)},
	// ~480k instructions, ~22 MB retained, from a 500-byte pattern.
	{`/(?:` + strings.Repeat(`.`, 480) + `){1000}/`, ""},
}

func mustIncludeCached(key string) bool {
	mustIncludeCache.mu.Lock()
	defer mustIncludeCache.mu.Unlock()
	_, ok := mustIncludeCache.idx[key]
	return ok
}

func mustIncludeCacheLen() int {
	mustIncludeCache.mu.Lock()
	defer mustIncludeCache.mu.Unlock()
	return mustIncludeCache.ll.Len()
}

// resetMustIncludeCache swaps in an empty cache for the test and restores the
// previous one afterwards, so size assertions are not masked by an already
// full LRU (which evicts one entry per insert and so keeps its length constant).
func resetMustIncludeCache(t *testing.T) {
	t.Helper()
	mustIncludeCache.mu.Lock()
	oldLL, oldIdx := mustIncludeCache.ll, mustIncludeCache.idx
	mustIncludeCache.ll, mustIncludeCache.idx = list.New(), map[string]*list.Element{}
	mustIncludeCache.mu.Unlock()
	t.Cleanup(func() {
		mustIncludeCache.mu.Lock()
		mustIncludeCache.ll, mustIncludeCache.idx = oldLL, oldIdx
		mustIncludeCache.mu.Unlock()
	})
}

// TestMustIncludeCacheSkipsLargePrograms: the LRU bounds pattern text, not
// compiled size, so a short pattern with a huge compiled program must still
// compile and match but never be retained.
func TestMustIncludeCacheSkipsLargePrograms(t *testing.T) {
	resetMustIncludeCache(t)
	for _, h := range heavyMustIncludePatterns {
		p := h.pat
		if len(p) > mustIncludeCacheMaxLen {
			t.Fatalf("test pattern %.48q is longer than the text bound; it must hit the size check, not the length check", p)
		}
		out := compileMustInclude([]string{p})
		if len(out) != 1 {
			t.Fatalf("%.48q: got %d regexps, want 1 (must still compile and be usable)", p, len(out))
		}
		if h.match != "" && !out[0].MatchString(h.match) {
			t.Errorf("%.48q: uncached large pattern does not match a matching input", p)
		}
		if mustIncludeCached(p) {
			t.Errorf("%.48q: large compiled program was retained in the cache", p)
		}
	}
	if n := mustIncludeCacheLen(); n != 0 {
		t.Errorf("cache holds %d entries after only heavy patterns, want 0", n)
	}
}

// TestMustIncludeCacheLargeProgramsCannotFillCache: a flood of distinct heavy
// patterns must neither grow the cache nor evict useful small entries.
func TestMustIncludeCacheLargeProgramsCannotFillCache(t *testing.T) {
	resetMustIncludeCache(t)
	const small = "keep-me.mkv"
	compileMustInclude([]string{small})
	for i := range 2 * mustIncludeCacheMax {
		compileMustInclude([]string{fmt.Sprintf(`/(?:\pL\pN\pL\pN\pL\pN\pL\pN){%d}/`, 500+i)})
	}
	if n := mustIncludeCacheLen(); n != 1 {
		t.Errorf("cache size = %d after heavy-pattern flood, want 1 (only the small entry)", n)
	}
	if !mustIncludeCached(small) {
		t.Error("small entry was evicted by heavy patterns")
	}
}

// TestMustIncludeCacheKeepsTypicalPatterns guards against the size check
// being so tight that real-world file filters stop being cached.
func TestMustIncludeCacheKeepsTypicalPatterns(t *testing.T) {
	resetMustIncludeCache(t)
	typical := []string{
		`/S01E0[1-3]\.mkv$/i`,
		`/^.*sample.*\.(mkv|mp4|avi)$/i`,
		`/\b(2160p|1080p|720p)\b.*(x26[45]|hevc)/i`,
		`/[\p{L}\p{N} ._-]+\.(srt|ass|sub)$/i`,
		`/(?i)s\d{1,2}e\d{1,3}.*\.(mkv|mp4)$/`,
		`Movie.Name.2024.1080p.WEB-DL.mkv`,
		`/` + strings.Repeat(`ab?c`, 100) + `/`, // ~400 insts, still ordinary
	}
	for _, p := range typical {
		if len(compileMustInclude([]string{p})) != 1 {
			t.Fatalf("%q did not compile", p)
		}
		if !mustIncludeCached(p) {
			t.Errorf("%q: typical pattern was not cached", p)
		}
	}
}

func TestMustIncludeCacheConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				out := compileMustInclude([]string{fmt.Sprintf("conc-%d.mkv", (g*7+i)%300)})
				if len(out) != 1 || !out[0].MatchString(fmt.Sprintf("conc-%d.mkv", (g*7+i)%300)) {
					t.Error("bad regexp from cache")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPruneLocalCaches(t *testing.T) {
	imdbCacheMu.Lock()
	oldIMDB := imdbByLocal
	imdbByLocal = map[string]imdbEntry{}
	imdbCacheMu.Unlock()
	ttPathMu.Lock()
	oldTT := ttToPath
	ttToPath = map[string]string{}
	ttPathMu.Unlock()
	t.Cleanup(func() {
		imdbCacheMu.Lock()
		imdbByLocal = oldIMDB
		imdbCacheMu.Unlock()
		ttPathMu.Lock()
		ttToPath = oldTT
		ttPathMu.Unlock()
	})

	total := localCacheSlack + 50
	seen := map[string]struct{}{}
	for i := range total {
		p := fmt.Sprintf("/lib/file-%d.mkv", i)
		hex := localID(p)
		imdbByLocal[hex] = imdbEntry{TTID: fmt.Sprintf("tt%07d", i)}
		ttToPath[fmt.Sprintf("tt%07d", i)] = p
		if i < 10 { // only 10 files still on disk
			seen[hex] = struct{}{}
		}
	}

	// Empty scan (e.g. unmounted library): never prune.
	pruneLocalCaches(map[string]struct{}{}, 0)
	if len(imdbByLocal) != total || len(ttToPath) != total {
		t.Fatalf("empty scan pruned caches: %d/%d", len(imdbByLocal), len(ttToPath))
	}

	pruneLocalCaches(seen, len(seen))
	if len(imdbByLocal) != len(seen) || len(ttToPath) != len(seen) {
		t.Errorf("after prune imdb=%d tt=%d, want %d", len(imdbByLocal), len(ttToPath), len(seen))
	}

	// Within slack of the live size: nothing is pruned.
	imdbByLocal["stale"] = imdbEntry{}
	pruneLocalCaches(seen, len(seen))
	if _, ok := imdbByLocal["stale"]; !ok {
		t.Error("entry pruned although cache is within slack")
	}
}
