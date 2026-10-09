// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"container/list"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
)

const (
	// mustIncludeCacheMax bounds the number of distinct f= / fileMustInclude
	// patterns whose compiled form is kept.
	mustIncludeCacheMax = 128
	// mustIncludeCacheMaxLen is the longest raw pattern that is cached; longer
	// (client-controlled) patterns are compiled per use so the cache's memory
	// stays bounded.
	mustIncludeCacheMaxLen = 512
	// mustIncludeCacheMaxProgBytes is the largest estimated compiled-program
	// size that is cached (see mustIncludeCacheable): 128 entries of at most
	// this size keep the cache within roughly 8-10 MiB. Ordinary file filters
	// are a few KiB; even a 512-byte literal is ~25 KiB.
	mustIncludeCacheMaxProgBytes = 64 << 10
	// mustIncludeInstBytes / mustIncludeRuneBytes are the per-instruction and
	// per-rune retained cost used by that estimate (measured: ~46 B per
	// regexp/syntax.Inst once the onepass copy is included; runes are int32).
	mustIncludeInstBytes = 48
	mustIncludeRuneBytes = 4
)

type mustIncludeEntry struct {
	key string
	re  *regexp.Regexp
}

// mustIncludeLRU is a small mutex-guarded LRU of compiled fileMustInclude
// patterns keyed by their raw request value. *regexp.Regexp is safe for
// concurrent use, so entries are shared across requests.
type mustIncludeLRU struct {
	mu  sync.Mutex
	ll  *list.List
	idx map[string]*list.Element
}

var mustIncludeCache = &mustIncludeLRU{ll: list.New(), idx: map[string]*list.Element{}}

func (c *mustIncludeLRU) get(key string) (*regexp.Regexp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*mustIncludeEntry).re, true
}

func (c *mustIncludeLRU) put(key string, re *regexp.Regexp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		el.Value.(*mustIncludeEntry).re = re
		c.ll.MoveToFront(el)
		return
	}
	c.idx[key] = c.ll.PushFront(&mustIncludeEntry{key: key, re: re})
	for c.ll.Len() > mustIncludeCacheMax {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.idx, last.Value.(*mustIncludeEntry).key)
	}
}

// compileMustIncludeOne compiles one raw f= value: a "/pattern/flags" literal
// (only the "i" flag is honoured) or, failing that, a quoted plain substring.
// It returns nil when nothing compiles.
func compileMustIncludeOne(v string) *regexp.Regexp {
	if len(v) > 1 && v[0] == '/' {
		if i := strings.LastIndex(v, "/"); i > 0 {
			body, flags := v[1:i], v[i+1:]
			if strings.Contains(flags, "i") {
				body = "(?i)" + body
			}
			if re, err := regexp.Compile(body); err == nil {
				return re
			}
		}
	}
	if re, err := regexp.Compile(regexp.QuoteMeta(v)); err == nil {
		return re
	}
	return nil
}

// cachedMustInclude returns the compiled form of v, going through the LRU.
func cachedMustInclude(v string) *regexp.Regexp {
	if len(v) > mustIncludeCacheMaxLen {
		return compileMustIncludeOne(v)
	}
	if re, ok := mustIncludeCache.get(v); ok {
		return re
	}
	re := compileMustIncludeOne(v)
	if re != nil && mustIncludeCacheable(re) {
		mustIncludeCache.put(v, re)
	}
	return re
}

// mustIncludeCacheable reports whether re's compiled program is small enough
// to retain. mustIncludeCacheMaxLen only bounds the pattern TEXT: `(?:` + 480
// dots + `){1000}` fits in 500 bytes yet compiles to ~480k instructions
// (~22 MB retained), and each distinct Unicode class (\pL) carries a ~5 KiB
// rune table, so a client could otherwise pin 128 such entries for the life
// of the process. Oversized programs are still compiled and used for the
// request, just never kept.
//
// The cost mirrors what regexp.Compile builds (parse, simplify, compile) from
// re's own source text, counting each distinct rune table once because
// repeated subexpressions share theirs.
func mustIncludeCacheable(re *regexp.Regexp) bool {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return false
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return false
	}
	cost := len(prog.Inst) * mustIncludeInstBytes
	var seen map[*rune]struct{}
	for i := range prog.Inst {
		if cost > mustIncludeCacheMaxProgBytes {
			return false
		}
		r := prog.Inst[i].Rune
		switch {
		case len(r) == 0:
		case len(r) <= 2: // literals / single folds: tiny, not worth tracking
			cost += len(r) * mustIncludeRuneBytes
		default:
			if seen == nil {
				seen = make(map[*rune]struct{})
			}
			if _, dup := seen[&r[0]]; !dup {
				seen[&r[0]] = struct{}{}
				cost += len(r) * mustIncludeRuneBytes
			}
		}
	}
	return cost <= mustIncludeCacheMaxProgBytes
}
