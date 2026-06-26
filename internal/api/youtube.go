// Package api — GET /yt/:id and GET /yt/:id.json
//
// Shells out to the system yt-dlp binary to resolve YouTube video formats.
// /yt/:id.json   → JSON {url, itag, quality, container, hasVideo, hasAudio,
//
//	isLive, isHLS, isDashMPD, approxDurationMs, mimeType}
//
// /yt/:id        → 307 redirect to the resolved format URL
//
// The reference (server/src/routes/youtube.rs) uses the rusty_ytdl library;
// we shell out to yt-dlp instead because it is widely available and actively
// maintained. Behaviour is equivalent: pick a progressive (audio+video) mp4
// format, fall back to any progressive format.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ytIDRe matches valid YouTube video IDs: 1-20 URL-safe characters.
// Full URLs are rejected to prevent SSRF via yt-dlp.
var ytIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,20}$`)

// ytFormat is the subset of yt-dlp's per-format JSON we consume.
type ytFormat struct {
	FormatID   string  `json:"format_id"`
	URL        string  `json:"url"`
	Ext        string  `json:"ext"`
	VCodec     string  `json:"vcodec"`
	ACodec     string  `json:"acodec"`
	Protocol   string  `json:"protocol"`
	IsLive     bool    `json:"is_live"`
	Duration   float64 `json:"duration"`    // seconds; multiply × 1000 for ms
	Quality    float64 `json:"quality"`     // yt-dlp numeric quality score
	FormatNote string  `json:"format_note"` // e.g. "720p"
	MimeType   string  `json:"mimetype"`    // sometimes populated
	TBR        float64 `json:"tbr"`
}

// ytInfo is the top-level yt-dlp JSON output.
type ytInfo struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Duration float64    `json:"duration"`
	IsLive   bool       `json:"is_live"`
	Formats  []ytFormat `json:"formats"`
}

// handleYT dispatches /yt/:id and /yt/:id.json.
// seg1 is the path segment after "yt" — may end in ".json".
//
// @Summary  Resolve a YouTube id via yt-dlp
// @Tags     YouTube
// @Param    id   path  string  true  "YouTube video id (optionally .json)"
// @Success  307  {string}  string  "redirect"
// @Success  200  {object}  map[string]interface{}  "format JSON for .json"
// @Failure  400
// @Failure  501
// @Router   /yt/{id} [get]
func (s *server) handleYT(w http.ResponseWriter, r *http.Request, seg1 string) {
	wantJSON := strings.HasSuffix(seg1, ".json")
	id := strings.TrimSuffix(seg1, ".json")

	// Validate: accept only bare YouTube video IDs (1-20 URL-safe chars).
	// Reject anything else — including full URLs — to prevent SSRF.
	if !ytIDRe.MatchString(id) {
		http.Error(w, "invalid YouTube video ID", http.StatusBadRequest)
		return
	}

	videoURL := "https://www.youtube.com/watch?v=" + id

	ytPath, err := exec.LookPath("yt-dlp")
	if err != nil {
		http.Error(w, "yt-dlp not found on PATH — YouTube routes require yt-dlp installed", http.StatusNotImplemented)
		return
	}

	// -j: dump JSON info without downloading; --no-warnings: quieter stderr.
	cmd := exec.CommandContext(r.Context(), ytPath, "-j", "--no-warnings", videoURL)
	out, err := cmd.Output()
	if err != nil {
		// Exit code + stderr from yt-dlp
		msg := fmt.Sprintf("yt-dlp error: %v", err)
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg = fmt.Sprintf("yt-dlp: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		http.Error(w, msg, http.StatusBadGateway)
		return
	}

	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		http.Error(w, "parse yt-dlp JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}

	chosen := pickYTFormat(info.Formats)
	if chosen == nil {
		http.Error(w, "no suitable progressive video format found", http.StatusNotFound)
		return
	}

	if !wantJSON {
		// /yt/:id → 307 redirect to the direct video URL
		http.Redirect(w, r, chosen.URL, http.StatusTemporaryRedirect)
		return
	}

	// /yt/:id.json → shaped JSON (mirrors rusty_ytdl output used by reference)
	hasVideo := chosen.VCodec != "" && chosen.VCodec != "none"
	hasAudio := chosen.ACodec != "" && chosen.ACodec != "none"
	isHLS := strings.Contains(chosen.Protocol, "m3u8")
	isDashMPD := strings.Contains(chosen.Protocol, "dash")
	approxMs := int64(chosen.Duration * 1000)
	if approxMs == 0 && info.Duration > 0 {
		approxMs = int64(info.Duration * 1000)
	}

	mimeType := chosen.MimeType
	if mimeType == "" {
		// Build a synthetic MIME from vcodec + acodec, similar to how YouTube
		// reports it. We use a simplified form without codec parameters.
		container := chosen.Ext
		if container == "" {
			container = "mp4"
		}
		mimeType = "video/" + container
		if hasVideo && hasAudio {
			// Append both codec hints if available.
			codecs := []string{}
			if chosen.VCodec != "" && chosen.VCodec != "none" {
				codecs = append(codecs, chosen.VCodec)
			}
			if chosen.ACodec != "" && chosen.ACodec != "none" {
				codecs = append(codecs, chosen.ACodec)
			}
			if len(codecs) > 0 {
				mimeType += `; codecs="` + strings.Join(codecs, ", ") + `"`
			}
		}
	}

	quality := chosen.FormatNote
	if quality == "" {
		quality = strconv.FormatFloat(chosen.Quality, 'f', 0, 64)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":              chosen.URL,
		"itag":             chosen.FormatID,
		"quality":          quality,
		"container":        chosen.Ext,
		"hasVideo":         hasVideo,
		"hasAudio":         hasAudio,
		"isLive":           chosen.IsLive || info.IsLive,
		"isHLS":            isHLS,
		"isDashMPD":        isDashMPD,
		"approxDurationMs": approxMs,
		"mimeType":         mimeType,
	})
}

// pickYTFormat selects a progressive (audio + video) format from the list,
// preferring mp4. Falls back to any progressive format.
// Mirrors the selection logic in the Rust reference (youtube.rs).
func pickYTFormat(formats []ytFormat) *ytFormat {
	// First pass: mp4 with both audio and video.
	for i := len(formats) - 1; i >= 0; i-- {
		f := &formats[i]
		if isProgressive(f) && f.Ext == "mp4" {
			return f
		}
	}
	// Second pass: any progressive format.
	for i := len(formats) - 1; i >= 0; i-- {
		f := &formats[i]
		if isProgressive(f) {
			return f
		}
	}
	return nil
}

func isProgressive(f *ytFormat) bool {
	return f.VCodec != "" && f.VCodec != "none" &&
		f.ACodec != "" && f.ACodec != "none"
}
