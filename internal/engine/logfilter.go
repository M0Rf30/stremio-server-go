package engine

import (
	"context"
	"errors"
	"log/slog"
)

// readCancelFilter drops Error-level records whose err attribute is a benign
// context cancellation/deadline (anacrolix reader.go logs these unconditionally
// on player seek/disconnect and on our background warm/prefetch cancellations).
// Everything else is delegated unchanged to next.
type readCancelFilter struct{ next slog.Handler }

func newReadCancelFilter(next slog.Handler) *readCancelFilter {
	return &readCancelFilter{next: next}
}

func (h *readCancelFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *readCancelFilter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		drop := false
		r.Attrs(func(a slog.Attr) bool {
			if err, ok := a.Value.Any().(error); ok {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					drop = true
					return false
				}
			}
			return true
		})
		if drop {
			return nil
		}
	}
	return h.next.Handle(ctx, r)
}

func (h *readCancelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &readCancelFilter{next: h.next.WithAttrs(attrs)}
}

func (h *readCancelFilter) WithGroup(name string) slog.Handler {
	return &readCancelFilter{next: h.next.WithGroup(name)}
}
