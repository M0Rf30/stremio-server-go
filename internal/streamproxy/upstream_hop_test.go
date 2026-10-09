// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// startSocks5 runs a minimal no-auth SOCKS5 server on 127.0.0.1 that answers
// every CONNECT, then serves one canned HTTP response on the tunnelled conn.
func startSocks5(t *testing.T, connects *atomic.Int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
					return
				}
				_, _ = c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, buf[:4]); err != nil {
					return
				}
				switch buf[3] {
				case 1:
					_, _ = io.ReadFull(c, buf[:6])
				case 3:
					_, _ = io.ReadFull(c, buf[:1])
					_, _ = io.ReadFull(c, buf[:int(buf[0])+2])
				case 4:
					_, _ = io.ReadFull(c, buf[:18])
				}
				connects.Add(1)
				_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				_ = req.Body.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: video/mp2t\r\nConnection: close\r\n\r\nok")
			}(c)
		}
	}()
	return "socks5://" + ln.Addr().String()
}

func hopStreamReq(dest string) *http.Request {
	d := base64.RawURLEncoding.EncodeToString([]byte(dest))
	return httptest.NewRequest(http.MethodGet, "/proxy/stream?d="+d+"&api_password=s", nil)
}

// The operator-configured upstream proxy on loopback must be dialable even
// when private destinations are blocked; destinations remain guarded.
func TestUpstreamProxyOnLoopbackReachableButDestsGuarded(t *testing.T) {
	var connects atomic.Int32
	socks := startSocks5(t, &connects)

	var httpHits atomic.Int32
	httpProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer httpProxy.Close()

	for name, up := range map[string]string{"socks5": socks, "http": httpProxy.URL} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{Password: "s", UpstreamProxy: up, BlockPrivate: true})
			defer h.Close()

			w := httptest.NewRecorder()
			h.serveStream(w, hopStreamReq("http://203.0.113.5/video.ts"))
			if w.Code != http.StatusOK || w.Body.String() != "ok" {
				t.Fatalf("public dest via loopback upstream: code=%d body=%q", w.Code, w.Body.String())
			}

			before := connects.Load() + httpHits.Load()
			for _, dest := range []string{"http://127.0.0.1/x", "http://169.254.169.254/latest"} {
				w := httptest.NewRecorder()
				h.serveStream(w, hopStreamReq(dest))
				if w.Code != http.StatusForbidden {
					t.Errorf("dest %s: code=%d, want 403", dest, w.Code)
				}
			}
			if after := connects.Load() + httpHits.Load(); after != before {
				t.Errorf("rejected destinations reached the upstream proxy (%d -> %d)", before, after)
			}
		})
	}
}

// A client-supplied ?proxy= on loopback stays blocked on a protected handler
// (only the configured upstream is exempt), and the exemption is port-exact.
func TestUpstreamHopControlPortExact(t *testing.T) {
	ctl := upstreamHopControl("socks5://127.0.0.1:18080")
	if err := ctl("tcp", "127.0.0.1:18080", nil); err != nil {
		t.Errorf("proxy host:port rejected: %v", err)
	}
	if err := ctl("tcp", "127.0.0.1:9999", nil); err == nil {
		t.Error("other loopback port allowed")
	}
	if err := ctl("tcp", "169.254.169.254:18080", nil); err == nil {
		t.Error("cloud metadata allowed")
	}
	h := New(Config{Password: "s", UpstreamProxy: "socks5://127.0.0.1:18080"})
	defer h.Close()
	if err := h.validateProxyHost("socks5://127.0.0.1:9999"); err == nil {
		t.Error("client-supplied loopback proxy allowed on protected handler")
	}
}
