// Package observability wires up logging and metrics.
package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// NewLogger builds the application logger.
//
// JSON in production so logs are machine-readable; text in development because
// a human is reading them. Every record carries the service name and version,
// so logs from two versions during a rolling update are distinguishable.
func NewLogger(c config.Config, w *os.File) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(c.Log.Level),
		ReplaceAttr: redactAttrs,
	}

	var h slog.Handler
	if c.Log.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	info := build.Get()
	return slog.New(h).With(
		slog.String("service", "linkctrl"),
		slog.String("version", info.Version),
	)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// sensitiveKeys are redacted wherever they appear, at any nesting depth.
//
// config.Secret already refuses to print itself, so this is a second line of
// defence for the values that are NOT wrapped in Secret: a raw header map, a
// form value, a cookie, a query string containing a token. The realistic
// accident is someone adding slog.Any("headers", r.Header) while debugging and
// leaving it in.
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "pepper",
	"authorization", "cookie", "set-cookie", "api_key", "apikey",
	"session", "credential", "private_key", "signature",
}

func redactAttrs(_ []string, a slog.Attr) slog.Attr {
	lower := strings.ToLower(a.Key)
	for _, k := range sensitiveKeys {
		if strings.Contains(lower, k) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

// loggerKey is the context key for the request-scoped logger.
type loggerKey struct{}

// ContextWithLogger returns a context carrying the given logger.
func ContextWithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// LoggerFrom returns the request-scoped logger, or the default logger when
// there is none. It never returns nil, so callers need no nil check and a
// missing middleware degrades to unattributed logs rather than a panic.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
