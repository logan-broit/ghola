package http_test

import (
	"io"
	"log/slog"
)

// slogNoop returns a logger that discards everything. Used by tests
// that don't care about log output.
func slogNoop() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
