package app

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// freePort asks the OS for an ephemeral TCP port, then releases it. There is
// an inherent (tiny) TOCTOU window before Run rebinds it — acceptable for a
// deterministic, offline unit test in an isolated CI sandbox.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("freePort: close: %v", err)
	}
	return port
}

// waitForPort polls addr until a TCP connection succeeds or timeout elapses.
func waitForPort(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to accept connections", addr)
}

// TestRunStartStopTwice proves the library-mode restart contract: Run must be
// callable a second time, on the same port, after a prior call returned from
// a clean ctx-cancel stop — with no leaked goroutines/state (janitor
// sync.Once elsewhere, HLS reaper, memory-reclaim ticker, cert renewer) that
// would make the second Start hang, error, or silently do nothing.
func TestRunStartStopTwice(t *testing.T) {
	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)

	for i := range 2 {
		env := map[string]string{
			"APP_PATH":                   t.TempDir(),
			"BIND_ADDRESS":               "127.0.0.1",
			"HTTP_PORT":                  strconv.Itoa(port),
			"HTTPS_PORT":                 "0", // disable HTTPS listener for this test
			"BT_LISTEN_PORT":             "0",
			"STREMIO_DISABLE_TRACKERS":   "1", // avoid upstream tracker/udp network flake
			"STREMIO_DISABLE_WEBTORRENT": "1", // avoid spawning pion goroutines
			"STREMIO_TRACKERS_URL":       "off",
			"STREMIO_METADATA_URL":       "off",
			"STREMIO_ENABLE_DLNA":        "0",
		}
		ctx, cancel := context.WithCancel(context.Background())
		runErr := make(chan error, 1)
		go func() { runErr <- Run(ctx, Config{Lookup: MapLookup(env)}, io.Discard) }()

		waitForPort(t, addr, 10*time.Second)

		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Fatalf("iteration %d: Run returned error: %v", i, err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("iteration %d: Run did not return after ctx cancel", i)
		}
	}
}

// findBindableNonLoopbackIPv4 returns a non-loopback IPv4 address this host
// can actually bind a TCP listener to (e.g. a LAN interface address), or ""
// if none is available — callers must skip rather than fail when empty,
// since CI sandboxes vary in what network interfaces they expose.
func findBindableNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		ln, lerr := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
		if lerr != nil {
			continue // listed but not actually bindable in this sandbox
		}
		_ = ln.Close()
		return ip.String()
	}
	return ""
}

// TestRunLoopbackFallbackForNonLoopbackBindAddress is the regression test
// for issue #20's "Related: Loopback self-URL vs BIND_ADDRESS" item: when
// BIND_ADDRESS pins the main listener to a specific non-loopback,
// non-wildcard interface, Run must also bring up a dedicated loopback
// listener on 127.0.0.1:HTTP_PORT (same handler) so ffmpeg/ffprobe
// self-requests keep working, in addition to the configured bind address
// actually serving traffic.
func TestRunLoopbackFallbackForNonLoopbackBindAddress(t *testing.T) {
	bindIP := findBindableNonLoopbackIPv4(t)
	if bindIP == "" {
		t.Skip("no bindable non-loopback IPv4 address available in this environment")
	}

	port := freePort(t)
	mainAddr := net.JoinHostPort(bindIP, strconv.Itoa(port))
	loopbackAddr := "127.0.0.1:" + strconv.Itoa(port)

	env := map[string]string{
		"APP_PATH":                   t.TempDir(),
		"BIND_ADDRESS":               bindIP,
		"HTTP_PORT":                  strconv.Itoa(port),
		"HTTPS_PORT":                 "0",
		"BT_LISTEN_PORT":             "0",
		"STREMIO_DISABLE_TRACKERS":   "1",
		"STREMIO_DISABLE_WEBTORRENT": "1",
		"STREMIO_TRACKERS_URL":       "off",
		"STREMIO_METADATA_URL":       "off",
		"STREMIO_ENABLE_DLNA":        "0",
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, Config{Lookup: MapLookup(env)}, io.Discard) }()

	waitForPort(t, mainAddr, 10*time.Second)
	// The fallback loopback listener must also be reachable, sharing the
	// same HTTP_PORT number as the configured bind address.
	waitForPort(t, loopbackAddr, 10*time.Second)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRunNoLoopbackFallbackForLoopbackBindAddress is the negative
// counterpart: BIND_ADDRESS=127.0.0.1 is already loopback, so no extra
// listener should be needed (Wildcard/loopback binds are unchanged).
// Run must still start and stop cleanly with only the main listener.
func TestRunNoLoopbackFallbackForLoopbackBindAddress(t *testing.T) {
	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	env := map[string]string{
		"APP_PATH":                   t.TempDir(),
		"BIND_ADDRESS":               "127.0.0.1",
		"HTTP_PORT":                  strconv.Itoa(port),
		"HTTPS_PORT":                 "0",
		"BT_LISTEN_PORT":             "0",
		"STREMIO_DISABLE_TRACKERS":   "1",
		"STREMIO_DISABLE_WEBTORRENT": "1",
		"STREMIO_TRACKERS_URL":       "off",
		"STREMIO_METADATA_URL":       "off",
		"STREMIO_ENABLE_DLNA":        "0",
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, Config{Lookup: MapLookup(env)}, io.Discard) }()

	waitForPort(t, addr, 10*time.Second)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
