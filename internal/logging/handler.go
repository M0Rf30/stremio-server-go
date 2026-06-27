package logging

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// textHandler is a compact, human-readable slog.Handler. Each record renders as
//
//	<RFC3339-millis> <LEVEL> <component>: <message>  key=value key=value
//
// The reserved "component" attribute is lifted out as the "name:" prefix; all
// other attributes are appended as space-separated key=value pairs, quoted when
// they contain spaces, quotes, or '='. Groups prefix their attribute keys with
// "group.". The handler is safe for concurrent use.
type textHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level

	// attrs holds preformatted " key=value" fragments accumulated via WithAttrs.
	attrs string
	// component is the most recent component value seen via WithAttrs.
	component string
	// groupPrefix is the dotted prefix applied to keys from WithGroup.
	groupPrefix string
}

func newTextHandler(w io.Writer, level slog.Level) slog.Handler {
	return &textHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)

	// Timestamp (millisecond precision, UTC-aware via the record's own zone).
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	buf = t.AppendFormat(buf, "2006-01-02T15:04:05.000Z07:00")
	buf = append(buf, ' ')

	// Fixed-width level for column alignment.
	buf = append(buf, levelLabel(r.Level)...)
	buf = append(buf, ' ')

	// Component prefix (from accumulated attrs or this record's attrs).
	component := h.component
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == componentKey && h.groupPrefix == "" {
			component = a.Value.String()
		}
		return true
	})
	if component != "" {
		buf = append(buf, component...)
		buf = append(buf, ':', ' ')
	}

	buf = append(buf, r.Message...)

	// Accumulated attrs first, then this record's attrs.
	buf = append(buf, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == componentKey && h.groupPrefix == "" {
			return true // already rendered as the prefix
		}
		buf = appendAttr(buf, h.groupPrefix, a)
		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	for _, a := range attrs {
		if a.Key == componentKey && h.groupPrefix == "" {
			clone.component = a.Value.String()
			continue
		}
		clone.attrs = string(appendAttr([]byte(clone.attrs), h.groupPrefix, a))
	}
	return &clone
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groupPrefix = h.groupPrefix + name + "."
	return &clone
}

// appendAttr writes " key=value" for a, prefixing the key with prefix and
// quoting the value when it contains spaces, quotes, or '='.
func appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	// Flatten groups into prefixed keys.
	if a.Value.Kind() == slog.KindGroup {
		gs := a.Value.Group()
		if len(gs) == 0 {
			return buf
		}
		np := prefix
		if a.Key != "" {
			np = prefix + a.Key + "."
		}
		for _, ga := range gs {
			buf = appendAttr(buf, np, ga)
		}
		return buf
	}
	buf = append(buf, ' ')
	buf = append(buf, prefix...)
	buf = append(buf, a.Key...)
	buf = append(buf, '=')
	return append(buf, quoteValue(a.Value.String())...)
}

// quoteValue returns v unchanged when it is a simple token, otherwise a
// strconv-quoted form so the key=value stream stays unambiguous.
func quoteValue(v string) string {
	if v == "" {
		return `""`
	}
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == ' ' || c == '=' || c == '"' || c < 0x20:
			return strconv.Quote(v)
		}
	}
	return v
}

func levelLabel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO "
	case l < slog.LevelError:
		return "WARN "
	default:
		return "ERROR"
	}
}
