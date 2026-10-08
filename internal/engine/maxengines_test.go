// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

func TestEngineCapEvictsOldestIdleThenRejects(t *testing.T) {
	em, err := New(newJanitorTestCfg(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = em.Close() }()
	m := em.(*manager)
	m.maxEngines = 2

	const a = "08ada5a7a6183aae1e09d831df6748d566095a10"
	const b = "18ada5a7a6183aae1e09d831df6748d566095a10"
	const c = "28ada5a7a6183aae1e09d831df6748d566095a10"
	const d = "38ada5a7a6183aae1e09d831df6748d566095a10"
	for _, ih := range []string{a, b} {
		if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
			t.Fatalf("EnsureEngine %s: %v", ih, err)
		}
	}
	m.engines[a].mu.Lock()
	m.engines[a].lastAccess = time.Now().Add(-time.Hour)
	m.engines[a].mu.Unlock()

	if _, err := m.EnsureEngine(c, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine c (should evict a): %v", err)
	}
	if _, ok := m.engines[a]; ok {
		t.Fatal("oldest idle engine a not evicted")
	}
	if len(m.engines) != 2 {
		t.Fatalf("engines = %d, want 2", len(m.engines))
	}

	for _, ih := range []string{b, c} {
		m.engines[ih].mu.Lock()
		m.engines[ih].openReaders = 1
		m.engines[ih].mu.Unlock()
	}
	_, err = m.EnsureEngine(d, types.AddOptions{})
	if !errors.Is(err, ErrTooManyEngines) || !errors.Is(err, types.ErrTooManyEngines) {
		t.Fatalf("err = %v, want ErrTooManyEngines", err)
	}
}
