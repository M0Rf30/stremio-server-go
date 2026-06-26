// Package types holds the shared contract between the engine, api, settings and
// media packages. Everything stremio-web depends on is mirrored here so the
// individual packages can be developed independently against a stable surface.
package types

import (
	"context"
	"encoding/json"
	"io"
)

// Config is the runtime configuration shared by all subsystems.
type Config struct {
	HTTPPort        int    // enginefs HTTP API port (default 11470)
	HTTPSPort       int    // optional HTTPS port (default 12470, 0 = disabled)
	AppPath         string // application/cache root, e.g. ~/.stremio-server
	CacheRoot       string // torrent piece cache root (defaults to AppPath)
	MemoryCacheSize int64  // opt-in in-RAM piece cache budget in bytes; 0 = disabled (write pieces to disk)
	ListenPort      int    // BitTorrent peer listen port (0 = random)
	WebUI           string // redirect target for "GET /" (e.g. https://web.stremio.com/)
	Version         string // value reported as settings.serverVersion
	TrackersMax     int    // max ranked UDP/HTTP trackers per torrent (STREMIO_TRACKERS_MAX; 0 = default)
	// disable ALL BitTorrent tracker announces (DHT/PEX/webseeds still used); STREMIO_DISABLE_TRACKERS; default false
	DisableTrackers bool
	// disable WebTorrent/WebRTC peers (pion); cuts ~60% of goroutines + RAM, useful on RAM-constrained hosts; STREMIO_DISABLE_WEBTORRENT; default false
	DisableWebtorrent bool

	// Stream proxy configuration.
	ProxyPassword    string // api_password for stream proxy; "" = no auth
	ProxySecret      string // hex-encoded 32-byte key for signed-URL tokens
	ProxyIPACL       string // comma-separated CIDR allowlist for proxy clients
	ProxyPrebuffer   int    // number of segments to prefetch (0 = off)
	ProxySegCacheTTL int    // segment cache TTL in seconds (0 = caching off)
	ProxyPublicURL   string // explicit external base URL for proxy; "" = derive
	ProxyUpstream    string // global upstream proxy for stream-proxy fetches; "" = direct (STREMIO_PROXY_UPSTREAM; socks5/http)

	// Bitmagnet integration (self-hosted DHT index).
	BitmagnetURL string // GraphQL endpoint, e.g. http://localhost:3333/graphql; "" disables stream queries

	// Torznab integration (Prowlarr/Jackett/NZBHydra/Bitmagnet /torznab).
	TorznabURL    string // Torznab endpoint base URL; "" disables stream queries
	TorznabAPIKey string // Optional API key appended as &apikey=; "" = no auth
}

// FileInfo mirrors an entry of stats.files as consumed by stremio-web.
type FileInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
	Offset int64  `json:"offset"`
}

// Wire is one connected, unchoked peer (stats.wires[]).
type Wire struct {
	Requests     int     `json:"requests"`
	Address      string  `json:"address"`
	AmInterested bool    `json:"amInterested"`
	IsSeeder     bool    `json:"isSeeder"`
	DownSpeed    float64 `json:"downSpeed"`
	UpSpeed      float64 `json:"upSpeed"`
}

// Source is one tracker-announcement entry emitted in Stats.Sources.
// All fields are required (non-null) to match stremio-core's Source shape, which
// the strict (deny-missing) Statistics deserializer rejects if any are absent.
// url MUST be a parseable URL (udp:// or http(s)://) — never a "dht:" pseudo-URI.
type Source struct {
	LastStarted  string `json:"lastStarted"`
	URL          string `json:"url"`
	NumFound     int    `json:"numFound"`
	NumFoundUniq int    `json:"numFoundUniq"`
	NumRequests  int    `json:"numRequests"`
}

// Stats mirrors getStatistics(engine[, idx]) from the original server.js.
// Field names and JSON tags MUST match exactly. Per-file (idx>=0) fields are
// pointers so they are omitted at torrent level.
type Stats struct {
	InfoHash         string      `json:"infoHash"`
	Name             string      `json:"name"`
	Peers            int         `json:"peers"`
	Unchoked         int         `json:"unchoked"`
	Queued           int         `json:"queued"`
	Unique           int         `json:"unique"`
	ConnectionTries  int         `json:"connectionTries"`
	SwarmPaused      bool        `json:"swarmPaused"`
	SwarmConnections int         `json:"swarmConnections"`
	SwarmSize        int         `json:"swarmSize"`
	Selections       interface{} `json:"selections"`
	Wires            []Wire      `json:"wires"`
	Files            []FileInfo  `json:"files"`
	Downloaded       int64       `json:"downloaded"`
	Uploaded         int64       `json:"uploaded"`
	DownloadSpeed    float64     `json:"downloadSpeed"`
	UploadSpeed      float64     `json:"uploadSpeed"`
	Sources          interface{} `json:"sources"`
	Opts             interface{} `json:"opts"`
	// PeerSearchRunning is REQUIRED (non-null) by stremio-core's Statistics.
	PeerSearchRunning bool `json:"peerSearchRunning"`

	// Per-file extras (only when a file index is requested).
	StreamLen      *int64   `json:"streamLen,omitempty"`
	StreamName     *string  `json:"streamName,omitempty"`
	StreamProgress *float64 `json:"streamProgress,omitempty"`

	// GuessedFileIdx is set only in the /create response when the client asked
	// the server to guess (guessFileIdx) or a fileMustInclude matched.
	GuessedFileIdx *int `json:"guessedFileIdx,omitempty"`
}

// AddOptions carries everything needed to add/locate a torrent. It is derived
// by the api layer from the create body and/or the streaming URL query string.
type AddOptions struct {
	// MetaInfo holds raw .torrent bytes when available (from /create from=..).
	MetaInfo []byte
	// Torrent is the raw "torrent" object from a /:infoHash/create body, if any
	// (stremio-web sends parsed torrent metadata: {infoHash, announce, files...}).
	Torrent json.RawMessage
	// Trackers / Sources are announce URLs (?tr=) or peerSearch sources
	// ("tracker:<url>", "dht:<ih>"). The engine should extract real tracker URLs.
	Trackers []string
	Sources  []string
}

// Engine is a single torrent handle.
type Engine interface {
	InfoHash() string
	// Ready blocks until metadata (file list) is available, the context is
	// cancelled, or the engine errors.
	Ready(ctx context.Context) error
	Files() []FileInfo
	// Stats returns torrent-level stats when idx < 0, otherwise augmented with
	// streamLen/streamName/streamProgress for that file.
	Stats(idx int) *Stats
	// NewReader returns a streaming reader for file idx plus the file length.
	// The reader MUST support Seek (for HTTP Range) and prioritise pieces near
	// the current read offset.
	NewReader(idx int) (io.ReadSeekCloser, int64, error)
	// GuessFileIdx returns the index of the most likely playable file
	// (largest video file), or -1 when none.
	GuessFileIdx() int
}

// EngineManager owns the anacrolix client and the set of live engines.
type EngineManager interface {
	// EnsureEngine adds (or returns the existing) torrent for infoHash. It is
	// idempotent: a second call with more trackers should merge them.
	EnsureEngine(infoHash string, opts AddOptions) (Engine, error)
	GetEngine(infoHash string) (Engine, bool)
	RemoveEngine(infoHash string) error
	RemoveAll()
	ListEngines() []string
	// AllStats returns torrent-level stats keyed by infoHash (for /stats.json).
	AllStats() map[string]*Stats
	Close() error
}

// SettingsStore persists the user-facing server settings (cacheSize, bt*, ...).
type SettingsStore interface {
	// Values returns the current settings.values object.
	Values() map[string]interface{}
	// OptionsSchema returns the settings.options array (GUI schema). The list of
	// available interface addresses feeds the remoteHttps selector.
	OptionsSchema(availableInterfaces []string) []map[string]interface{}
	// Extend merges a patch (POST /settings body) into the values.
	Extend(patch map[string]interface{})
	// Get returns a single value.
	Get(key string) interface{}
	// Save persists to disk.
	Save() error
}

// MediaProber backs the ffmpeg-based helper routes. Implementations shell out
// to ffprobe/ffmpeg. Methods return values that are JSON-encoded verbatim.
type MediaProber interface {
	Probe(streamURL string) (interface{}, error)
	Tracks(rawURL string) (interface{}, error)
	OpenSubHash(videoURL string) (result interface{}, err error)
	SubtitlesTracks(subsURL string) (result interface{}, err error)
	// WriteSubtitles converts the subtitle track at `from` into SRT (ext=="srt")
	// or WEBVTT (ext=="vtt"), applying offsetMs, writing to w.
	WriteSubtitles(w io.Writer, from, ext string, offsetMs int) error
	// StartHLS starts (or reuses) an ffmpeg HLS transcode for session id and
	// returns the master playlist text.
	StartHLS(id, mediaURL string) (string, error)
	// HLSFile returns the filesystem path and content-type for a file in the
	// HLS session (playlist or .ts segment).
	HLSFile(ctx context.Context, id, name string) (path, contentType string, err error)
}
