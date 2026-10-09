// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package ftpstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"testing"
	"time"
)

// waitCtxHookFired blocks until the request-context hook of rc's session has
// force-closed the data connection of the lease — i.e. until the cancelled
// request context has really been acted upon. It makes the abort tests
// deterministic: context.AfterFunc runs its function in its own goroutine, so
// without this a handler could race ahead of the hook and Close the stream
// before it fired (which is not the case under test).
func waitCtxHookFired(tb testing.TB, rc io.ReadCloser) {
	tb.Helper()
	f, ok := rc.(*ftpReadCloser)
	if !ok {
		tb.Errorf("stream is %T, want *ftpReadCloser", rc)
		return
	}
	l := f.sess.lease.Load()
	if l == nil {
		tb.Error("session has no lease")
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.data.mu.Lock()
		closed := l.data.closed
		l.data.mu.Unlock()
		if closed {
			return
		}
		if time.Now().After(deadline) {
			tb.Error("request-context hook never fired")
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// TestOpenFTPRequestAbortSessionReused is the real seek pattern: an HTTP
// handler opens the FTP stream with the *request* context, the client reads a
// few KiB and disconnects (an aborted range request), the context is
// cancelled and the handler closes the stream. Each abort used to discard the
// logged-in control connection (the cancel hook force-closed it), so N aborted
// seeks cost N logins; they must now share one session.
func TestOpenFTPRequestAbortSessionReused(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(8 << 20)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	ftpURL := "ftp://" + srv.addr() + "/movie.mkv"

	const first = 4096
	handled := make(chan struct{}, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { handled <- struct{}{} }()
		off, _ := strconv.ParseInt(r.URL.Query().Get("off"), 10, 64)
		rc, size, pos, err := OpenRanged(r.Context(), ftpURL, off)
		if err != nil {
			t.Errorf("OpenRanged(off=%d): %v", off, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("Content-Length", strconv.FormatInt(size-pos, 10))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, first)
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Errorf("read(off=%d): %v", off, err)
			return
		}
		_, _ = w.Write(buf)
		w.(http.Flusher).Flush()
		// The client now disconnects mid-body: wait for the request context
		// to be cancelled and acted upon, then try to carry on and return.
		<-r.Context().Done()
		waitCtxHookFired(t, rc)
		_, _ = io.Copy(io.Discard, rc)
	}))
	defer hs.Close()

	offsets := []int64{0, 1 << 20, 5 << 20, 100, 7 << 20}
	for _, off := range offsets {
		resp, err := http.Get(fmt.Sprintf("%s/?off=%d", hs.URL, off))
		if err != nil {
			t.Fatalf("GET off=%d: %v", off, err)
		}
		got := make([]byte, first)
		if _, err := io.ReadFull(resp.Body, got); err != nil {
			t.Fatalf("client read off=%d: %v", off, err)
		}
		if want := content[off : off+first]; !bytes.Equal(got, want) {
			t.Fatalf("off=%d body mismatch", off)
		}
		_ = resp.Body.Close() // disconnect mid-body
		select {
		case <-handled:
		case <-time.After(10 * time.Second):
			t.Fatalf("handler for off=%d did not finish after the client disconnected", off)
		}
		if got := poolIdle(); got != 1 {
			t.Fatalf("after aborted request off=%d: idle sessions = %d, want 1 (logins=%d conns=%d)",
				off, got, srv.logins.Load(), srv.conns.Load())
		}
	}
	t.Logf("aborted seeks=%d control_conns=%d logins=%d", len(offsets), srv.conns.Load(), srv.logins.Load())
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
		t.Errorf("control conns = %d, logins = %d; want 1 and 1 for %d aborted seeks", c, l, len(offsets))
	}
}

// TestOpenFTPCtxCancelDuringOpenUnblocks guards the part of the cancel hook
// that must stay aggressive: while the session is still opening (greeting,
// login, SIZE, RETR — a control-connection read may be pending) a cancelled
// context has to kill the control connection too, or the goroutine would sit
// there for the whole idle timeout.
func TestOpenFTPCtxCancelDuringOpenUnblocks(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{silentGreeting: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, _, err := openFTP(ctx, "ftp://"+srv.addr()+"/f", 0)
		errc <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); srv.conns.Load() == 0; { // the open is now parked on the greeting read
		if time.Now().After(deadline) {
			t.Fatal("open never reached the server")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected an error after cancelling a stalled open")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open still blocked on the control connection after ctx cancel")
	}
}

// setAbortGrace shortens the wait for the final reply of an aborted transfer
// for the duration of the test, so tests against a server that never answers
// do not sit out the production grace.
func setAbortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := ftpAbortGrace
	ftpAbortGrace = d
	t.Cleanup(func() { ftpAbortGrace = old })
}

// TestOpenFTPCtxDeadlineSessionReused covers an aborted request whose context
// ended by deadline: the deadline is long past when Close reads the server's
// reply, which must therefore not be bounded by it (or every read would fail
// instantly and the healthy session be thrown away).
func TestOpenFTPCtxDeadlineSessionReused(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(8 << 20)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	u := "ftp://" + srv.addr() + "/big"

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	rc, _, err := openFTP(ctx, u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	waitCtxHookFired(t, rc)
	_ = rc.Close()
	if got := poolIdle(); got != 1 {
		t.Fatalf("idle sessions after a deadline abort = %d, want 1", got)
	}
	got, _ := readRange(t, u, 1<<20, 16)
	if !bytes.Equal(got, content[1<<20:1<<20+16]) {
		t.Errorf("open after deadline abort read %q", got)
	}
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
		t.Errorf("control conns = %d, logins = %d; want 1 and 1", c, l)
	}
}

// TestOpenFTPCtxCancelAfterCompleteTransferSessionReused covers the abort
// that lands after the server finished the whole transfer (a small file read
// only partly): the final reply is a plain 226, and the data connection the
// cancel hook already closed must not turn Close's result into an error that
// hides it — the session is still in sync and must be pooled.
func TestOpenFTPCtxCancelAfterCompleteTransferSessionReused(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(64 << 10)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	u := "ftp://" + srv.addr() + "/small"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rc, _, err := openFTP(ctx, u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); srv.completed.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("server never finished the transfer")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	waitCtxHookFired(t, rc)
	if err := rc.Close(); err != nil {
		t.Errorf("Close = %v, want nil (server replied 226)", err)
	}
	if got := poolIdle(); got != 1 {
		t.Fatalf("idle sessions = %d, want 1", got)
	}
	got, _ := readRange(t, u, 100, 16)
	if !bytes.Equal(got, content[100:116]) {
		t.Errorf("open after abort read %q", got)
	}
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
		t.Errorf("control conns = %d, logins = %d; want 1 and 1", c, l)
	}
}

// poolOneSession parks one healthy session for u (one control connection, one
// login) and fails the test if that did not happen.
func poolOneSession(t *testing.T, srv *fakeFTPServer, u string) {
	t.Helper()
	readRange(t, u, 0, 8)
	if got := poolIdle(); got != 1 {
		t.Fatalf("idle sessions = %d, want 1", got)
	}
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
		t.Fatalf("setup: control conns = %d, logins = %d; want 1 and 1", c, l)
	}
}

// TestOpenFTPRefusedRetrKeepsPooledSession verifies a complete negative reply
// (550 no such file, 450 busy, 425 no data connection) on a pooled session
// is returned as is: the session is healthy, so it is neither discarded nor
// is a fresh one dialled just to be refused again. Previously every such
// failure cost a QUIT, a redial, a login and a second identical RETR.
func TestOpenFTPRefusedRetrKeepsPooledSession(t *testing.T) {
	for _, tc := range []struct {
		reply string
		code  int
	}{
		{"550 no such file", 550},
		{"450 file busy", 450},
		{"425 can't open data connection", 425},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
			resetFTPCaches(t)
			content := poolTestContent(1000)
			srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
			u := "ftp://" + srv.addr() + "/f"
			poolOneSession(t, srv, u)
			retrs := srv.retrs.Load()

			reply := tc.reply
			srv.retrReply.Store(&reply)
			_, _, err := openFTP(t.Context(), u, 100)
			var te *textproto.Error
			if !errors.As(err, &te) || te.Code != tc.code {
				t.Fatalf("open error = %v, want the server's %d reply", err, tc.code)
			}
			if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
				t.Errorf("control conns = %d, logins = %d; want 1 and 1 (no redial for a refusal)", c, l)
			}
			if got := srv.retrs.Load() - retrs; got != 1 {
				t.Errorf("RETR sent %d times, want 1 (a refusal is not retried)", got)
			}
			if got := poolIdle(); got != 1 {
				t.Errorf("idle sessions after a refusal = %d, want 1 (healthy session kept)", got)
			}

			// And the kept session really is usable.
			srv.retrReply.Store(nil)
			got, _ := readRange(t, u, 100, 16)
			if !bytes.Equal(got, content[100:116]) {
				t.Errorf("open after refusal read %q", got)
			}
			if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
				t.Errorf("after recovery: control conns = %d, logins = %d; want 1 and 1", c, l)
			}
		})
	}
}

// TestOpenFTPServerClosingStillRedials verifies the other side of the retry
// rule: 421 (service closing the control connection) is a sign the pooled
// session is gone, so one redial on a fresh session is still made.
func TestOpenFTPServerClosingStillRedials(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(1000)})
	u := "ftp://" + srv.addr() + "/f"
	poolOneSession(t, srv, u)

	reply := "421 service not available, closing control connection"
	srv.retrReply.Store(&reply)
	_, _, err := openFTP(t.Context(), u, 100)
	var te *textproto.Error
	if !errors.As(err, &te) || te.Code != 421 {
		t.Fatalf("open error = %v, want the server's 421 reply", err)
	}
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 2 || l != 2 {
		t.Errorf("control conns = %d, logins = %d; want 2 and 2 (one redial after 421)", c, l)
	}
	if got := poolIdle(); got != 0 {
		t.Errorf("idle sessions after 421 = %d, want 0 (server closed them)", got)
	}
}

// TestOpenFTPDataDialFailureKeepsPooledSession verifies a data connection
// that cannot be dialled (here: a closed port; in production also a dial the
// SSRF guard rejects) is reported without sacrificing the pooled control
// connection: the PASV/EPSV reply was consumed, so the session is in sync and
// a redial would only fail the same way.
func TestOpenFTPDataDialFailureKeepsPooledSession(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(1000)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	u := "ftp://" + srv.addr() + "/f"
	poolOneSession(t, srv, u)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := int64(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	srv.epsvPort.Store(deadPort)

	if _, _, err := openFTP(t.Context(), u, 100); err == nil {
		t.Fatal("open succeeded against a data port nobody listens on")
	}
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 1 || l != 1 {
		t.Errorf("control conns = %d, logins = %d; want 1 and 1 (no redial for a data dial failure)", c, l)
	}
	if got := poolIdle(); got != 1 {
		t.Errorf("idle sessions after a data dial failure = %d, want 1", got)
	}

	srv.epsvPort.Store(0)
	got, _ := readRange(t, u, 100, 16)
	if !bytes.Equal(got, content[100:116]) {
		t.Errorf("open after data dial failure read %q", got)
	}
	if c := srv.conns.Load(); c != 1 {
		t.Errorf("after recovery: control conns = %d, want 1", c)
	}
}
