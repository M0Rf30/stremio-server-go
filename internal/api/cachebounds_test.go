// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
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
