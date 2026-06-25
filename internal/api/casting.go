// Package api — DLNA/UPnP casting device discovery via SSDP + AVTransport control.
//
// GET /casting         → run SSDP M-SEARCH, fetch device descriptions, return
//
//	JSON array [{id, name, model, location}] (may be empty)
//
// GET /casting/:id     → single device by USN id (404 if unknown)
// GET|POST /casting/:id/player[/:cmd] → real UPnP AVTransport:1 SOAP control
//
// Discovery: The reference (server/src/ssdp.rs + routes/casting.rs) runs SSDP
// discovery as a background daemon. We do on-demand discovery with a 30-second
// cache so catalog responses are never blocked.
//
// Player control (full UPnP AVTransport:1):
//
//	Command → SOAPAction mapping
//	  load / setUrl → SetAVTransportURI(CurrentURI, DIDL-Lite meta) then Play
//	  play          → Play(Speed=1)
//	  pause         → Pause
//	  stop          → Stop
//	  seek          → Seek(Unit=REL_TIME, Target=HH:MM:SS from "time" param in seconds)
//	  status        → GetPositionInfo → {trackDuration, relTime, trackURI}
//
// Command comes from: (1) path suffix /casting/:id/player/:cmd, (2) ?command=,
// (3) inferred from param presence (reference style: source→load, stop→stop, etc.).
//
// Note: DLNA renderer control requires a real device on the LAN at runtime.
// The SOAP envelopes are conformant AVTransport:1 and will work against any
// UPnP MediaRenderer (Kodi, VLC, smart TV, etc.).
package api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CastingDevice is one UPnP MediaRenderer discovered on the LAN.
type CastingDevice struct {
	ID         string `json:"id"`       // USN from SSDP (unique service name)
	Name       string `json:"name"`     // friendlyName from device description XML
	Model      string `json:"model"`    // modelName from device description XML
	Location   string `json:"location"` // URL of device description XML
	controlURL string // AVTransport service controlURL — not exported via JSON
}

// Package-level device cache — shared across all requests.
var (
	devicesMu    sync.RWMutex
	deviceCache  []CastingDevice
	deviceCachAt time.Time
)

const (
	ssdpMulticast      = "239.255.255.250:1900"
	ssdpSearchTarget   = "urn:schemas-upnp-org:device:MediaRenderer:1"
	ssdpTimeout        = 2 * time.Second
	deviceCacheTTL     = 30 * time.Second
	avTransportSvcType = "urn:schemas-upnp-org:service:AVTransport:1"
	soapTimeout        = 5 * time.Second
)

// handleCasting dispatches /casting, /casting/:id, /casting/:id/player[/:cmd].
//
// @Summary  Discover DLNA renderers via SSDP
// @Tags     Casting
// @Produce  json
// @Success  200  {array}   object
// @Router   /casting [get]
func (s *server) handleCasting(w http.ResponseWriter, r *http.Request, seg []string) {
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
// Full UPnP AVTransport:1 SOAP command dispatch. The command may come from:
//   - Path suffix:    /casting/:id/player/play
//   - Query param:    ?command=play
//   - Param inference: ?source=… → load, ?stop=1 → stop, ?paused=true → pause, ?time=N → seek
//
// All SOAP POSTs time out after 5 s.
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

	// Locate the device — we need its AVTransport controlURL.
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
	if dev.controlURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "device has no AVTransport service; either not a MediaRenderer or description XML unavailable",
		})
		return
	}

	params := castingParams(r)
	cmd := castingCommand(seg, params)

	switch cmd {
	case "load", "seturl":
		s.castingLoad(w, dev, params)
	case "play":
		s.castingPlay(w, dev)
	case "pause":
		s.castingPause(w, dev)
	case "stop":
		s.castingStop(w, dev)
	case "seek":
		s.castingSeek(w, dev, params)
	case "status":
		s.castingStatus(w, dev)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unknown command %q; valid: load, setUrl, play, pause, stop, seek, status", cmd),
		})
	}
}

// castingParams returns consolidated query/form params for a casting request.
func castingParams(r *http.Request) url.Values {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		return r.Form
	}
	return r.URL.Query()
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
// AVTransport action: SetAVTransportURI → Play.
func (s *server) castingLoad(w http.ResponseWriter, dev *CastingDevice, params url.Values) {
	mediaURI := params.Get("source")
	if mediaURI == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing source param"})
		return
	}

	// Build DIDL-Lite metadata blob (embedded inside SetAVTransportURI envelope).
	// The URI appears twice: once in CurrentURI, once inside the DIDL <res> tag.
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
	// The DIDL string itself is XML-escaped when embedded in CurrentURIMetaData.
	body := fmt.Sprintf(
		`<u:SetAVTransportURI xmlns:u="%s">`+
			`<InstanceID>0</InstanceID>`+
			`<CurrentURI>%s</CurrentURI>`+
			`<CurrentURIMetaData>%s</CurrentURIMetaData>`+
			`</u:SetAVTransportURI>`,
		avTransportSvcType, xmlEscape(mediaURI), xmlEscape(didl),
	)
	if _, err := avSOAP(dev.controlURL, "SetAVTransportURI", body); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	// Issue Play immediately after loading.
	playBody := fmt.Sprintf(
		`<u:Play xmlns:u="%s"><InstanceID>0</InstanceID><Speed>1</Speed></u:Play>`,
		avTransportSvcType,
	)
	if _, err := avSOAP(dev.controlURL, "Play", playBody); err != nil {
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
func (s *server) castingPlay(w http.ResponseWriter, dev *CastingDevice) {
	body := fmt.Sprintf(
		`<u:Play xmlns:u="%s"><InstanceID>0</InstanceID><Speed>1</Speed></u:Play>`,
		avTransportSvcType,
	)
	if _, err := avSOAP(dev.controlURL, "Play", body); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": "play", "deviceId": dev.ID})
}

// castingPause sends Pause.
func (s *server) castingPause(w http.ResponseWriter, dev *CastingDevice) {
	body := fmt.Sprintf(
		`<u:Pause xmlns:u="%s"><InstanceID>0</InstanceID></u:Pause>`,
		avTransportSvcType,
	)
	if _, err := avSOAP(dev.controlURL, "Pause", body); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": "pause", "deviceId": dev.ID})
}

// castingStop sends Stop.
func (s *server) castingStop(w http.ResponseWriter, dev *CastingDevice) {
	body := fmt.Sprintf(
		`<u:Stop xmlns:u="%s"><InstanceID>0</InstanceID></u:Stop>`,
		avTransportSvcType,
	)
	if _, err := avSOAP(dev.controlURL, "Stop", body); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "action": "stop", "deviceId": dev.ID})
}

// castingSeek sends Seek(Unit=REL_TIME, Target=HH:MM:SS).
// The "time" query/form param is seconds (int or float).
func (s *server) castingSeek(w http.ResponseWriter, dev *CastingDevice, params url.Values) {
	secs, _ := strconv.ParseFloat(params.Get("time"), 64)
	target := secsToHHMMSS(secs)
	body := fmt.Sprintf(
		`<u:Seek xmlns:u="%s"><InstanceID>0</InstanceID><Unit>REL_TIME</Unit><Target>%s</Target></u:Seek>`,
		avTransportSvcType, target,
	)
	if _, err := avSOAP(dev.controlURL, "Seek", body); err != nil {
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

// castingStatus sends GetPositionInfo and returns parsed {trackDuration, relTime, trackURI}.
func (s *server) castingStatus(w http.ResponseWriter, dev *CastingDevice) {
	body := fmt.Sprintf(
		`<u:GetPositionInfo xmlns:u="%s"><InstanceID>0</InstanceID></u:GetPositionInfo>`,
		avTransportSvcType,
	)
	resp, err := avSOAP(dev.controlURL, "GetPositionInfo", body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"deviceId":      dev.ID,
		"trackDuration": xmlExtractText(resp, "TrackDuration"),
		"relTime":       xmlExtractText(resp, "RelTime"),
		"trackURI":      xmlExtractText(resp, "TrackURI"),
	})
}

// avSOAP posts a UPnP AVTransport:1 SOAP action to controlURL.
//
// SOAP envelope: <?xml version="1.0" …><s:Envelope …><s:Body>bodyXML</s:Body></s:Envelope>
// SOAPAction header: "urn:schemas-upnp-org:service:AVTransport:1#<action>"
//
// Returns the response body on 2xx, or an error including any SOAP fault message.
// Timeout: 5 s.
func avSOAP(controlURL, action, bodyXML string) ([]byte, error) {
	envelope := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"` +
		` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` + bodyXML + `</s:Body></s:Envelope>`

	req, err := http.NewRequest(http.MethodPost, controlURL, bytes.NewBufferString(envelope))
	if err != nil {
		return nil, fmt.Errorf("building SOAP request: %w", err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+avTransportSvcType+`#`+action+`"`)

	client := &http.Client{Timeout: soapTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SOAP POST to %s: %w", controlURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("reading SOAP response: %w", err)
	}

	// Detect SOAP faults (may arrive with HTTP 200 or 500).
	if bytes.Contains(data, []byte(":Fault>")) || bytes.Contains(data, []byte("<Fault>")) {
		msg := xmlExtractText(data, "faultstring")
		if msg == "" {
			msg = xmlExtractText(data, "errorDescription")
		}
		if msg == "" {
			msg = "SOAP fault"
		}
		return nil, fmt.Errorf("SOAP fault: %s", msg)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("SOAP HTTP %d from %s", resp.StatusCode, controlURL)
	}
	return data, nil
}

// xmlExtractText returns the inner text of the first element matching tag in data.
// Works without namespace awareness — suitable for SOAP response bodies where element
// names are unique within the response.
func xmlExtractText(data []byte, tag string) string {
	open := "<" + tag + ">"
	s := string(data)
	start := strings.Index(s, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(s[start:], "</"+tag+">")
	if end < 0 {
		return ""
	}
	return s[start : start+end]
}

// xmlEscape returns s with XML special characters (&, <, >, ', ") escaped.
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

// — SSDP discovery —

// refreshDevices returns the cached device list, re-running SSDP discovery if
// the cache has expired (or is empty). Never blocks longer than ssdpTimeout.
func refreshDevices() []CastingDevice {
	devicesMu.RLock()
	if time.Since(deviceCachAt) < deviceCacheTTL && deviceCache != nil {
		out := make([]CastingDevice, len(deviceCache))
		copy(out, deviceCache)
		devicesMu.RUnlock()
		return out
	}
	devicesMu.RUnlock()

	found := ssdpDiscover()

	devicesMu.Lock()
	deviceCache = found
	deviceCachAt = time.Now()
	devicesMu.Unlock()

	return found
}

// ssdpDiscover sends an SSDP M-SEARCH multicast, collects responses for
// ssdpTimeout, fetches device description XMLs concurrently, and returns
// the discovered device list (with AVTransport controlURLs populated).
func ssdpDiscover() []CastingDevice {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return []CastingDevice{}
	}
	defer func() { _ = conn.Close() }()

	dst, err := net.ResolveUDPAddr("udp4", ssdpMulticast)
	if err != nil {
		return []CastingDevice{}
	}

	req := fmt.Sprintf(
		"M-SEARCH * HTTP/1.1\r\n"+
			"HOST: 239.255.255.250:1900\r\n"+
			"MAN: \"ssdp:discover\"\r\n"+
			"MX: 2\r\n"+
			"ST: %s\r\n"+
			"\r\n",
		ssdpSearchTarget,
	)
	if _, err := conn.WriteTo([]byte(req), dst); err != nil {
		return []CastingDevice{}
	}

	conn.SetReadDeadline(time.Now().Add(ssdpTimeout)) //nolint:errcheck
	buf := make([]byte, 4096)

	type rawResp struct{ location, usn string }
	seen := map[string]bool{}
	var raws []rawResp

	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // deadline elapsed or closed
		}
		r := parseSSDPHeaders(buf[:n])
		if r.location == "" || seen[r.usn] {
			continue
		}
		seen[r.usn] = true
		raws = append(raws, r)
	}

	type result struct {
		dev CastingDevice
		ok  bool
	}
	results := make([]result, len(raws))
	var wg sync.WaitGroup
	for i, raw := range raws {
		wg.Add(1)
		go func(idx int, r rawResp) {
			defer wg.Done()
			dev, ok := fetchDeviceDesc(r.location, r.usn)
			results[idx] = result{dev, ok}
		}(i, raw)
	}
	wg.Wait()

	devices := make([]CastingDevice, 0, len(results))
	for _, res := range results {
		if res.ok {
			devices = append(devices, res.dev)
		}
	}
	return devices
}

// parseSSDPHeaders extracts LOCATION and USN from an SSDP HTTP/1.1 response.
// SSDP responses look like HTTP but arrive over UDP; we parse manually.
func parseSSDPHeaders(data []byte) struct{ location, usn string } {
	var out struct{ location, usn string }
	for _, line := range strings.Split(string(data), "\r\n") {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "location:"):
			out.location = strings.TrimSpace(line[9:])
		case strings.HasPrefix(lower, "usn:"):
			out.usn = strings.TrimSpace(line[4:])
		}
	}
	return out
}

// upnpRoot is the top-level element of a UPnP device description XML.
type upnpRoot struct {
	XMLName xml.Name `xml:"root"`
	Device  struct {
		FriendlyName string `xml:"friendlyName"`
		ModelName    string `xml:"modelName"`
		UDN          string `xml:"UDN"`
		ServiceList  struct {
			Services []upnpService `xml:"service"`
		} `xml:"serviceList"`
	} `xml:"device"`
}

// upnpService is one entry in a UPnP device's serviceList.
type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ServiceID   string `xml:"serviceId"`
	ControlURL  string `xml:"controlURL"`
}

// fetchDeviceDesc fetches a UPnP device description XML from location,
// parses friendlyName/modelName, and extracts the AVTransport controlURL
// resolved relative to the LOCATION base URL.
func fetchDeviceDesc(location, usn string) (CastingDevice, bool) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(location)
	if err != nil {
		// Unreachable — still return a minimal entry so the device appears.
		return CastingDevice{
			ID:       usn,
			Name:     fmt.Sprintf("Device (%s)", location),
			Location: location,
		}, true
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return CastingDevice{ID: usn, Location: location}, true
	}

	var root upnpRoot
	if err := xml.Unmarshal(body, &root); err != nil {
		return CastingDevice{
			ID:       usn,
			Name:     fmt.Sprintf("Device (%s)", location),
			Location: location,
		}, true
	}

	name := root.Device.FriendlyName
	if name == "" {
		name = fmt.Sprintf("Device (%s)", location)
	}
	id := usn
	if id == "" {
		id = root.Device.UDN
	}

	dev := CastingDevice{
		ID:       id,
		Name:     name,
		Model:    root.Device.ModelName,
		Location: location,
	}

	// Resolve the AVTransport service controlURL relative to the LOCATION base.
	base, baseErr := url.Parse(location)
	for _, svc := range root.Device.ServiceList.Services {
		if strings.HasSuffix(svc.ServiceType, "AVTransport:1") && svc.ControlURL != "" {
			if baseErr == nil {
				ref, err := url.Parse(svc.ControlURL)
				if err == nil {
					dev.controlURL = base.ResolveReference(ref).String()
				} else {
					dev.controlURL = svc.ControlURL
				}
			} else {
				dev.controlURL = svc.ControlURL
			}
			break
		}
	}

	return dev, true
}
