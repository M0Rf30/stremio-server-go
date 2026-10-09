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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFTPServer is a minimal FTP server that speaks just enough of RFC 959
// for jlaffaye/ftp to Login, SIZE, REST and RETR. Behaviour is tunable per
// test and the server counts what it served so tests can assert how many
// control connections / logins / commands a sequence of opens cost.
type fakeFTPServer struct {
	ln net.Listener
	// silentGreeting makes the server accept but never send the 220 banner.
	silentGreeting bool
	// stallData makes RETR send first bytes and then block until the test ends.
	stallData bool
	// noSize makes SIZE fail with 502 (server without SIZE support).
	noSize bool
	// delay is slept before every reply line to model a network RTT.
	delay   time.Duration
	content []byte
	done    chan struct{}

	// retrReply, when set, is sent (instead of serving a transfer) in answer
	// to every RETR — e.g. "550 no such file". A "421 ..." reply also drops
	// the control connection, as a server closing on the client does.
	retrReply atomic.Pointer[string]
	// epsvPort, when non-zero, is advertised by EPSV instead of the port of
	// the real data listener (e.g. a closed port, so the data dial fails).
	epsvPort atomic.Int64

	// Counters (control connections accepted, PASS, SIZE, REST, RETR, QUIT).
	conns, logins, sizes, rests, retrs, quits atomic.Int64
	// completed counts transfers the server finished writing (226 sent).
	completed atomic.Int64

	ctlMu sync.Mutex
	ctl   []net.Conn
}

// dropControl closes every accepted control connection, emulating a server
// idle-timeout / restart that kills pooled sessions client-side.
func (f *fakeFTPServer) dropControl() {
	f.ctlMu.Lock()
	defer f.ctlMu.Unlock()
	for _, c := range f.ctl {
		_ = c.Close()
	}
	f.ctl = nil
}

func newFakeFTPServer(t testing.TB, f *fakeFTPServer) *fakeFTPServer {
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
	f.conns.Add(1)
	f.ctlMu.Lock()
	f.ctl = append(f.ctl, c)
	f.ctlMu.Unlock()
	if f.silentGreeting {
		<-f.done
		return
	}
	w := func(s string) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		_, _ = io.WriteString(c, s+"\r\n")
	}
	w("220 fake ready")
	var dataLn net.Listener
	closeData := func() {
		if dataLn != nil {
			_ = dataLn.Close()
			dataLn = nil
		}
	}
	defer closeData()
	var restOff int64
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			w("331 need password")
		case "PASS":
			f.logins.Add(1)
			w("230 logged in")
		case "FEAT":
			w("502 not implemented")
		case "TYPE":
			w("200 ok")
		case "NOOP":
			w("200 ok")
		case "SIZE":
			f.sizes.Add(1)
			if f.noSize {
				w("502 SIZE not implemented")
				continue
			}
			w(fmt.Sprintf("213 %d", len(f.content)))
		case "EPSV":
			closeData()
			dataLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				w("425 no data")
				continue
			}
			port := int64(dataLn.Addr().(*net.TCPAddr).Port)
			if o := f.epsvPort.Load(); o != 0 {
				port = o
			}
			w(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", port))
		case "REST":
			f.rests.Add(1)
			restOff, _ = strconv.ParseInt(arg, 10, 64)
			w("350 ok")
		case "RETR":
			f.retrs.Add(1)
			if reply := f.retrReply.Load(); reply != nil {
				w(*reply)
				if strings.HasPrefix(*reply, "421") {
					return
				}
				continue
			}
			if dataLn == nil {
				w("425 no data")
				continue
			}
			w("150 opening")
			dc, err := dataLn.Accept()
			closeData()
			if err != nil {
				return
			}
			body := f.content[min(max(restOff, 0), int64(len(f.content))):]
			restOff = 0
			if f.stallData {
				_, _ = dc.Write(body[:min(1, len(body))])
				go func() { <-f.done; _ = dc.Close() }()
				continue
			}
			_, werr := dc.Write(body)
			_ = dc.Close()
			if werr != nil {
				// Client aborted the transfer mid-stream (as real servers do).
				w("426 transfer aborted")
				continue
			}
			f.completed.Add(1)
			w("226 done")
		case "QUIT":
			f.quits.Add(1)
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
	setAbortGrace(t, 100*time.Millisecond)
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
