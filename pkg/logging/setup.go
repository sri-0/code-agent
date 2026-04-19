package logging

import (
	"context"
	"fmt"
	stdlog "log"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"
)

// Setup initializes and returns a configured zerolog.Logger.
// When jsonOutput is true, logs are emitted as structured JSON to stdout
// (ideal for k8s/container runtimes). Otherwise, a human-friendly console
// writer is used on stderr.
func Setup(level string, jsonOutput bool) zerolog.Logger {
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	zerolog.TimeFieldFormat = time.RFC3339Nano

	var logger zerolog.Logger
	if jsonOutput {
		logger = zerolog.New(os.Stdout)
	} else {
		output := zerolog.ConsoleWriter{
			Out:        os.Stderr,
			TimeFormat: time.RFC3339,
			FormatMessage: func(i interface{}) string {
				return fmt.Sprintf("| %s |", i)
			},
			FormatCaller: func(i interface{}) string {
				return fmt.Sprintf("| %s", filepath.Base(fmt.Sprintf("%s", i)))
			},
		}
		logger = zerolog.New(output)
	}

	var gitRevision, goVersion string
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		goVersion = buildInfo.GoVersion
		for _, v := range buildInfo.Settings {
			if v.Key == "vcs.revision" {
				gitRevision = v.Value
				break
			}
		}
	}

	lvl, err := zerolog.ParseLevel(level)
	if err != nil || level == "" {
		lvl = zerolog.InfoLevel
	}

	logger = logger.Level(lvl).With().
		Timestamp().
		Caller().
		Int("pid", os.Getpid()).
		Str("git_revision", gitRevision).
		Str("go_version", goVersion).
		Logger()

	stdlog.SetFlags(0)
	stdlog.SetOutput(logger)

	return logger
}

// Get retrieves the logger from the context.
func Get(ctx context.Context) *zerolog.Logger {
	return zerolog.Ctx(ctx)
}
