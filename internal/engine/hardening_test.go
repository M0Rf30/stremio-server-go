package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/mse"
)

// TestApplyBTEncryption verifies that each encryption mode sets the correct
// anacrolix fields and that the default ("prefer" / "") leaves them untouched.
func TestApplyBTEncryption(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode            string
		wantPreferred   bool
		wantRequire     bool
		wantRC4Provides bool // true → CryptoProvides must equal mse.CryptoMethodRC4
		wantRC4Selector bool // true → CryptoSelector(AllSupportedCrypto) must return RC4
		touchesDefaults bool // false → assert anacrolix defaults preserved
	}{
		{
			mode:            "require",
			wantPreferred:   true,
			wantRequire:     true,
			wantRC4Provides: true,
			wantRC4Selector: true,
			touchesDefaults: true,
		},
		{
			mode:            "REQUIRE", // case-insensitive
			wantPreferred:   true,
			wantRequire:     true,
			wantRC4Provides: true,
			wantRC4Selector: true,
			touchesDefaults: true,
		},
		{
			mode:            "disable",
			wantPreferred:   false,
			wantRequire:     false,
			wantRC4Provides: false,
			wantRC4Selector: false,
			touchesDefaults: true,
		},
		{
			mode:            "prefer",
			wantPreferred:   true,  // anacrolix default: MSE preferred
			wantRequire:     false, // anacrolix default: plaintext also accepted
			wantRC4Provides: false,
			wantRC4Selector: false,
			touchesDefaults: false,
		},
		{
			mode:            "",
			wantPreferred:   true,
			wantRequire:     false,
			wantRC4Provides: false,
			wantRC4Selector: false,
			touchesDefaults: false,
		},
		{
			mode:            "  prefer  ", // trimmed whitespace → same as prefer
			wantPreferred:   true,
			wantRequire:     false,
			wantRC4Provides: false,
			wantRC4Selector: false,
			touchesDefaults: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("mode="+tc.mode, func(t *testing.T) {
			t.Parallel()
			cc := torrent.NewDefaultClientConfig()
			applyBTEncryption(cc, tc.mode)

			got := cc.HeaderObfuscationPolicy
			if got.Preferred != tc.wantPreferred {
				t.Errorf("Preferred: got %v, want %v", got.Preferred, tc.wantPreferred)
			}
			if got.RequirePreferred != tc.wantRequire {
				t.Errorf("RequirePreferred: got %v, want %v", got.RequirePreferred, tc.wantRequire)
			}
			if tc.wantRC4Provides {
				if cc.CryptoProvides != mse.CryptoMethodRC4 {
					t.Errorf("CryptoProvides: got %v, want mse.CryptoMethodRC4 (%v)", cc.CryptoProvides, mse.CryptoMethodRC4)
				}
			}
			if tc.wantRC4Selector {
				sel := cc.CryptoSelector(mse.AllSupportedCrypto)
				if sel != mse.CryptoMethodRC4 {
					t.Errorf("CryptoSelector(AllSupportedCrypto): got %v, want mse.CryptoMethodRC4 (%v)", sel, mse.CryptoMethodRC4)
				}
			}
			if !tc.touchesDefaults {
				// "prefer" and "" must leave CryptoProvides as the anacrolix default.
				if cc.CryptoProvides != mse.AllSupportedCrypto {
					t.Errorf("expected anacrolix default CryptoProvides=AllSupportedCrypto, got %v", cc.CryptoProvides)
				}
			}
		})
	}
}

// TestApplyBTProxy verifies that each proxy URL shape produces the expected
// cc fields or returns an error.
func TestApplyBTProxy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		proxyURL         string
		wantErr          bool
		wantHTTPProxy    bool // cc.HTTPProxy should be non-nil
		wantDialContexts bool // cc.TrackerDialContext + cc.HTTPDialContext non-nil (SOCKS5 only)
	}{
		{
			name:             "empty=direct",
			proxyURL:         "",
			wantErr:          false,
			wantHTTPProxy:    false,
			wantDialContexts: false,
		},
		{
			name:             "socks5",
			proxyURL:         "socks5://127.0.0.1:9050",
			wantErr:          false,
			wantHTTPProxy:    true,
			wantDialContexts: true,
		},
		{
			name:             "http_proxy",
			proxyURL:         "http://127.0.0.1:8080",
			wantErr:          false,
			wantHTTPProxy:    true,
			wantDialContexts: false, // HTTP proxy: only cc.HTTPProxy set
		},
		{
			name:             "https_proxy",
			proxyURL:         "https://127.0.0.1:8443",
			wantErr:          false,
			wantHTTPProxy:    true,
			wantDialContexts: false,
		},
		{
			// "http://[::1" — unclosed IPv6 bracket causes url.Parse to fail.
			name:     "invalid_url",
			proxyURL: "http://[::1",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cc := torrent.NewDefaultClientConfig()
			err := applyBTProxy(cc, tc.proxyURL)

			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantHTTPProxy {
				if cc.HTTPProxy == nil {
					t.Error("cc.HTTPProxy should be non-nil")
				}
			} else {
				if cc.HTTPProxy != nil {
					t.Error("cc.HTTPProxy should be nil for empty proxyURL")
				}
			}
			if tc.wantDialContexts {
				if cc.TrackerDialContext == nil {
					t.Error("cc.TrackerDialContext should be non-nil for socks5")
				}
				if cc.HTTPDialContext == nil {
					t.Error("cc.HTTPDialContext should be non-nil for socks5")
				}
			} else {
				if cc.TrackerDialContext != nil {
					t.Error("cc.TrackerDialContext should be nil for non-socks5 proxy")
				}
				if cc.HTTPDialContext != nil {
					t.Error("cc.HTTPDialContext should be nil for non-socks5 proxy")
				}
			}
		})
	}
}

// TestApplyDHTBootstrap verifies that extra bootstrap nodes are appended and
// that an empty input is a no-op. Uses literal IPs to avoid DNS.
func TestApplyDHTBootstrap(t *testing.T) {
	t.Parallel()

	t.Run("empty=noop", func(t *testing.T) {
		t.Parallel()
		cc := torrent.NewDefaultClientConfig()
		original := cc.DhtStartingNodes
		applyDHTBootstrap(cc, "")
		// DhtStartingNodes must not change.
		if original == nil && cc.DhtStartingNodes != nil {
			t.Error("empty nodes: DhtStartingNodes changed from nil to non-nil")
		}
		if original != nil && cc.DhtStartingNodes == nil {
			t.Error("empty nodes: DhtStartingNodes changed from non-nil to nil")
		}
		// Must be callable without panic.
		if cc.DhtStartingNodes != nil {
			getter := cc.DhtStartingNodes("udp")
			if getter == nil {
				t.Error("DhtStartingNodes(\"udp\") returned nil getter")
			}
		}
	})

	t.Run("two_custom_nodes", func(t *testing.T) {
		t.Parallel()
		cc := torrent.NewDefaultClientConfig()
		applyDHTBootstrap(cc, "1.2.3.4:6881, 5.6.7.8:6881")

		if cc.DhtStartingNodes == nil {
			t.Fatal("DhtStartingNodes should be non-nil after setting custom nodes")
		}
		getter := cc.DhtStartingNodes("udp")
		if getter == nil {
			t.Fatal("DhtStartingNodes(\"udp\") returned nil getter")
		}
		addrs, _ := getter()
		// Must include at least our two literal-IP nodes.
		// GlobalBootstrapAddrs may add more or fail (offline) — tolerate both.
		if len(addrs) < 2 {
			t.Errorf("expected at least 2 dht.Addr, got %d", len(addrs))
		}
		// Verify each custom IP:port appears in the result.
		wantIPs := map[string]bool{
			"1.2.3.4:6881": false,
			"5.6.7.8:6881": false,
		}
		for _, a := range addrs {
			key := a.String()
			if _, ok := wantIPs[key]; ok {
				wantIPs[key] = true
			}
		}
		for addr, found := range wantIPs {
			if !found {
				t.Errorf("custom node %s not found in getter result", addr)
			}
		}
	})

	t.Run("whitespace_and_empty_fields", func(t *testing.T) {
		t.Parallel()
		cc := torrent.NewDefaultClientConfig()
		// Commas with spaces and empty fields must be handled gracefully.
		applyDHTBootstrap(cc, "  , 1.2.3.4:6881 ,  ")
		if cc.DhtStartingNodes == nil {
			t.Fatal("DhtStartingNodes should be non-nil for non-empty valid entry")
		}
		getter := cc.DhtStartingNodes("udp4")
		if getter == nil {
			t.Fatal("DhtStartingNodes(\"udp4\") returned nil getter")
		}
		addrs, _ := getter()
		if len(addrs) < 1 {
			t.Errorf("expected at least 1 dht.Addr, got %d", len(addrs))
		}
	})
}

// TestFetchTrackerListProxyFallback confirms that an invalid proxy URL causes
// fetchTrackerList to fall back to a direct HTTP client and still parse the
// response successfully. Uses httptest.Server — no external network needed.
func TestFetchTrackerListProxyFallback(t *testing.T) {
	t.Parallel()

	const trackerList = "udp://tracker.example.com:6969/announce\nudp://open.tracker.example:1337/announce\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(trackerList))
	}))
	defer srv.Close()

	// "://bad" has an empty scheme — url.Parse returns an error in fetchTrackerList,
	// which falls back to a plain direct transport and still fetches successfully.
	got := fetchTrackerList(srv.URL, "://bad")
	if len(got) == 0 {
		t.Fatal("expected at least one tracker URL, got empty slice (proxy fallback failed?)")
	}
	if got[0] != "udp://tracker.example.com:6969/announce" {
		t.Errorf("unexpected first tracker: %s", got[0])
	}
}
