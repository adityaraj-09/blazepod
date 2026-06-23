// Layer: shared internal — structured logging setup for all sandock services.
// Wraps go.uber.org/zap, chosen for its structured fields, zero-allocation hot path,
// and sub-millisecond GC impact. Every service calls log.Init() at startup.
package log

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// L is the global logger. Services call log.Init() to configure it,
// then use log.L.Info(...) / log.L.With(...) throughout.
var L *zap.Logger

// Init configures the global logger from the given level and format strings.
// level: "debug" | "info" | "warn" | "error"
// format: "json" | "console"
func Init(level, format string) error {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return fmt.Errorf("log: invalid level %q: %w", level, err)
	}

	var cfg zap.Config
	if format == "console" {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(lvl)

	logger, err := cfg.Build(zap.AddCallerSkip(0))
	if err != nil {
		return fmt.Errorf("log: build logger: %w", err)
	}
	L = logger
	return nil
}

// MustInit calls Init and panics on error. Convenience for main().
func MustInit(level, format string) {
	if err := Init(level, format); err != nil {
		panic(err)
	}
}
