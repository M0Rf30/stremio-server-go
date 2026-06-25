package streamproxy

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func init() {
	mpdHandler = dashServe
}

// dashServe handles MPEG-DASH MPD proxy requests at /proxy/mpd/manifest.m3u8.
func dashServe(h *Handler, w http.ResponseWriter, r *http.Request) {
	if err := h.authorize(r); err != nil {
		writeAuthError(w, err)
		return
	}
	opts, err := h.parseOptions(r)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if opts.Dest == "" {
		http.Error(w, "missing destination", http.StatusBadRequest)
		return
	}
	resp, err := h.fetch(r.Context(), http.MethodGet, opts.Dest, opts.ReqHeaders, nil)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "read error: "+err.Error(), http.StatusBadGateway)
		return
	}
	out := dashRewrite(h, r, opts, body)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// dashQueryEscape is like url.QueryEscape but leaves '$', '(', and ')' unencoded.
// MPEG-DASH SegmentTemplate URIs embed '$Variable$' placeholder tokens that must
// survive the encoding so the DASH client can perform template expansion before
// requesting each segment through the proxy.
func dashQueryEscape(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "%24", "$")
	e = strings.ReplaceAll(e, "%28", "(")
	e = strings.ReplaceAll(e, "%29", ")")
	return e
}

// dashBuildTemplateURL constructs a /proxy/stream URL for a SegmentTemplate
// initialization or media attribute.  The destination is placed as a plain
// (non-base64) query-escaped value so embedded $…$ placeholder tokens remain
// visible to the DASH client for template expansion.  Auth parameters from opts
// are propagated exactly as buildProxyURL does.
func dashBuildTemplateURL(h *Handler, ext, abs string, opts *Options) string {
	u := ext + "/proxy/stream?d=" + dashQueryEscape(abs)
	if opts != nil {
		for k, vs := range opts.ReqHeaders {
			for _, v := range vs {
				u += "&h_" + url.QueryEscape(k) + "=" + url.QueryEscape(v)
			}
		}
		for k, vs := range opts.RespHeaders {
			for _, v := range vs {
				u += "&r_" + url.QueryEscape(k) + "=" + url.QueryEscape(v)
			}
		}
		if opts.APIPassword != "" {
			u += "&api_password=" + url.QueryEscape(opts.APIPassword)
		}
	}
	return u
}

// dashRewrite rewrites URL-bearing tokens in an MPEG-DASH MPD document so that
// all media references are routed through the proxy.  It uses a streaming
// token-based approach (encoding/xml) to preserve all other XML structure,
// attributes, and namespace declarations verbatim.
//
// Rewritten tokens:
//   - <BaseURL> character data — resolved and proxied via /proxy/stream
//     (buildProxyURL, base64-encoded destination).
//   - <SegmentTemplate initialization="…" media="…"> attributes — resolved and
//     proxied via dashBuildTemplateURL, which keeps $…$ placeholders unencoded.
//   - <SegmentURL media="…" index="…"> attributes — resolved and proxied via
//     buildProxyURL (base64-encoded destination, no placeholders).
//   - <Initialization sourceURL="…"> attribute — resolved and proxied via
//     buildProxyURL; the range attribute is left untouched.
//
// If XML tokenisation fails at any point the original bytes are returned
// unchanged (best-effort; no panics).
//
// Limitation: nested BaseURL chains are each resolved against opts.Dest rather
// than against each other's accumulated base.
func dashRewrite(h *Handler, r *http.Request, opts *Options, mpd []byte) []byte {
	dec := xml.NewDecoder(bytes.NewReader(mpd))
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)

	extBase := h.externalBase(r)
	inBaseURL := false

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return mpd
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "BaseURL":
				inBaseURL = true

			case "SegmentTemplate":
				for i, attr := range t.Attr {
					switch attr.Name.Local {
					case "initialization", "media":
						abs := resolveURL(opts.Dest, attr.Value)
						t.Attr[i].Value = dashBuildTemplateURL(h, extBase, abs, opts)
					}
				}

			case "SegmentURL":
				for i, attr := range t.Attr {
					switch attr.Name.Local {
					case "media", "index":
						if attr.Value != "" {
							abs := resolveURL(opts.Dest, attr.Value)
							t.Attr[i].Value = h.buildProxyURL(extBase, "/proxy/stream", abs, opts)
						}
					}
				}

			case "Initialization":
				for i, attr := range t.Attr {
					if attr.Name.Local == "sourceURL" {
						abs := resolveURL(opts.Dest, attr.Value)
						t.Attr[i].Value = h.buildProxyURL(extBase, "/proxy/stream", abs, opts)
					}
				}
			}

			if err := enc.EncodeToken(t); err != nil {
				return mpd
			}

		case xml.EndElement:
			inBaseURL = false
			if err := enc.EncodeToken(t); err != nil {
				return mpd
			}

		case xml.CharData:
			if inBaseURL {
				raw := strings.TrimSpace(string(t))
				if raw != "" {
					abs := resolveURL(opts.Dest, raw)
					proxied := h.buildProxyURL(extBase, "/proxy/stream", abs, opts)
					t = xml.CharData(proxied)
					inBaseURL = false // consumed; EndElement handles the empty-content case
				}
				// whitespace-only: keep inBaseURL=true so the real URL on the
				// next CharData token is still caught
			}
			if err := enc.EncodeToken(t); err != nil {
				return mpd
			}

		default:
			if err := enc.EncodeToken(tok); err != nil {
				return mpd
			}
		}
	}

	if err := enc.Flush(); err != nil {
		return mpd
	}
	return buf.Bytes()
}
