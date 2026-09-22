// Package api — regression tests for COMPAT-2 and CAST-2
// (see review REVIEW-2026-09-22.md): core's null-source Stop and bare
// CastingSubtitles body must not be silently treated as "status", and
// castingLoad must seek to a resume position ("time", ms) after Play.
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/av1"
	"github.com/huin/goupnp/soap"
)

// soapRecorder is a minimal fake AVTransport:1 SOAP endpoint. It records the
// action name (from the SOAPACTION header) and raw request body for every
// call, and answers every action with an empty-but-valid SOAP envelope.
// That's sufficient for every action castingLoad/castingSeek/castingStop
// issue: SetAVTransportURICtx, PlayCtx, SeekCtx and StopCtx all pass a nil
// outAction to PerformActionCtx, so the response body content is never
// unmarshalled for them.
type soapRecorder struct {
	mu    sync.Mutex
	calls []string
	body  map[string]string // action -> raw request body (last call wins)
}

func (r *soapRecorder) actionOf(soapAction string) string {
	// SOAPACTION header looks like `"urn:schemas-upnp-org:service:AVTransport:1#Play"`.
	if i := strings.LastIndexByte(soapAction, '#'); i >= 0 {
		return strings.TrimSuffix(soapAction[i+1:], `"`)
	}
	return soapAction
}

func (r *soapRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		action := r.actionOf(req.Header.Get("SOAPACTION"))
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.calls = append(r.calls, action)
		if r.body == nil {
			r.body = map[string]string{}
		}
		r.body[action] = string(body)
		r.mu.Unlock()

		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		_, _ = w.Write([]byte(`<?xml version="1.0"?>` +
			`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
			`<s:Body><u:Response xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"></u:Response></s:Body></s:Envelope>`))
	}
}

func (r *soapRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *soapRecorder) bodyOf(action string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body[action]
}

func containsAction(calls []string, action string) bool {
	for _, c := range calls {
		if c == action {
			return true
		}
	}
	return false
}

// newFakeAVTransport builds an *av1.AVTransport1 whose SOAP calls go to an
// httptest server driven by rec. Generated goupnp methods only ever touch
// client.SOAPClient (see PerformActionCtx in goupnp/soap), so RootDevice and
// Service are left nil.
func newFakeAVTransport(t *testing.T, rec *soapRecorder) (*av1.AVTransport1, func()) {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := &av1.AVTransport1{
		ServiceClient: goupnp.ServiceClient{SOAPClient: soap.NewSOAPClient(*u)},
	}
	return client, srv.Close
}

// seedCastingDevice registers dev/client in the package-level discovery
// cache so handlePlayerControl finds it without running real SSDP discovery.
func seedCastingDevice(dev CastingDevice, client *av1.AVTransport1) {
	devicesMu.Lock()
	deviceCache = []CastingDevice{dev}
	deviceClients = map[string]*av1.AVTransport1{dev.ID: client}
	deviceCachAt = time.Now()
	devicesMu.Unlock()
}

func newCastingPlayerRequest(devID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/casting/"+devID+"/player", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func castingSeg(req *http.Request) []string {
	return strings.Split(strings.Trim(req.URL.Path, "/"), "/")
}

func TestCastingLoad_NullSourceIsStop(t *testing.T) {
	rec := &soapRecorder{}
	client, closeSrv := newFakeAVTransport(t, rec)
	defer closeSrv()
	dev := CastingDevice{ID: "dev-null-stop", Name: "TV", Type: "dlna"}
	seedCastingDevice(dev, client)

	req := newCastingPlayerRequest(dev.ID, `{"source":null}`)
	w := httptest.NewRecorder()
	s := &server{}
	s.handlePlayerControl(w, req, castingSeg(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"action":"stop"`) {
		t.Errorf("body = %s; want action=stop", w.Body.String())
	}
	calls := rec.snapshot()
	if !containsAction(calls, "Stop") {
		t.Errorf("calls = %v; want Stop", calls)
	}
	if containsAction(calls, "SetAVTransportURI") || containsAction(calls, "Play") {
		t.Errorf("calls = %v; null source must not load/play", calls)
	}
}

func TestCastingSubtitles_NotTreatedAsStatus(t *testing.T) {
	rec := &soapRecorder{}
	client, closeSrv := newFakeAVTransport(t, rec)
	defer closeSrv()
	dev := CastingDevice{ID: "dev-subs", Name: "TV", Type: "dlna"}
	seedCastingDevice(dev, client)

	req := newCastingPlayerRequest(dev.ID, `{"subtitlesSrc":"http://example.com/subs.vtt","subtitlesDelay":1500}`)
	w := httptest.NewRecorder()
	s := &server{}
	s.handlePlayerControl(w, req, castingSeg(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"action":"subtitles"`) {
		t.Errorf("body = %s; want action=subtitles (not status)", w.Body.String())
	}
	calls := rec.snapshot()
	if containsAction(calls, "GetPositionInfo") {
		t.Errorf("calls = %v; subtitles body must not poll GetPositionInfo (that's the status command)", calls)
	}
	if len(calls) != 0 {
		t.Errorf("calls = %v; want no AVTransport action for an unsupported subtitle update", calls)
	}
}

func TestCastingSubtitles_NullSrcAcknowledged(t *testing.T) {
	// A cleared subtitle ({"subtitlesSrc":null,"subtitlesDelay":0}) must
	// resolve via subtitlesDelay's presence, not fall through to "status"
	// just because subtitlesSrc round-trips through castingParams as a
	// present-but-null (empty) value.
	rec := &soapRecorder{}
	client, closeSrv := newFakeAVTransport(t, rec)
	defer closeSrv()
	dev := CastingDevice{ID: "dev-subs-null", Name: "TV", Type: "dlna"}
	seedCastingDevice(dev, client)

	req := newCastingPlayerRequest(dev.ID, `{"subtitlesSrc":null,"subtitlesDelay":0}`)
	w := httptest.NewRecorder()
	s := &server{}
	s.handlePlayerControl(w, req, castingSeg(req))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":"subtitles"`) {
		t.Errorf("status=%d body=%s; want 200 action=subtitles", w.Code, w.Body.String())
	}
}

var seekTargetRx = regexp.MustCompile(`<Target>([^<]*)</Target>`)

func TestCastingLoad_TimeTriggersSeek(t *testing.T) {
	rec := &soapRecorder{}
	client, closeSrv := newFakeAVTransport(t, rec)
	defer closeSrv()
	dev := CastingDevice{ID: "dev-resume", Name: "TV", Type: "dlna"}
	seedCastingDevice(dev, client)

	// "time" is milliseconds per core's PlayOnDeviceArgs (runtime/msg/action.rs)
	// and cast_request (streaming_server/casting.rs): 12500 ms = 12.5 s.
	req := newCastingPlayerRequest(dev.ID, `{"source":"http://example.com/movie.mp4","time":12500}`)
	w := httptest.NewRecorder()
	s := &server{}
	s.handlePlayerControl(w, req, castingSeg(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	calls := rec.snapshot()
	wantOrder := []string{"SetAVTransportURI", "Play", "Seek"}
	if len(calls) < len(wantOrder) {
		t.Fatalf("calls = %v; want at least %v", calls, wantOrder)
	}
	for i, want := range wantOrder {
		if calls[i] != want {
			t.Errorf("calls[%d] = %q; want %q (full: %v)", i, calls[i], want, calls)
		}
	}
	m := seekTargetRx.FindStringSubmatch(rec.bodyOf("Seek"))
	if m == nil {
		t.Fatalf("Seek request body has no <Target>: %s", rec.bodyOf("Seek"))
	}
	if m[1] != "00:00:12" {
		t.Errorf("seek target = %q; want 00:00:12 (12500ms -> 12.5s truncated)", m[1])
	}
	if !strings.Contains(w.Body.String(), `"seek":"00:00:12"`) {
		t.Errorf("response body = %s; want seek target reported", w.Body.String())
	}
}

func TestCastingLoad_ZeroTimeSkipsSeek(t *testing.T) {
	rec := &soapRecorder{}
	client, closeSrv := newFakeAVTransport(t, rec)
	defer closeSrv()
	dev := CastingDevice{ID: "dev-fresh", Name: "TV", Type: "dlna"}
	seedCastingDevice(dev, client)

	req := newCastingPlayerRequest(dev.ID, `{"source":"http://example.com/movie.mp4","time":0}`)
	w := httptest.NewRecorder()
	s := &server{}
	s.handlePlayerControl(w, req, castingSeg(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	calls := rec.snapshot()
	if containsAction(calls, "Seek") {
		t.Errorf("calls = %v; time=0 must not seek", calls)
	}
}
