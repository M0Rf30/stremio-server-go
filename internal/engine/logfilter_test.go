package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
)

// captureHandler is a minimal in-memory slog.Handler that appends each
// record's message to msgs. It is not safe for concurrent use.
type captureHandler struct {
	msgs []string
}

func (c *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.msgs = append(c.msgs, r.Message)
	return nil
}

func (c *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(_ string) slog.Handler      { return c }

func TestReadCancelFilterDropsCanceled(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(newReadCancelFilter(cap))

	logger.Error("initial read failed", "err", context.Canceled)
	if got := len(cap.msgs); got != 0 {
		t.Errorf("context.Canceled Error: want 0 records, got %d", got)
	}
}

func TestReadCancelFilterDropsDeadlineExceeded(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(newReadCancelFilter(cap))

	logger.Error("x", "err", context.DeadlineExceeded)
	if got := len(cap.msgs); got != 0 {
		t.Errorf("context.DeadlineExceeded Error: want 0 records, got %d", got)
	}
}

func TestReadCancelFilterDropsWrappedCanceled(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(newReadCancelFilter(cap))

	logger.Error("x", "err", fmt.Errorf("wrap: %w", context.Canceled))
	if got := len(cap.msgs); got != 0 {
		t.Errorf("wrapped context.Canceled Error: want 0 records, got %d", got)
	}
}

func TestReadCancelFilterPassesRealError(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(newReadCancelFilter(cap))

	logger.Error("real failure", "err", errors.New("boom"))
	if got := len(cap.msgs); got != 1 {
		t.Errorf("real error: want 1 record, got %d", got)
	}
}

func TestReadCancelFilterPassesNoErrAttr(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(newReadCancelFilter(cap))

	logger.Error("no err attr")
	if got := len(cap.msgs); got != 1 {
		t.Errorf("no err attr: want 1 record, got %d", got)
	}
}

func TestReadCancelFilterPassesInfoLevel(t *testing.T) {
	cap := &captureHandler{}
	logger := slog.New(newReadCancelFilter(cap))

	logger.Info("info", "err", context.Canceled)
	if got := len(cap.msgs); got != 1 {
		t.Errorf("Info with context.Canceled: want 1 record (only Error+ filtered), got %d", got)
	}
}

// TestReadCancelFilterWithGroup verifies that the filter returned by WithGroup
// still suppresses context-cancel errors and forwards real errors.
func TestReadCancelFilterWithGroup(t *testing.T) {
	cap := &captureHandler{}
	grouped := newReadCancelFilter(cap).WithGroup("engine")

	logger := slog.New(grouped)

	// Real error must reach cap.
	logger.Error("disk fault", "err", errors.New("i/o error"))
	if len(cap.msgs) != 1 {
		t.Errorf("WithGroup real error: want 1 record, got %d", len(cap.msgs))
	}

	// Cancel error must still be dropped by the outer filter.
	logger.Error("cancel", "err", fmt.Errorf("wrapped: %w", context.Canceled))
	if len(cap.msgs) != 1 {
		t.Errorf("WithGroup cancel: must be filtered; got %d records", len(cap.msgs))
	}
}

// TestReadCancelFilterWithAttrs verifies that the filter returned by WithAttrs
// still suppresses context-cancel errors and forwards real errors.
func TestReadCancelFilterWithAttrs(t *testing.T) {
	cap := &captureHandler{}
	withAttrs := newReadCancelFilter(cap).WithAttrs([]slog.Attr{
		slog.String("component", "memstorage"),
	})

	logger := slog.New(withAttrs)

	// Real error must reach cap.
	logger.Error("piece read failed", "err", errors.New("EIO"))
	if len(cap.msgs) != 1 {
		t.Errorf("WithAttrs real error: want 1 record, got %d", len(cap.msgs))
	}

	// DeadlineExceeded must still be dropped.
	logger.Error("timeout", "err", context.DeadlineExceeded)
	if len(cap.msgs) != 1 {
		t.Errorf("WithAttrs DeadlineExceeded: must be filtered; got %d records", len(cap.msgs))
	}
}
