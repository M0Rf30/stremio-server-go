// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Declarative extractor definitions. A definition is a list of steps run
// against a page URL; it can fetch pages, follow JSON fields or links,
// capture values with RE2 regexes and apply a fixed set of transforms, then
// assemble the final stream URL. There is no scripting: hosts that need a
// JavaScript runtime or anti-bot challenges are out of scope.

const (
	defVersion      = 1
	maxDefSteps     = 32
	maxDefFetches   = 8
	maxDefTimeout   = 20 * time.Second
	maxDefTemplates = 4096
)

// DefSet is a parsed extractor definitions document.
type DefSet struct {
	Version    int                   `json:"version"`
	Extractors map[string]*Extractor `json:"extractors"`

	matchers []namedExtractor // extractors with a match pattern, sorted by name
}

type namedExtractor struct {
	name string
	ex   *Extractor
}

// Extractor is a single host definition.
type Extractor struct {
	// Match, when set, is an RE2 regex on /proxy/stream destination URLs:
	// a matching destination (an embed page rather than a media file) is
	// resolved with this definition before streaming, as EasyProxy does.
	Match  string  `json:"match,omitempty"`
	Steps  []*Step `json:"steps"`
	Result Result  `json:"result"`

	match *regexp.Regexp
}

// Step is one extraction action. Exactly one of Fetch, Regex, JSON-on-var or
// Set-literal is the primary action; IfPath/IfURL gate the step.
type Step struct {
	IfPath    string            `json:"if_path,omitempty"`   // regex on the input URL path
	IfURL     string            `json:"if_url,omitempty"`    // regex on the current page URL
	Fetch     string            `json:"fetch,omitempty"`     // template; body becomes the current page
	Headers   map[string]string `json:"headers,omitempty"`   // fetch headers (templates)
	JSON      string            `json:"json,omitempty"`      // dot path into the fetched (or From) JSON
	Regex     string            `json:"regex,omitempty"`     // RE2; group 1 (or whole match) is captured
	From      string            `json:"from,omitempty"`      // variable to read instead of the page body
	Transform []string          `json:"transform,omitempty"` // applied in order to the captured value or body
	Set       string            `json:"set,omitempty"`       // variable receiving the value
	Value     string            `json:"value,omitempty"`     // literal template, used when no other action
	Follow    bool              `json:"follow,omitempty"`    // fetch the captured URL as the new page
	Optional  bool              `json:"optional,omitempty"`  // failure skips the step instead of aborting

	ifPath, ifURL, re *regexp.Regexp
}

// Result describes the resolved stream.
type Result struct {
	URL      string            `json:"url"`
	Query    map[string]string `json:"query,omitempty"`   // set on the URL; empty values are dropped
	Headers  map[string]string `json:"headers,omitempty"` // forwarded to the proxy as h_ params
	Endpoint string            `json:"endpoint"`          // stream | hls | mpd
}

var defEndpoints = map[string]string{
	"stream": "/proxy/stream",
	"hls":    "/proxy/hls/manifest.m3u8",
	"mpd":    "/proxy/mpd/manifest.m3u8",
}

var defTransforms = map[string]func(string) (string, error){
	"base64": func(s string) (string, error) {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return string(b), nil
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
		return string(b), err
	},
	"html_unescape": func(s string) (string, error) { return html.UnescapeString(s), nil },
	"url_unescape":  url.QueryUnescape,
	"unpack_js":     unpackPacked,
	"reverse": func(s string) (string, error) {
		r := []rune(s)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return string(r), nil
	},
	"trim":         func(s string) (string, error) { return strings.TrimSpace(s), nil },
	"scheme_https": func(s string) (string, error) { return schemeHTTPS(s), nil },
}

func schemeHTTPS(s string) string {
	if strings.HasPrefix(s, "//") {
		return "https:" + s
	}
	return s
}

// ParseDefSet parses and validates a definitions document; regexes are
// compiled once here so a bad pattern rejects the whole document.
func ParseDefSet(b []byte) (*DefSet, error) {
	var ds DefSet
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ds); err != nil {
		return nil, fmt.Errorf("parse extractors: %w", err)
	}
	if ds.Version != defVersion {
		return nil, fmt.Errorf("unsupported extractors version %d", ds.Version)
	}
	norm := make(map[string]*Extractor, len(ds.Extractors))
	for name, ex := range ds.Extractors {
		if ex == nil {
			return nil, fmt.Errorf("extractor %q: empty", name)
		}
		if err := ex.compile(); err != nil {
			return nil, fmt.Errorf("extractor %q: %w", name, err)
		}
		norm[strings.ToLower(strings.TrimSpace(name))] = ex
	}
	ds.Extractors = norm
	// Matchers sorted by name, so /proxy/stream's per-request match is a
	// plain loop with a deterministic winner.
	for name, ex := range norm {
		if ex.match != nil {
			ds.matchers = append(ds.matchers, namedExtractor{name: name, ex: ex})
		}
	}
	slices.SortFunc(ds.matchers, func(a, b namedExtractor) int { return strings.Compare(a.name, b.name) })
	return &ds, nil
}

func (ex *Extractor) compile() error {
	if len(ex.Steps) == 0 || len(ex.Steps) > maxDefSteps {
		return fmt.Errorf("need 1..%d steps", maxDefSteps)
	}
	if _, ok := defEndpoints[ex.Result.Endpoint]; !ok {
		return fmt.Errorf("unknown endpoint %q", ex.Result.Endpoint)
	}
	if ex.Result.URL == "" {
		return fmt.Errorf("result.url is required")
	}
	var err error
	if ex.match, err = compileOpt(ex.Match); err != nil {
		return fmt.Errorf("match: %w", err)
	}
	for i, s := range ex.Steps {
		if s == nil {
			return fmt.Errorf("step %d: empty", i)
		}
		for _, t := range s.Transform {
			if _, ok := defTransforms[t]; !ok {
				return fmt.Errorf("step %d: unknown transform %q", i, t)
			}
		}
		if s.Follow && s.Regex == "" && s.JSON == "" {
			return fmt.Errorf("step %d: follow needs regex or json", i)
		}
		if s.Fetch == "" && s.Regex == "" && s.JSON == "" && s.Value == "" && len(s.Transform) == 0 {
			return fmt.Errorf("step %d: no action", i)
		}
		if s.ifPath, err = compileOpt(s.IfPath); err != nil {
			return fmt.Errorf("step %d if_path: %w", i, err)
		}
		if s.ifURL, err = compileOpt(s.IfURL); err != nil {
			return fmt.Errorf("step %d if_url: %w", i, err)
		}
		if s.re, err = compileOpt(s.Regex); err != nil {
			return fmt.Errorf("step %d regex: %w", i, err)
		}
	}
	return nil
}

func compileOpt(p string) (*regexp.Regexp, error) {
	if p == "" {
		return nil, nil
	}
	return regexp.Compile(p)
}

// defRun holds the variables of one extraction.
type defRun struct {
	vars    map[string]string
	fetches int
}

// run executes the definition for input page URL.
func (ex *Extractor) run(r *http.Request, input string, get pageFetcher) (*extractResult, error) {
	in, err := url.Parse(input)
	if err != nil {
		return nil, fmt.Errorf("%w: bad input url", errExtract)
	}
	st := &defRun{vars: map[string]string{
		"input":       input,
		"origin":      in.Scheme + "://" + in.Host,
		"host":        in.Host,
		"path":        in.Path,
		"query":       in.RawQuery,
		"url":         input,
		"page_origin": in.Scheme + "://" + in.Host,
		"body":        "",
	}}
	for i, s := range ex.Steps {
		if err := st.step(r, s, in, get); err != nil {
			if s.Optional {
				continue
			}
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
	}
	return st.result(&ex.Result)
}

func (st *defRun) step(r *http.Request, s *Step, in *url.URL, get pageFetcher) error {
	if s.ifPath != nil && !s.ifPath.MatchString(in.Path) {
		return nil
	}
	if s.ifURL != nil && !s.ifURL.MatchString(st.vars["url"]) {
		return nil
	}
	if s.Fetch != "" {
		target := resolveURL(st.vars["url"], st.expand(s.Fetch))
		body, err := st.fetch(r, target, s.Headers, get)
		if err != nil {
			return err
		}
		if s.JSON == "" && s.Regex == "" {
			st.setPage(target, body)
			return st.applyTransforms(s, "body", body)
		}
		return st.capture(r, s, body, get)
	}
	src := st.vars["body"]
	if s.From != "" {
		src = st.vars[s.From]
	}
	if s.JSON == "" && s.Regex == "" {
		if s.Value != "" {
			return st.store(r, s, st.expand(s.Value), get)
		}
		name := s.From
		if name == "" {
			name = "body"
		}
		if s.Set != "" {
			name = s.Set
		}
		return st.applyTransforms(s, name, src)
	}
	return st.capture(r, s, src, get)
}

func (st *defRun) capture(r *http.Request, s *Step, src string, get pageFetcher) error {
	val := src
	if s.JSON != "" {
		var doc any
		if err := json.Unmarshal([]byte(val), &doc); err != nil {
			return fmt.Errorf("%w: not json", errExtract)
		}
		v, ok := jsonPath(doc, s.JSON)
		if !ok {
			return fmt.Errorf("%w: json %q missing", errExtract, s.JSON)
		}
		val = v
	}
	if s.re != nil {
		m := s.re.FindStringSubmatch(val)
		if m == nil {
			return fmt.Errorf("%w: regex did not match", errExtract)
		}
		val = m[0]
		if len(m) > 1 {
			val = m[1]
		}
	}
	return st.store(r, s, val, get)
}

func (st *defRun) store(r *http.Request, s *Step, val string, get pageFetcher) error {
	for _, t := range s.Transform {
		v, err := defTransforms[t](val)
		if err != nil {
			return fmt.Errorf("%w: transform %s: %w", errExtract, t, err)
		}
		val = v
	}
	if s.Set != "" {
		st.vars[s.Set] = val
	}
	if s.Follow {
		target := resolveURL(st.vars["url"], schemeHTTPS(val))
		body, err := st.fetch(r, target, mergeReferer(s.Headers, st.vars["url"]), get)
		if err != nil {
			return err
		}
		st.setPage(target, body)
	}
	return nil
}

// setPage makes target the current page and updates {url}/{page_origin}.
func (st *defRun) setPage(target, body string) {
	st.vars["url"], st.vars["body"] = target, body
	if u, err := url.Parse(target); err == nil {
		st.vars["page_origin"] = u.Scheme + "://" + u.Host
	}
}

func (st *defRun) applyTransforms(s *Step, name, val string) error {
	for _, t := range s.Transform {
		v, err := defTransforms[t](val)
		if err != nil {
			return fmt.Errorf("%w: transform %s: %w", errExtract, t, err)
		}
		val = v
	}
	st.vars[name] = val
	return nil
}

func mergeReferer(h map[string]string, ref string) map[string]string {
	out := map[string]string{"Referer": ref}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func (st *defRun) fetch(r *http.Request, target string, hdr map[string]string, get pageFetcher) (string, error) {
	st.fetches++
	if st.fetches > maxDefFetches {
		return "", fmt.Errorf("%w: too many fetches", errExtract)
	}
	h := make(map[string]string, len(hdr))
	for k, v := range hdr {
		h[k] = st.expand(v)
	}
	return get(r, target, h)
}

func (st *defRun) result(res *Result) (*extractResult, error) {
	raw := schemeHTTPS(st.expand(res.URL))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: bad result url", errExtract)
	}
	if len(res.Query) > 0 {
		q := u.Query()
		for k, v := range res.Query {
			if ev := st.expand(v); ev != "" {
				q.Set(k, ev)
			}
		}
		u.RawQuery = q.Encode()
	}
	hdr := make(map[string]string, len(res.Headers))
	for k, v := range res.Headers {
		if ev := st.expand(v); ev != "" {
			hdr[k] = ev
		}
	}
	return &extractResult{URL: u.String(), Headers: hdr, Endpoint: defEndpoints[res.Endpoint]}, nil
}

var tmplRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(\?[^{}]*)?\}`)

// expand substitutes {var} with its value and {var?text} with text when var
// is non-empty (else ""). Unknown variables expand to "".
func (st *defRun) expand(t string) string {
	if len(t) > maxDefTemplates {
		t = t[:maxDefTemplates]
	}
	return tmplRe.ReplaceAllStringFunc(t, func(m string) string {
		sm := tmplRe.FindStringSubmatch(m)
		v := st.vars[sm[1]]
		if sm[2] != "" {
			if v == "" {
				return ""
			}
			return sm[2][1:]
		}
		return v
	})
}

// jsonPath walks a dot path (numeric segments index arrays) and returns the
// leaf as a string.
func jsonPath(doc any, path string) (string, bool) {
	cur := doc
	for _, p := range strings.Split(path, ".") {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[p]
			if !ok {
				return "", false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return "", false
			}
			cur = c[i]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

var (
	packedRe   = regexp.MustCompile(`}\('((?:[^'\\]|\\.)*)',\s*(\d+),\s*(\d+),\s*'((?:[^'\\]|\\.)*)'\.split\('\|'\)`)
	packedWord = regexp.MustCompile(`\b\w+\b`)
)

const base62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// unpackPacked finds a p,a,c,k,e,d-packed script in s and returns it with the
// unpacked source appended, so later regexes can match either.
func unpackPacked(s string) (string, error) {
	m := packedRe.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("no packed script")
	}
	a, err := strconv.Atoi(m[2])
	if err != nil || a < 2 || a > 62 {
		return "", fmt.Errorf("bad radix")
	}
	c, err := strconv.Atoi(m[3])
	if err != nil {
		return "", fmt.Errorf("bad count")
	}
	k := strings.Split(m[4], "|")
	p := strings.ReplaceAll(m[1], `\'`, `'`)
	out := packedWord.ReplaceAllStringFunc(p, func(word string) string {
		n, ok := parseBase(word, a)
		if !ok || n >= c || n >= len(k) || k[n] == "" {
			return word
		}
		return k[n]
	})
	return s + "\n" + out, nil
}

func parseBase(s string, radix int) (int, bool) {
	n := 0
	for _, ch := range s {
		d := strings.IndexRune(base62[:radix], ch)
		if d < 0 || n > (1<<30)/radix {
			return 0, false
		}
		n = n*radix + d
	}
	return n, true
}
