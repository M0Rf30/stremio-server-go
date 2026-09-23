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
