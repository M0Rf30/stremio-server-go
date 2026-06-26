package engine

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Curated public trackers from XIU2/TrackersListCollection (best list) plus a
// few verified WebTorrent (wss) trackers so WebRTC peers can be discovered. The
// list is refreshed from upstream at startup; this embedded snapshot is the
// fallback.
const trackersBestURL = "https://raw.githubusercontent.com/XIU2/TrackersListCollection/master/best.txt"

// defaultTrackerTopN is the fallback number of ranked UDP/HTTP trackers attached
// per torrent when STREMIO_TRACKERS_MAX is unset or invalid. Kept small:
// anacrolix's announce dispatcher does work proportional to (torrents x trackers)
// and recomputes bytesLeft over every piece per announce, so a large list
// dominates CPU on big 4K torrents. DHT + PEX + webseeds cover peer discovery;
// a handful of fast trackers is plenty for streaming.
const defaultTrackerTopN = 5

// trackerRefreshIntv is how often the tracker list is re-fetched and re-ranked.
const trackerRefreshIntv = 24 * time.Hour

// probeHTTPTimeout / probeUDPTimeout mirror the reference tracker_prober.rs constants.
const probeHTTPTimeout = 2 * time.Second
const probeUDPTimeout = 1 * time.Second

// probeWSTimeout bounds the WebSocket-tracker reachability handshake.
const probeWSTimeout = 5 * time.Second

// probeMaxRTT is the sentinel RTT assigned to failed probes so they sort last.
const probeMaxRTT = time.Hour

var embeddedTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://tracker.dler.org:6969/announce",
	"udp://tracker.qu.ax:6969/announce",
	"udp://tracker.filemail.com:6969/announce",
	"udp://tracker.ducks.party:1984/announce",
	"udp://tracker.bittor.pw:1337/announce",
	"udp://open.free-tracker.ga:6969/announce",
	"udp://explodie.org:6969/announce",
	// WebTorrent (WebRTC) trackers for browser/webtorrent peers:
	"wss://tracker.openwebtorrent.com",
	"wss://tracker.webtorrent.dev",
	"wss://tracker.btorrent.xyz",
}

var (
	trackersMu      sync.RWMutex
	currentTrackers = embeddedTrackers
)

// getTrackers returns the current tracker list (thread-safe).
func getTrackers() []string {
	trackersMu.RLock()
	defer trackersMu.RUnlock()
	return currentTrackers
}

func setTrackers(t []string) {
	if len(t) == 0 {
		return
	}
	trackersMu.Lock()
	currentTrackers = t
	trackersMu.Unlock()
}

// parseTrackers extracts one tracker URL per non-empty line.
func parseTrackers(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "udp://") || strings.HasPrefix(line, "http://") ||
			strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "ws://") ||
			strings.HasPrefix(line, "wss://") {
			out = append(out, line)
		}
	}
	return out
}

// initTrackers loads a cached list immediately (if present) and launches a
// background goroutine that fetches, probes, ranks, and re-persists the list.
// The goroutine repeats every 24 h and stops when done is closed.
// The embedded snapshot always serves as the fallback so startup is never blocked.
func initTrackers(cacheDir string, maxTrackers int, done <-chan struct{}) {
	cache := filepath.Join(cacheDir, "trackers_best.txt")
	// Synchronous fast-path: serve the last ranked list immediately if cached.
	if b, err := os.ReadFile(cache); err == nil {
		if t := parseTrackers(string(b)); len(t) > 0 {
			setTrackers(mergeWS(t))
		}
	}
	// Background: fetch → probe → rank → persist, then repeat every 24 h.
	go func() {
		doRefreshTrackers(cache, maxTrackers)
		ticker := time.NewTicker(trackerRefreshIntv)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				doRefreshTrackers(cache, maxTrackers)
			}
		}
	}()
}

// doRefreshTrackers fetches the upstream curated UDP/HTTP list plus ngosang's
// WebSocket list, probes the UDP/HTTP trackers in parallel to measure RTT, keeps
// the fastest maxTrackers, merges all wss trackers (fetched + embedded) back in,
// updates the in-memory list, and persists the ranked UDP/HTTP list for a fast
// next startup.
func doRefreshTrackers(cache string, maxTrackers int) {
	if maxTrackers <= 0 {
		maxTrackers = defaultTrackerTopN
	}
	candidates := fetchTrackerList(trackersBestURL)
	if len(candidates) == 0 {
		return // upstream unreachable — keep the current/embedded list
	}

	// Separate wss (un-probeable) from UDP/HTTP candidates.
	var probeable, wss []string
	for _, u := range candidates {
		if strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://") {
			wss = append(wss, u)
		} else {
			probeable = append(probeable, u)
		}
	}

	// Probe and keep the fastest topN UDP/HTTP trackers.
	ranked := rankAndKeep(probeable, maxTrackers)

	// Persist only the ranked UDP/HTTP list; wss are merged at runtime via mergeWS.
	_ = os.WriteFile(cache, []byte(strings.Join(ranked, "\n")+"\n"), 0o644)

	// Probe wss trackers (fetched + embedded) and keep only reachable ones so the
	// client never wastes announces (or log noise) on dead WebTorrent trackers.
	embeddedWSS := make([]string, 0, 4)
	for _, t := range embeddedTrackers {
		if strings.HasPrefix(t, "wss://") || strings.HasPrefix(t, "ws://") {
			embeddedWSS = append(embeddedWSS, t)
		}
	}
	liveWSS := rankWSS(append(wss, embeddedWSS...))
	setTrackers(dedup(append(ranked, liveWSS...)))
}

// fetchTrackerList GETs a newline-delimited tracker list and returns the parsed
// URLs. Returns nil on any error so the caller falls back to the current list.
func fetchTrackerList(url string) []string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil
	}
	return parseTrackers(string(body))
}

// rankAndKeep probes all candidates in parallel, sorts by RTT ascending
// (failed probes get probeMaxRTT so they sink to the bottom), and returns
// the top topN entries.
func rankAndKeep(candidates []string, topN int) []string {
	type result struct {
		url string
		rtt time.Duration
	}

	// Bound concurrency: at most 32 probe goroutines in flight at once.
	// Without this, hundreds of goroutines could be spawned for a large upstream list.
	const maxConcurrent = 32
	sem := make(chan struct{}, maxConcurrent)

	ch := make(chan result, len(candidates))
	for _, u := range candidates {
		u := u
		sem <- struct{}{} // acquire slot; blocks when 32 probes are in flight
		go func() {
			defer func() { <-sem }() // release slot when done
			ch <- result{u, probeTracker(u)}
		}()
	}

	results := make([]result, 0, len(candidates))
	for range candidates {
		results = append(results, <-ch)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].rtt < results[j].rtt
	})

	n := min(topN, len(results))
	out := make([]string, n)
	for i := range n {
		out[i] = results[i].url
	}
	return out
}

// probeTracker dispatches to the appropriate protocol prober.
// Returns probeMaxRTT for unknown schemes (wss, etc.).
func probeTracker(rawURL string) time.Duration {
	switch {
	case strings.HasPrefix(rawURL, "udp://"):
		return probeTrackerUDP(rawURL)
	case strings.HasPrefix(rawURL, "http://"), strings.HasPrefix(rawURL, "https://"):
		return probeTrackerHTTP(rawURL)
	default:
		return probeMaxRTT
	}
}

// probeTrackerHTTP sends an HTTP HEAD request with a 2-second timeout.
// Falls back to GET if HEAD is refused. Matches reference tracker_prober.rs.
func probeTrackerHTTP(rawURL string) time.Duration {
	hc := &http.Client{Timeout: probeHTTPTimeout}
	start := time.Now()
	resp, err := hc.Head(rawURL)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return time.Since(start)
	}
	// Fall back to GET (some trackers reject HEAD).
	resp2, err2 := hc.Get(rawURL)
	if err2 != nil {
		return probeMaxRTT
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	return time.Since(start)
}

// probeTrackerUDP performs a BEP 15 UDP tracker connect handshake with a
// 1-second timeout. Returns the round-trip time on success, probeMaxRTT on
// failure (no response, wrong action/transaction_id, or network error).
func probeTrackerUDP(rawURL string) time.Duration {
	// Parse host:port from "udp://host:port/path".
	hostPort := strings.TrimPrefix(rawURL, "udp://")
	if i := strings.IndexByte(hostPort, '/'); i >= 0 {
		hostPort = hostPort[:i]
	}
	if hostPort == "" {
		return probeMaxRTT
	}

	conn, err := net.DialTimeout("udp", hostPort, probeUDPTimeout)
	if err != nil {
		return probeMaxRTT
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(probeUDPTimeout)); err != nil {
		return probeMaxRTT
	}

	// BEP 15 connect request:
	//   0  8  protocol_id   = 0x41727101980 (magic)
	//   8  4  action        = 0 (connect)
	//  12  4  transaction_id
	const (
		protocolID    uint64 = 0x41727101980
		action        uint32 = 0
		transactionID uint32 = 12345
	)
	var req [16]byte
	binary.BigEndian.PutUint64(req[0:8], protocolID)
	binary.BigEndian.PutUint32(req[8:12], action)
	binary.BigEndian.PutUint32(req[12:16], transactionID)

	start := time.Now()
	if _, err := conn.Write(req[:]); err != nil {
		return probeMaxRTT
	}

	var resp [16]byte
	n, err := conn.Read(resp[:])
	if err != nil || n < 8 {
		return probeMaxRTT
	}

	// Validate: action == 0, transaction_id matches.
	recvAction := binary.BigEndian.Uint32(resp[0:4])
	recvTxID := binary.BigEndian.Uint32(resp[4:8])
	if recvAction != 0 || recvTxID != transactionID {
		return probeMaxRTT
	}

	return time.Since(start)
}

// mergeWS appends our WebTorrent (wss) trackers to a fetched list that only
// contains udp/http trackers, so WebRTC peers remain discoverable.
func mergeWS(t []string) []string {
	have := make(map[string]bool, len(t))
	out := make([]string, 0, len(t)+len(embeddedTrackers))
	for _, x := range t {
		if !have[x] {
			have[x] = true
			out = append(out, x)
		}
	}
	for _, ws := range embeddedTrackers {
		if (strings.HasPrefix(ws, "wss://") || strings.HasPrefix(ws, "ws://")) && !have[ws] {
			have[ws] = true
			out = append(out, ws)
		}
	}
	return out
}

// probeTrackerWS reports whether a WebSocket tracker completes the WS upgrade
// handshake within probeWSTimeout. Dead or non-WebSocket endpoints are filtered
// out so the client does not waste announces on them.
func probeTrackerWS(rawURL string) bool {
	d := websocket.Dialer{HandshakeTimeout: probeWSTimeout}
	c, resp, err := d.Dial(rawURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// rankWSS probes WebSocket trackers in parallel and returns the reachable,
// deduplicated subset.
func rankWSS(list []string) []string {
	seen := make(map[string]bool, len(list))
	uniq := make([]string, 0, len(list))
	for _, u := range list {
		if (strings.HasPrefix(u, "wss://") || strings.HasPrefix(u, "ws://")) && !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	results := make([]bool, len(uniq))
	var wg sync.WaitGroup
	for i, u := range uniq {
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			results[idx] = probeTrackerWS(url)
		}(i, u)
	}
	wg.Wait()
	live := make([]string, 0, len(uniq))
	for i, ok := range results {
		if ok {
			live = append(live, uniq[i])
		}
	}
	return live
}

// dedup returns t with duplicate entries removed, preserving order.
func dedup(t []string) []string {
	have := make(map[string]bool, len(t))
	out := make([]string, 0, len(t))
	for _, x := range t {
		if !have[x] {
			have[x] = true
			out = append(out, x)
		}
	}
	return out
}
