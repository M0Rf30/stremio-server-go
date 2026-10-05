// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package ftpstream provides a unified Open function for streaming files from
// FTP/FTPS servers and HTTP/HTTPS URLs.
//
// FTP connections are established using the RFC 959 protocol. FTPS uses
// implicit TLS (the connection is wrapped in TLS from the start). HTTP/HTTPS
// connections use the standard net/http client with a Range header when an
// offset is requested.
package ftpstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
)

// ftpAllowPrivate reports whether STREMIO_FTP_ALLOW_PRIVATE opts into
// reaching private/loopback/RFC1918 addresses (e.g. a LAN NAS) from the
// unauthenticated /ftp?lz= route. Default is false (secure): only public
// addresses are reachable. The cloud-metadata address (169.254.169.254) is
// always blocked regardless, via netguard.ValidateIP's unconditional check.
func ftpAllowPrivate() bool {
	v := strings.TrimSpace(os.Getenv("STREMIO_FTP_ALLOW_PRIVATE"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
}

// preflightHost resolves host and rejects it up front when every candidate
// address is disallowed by netguard.ValidateIP, giving a fast, clear error
// before any protocol handshake begins. The real enforcement point remains
// the dialer's Control hook (netguard.DialControl via ftpDialControl /
// openFTP's dialer), which re-validates the specific resolved IP at connect
// time and forecloses DNS-rebinding; this is a defense-in-depth fast-fail
// layer, not a substitute for it. A DNS resolution failure is not itself
// treated as a block — the real dial/request attempt surfaces that error
// naturally.
func preflightHost(ctx context.Context, host string, blockPrivate bool) error {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	var firstErr error
	for _, a := range addrs {
		if verr := netguard.ValidateIP(a.IP, blockPrivate); verr != nil {
			if firstErr == nil {
				firstErr = verr
			}
			continue
		}
		return nil
	}
	return firstErr
}

// ftpDialControl is httpGuardedClient's net.Dialer.Control hook. It re-reads
// ftpAllowPrivate() on every dial — rather than baking the decision in at
// package-init time — so the pooled client's behaviour always reflects the
// current opt-in state.
func ftpDialControl(network, address string, c syscall.RawConn) error {
	return netguard.DialControl(!ftpAllowPrivate())(network, address, c)
}

// httpGuardedClient is a package-level HTTP client whose dialer rejects the
// cloud-metadata address and, by default, all private/loopback/RFC1918
// addresses at connect time (see ftpDialControl / STREMIO_FTP_ALLOW_PRIVATE).
// Reused across requests to pool connections.
var httpGuardedClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: ftpDialControl,
		}).DialContext,
	},
}

// ftpParsed holds the components extracted from an ftp:// or ftps:// URL.
type ftpParsed struct {
	addr string // host:port
	host string // hostname only, used as TLS ServerName
	user string
	pass string
	path string
	tls  bool // true for ftps://
}

// parseFTPURL extracts the dial address, credentials, path, and TLS flag from
// an ftp:// or ftps:// URL.
//
// Defaults: port 21, user "anonymous", empty password.
func parseFTPURL(rawURL string) (*ftpParsed, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ftpstream: parse URL: %w", err)
	}
	if u.Scheme != "ftp" && u.Scheme != "ftps" {
		return nil, fmt.Errorf("ftpstream: unsupported scheme %q (want ftp or ftps)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("ftpstream: missing host in URL %q", rawURL)
	}
	port := u.Port()
	if port == "" {
		port = "21"
	}
	user, pass := "anonymous", ""
	if u.User != nil {
		if n := u.User.Username(); n != "" {
			user = n
		}
		pass, _ = u.User.Password()
	}
	return &ftpParsed{
		addr: host + ":" + port,
		host: host,
		user: user,
		pass: pass,
		path: u.Path,
		tls:  u.Scheme == "ftps",
	}, nil
}

// ftpIdleTimeout bounds how long any single read or write on an FTP control
// or data connection may block with no progress. It is a variable so tests can
// shorten it.
var ftpIdleTimeout = 30 * time.Second

// ftpDialTimeout bounds the TCP connect of each FTP control/data connection.
const ftpDialTimeout = 10 * time.Second

// deadlineConn re-arms a per-operation deadline before every Read and Write so
// a stalling server cannot block the caller forever. The deadline is the
// earlier of now+idle and the request context's deadline (when it has one).
type deadlineConn struct {
	net.Conn
	idle  time.Duration
	ctxDL time.Time
}

func (c *deadlineConn) arm() {
	dl := time.Now().Add(c.idle)
	if !c.ctxDL.IsZero() && c.ctxDL.Before(dl) {
		dl = c.ctxDL
	}
	_ = c.SetDeadline(dl)
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	c.arm()
	return c.Conn.Read(p)
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	c.arm()
	return c.Conn.Write(p)
}

// ftpConns tracks every raw connection of one FTP session so ctx cancellation
// can force-close them all, unblocking any goroutine stuck in a Read.
type ftpConns struct {
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

// add registers c. It reports false (and closes c) when the set was already
// force-closed.
func (t *ftpConns) add(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = c.Close()
		return false
	}
	t.conns = append(t.conns, c)
	return true
}

func (t *ftpConns) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, c := range t.conns {
		_ = c.Close()
	}
	t.conns = nil
}

// ftpReadCloser wraps an FTP data response and its underlying control
// connection. Close drains the data response then quits the control connection.
type ftpReadCloser struct {
	resp *ftp.Response
	conn *ftp.ServerConn
	stop func() bool // detaches the ctx-cancellation hook
	all  *ftpConns
}

func (f *ftpReadCloser) Read(p []byte) (int, error) {
	return f.resp.Read(p)
}

func (f *ftpReadCloser) Close() error {
	f.stop()
	err := f.resp.Close()
	_ = f.conn.Quit()
	f.all.closeAll()
	return err
}

// openFTP opens a data connection for path on the FTP server described by
// rawURL, positioned at offset bytes from the beginning.
//
// Every control and data connection is wrapped with a per-operation deadline
// (ftpIdleTimeout) and is force-closed when ctx is cancelled, so neither a
// stalling server nor a disconnected client can pin the goroutine and the FTP
// session.
func openFTP(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
	p, err := parseFTPURL(rawURL)
	if err != nil {
		return nil, -1, err
	}
	if err := preflightHost(ctx, p.host, !ftpAllowPrivate()); err != nil {
		return nil, -1, fmt.Errorf("ftpstream: %w", err)
	}

	dialer := net.Dialer{
		Timeout: ftpDialTimeout,
		Control: netguard.DialControl(!ftpAllowPrivate()),
	}
	var tlsCfg *tls.Config
	if p.tls {
		tlsCfg = &tls.Config{ServerName: p.host}
	}
	ctxDL, _ := ctx.Deadline()
	all := &ftpConns{}
	// Registered before the first dial so a cancel during greeting/Login/
	// SIZE/RETR also aborts the blocked operation.
	stop := context.AfterFunc(ctx, all.closeAll)

	// dialFunc serves the control connection and every data connection.
	dialFunc := func(network, address string) (net.Conn, error) {
		raw, derr := dialer.DialContext(ctx, network, address)
		if derr != nil {
			return nil, derr
		}
		if !all.add(raw) {
			return nil, ctx.Err()
		}
		c := raw
		if tlsCfg != nil {
			// Handshake is deferred to the first Read/Write, which the
			// deadline wrapper below bounds.
			c = tls.Client(raw, tlsCfg)
		}
		return &deadlineConn{Conn: c, idle: ftpIdleTimeout, ctxDL: ctxDL}, nil
	}

	fail := func(conn *ftp.ServerConn) {
		if conn != nil {
			_ = conn.Quit()
		}
		stop()
		all.closeAll()
	}

	conn, err := ftp.Dial(p.addr, ftp.DialWithDialFunc(dialFunc))
	if err != nil {
		fail(nil)
		return nil, -1, fmt.Errorf("ftpstream: dial %s: %w", p.addr, err)
	}

	if err := conn.Login(p.user, p.pass); err != nil {
		fail(conn)
		return nil, -1, fmt.Errorf("ftpstream: login as %q: %w", p.user, err)
	}

	// Best-effort size; -1 when the server does not support SIZE or the command
	// fails (e.g., the path does not exist — RETR will surface the real error).
	size := int64(-1)
	if s, ferr := conn.FileSize(p.path); ferr == nil {
		size = s
	}

	var resp *ftp.Response
	if offset > 0 {
		resp, err = conn.RetrFrom(p.path, uint64(offset))
	} else {
		resp, err = conn.Retr(p.path)
	}
	if err != nil {
		fail(conn)
		return nil, -1, fmt.Errorf("ftpstream: RETR %s: %w", p.path, err)
	}
	if cerr := ctx.Err(); cerr != nil {
		_ = resp.Close()
		fail(conn)
		return nil, -1, fmt.Errorf("ftpstream: %w", cerr)
	}

	return &ftpReadCloser{resp: resp, conn: conn, stop: stop, all: all}, size, nil
}

// ErrRangeNotSatisfiable is returned (wrapped) by Open when the requested
// offset lies beyond the end of the resource.
var ErrRangeNotSatisfiable = errors.New("requested range not satisfiable")

// openHTTP opens an HTTP or HTTPS connection for rawURL, positioned at offset.
//
// When offset > 0, a Range header is sent. The total resource size is inferred
// from Content-Range (206 response) or Content-Length (200 response).
func openHTTP(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, -1, fmt.Errorf("ftpstream: parse URL: %w", err)
	}
	if err := preflightHost(ctx, u.Hostname(), !ftpAllowPrivate()); err != nil {
		return nil, -1, fmt.Errorf("ftpstream: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, -1, fmt.Errorf("ftpstream: build request for %s: %w", rawURL, err)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}

	resp, err := httpGuardedClient.Do(req)
	if err != nil {
		return nil, -1, fmt.Errorf("ftpstream: GET %s: %w", rawURL, err)
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		_ = resp.Body.Close()
		return nil, -1, fmt.Errorf("ftpstream: GET %s: %w", rawURL, ErrRangeNotSatisfiable)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, -1, fmt.Errorf("ftpstream: GET %s: unexpected status %d", rawURL, resp.StatusCode)
	}
	// Range ignored: body starts at 0, so skip to the requested offset. When
	// the offset is already past a known Content-Length, leave the body alone
	// so the caller can answer 416 using the reported size.
	if offset > 0 && resp.StatusCode == http.StatusOK && (resp.ContentLength < 0 || offset < resp.ContentLength) {
		if _, derr := io.CopyN(io.Discard, resp.Body, offset); derr != nil {
			_ = resp.Body.Close()
			if errors.Is(derr, io.EOF) {
				return nil, -1, fmt.Errorf("ftpstream: GET %s: %w", rawURL, ErrRangeNotSatisfiable)
			}
			return nil, -1, fmt.Errorf("ftpstream: GET %s: skip to offset: %w", rawURL, derr)
		}
	}

	size := int64(-1)
	// Content-Range is present on 206 responses: "bytes start-end/total"
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, perr := strconv.ParseInt(cr[i+1:], 10, 64); perr == nil && n >= 0 {
				size = n
			}
		}
	}
	// Fall back to Content-Length; for a full (200) response this is the total
	// size; for a partial (206) response it is the remaining byte count.
	if size < 0 && resp.ContentLength > 0 {
		if resp.StatusCode == http.StatusOK {
			size = resp.ContentLength
		} else {
			// 206: total = start_offset + remaining
			size = offset + resp.ContentLength
		}
	}

	return resp.Body, size, nil
}

// Open returns a ReadCloser for the resource at rawURL starting at byte offset
// (0 = from the beginning), the total resource size if known (−1 otherwise),
// and any error.
//
// Supported schemes: ftp, ftps, http, https.
// The caller must close the returned ReadCloser when done.
func Open(ctx context.Context, rawURL string, offset int64) (rc io.ReadCloser, size int64, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, -1, fmt.Errorf("ftpstream: parse URL: %w", err)
	}
	switch u.Scheme {
	case "ftp", "ftps":
		return openFTP(ctx, rawURL, offset)
	case "http", "https":
		return openHTTP(ctx, rawURL, offset)
	default:
		return nil, -1, fmt.Errorf("ftpstream: unsupported URL scheme %q", u.Scheme)
	}
}
