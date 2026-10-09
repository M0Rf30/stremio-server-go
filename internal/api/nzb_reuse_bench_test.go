// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lzstring "github.com/daku10/go-lz-string"
)

// BenchmarkNzbCreateSeek models a player (ExoPlayer) that re-requests the
// GET /nzb/create/{key}?lz=… stream URL on every open/seek and then ranges into
// the file: one op = create (307) + a 64 KiB range GET at a moving offset.
// Before session reuse every op fetched the NZB again and re-assembled the file
// from a new session (nzb-fetches/op ≈ 1, articles/op ≈ the articles the range
// needed plus whatever the background assembly got through); now the NZB is
// fetched once and every article at most once (both ≈ 1/b.N, articles/op ≤ nseg/b.N).
func BenchmarkNzbCreateSeek(b *testing.B) {
	const (
		nseg    = 40
		segSize = 64 * 1024
	)
	b.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	tmp := b.TempDir()
	b.Setenv("TMPDIR", tmp)
	b.Setenv("TMP", tmp)
	b.Setenv("TEMP", tmp)

	total := nseg * segSize
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i*131 + i>>8)
	}
	articles := map[string][]byte{}
	var xml strings.Builder
	xml.WriteString(`<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file subject="&quot;movie.mkv&quot; yEnc" poster="t@t"><segments>`)
	ids := make([]string, 0, nseg)
	for i := range nseg {
		lo := i * segSize
		art := nzbYencPart(content[lo:lo+segSize], int64(total), int64(lo+1), i+1, nseg)
		id := fmt.Sprintf("createseek-%d@test", i+1)
		articles[id] = art
		ids = append(ids, id)
		fmt.Fprintf(&xml, `<segment bytes="%d" number="%d">%s</segment>`, len(art), i+1, id)
	}
	xml.WriteString(`</segments></file></nzb>`)
	fake := newNNTPFake(b, articles)
	fake.setLatency(2 * time.Millisecond)

	var nzbFetches atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nzbFetches.Add(1)
		_, _ = io.WriteString(w, xml.String())
	}))
	defer origin.Close()
	srv := httptest.NewServer(benchHandler())
	defer srv.Close()

	key := "bench-createseek-" + b.Name()
	port := fake.ln.Addr().(*net.TCPAddr).Port
	lz, err := lzstring.CompressToEncodedURIComponent(fmt.Sprintf(`{"nzbUrl":%q,"servers":["nntp://127.0.0.1:%d"]}`, origin.URL+"/m.nzb", port))
	if err != nil {
		b.Fatal(err)
	}
	createURL := srv.URL + "/nzb/create/" + url.PathEscape(key) + "?lz=" + url.QueryEscape(lz)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client := &http.Client{Timeout: time.Minute}
	defer func() {
		nzbSessionsMu.Lock()
		cur := nzbSessions[key]
		delete(nzbSessions, key)
		nzbSessionsMu.Unlock()
		if cur != nil {
			cur.discard()
		}
	}()

	i := 0
	for b.Loop() {
		resp, err := noFollow.Get(createURL)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect {
			b.Fatalf("create status %d", resp.StatusCode)
		}
		off := int64(i%nseg) * segSize
		i++
		req, err := http.NewRequest(http.MethodGet, srv.URL+resp.Header.Get("Location"), nil)
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+64<<10-1))
		rresp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, rresp.Body)
		_ = rresp.Body.Close()
		if err != nil || rresp.StatusCode != http.StatusPartialContent || n != 64<<10 {
			b.Fatalf("range GET: status %d, %d bytes, err %v", rresp.StatusCode, n, err)
		}
	}
	var bodies int
	for _, id := range ids {
		bodies += fake.bodies(id)
	}
	b.ReportMetric(float64(nzbFetches.Load())/float64(b.N), "nzb-fetches/op")
	b.ReportMetric(float64(bodies)/float64(b.N), "articles/op")
}
