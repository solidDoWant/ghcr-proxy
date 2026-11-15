// Package audit writes one JSON line per request to a writer, normally stdout. A line carries who asked, what for
// and what the proxy decided. It never carries header values, tokens or query strings: Entry has no field for them.
package audit

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Entry is one audited request.
type Entry struct {
	Time       time.Time
	ClientIP   string
	Namespace  string
	SA         string
	Pod        string
	Method     string
	Path       string // escaped path, never the query
	Repository string
	From       string // mount source, when forwarded
	Action     string // none, pull, push, delete, unknown
	Decision   string // allowed, anonymous, denied, unauthenticated, unavailable, notfound, invalid
	Policy     string // the allowing policy's name
	Status     int    // what the client got
	Upstream   int
	Bytes      int64
	DurationMS int64
	Digest     string // manifest push or delete
}

// Logger writes entries. A nil Logger discards them.
type Logger struct {
	logger *slog.Logger
}

// New returns a Logger that writes JSON lines to out.
func New(out io.Writer) *Logger {
	return &Logger{logger: slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			// The line is an audit record, not a log message: keep only the entry's own fields.
			if len(groups) == 0 && (attr.Key == slog.LevelKey || attr.Key == slog.MessageKey) {
				return slog.Attr{}
			}

			return attr
		},
	}))}
}

// Log writes entry as one line. Empty optional fields are left out.
func (l *Logger) Log(entry Entry) {
	if l == nil {
		return
	}

	attrs := make([]slog.Attr, 0, 16)
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}

	add("clientIP", entry.ClientIP)
	add("namespace", entry.Namespace)
	add("serviceAccount", entry.SA)
	add("pod", entry.Pod)
	add("method", entry.Method)
	add("path", entry.Path)
	add("repository", entry.Repository)
	add("from", entry.From)
	attrs = append(attrs, slog.String("action", entry.Action), slog.String("decision", entry.Decision))
	add("policy", entry.Policy)

	attrs = append(attrs, slog.Int("status", entry.Status))
	if entry.Upstream != 0 {
		attrs = append(attrs, slog.Int("upstreamStatus", entry.Upstream))
	}

	attrs = append(attrs, slog.Int64("bytes", entry.Bytes), slog.Int64("durationMs", entry.DurationMS))
	add("digest", entry.Digest)

	// The record time is the entry's, so the line's "time" is when the request started.
	record := slog.NewRecord(entry.Time, slog.LevelInfo, "", 0)
	record.AddAttrs(attrs...)
	_ = l.logger.Handler().Handle(context.Background(), record)
}
