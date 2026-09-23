// Command libstremio builds libstremio-server.so, a c-shared version of
// stremio-server for SELinux-enforcing Android hosts where the shell's exec()
// of a bundled executable is blocked by policy but dlopen() of a shared
// library dropped next to the calling app's own native libs is not. It wraps
// internal/app.Run behind a tiny, panic-safe C ABI so a Kodi addon (or any
// other host process) can start/stop the server on a background thread
// in-process instead of spawning a child.
//
// Build: go build -buildmode=c-shared -o libstremio-server.so ./cmd/libstremio
//
// No os.Exit and no signal.Notify are reachable from this package or from
// internal/app: a fatal condition here must return an error code to the
// caller, never terminate the host process, and no signal handler is
// installed that could steal SIGINT/SIGTERM from the embedding app.
package main

/*
 */
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/app"
	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// stopGrace bounds how long ServerStop waits for a blocking ServerStart call
// to return after cancellation before giving up (app.Run's own listener
// shutdowns are bounded at 5s each; this adds slack for goroutine teardown).
const stopGrace = 6 * time.Second

// serverVersionCStr is allocated exactly once and never freed: ServerVersion
// is documented as returning a static string the caller must not free, so a
// per-call C.CString (which the caller would then be responsible for
// freeing) would violate the contract. Deliberately leaked for the life of
// the process, like any other .rodata string.
var serverVersionCStr = C.CString(app.DefaultVersion)

// mu guards the fields below, which together describe the single in-process
// server lifecycle library mode supports. A Start after a Stop finds cancel
// and stopped both nil (Start's own goroutine-equivalent — the blocking
// ServerStart call itself — clears them right before returning), so there is
// no leaked sync.Once or other one-shot gate that would make a second Start
// silently do nothing.
var (
	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
)

// ServerVersion returns the Stremio-compatible API version string
// (settings.serverVersion), e.g. "4.21.0". The returned pointer is static;
// the caller must not free it.
//
//export ServerVersion
func ServerVersion() *C.char {
	defer func() { _ = recover() }()
	return serverVersionCStr
}

// ServerStart runs the server until ServerStop is called or an
// unrecoverable error occurs. It BLOCKS the calling thread for the entire
// server lifetime — callers embed it in a dedicated worker thread. logPath
// receives appended log output (created if missing); a nil/empty logPath
// falls back to the process's stderr. envJSON is a JSON object of string
// env-var values (APP_PATH, HTTP_PORT, STREMIO_*, ...) — the same knobs the
// stremio-server executable reads from its real environment; a nil/empty
// envJSON means "use every default".
//
// Returns 0 on a clean stop (ServerStop called, or ctx-equivalent shutdown),
// 1 on an init or runtime error, 2 if a server is already running.
//
//export ServerStart
func ServerStart(logPath *C.char, envJSON *C.char) (ret C.int) {
	defer func() {
		if r := recover(); r != nil {
			logging.For("libstremio").Error("recovered from panic in ServerStart", "panic", r)
			mu.Lock()
			cancel = nil
			stopped = nil
			mu.Unlock()
			ret = 1
		}
	}()

	mu.Lock()
	if cancel != nil {
		mu.Unlock()
		return 2
	}
	runCtx, cancelFn := context.WithCancel(context.Background())
	cancel = cancelFn
	myStopped := make(chan struct{})
	stopped = myStopped
	mu.Unlock()

	// Always clear the shared lifecycle state before returning, however we
	// get there (clean stop, init error, or the recover above), so a later
	// ServerStart is never wrongly told "already running".
	defer func() {
		mu.Lock()
		cancel = nil
		stopped = nil
		mu.Unlock()
		close(myStopped)
	}()

	logw, closeLog := openLog(logPath)
	defer closeLog()

	envMap, err := parseEnvJSON(envJSON)
	if err != nil {
		logging.For("libstremio").Error("invalid envJSON", "err", err)
		return 1
	}

	cfg := app.Config{Lookup: app.MapLookup(envMap)}
	if v, ok := envMap["STREMIO_SERVER_VERSION"]; ok && v != "" {
		cfg.Version = v
	}

	if err := app.Run(runCtx, cfg, logw); err != nil {
		logging.For("libstremio").Error("server exited with error", "err", err)
		return 1
	}
	return 0
}

// ServerStop requests a graceful shutdown (5s per listener, matching
// internal/app.Run) of a running ServerStart call and waits for it to
// return, up to stopGrace. Safe to call when no server is running (returns 0
// immediately) and safe to call more than once.
//
//export ServerStop
func ServerStop() (ret C.int) {
	defer func() {
		if r := recover(); r != nil {
			logging.For("libstremio").Error("recovered from panic in ServerStop", "panic", r)
			ret = 0 // Stop is documented as always "safe"; never surface a Stop failure code
		}
	}()

	mu.Lock()
	c := cancel
	s := stopped
	mu.Unlock()

	if c == nil {
		return 0 // not running
	}
	c()
	if s != nil {
		select {
		case <-s:
		case <-time.After(stopGrace):
			logging.For("libstremio").Warn("ServerStop: timed out waiting for ServerStart to return")
		}
	}
	return 0
}

// parseEnvJSON decodes a JSON object of string env-var values. A nil or
// blank envJSON yields an empty map (every knob defaults).
func parseEnvJSON(envJSON *C.char) (map[string]string, error) {
	if envJSON == nil {
		return map[string]string{}, nil
	}
	raw := strings.TrimSpace(C.GoString(envJSON))
	if raw == "" {
		return map[string]string{}, nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("decode envJSON: %w", err)
	}
	return m, nil
}

// openLog opens logPath for appending (creating it if needed) and returns it
// plus a close func. A nil/blank logPath, or a path that fails to open,
// falls back to os.Stderr with a no-op close.
func openLog(logPath *C.char) (io.Writer, func()) {
	if logPath == nil {
		return os.Stderr, func() {}
	}
	p := strings.TrimSpace(C.GoString(logPath))
	if p == "" {
		return os.Stderr, func() {}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return os.Stderr, func() {}
	}
	return f, func() { _ = f.Close() }
}

// main is required for a c-shared buildmode package but is never executed;
// the library has no process lifetime of its own.
func main() {}
