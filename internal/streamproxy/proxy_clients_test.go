package streamproxy

import (
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// proxyClients TTL + hard-cap eviction
// ---------------------------------------------------------------------------

func TestSweepProxyClients(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]proxyClientEntry
		wantLen int
		wantHas []string
		wantNot []string
	}{
		{
			name: "expired entry evicted",
			entries: map[string]proxyClientEntry{
				"stale": {client: &http.Client{Transport: &http.Transport{}}, expiresAt: time.Now().Add(-time.Minute)},
				"fresh": {client: &http.Client{Transport: &http.Transport{}}, expiresAt: time.Now().Add(time.Hour)},
			},
			wantLen: 1,
			wantHas: []string{"fresh"},
			wantNot: []string{"stale"},
		},
		{
			name:    "over cap evicts soonest-expiring until at limit",
			entries: manyFreshProxyClients(proxyClientMaxEntries + 3),
			wantLen: proxyClientMaxEntries,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{proxyClients: tc.entries}
			h.sweepProxyClients()
			if len(h.proxyClients) != tc.wantLen {
				t.Fatalf("len(proxyClients) = %d, want %d", len(h.proxyClients), tc.wantLen)
			}
			if len(h.proxyClients) > proxyClientMaxEntries {
				t.Fatalf("proxyClients exceeds hard cap: %d > %d", len(h.proxyClients), proxyClientMaxEntries)
			}
			for _, k := range tc.wantHas {
				if _, ok := h.proxyClients[k]; !ok {
					t.Errorf("expected key %q to remain", k)
				}
			}
			for _, k := range tc.wantNot {
				if _, ok := h.proxyClients[k]; ok {
					t.Errorf("expected key %q to be evicted", k)
				}
			}
		})
	}
}

// manyFreshProxyClients builds n non-expired entries with strictly
// increasing expiry times so eviction order is deterministic.
func manyFreshProxyClients(n int) map[string]proxyClientEntry {
	m := make(map[string]proxyClientEntry, n)
	base := time.Now().Add(time.Hour)
	for i := range n {
		m[string(rune('a'+i))] = proxyClientEntry{
			client:    &http.Client{Transport: &http.Transport{}},
			expiresAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	return m
}

// TestClientForCachesAndExpires drives clientFor directly (no real proxy
// dial happens until a request is made) to confirm the same proxyURL is
// served from cache on repeat calls and rebuilt once its TTL entry expires.
func TestClientForCachesAndExpires(t *testing.T) {
	h := &Handler{
		cfg:          Config{Client: http.DefaultClient},
		proxyClients: make(map[string]proxyClientEntry),
	}

	c1 := h.clientFor("http://127.0.0.1:9: invalid")
	// Invalid proxy URL falls back to the base client and is never cached.
	if c1 != http.DefaultClient {
		t.Fatalf("invalid proxy URL should fall back to base client")
	}
	if len(h.proxyClients) != 0 {
		t.Fatalf("invalid proxy URL should not be cached, got len=%d", len(h.proxyClients))
	}

	const p = "http://127.0.0.1:8080"
	c2 := h.clientFor(p)
	c3 := h.clientFor(p)
	if c2 != c3 {
		t.Fatalf("repeated clientFor(%q) should reuse the cached client", p)
	}
	if len(h.proxyClients) != 1 {
		t.Fatalf("expected exactly 1 cached client, got %d", len(h.proxyClients))
	}

	// Force expiry and confirm a fresh client is built.
	h.proxyMu.Lock()
	e := h.proxyClients[p]
	e.expiresAt = time.Now().Add(-time.Second)
	h.proxyClients[p] = e
	h.proxyMu.Unlock()

	c4 := h.clientFor(p)
	if c4 == c3 {
		t.Fatalf("expired cached client should be rebuilt, got the same instance")
	}
}
