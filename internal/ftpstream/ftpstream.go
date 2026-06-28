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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
)

// httpGuardedClient is a package-level HTTP client whose dialer rejects the
// cloud-metadata address (169.254.169.254) at connect time, matching the
// localhost-trust posture of the proxy (private/LAN targets remain reachable so
// a trusted local caller can stream from a NAS or LAN host). Reused to pool conns.
var httpGuardedClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: netguard.DialControl(false),
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

// ftpReadCloser wraps an FTP data response and its underlying control
// connection. Close drains the data response then quits the control connection.
type ftpReadCloser struct {
	resp *ftp.Response
	conn *ftp.ServerConn
}

func (f *ftpReadCloser) Read(p []byte) (int, error) {
	return f.resp.Read(p)
}

func (f *ftpReadCloser) Close() error {
	err := f.resp.Close()
	_ = f.conn.Quit()
	return err
}

// openFTP opens a data connection for path on the FTP server described by
// rawURL, positioned at offset bytes from the beginning.
func openFTP(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
	p, err := parseFTPURL(rawURL)
	if err != nil {
		return nil, -1, err
	}

	opts := []ftp.DialOption{ftp.DialWithContext(ctx)}
	if p.tls {
		opts = append(opts, ftp.DialWithTLS(&tls.Config{
			ServerName: p.host,
		}))
	}

	conn, err := ftp.Dial(p.addr, opts...)
	if err != nil {
		return nil, -1, fmt.Errorf("ftpstream: dial %s: %w", p.addr, err)
	}

	if err := conn.Login(p.user, p.pass); err != nil {
		_ = conn.Quit()
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
		_ = conn.Quit()
		return nil, -1, fmt.Errorf("ftpstream: RETR %s: %w", p.path, err)
	}

	// jlaffaye/ftp does not expose context parameters for Login, FileSize,
	// Retr, or RetrFrom — DialWithContext applies to the initial dial only.
	// Apply any context deadline to the data-connection response so that
	// reads respect the caller's deadline/cancellation as closely as possible.
	if dl, ok := ctx.Deadline(); ok {
		_ = resp.SetDeadline(dl)
	}

	return &ftpReadCloser{resp: resp, conn: conn}, size, nil
}

// openHTTP opens an HTTP or HTTPS connection for rawURL, positioned at offset.
//
// When offset > 0, a Range header is sent. The total resource size is inferred
// from Content-Range (206 response) or Content-Length (200 response).
func openHTTP(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
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
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, -1, fmt.Errorf("ftpstream: GET %s: unexpected status %d", rawURL, resp.StatusCode)
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
