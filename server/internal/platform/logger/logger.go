package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New create a structured logger for the selected environment.
func New(appEnv string) *slog.Logger {
	return NewTo(os.Stdout, appEnv)
}

// NewTo is New writing to w, for commands whose stdout carries a report.
func NewTo(w io.Writer, appEnv string) *slog.Logger {
	level := slog.LevelInfo

	if strings.EqualFold(appEnv, "development") {
		level = slog.LevelDebug
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	})

	return slog.New(handler)
}
