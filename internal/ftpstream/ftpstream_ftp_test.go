// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package ftpstream

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeFTPServer is a minimal FTP server that speaks just enough of RFC 959
// for jlaffaye/ftp to Login, SIZE and RETR. Behaviour is tunable per test.
type fakeFTPServer struct {
	ln net.Listener
	// silentGreeting makes the server accept but never send the 220 banner.
	silentGreeting bool
	// stallData makes RETR send first bytes and then block until the test ends.
	stallData bool
	content   []byte
	done      chan struct{}
}

func newFakeFTPServer(t *testing.T, f *fakeFTPServer) *fakeFTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.ln = ln
	f.done = make(chan struct{})
	t.Cleanup(func() {
		close(f.done)
		_ = ln.Close()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeFTPServer) addr() string { return f.ln.Addr().String() }

func (f *fakeFTPServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	go func() { <-f.done; _ = c.Close() }()
	if f.silentGreeting {
		<-f.done
		return
	}
	w := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	w("220 fake ready")
	var dataLn net.Listener
	defer func() {
		if dataLn != nil {
			_ = dataLn.Close()
		}
	}()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		cmd, _, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			w("331 need password")
		case "PASS":
			w("230 logged in")
		case "FEAT":
			w("502 not implemented")
		case "TYPE":
			w("200 ok")
		case "SIZE":
			w(fmt.Sprintf("213 %d", len(f.content)))
		case "EPSV":
			dataLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				w("425 no data")
				continue
			}
			port := dataLn.Addr().(*net.TCPAddr).Port
			w(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", port))
		case "REST":
			w("350 ok")
		case "RETR":
			if dataLn == nil {
				w("425 no data")
				continue
			}
			w("150 opening")
			dc, err := dataLn.Accept()
			if err != nil {
				return
			}
			if f.stallData {
				_, _ = dc.Write(f.content[:1])
				go func() { <-f.done; _ = dc.Close() }()
				continue
			}
			_, _ = dc.Write(f.content)
			_ = dc.Close()
			w("226 done")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("502 not implemented")
		}
	}
}

func TestOpenFTPStalledGreetingTimesOut(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	old := ftpIdleTimeout
	ftpIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ftpIdleTimeout = old })

	srv := newFakeFTPServer(t, &fakeFTPServer{silentGreeting: true})

	errc := make(chan error, 1)
	go func() {
		_, _, err := openFTP(t.Context(), "ftp://"+srv.addr()+"/f", 0)
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected error from stalled greeting")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("openFTP did not time out against a stalled server")
	}
}

func TestOpenFTPReadsContent(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	want := []byte("hello ftp world")
	srv := newFakeFTPServer(t, &fakeFTPServer{content: want})

	rc, size, err := openFTP(t.Context(), "ftp://"+srv.addr()+"/f", 0)
	if err != nil {
		t.Fatalf("openFTP: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if size != int64(len(want)) {
		t.Errorf("size = %d, want %d", size, len(want))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOpenFTPCtxCancelUnblocksRead(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	srv := newFakeFTPServer(t, &fakeFTPServer{content: []byte("abcdef"), stallData: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rc, _, err := openFTP(ctx, "ftp://"+srv.addr()+"/f", 0)
	if err != nil {
		t.Fatalf("openFTP: %v", err)
	}
	defer func() { _ = rc.Close() }()

	buf := make([]byte, 16)
	if n, err := rc.Read(buf); err != nil || n != 1 {
		t.Fatalf("first Read = %d, %v; want 1, nil", n, err)
	}

	readErr := make(chan error, 1)
	go func() {
		_, err := rc.Read(buf) // blocks: server stalls after the first byte
		readErr <- err
	}()
	cancel()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("expected read error after ctx cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Read still blocked after ctx cancel")
	}
}
