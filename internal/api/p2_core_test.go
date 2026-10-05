// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/huin/goupnp"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// genCertPair returns a freshly generated self-signed cert + key PEM pair.
func genCertPair(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*.test.stremio.rocks"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
	return certPEM, keyPEM
}

func TestInstallValidatedCert_RejectsInvalidKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	goodCert, goodKey := genCertPair(t)
	if err := installValidatedCert(dir, goodCert, goodKey); err != nil {
		t.Fatalf("install valid pair: %v", err)
	}
	_, otherKey := genCertPair(t)

	cases := map[string][2]string{
		"mismatched key": {goodCert, otherKey},
		"garbage":        {"not pem", "not pem either"},
		"truncated cert": {goodCert[:len(goodCert)/2], goodKey},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := installValidatedCert(dir, c[0], c[1])
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var pe *provisionError
			if !errors.As(err, &pe) || pe.status != http.StatusBadGateway {
				t.Errorf("err = %#v; want *provisionError 502", err)
			}
			gotCert, _ := os.ReadFile(filepath.Join(dir, "https-cert.pem"))
			gotKey, _ := os.ReadFile(filepath.Join(dir, "https-key.pem"))
			if string(gotCert) != goodCert || string(gotKey) != goodKey {
				t.Error("existing cert/key were modified by a rejected install")
			}
			if _, err := os.Stat(filepath.Join(dir, "https-cert.pem.bak")); err == nil {
				t.Error("stray .bak left behind")
			}
		})
	}
}

func TestProvisionCert_SerializedByMutex(t *testing.T) {
	provisionMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		_, _ = ProvisionCert(t.TempDir(), "", "") // fails fast once the lock is acquired
		close(done)
	}()
	<-started
	select {
	case <-done:
		provisionMu.Unlock()
		t.Fatal("ProvisionCert ran while provisionMu was held")
	default:
	}
	provisionMu.Unlock()
	<-done
}

func TestValidateUPnPIP(t *testing.T) {
	cases := map[string]bool{
		"192.168.1.20":    true,
		"10.0.0.5":        true,
		"172.16.3.4":      true,
		"169.254.10.10":   true,
		"fe80::1":         true,
		"127.0.0.1":       false,
		"::1":             false,
		"0.0.0.0":         false,
		"169.254.169.254": false,
		"8.8.8.8":         false,
		"224.0.0.251":     false,
	}
	for s, ok := range cases {
		err := validateUPnPIP(net.ParseIP(s))
		if (err == nil) != ok {
			t.Errorf("validateUPnPIP(%s) err=%v; want allowed=%v", s, err, ok)
		}
	}
	if validateUPnPIP(nil) == nil {
		t.Error("nil IP must be rejected")
	}
}

func TestUPnPHTTPClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("loopback server must not be reached")
	}))
	defer srv.Close()
	resp, err := upnpHTTPClient.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected loopback dial to be blocked")
	}
	if goupnp.HTTPClientDefault != upnpHTTPClient {
		t.Error("goupnp.HTTPClientDefault is not the guarded client")
	}
}

func TestLimitedBodyTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 100)))
	}))
	defer srv.Close()

	// Declared Content-Length above the cap is rejected outright.
	c := &http.Client{Transport: &limitedBodyTransport{rt: http.DefaultTransport, max: 10}}
	if resp, err := c.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected oversize response to be rejected")
	}

	// Chunked (unknown length) bodies are truncated at the cap.
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		for range 10 {
			_, _ = w.Write([]byte(strings.Repeat("b", 10)))
			fl.Flush()
		}
	}))
	defer chunked.Close()
	resp, err := c.Get(chunked.URL)
	if err != nil {
		t.Fatalf("chunked Get: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 200)
	n := 0
	for {
		m, rerr := resp.Body.Read(buf[n:])
		n += m
		if rerr != nil {
			break
		}
	}
	if n != 10 {
		t.Errorf("read %d bytes; want 10 (capped)", n)
	}
}

func TestUPnPLocationAndSameHost(t *testing.T) {
	loc, _ := url.Parse("http://192.168.1.20:49152/desc.xml")
	ctl, _ := url.Parse("http://192.168.1.20:8080/ctl")
	evil, _ := url.Parse("http://127.0.0.1:8080/ctl")
	ftp, _ := url.Parse("ftp://192.168.1.20/ctl")
	if !upnpLocationOK(loc) {
		t.Error("valid location rejected")
	}
	if upnpLocationOK(ftp) || upnpLocationOK(nil) {
		t.Error("non-http(s)/nil location accepted")
	}
	if !upnpSameHost(loc, ctl) {
		t.Error("same-host control URL rejected")
	}
	if upnpSameHost(loc, evil) || upnpSameHost(loc, ftp) || upnpSameHost(loc, nil) {
		t.Error("off-host/non-http control URL accepted")
	}
}

func TestRedactRequestURI(t *testing.T) {
	cases := map[string]string{
		"/ftp/x.mkv?lz=NoIgVgNg&a=1":                    "/ftp/x.mkv?lz=REDACTED&a=1",
		"/nzb/create?lz=abc":                            "/nzb/create?lz=REDACTED",
		"/x?apikey=sek&TOKEN=t&keep=v":                  "/x?apikey=REDACTED&TOKEN=REDACTED&keep=v",
		"/proxy/stream?d=aHR0cA&api_password=hunter2":   "/proxy/stream?d=aHR0cA&api_password=REDACTED",
		"/probe?url=ftp%3A%2F%2Fuser%3Apass%40host%2Ff": "/probe?url=ftp%3A%2F%2FREDACTED@host%2Ff",
		"/probe?url=http://user:pw@host/f":              "/probe?url=http://REDACTED@host/f",
		"/proxy/d=http%3A%2F%2Fu%3Ap%40h/x":             "/proxy/d=http%3A%2F%2FREDACTED@h/x",
		"/e?email=a@b.c&lz=%zz":                         "/e?email=a@b.c&lz=REDACTED",
		"/plain/path?x=y":                               "/plain/path?x=y",
		"/plain":                                        "/plain",
		"/ftp/x?lzz=notsecret":                          "/ftp/x?lzz=notsecret",
		"/ftp/x?lz":                                     "/ftp/x?lz",
	}
	for in, want := range cases {
		u, err := url.ParseRequestURI(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := redactRequestURI(u); got != want {
			t.Errorf("redact(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFilterRequestTrackers(t *testing.T) {
	in := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"https://tracker.example.com/announce",
		"http://127.0.0.1:8080/admin",
		"http://localhost/x",
		"http://foo.localhost/x",
		"udp://[::1]:80/announce",
		"http://0.0.0.0/a",
		"http://169.254.169.254/latest/meta-data",
		"http://169.254.1.1/a",
		"http://192.168.1.10:6969/announce", // LAN trackers stay allowed
		"  wss://tracker.example.org/ws  ",
		"http://%zz",
	}
	want := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"https://tracker.example.com/announce",
		"http://192.168.1.10:6969/announce",
		"  wss://tracker.example.org/ws  ",
	}
	got := filterRequestTrackers(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterRequestTrackers = %v; want %v", got, want)
	}
	if got := filterRequestTrackers(nil); len(got) != 0 {
		t.Errorf("nil input -> %v", got)
	}
}

// trackerSpyEM records the AddOptions passed to EnsureEngine.
type trackerSpyEM struct {
	*fakeEM
	last types.AddOptions
}

func (m *trackerSpyEM) EnsureEngine(ih string, o types.AddOptions) (types.Engine, error) {
	m.last = o
	return m.fakeEM.EnsureEngine(ih, o)
}

func TestHandleStreamDropsLoopbackTrackers(t *testing.T) {
	em := &trackerSpyEM{fakeEM: newFakeEM(testEngine())}
	h := New(em, &fakeSS{}, &fakeProber{}, types.Config{HTTPPort: 11470})
	q := url.Values{"tr": {"http://127.0.0.1:9/a", "udp://tracker.example.com:80"}}
	rec := serve(t, h, http.MethodGet, "/"+testIH+"/0?"+q.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	want := []string{"udp://tracker.example.com:80"}
	if !reflect.DeepEqual(em.last.Trackers, want) {
		t.Errorf("trackers = %v; want %v", em.last.Trackers, want)
	}
}

func TestSubtitleAndHLSHandlersSetActiveContentGuards(t *testing.T) {
	h := newHandler(t, testEngine())
	rec := serve(t, h, http.MethodGet, "/"+testIH+"/0/subtitles.vtt", nil)
	assertActiveContentGuards(t, rec)
	rec = serve(t, h, http.MethodGet, "/subtitles.vtt?from="+url.QueryEscape("http://example.com/a.srt"), nil)
	assertActiveContentGuards(t, rec)
}
