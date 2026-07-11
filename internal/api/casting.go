// Package api — DLNA/UPnP casting device discovery and AVTransport control.
//
// GET /casting         → discover UPnP MediaRenderers, return
//
//	JSON array [{id, name, type, model, location}] (may be empty)
//
// GET /casting/:id     → single device by UDN/USN id (404 if unknown)
// GET|POST /casting/:id/player[/:cmd] → AVTransport:1 control
//
// Discovery: Uses goupnp to search for urn:schemas-upnp-org:device:MediaRenderer:1
// devices on the LAN. Only devices that expose an AVTransport:1 service are
// listed. Results are cached for 30 seconds; /casting triggers discovery only
// on a cache miss so cache hits never block.
//
// Player control (UPnP AVTransport:1 via goupnp):
//
//	Command → AVTransport action
//	  load / setUrl → SetAVTransportURI(CurrentURI, DIDL-Lite meta) then Play
//	  play          → Play(Speed=1)
//	  pause         → Pause
//	  stop          → Stop
//	  seek          → Seek(Unit=REL_TIME, Target=HH:MM:SS from "time" param)
//	  status        → GetPositionInfo → {trackDuration, relTime, trackURI}
//
// Command resolution: (1) path suffix /casting/:id/player/:cmd, (2) ?command=,
// (3) inferred from param presence (source→load, stop→stop, etc.).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/av1"
	"golang.org/x/sync/singleflight"
)

// CastingDevice is one UPnP MediaRenderer discovered on the LAN.
type CastingDevice struct {
	ID       string `json:"id"`       // USN from SSDP (unique service name) or UDN
	Name     string `json:"name"`     // friendlyName from device description
	Type     string `json:"type"`     // always "dlna" — required by stremio-core PlaybackDevice
	Model    string `json:"model"`    // modelName from device description
	Location string `json:"location"` // URL of device description
}

// Package-level device cache — shared across all requests.
var (
	devicesMu     sync.RWMutex
	deviceCache   []CastingDevice
	deviceCachAt  time.Time
	deviceClients map[string]*av1.AVTransport1 // device id → AVTransport client

	// discoverGroup dedupes concurrent cache-miss discoveries: only one SSDP
	// search runs at a time; all concurrent callers share its result.
	discoverGroup singleflight.Group
)

const (
	ssdpSearchTarget = "urn:schemas-upnp-org:device:MediaRenderer:1"
	deviceCacheTTL   = 30 * time.Second
	discoverTimeout  = 10 * time.Second // 2 s SSDP + sequential desc fetches
	soapTimeout      = 5 * time.Second
)

// handleCasting dispatches /casting, /casting/:id, /casting/:id/player[/:cmd].
//
// @Summary  Discover DLNA renderers
// @Tags     Casting
// @Produce  json
// @Success  200  {array}   object
// @Router   /casting [get]
func (s *server) handleCasting(w http.ResponseWriter, r *http.Request, seg []string) {
	// DLNA is opt-in (STREMIO_ENABLE_DLNA). When disabled, advertise no devices
	// so clients show nothing castable, and 404 any device/control sub-route — no
	// SSDP discovery runs in this mode.
	if !s.cfg.EnableDLNA {
		if len(seg) == 1 {
			writeJSON(w, http.StatusOK, []CastingDevice{})
		} else {
			http.NotFound(w, r)
		}
		return
	}
	switch {
	case len(seg) == 1:
		// GET /casting — run discovery and return device list.
		devices := refreshDevices()
		writeJSON(w, http.StatusOK, devices)

	case len(seg) == 2:
		// GET /casting/:id — single device details.
		devID := seg[1]
		devices := refreshDevices()
		var found *CastingDevice
		for i := range devices {
			if devices[i].ID == devID {
				found = &devices[i]
				break
			}
		}
		if found == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, found)

	case len(seg) >= 3 && seg[2] == "player":
		// GET|POST /casting/:id/player[/:cmd] — AVTransport control.
		s.handlePlayerControl(w, r, seg)

	default:
		http.NotFound(w, r)
	}
}

// handlePlayerControl implements GET|POST /casting/:id/player[/:cmd].
//
// Command dispatch via goupnp AVTransport:1 client. The command may come from:
//   - Path suffix:    /casting/:id/player/play
//   - Query param:    ?command=play
//   - Param inference: ?source=… → load, ?stop=1 → stop, ?paused=true → pause, ?time=N → seek
//
// All AVTransport calls time out after 5 s.
//
// @Summary  UPnP AVTransport control
// @Tags     Casting
// @Param    id       path   string  true   "device id"
// @Param    command  query  string  false  "load|play|pause|stop|seek|status"
// @Success  200  {object}  map[string]interface{}
// @Failure  404
// @Router   /casting/{id}/player [get]
// @Router   /casting/{id}/player [post]
func (s *server) handlePlayerControl(w http.ResponseWriter, r *http.Request, seg []string) {
	devID := seg[1]

	// Locate the device in the cache.
	devices := refreshDevices()
	var dev *CastingDevice
	for i := range devices {
		if devices[i].ID == devID {
			dev = &devices[i]
			break
		}
	}
	if dev == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("device %q not found", devID),
		})
		return
	}

	// Retrieve the AVTransport client built during discovery.
	devicesMu.RLock()
	client := deviceClients[devID]
	devicesMu.RUnlock()

	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "device has no AVTransport service; either not a MediaRenderer or description XML unavailable",
		})
		return
	}

	params := castingParams(r)
	cmd := castingCommand(seg, params)

	switch cmd {
	case "load", "seturl":
		s.castingLoad(w, client, dev, params)
	case "play":
		s.castingPlay(w, client, dev)
	case "pause":
		s.castingPause(w, client, dev)
	case "stop":
		s.castingStop(w, client, dev)
	case "seek":
		s.castingSeek(w, client, dev, params)
	case "status":
		s.castingStatus(w, client, dev)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unknown command %q; valid: load, setUrl, play, pause, stop, seek, status", cmd),
		})
	}
}

// castingParams returns consolidated query/form/JSON params for a casting request.
//
// GET → query string. POST → x-www-form-urlencoded form, OR — when the request
// carries Content-Type: application/json (as stremio-core does for
// /casting/{id}/player with {"source","time"}) — the decoded JSON body merged
// onto any query params. Numbers are preserved verbatim via UseNumber so a u64
// `time` is never mangled into float/scientific notation.
func castingParams(r *http.Request) url.Values {
	if r.Method != http.MethodPost {
		return r.URL.Query()
	}
	if ct := r.Header.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "application/json") {
		params := url.Values{}
		for k, vs := range r.URL.Query() {
			params[k] = append(params[k], vs...)
		}
		var obj map[string]any
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.UseNumber()
		if err := dec.Decode(&obj); err == nil {
			for k, v := range obj {
				switch val := v.(type) {
				case string:
					params.Set(k, val)
				case json.Number:
					params.Set(k, val.String())
				case bool:
					params.Set(k, strconv.FormatBool(val))
				}
			}
		}
		return params
	}
	_ = r.ParseForm()
	return r.Form
}

// castingCommand resolves the player command from the path suffix or params.
func castingCommand(seg []string, params url.Values) string {
	// RESTful suffix: /casting/:id/player/:cmd
	if len(seg) >= 4 && seg[3] != "" {
		return strings.ToLower(seg[3])
	}
	// Explicit command= param.
	if c := params.Get("command"); c != "" {
		return strings.ToLower(c)
	}
	// Infer from param presence (mirrors reference PlayerParams semantics).
	if params.Get("source") != "" {
		return "load"
	}
	if params.Get("stop") != "" {
		return "stop"
	}
	if strings.EqualFold(params.Get("paused"), "true") {
		return "pause"
	}
	if params.Get("time") != "" {
		return "seek"
	}
	return "status"
}

// castingLoad sends SetAVTransportURI(source) followed by Play.
// AVTransport actions: SetAVTransportURI → Play.
func (s *server) castingLoad(w http.ResponseWriter, client *av1.AVTransport1, dev *CastingDevice, params url.Values) {
	mediaURI := params.Get("source")
	if mediaURI == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing source param"})
		return
	}
	// Reject non-http(s) schemes before passing the URI to the device.
	if u, err := url.Parse(mediaURI); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source must be an http or https URL"})
		return
	}

	// Build DIDL-Lite metadata. The URI inside <res> is XML-escaped because it
	// is embedded in manually-constructed XML. The full DIDL string is passed
	// raw to SetAVTransportURI — goupnp encodes it for the SOAP body.
	didl := fmt.Sprintf(
		`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"`+
			` xmlns:dc="http://purl.org/dc/elements/1.1/"`+
			` xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<item id="1" parentID="0" restricted="1">`+
			`<dc:title>Stream</dc:title>`+
			`<upnp:class>object.item.videoItem</upnp:class>`+
			`<res>%s</res>`+
			`</item></DIDL-Lite>`,
		xmlEscape(mediaURI),
	)

	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	if err := client.SetAVTransportURICtx(ctx, 0, mediaURI, didl); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel2()
	if err := client.PlayCtx(ctx2, 0, "1"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "SetAVTransportURI OK but Play failed: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"action":   "load+play",
		"deviceId": dev.ID,
		"uri":      mediaURI,
	})
}

// castingPlay sends Play(Speed=1).
func (s *server) castingPlay(w http.ResponseWriter, client *av1.AVTransport1, dev *CastingDevice) {
	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	if err := client.PlayCtx(ctx, 0, "1"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": "play", "deviceId": dev.ID})
}

// castingPause sends Pause.
func (s *server) castingPause(w http.ResponseWriter, client *av1.AVTransport1, dev *CastingDevice) {
	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	if err := client.PauseCtx(ctx, 0); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": "pause", "deviceId": dev.ID})
}

// castingStop sends Stop.
func (s *server) castingStop(w http.ResponseWriter, client *av1.AVTransport1, dev *CastingDevice) {
	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	if err := client.StopCtx(ctx, 0); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": "stop", "deviceId": dev.ID})
}

// castingSeek sends Seek(Unit=REL_TIME, Target=HH:MM:SS).
// The "time" query/form param is seconds (int or float).
func (s *server) castingSeek(w http.ResponseWriter, client *av1.AVTransport1, dev *CastingDevice, params url.Values) {
	rawTime := params.Get("time")
	if rawTime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing or invalid time parameter"})
		return
	}
	secs, err := strconv.ParseFloat(rawTime, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing or invalid time parameter"})
		return
	}
	target := secsToHHMMSS(secs)
	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	if err := client.SeekCtx(ctx, 0, "REL_TIME", target); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"action":   "seek",
		"deviceId": dev.ID,
		"target":   target,
	})
}

// castingStatus sends GetPositionInfo and returns {trackDuration, relTime, trackURI}.
func (s *server) castingStatus(w http.ResponseWriter, client *av1.AVTransport1, dev *CastingDevice) {
	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	// GetPositionInfo returns: Track, TrackDuration, TrackMetaData, TrackURI,
	// RelTime, AbsTime, RelCount, AbsCount, err
	_, trackDuration, _, trackURI, relTime, _, _, _, err := client.GetPositionInfoCtx(ctx, 0)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"deviceId":      dev.ID,
		"trackDuration": trackDuration,
		"relTime":       relTime,
		"trackURI":      trackURI,
	})
}

// xmlEscape returns s with XML special characters (&, <, >, ', ") escaped.
// Used to safely embed values inside manually-constructed DIDL-Lite XML.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// secsToHHMMSS converts seconds (float) to "HH:MM:SS" for a UPnP Seek target.
func secsToHHMMSS(secs float64) string {
	total := int(secs)
	if total < 0 {
		total = 0
	}
	h := total / 3600
	m := (total % 3600) / 60
	sec := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
}

// refreshDevices returns the cached device list, re-running discovery if the
// cache has expired (or is empty). Never blocks longer than discoverTimeout on
// a miss; cache hits return immediately.
func refreshDevices() []CastingDevice {
	devicesMu.RLock()
	if time.Since(deviceCachAt) < deviceCacheTTL && deviceCache != nil {
		// Cache hit: all call sites are read-only (lines 97, 103, 147), so return
		// the shared slice directly — avoids an alloc + copy on every hot path.
		out := deviceCache
		devicesMu.RUnlock()
		return out
	}
	devicesMu.RUnlock()

	// On a cache miss, use singleflight so that only one SSDP discovery runs
	// at a time. Concurrent callers block here and receive the same result.
	type discoverResult struct {
		devs    []CastingDevice
		clients map[string]*av1.AVTransport1
	}
	v, _, _ := discoverGroup.Do("discover", func() (interface{}, error) {
		devs, clients := discoverDevices()
		devicesMu.Lock()
		deviceCache = devs
		deviceClients = clients
		deviceCachAt = time.Now()
		devicesMu.Unlock()
		return discoverResult{devs: devs, clients: clients}, nil
	})
	return v.(discoverResult).devs
}

// discoverDevices runs goupnp UPnP discovery for MediaRenderer:1 devices,
// filters to those exposing AVTransport:1, and returns the device list paired
// with an id→AVTransport1 client map for control.
//
// On any discovery failure an empty (non-nil) slice is returned so that
// /casting always produces [] rather than null.
func discoverDevices() ([]CastingDevice, map[string]*av1.AVTransport1) {
	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()

	// goupnp.DiscoverDevicesCtx: sends SSDP M-SEARCH (2 s internal window),
	// then fetches and parses the device description XML for each responder.
	maybeDevs, err := goupnp.DiscoverDevicesCtx(ctx, ssdpSearchTarget)
	if err != nil {
		return []CastingDevice{}, nil
	}

	clients := make(map[string]*av1.AVTransport1)
	devices := make([]CastingDevice, 0, len(maybeDevs))

	for _, mrd := range maybeDevs {
		if mrd.Err != nil || mrd.Root == nil {
			continue
		}

		// av1.NewAVTransport1ClientsFromRootDevice: pure in-memory; searches
		// the device tree for AVTransport:1 services and wraps each in a
		// goupnp.ServiceClient with the resolved control URL.
		avClients, err := av1.NewAVTransport1ClientsFromRootDevice(mrd.Root, mrd.Location)
		if err != nil || len(avClients) == 0 {
			// Device is a MediaRenderer but lacks AVTransport:1; skip.
			continue
		}

		d := &mrd.Root.Device
		id := mrd.USN
		if id == "" {
			id = d.UDN
		}
		name := d.FriendlyName
		if name == "" {
			name = mrd.Location.String()
		}

		dev := CastingDevice{
			ID:       id,
			Name:     name,
			Type:     "dlna",
			Model:    d.ModelName,
			Location: mrd.Location.String(),
		}

		devices = append(devices, dev)
		clients[id] = avClients[0] // use the first (and usually only) AVTransport instance
	}

	return devices, clients
}
