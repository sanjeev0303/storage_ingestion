package logger

import (
	"log/slog"
	"os"
	"strings"
)

// New create a structured logger for the selected environment.
func New(appEnv string) *slog.Logger {
	level := slog.LevelInfo

	if strings.EqualFold(appEnv, "development") {
		level = slog.LevelDebug
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})

	return slog.New(handler)
}
