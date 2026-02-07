package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Init sets a JSON (default) or text slog handler based on the provided format.
// Supported: "json" (default), "text".
func Init(service, format string) *slog.Logger {
	format = strings.ToLower(strings.TrimSpace(format))
	opts := &slog.HandlerOptions{}

	var handler slog.Handler
	switch format {
	case "", "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	logger := slog.New(handler).With("service", service)
	slog.SetDefault(logger)

	if format != "" && format != "json" && format != "text" {
		logger.Warn("unknown log format, defaulting to json", "format", format)
	}
	return logger
}
