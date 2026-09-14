// Package logger provides a minimal logging facade for agenticgo.
//
// Call sites depend on the small Logger interface (structured key/value
// pairs, four levels), so any backend can be swapped in without touching
// callers. The default backend is stdlib log/slog writing human-readable
// text to stderr; New(false) logs at info level, New(true) at debug level
// (set AGENTICGO_DEBUG=true).
package logger

import (
	"log/slog"
	"os"
)

// Logger is the minimal structured logging surface used across agenticgo.
// keysvals are alternating keys and values, slog-style:
//
//	log.Debug("testing provider", "name", name, "base_url", url)
type Logger interface {
	Info(msg string, keysvals ...any)
	Error(msg string, keysvals ...any)
	Debug(msg string, keysvals ...any)
	Warn(msg string, keysvals ...any)
}

// SlogLogger adapts a stdlib slog.Logger to Logger.
type SlogLogger struct {
	l *slog.Logger
}

func (s SlogLogger) Info(msg string, keysvals ...any)  { s.l.Info(msg, keysvals...) }
func (s SlogLogger) Error(msg string, keysvals ...any) { s.l.Error(msg, keysvals...) }
func (s SlogLogger) Debug(msg string, keysvals ...any) { s.l.Debug(msg, keysvals...) }
func (s SlogLogger) Warn(msg string, keysvals ...any)  { s.l.Warn(msg, keysvals...) }

// New returns a Logger writing human-readable text to stderr. When debug is
// true the debug level is enabled (verbose logging); otherwise info and up.
func New(debug bool) Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return SlogLogger{l: slog.New(h)}
}

// Nop returns a Logger that discards everything — handy for tests and as a
// zero value before a real logger is wired in.
func Nop() Logger {
	return SlogLogger{l: slog.New(slog.DiscardHandler)}
}
