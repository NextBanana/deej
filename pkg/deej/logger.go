package deej

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/omriharel/deej/pkg/deej/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	buildTypeNone    = ""
	buildTypeDev     = "dev"
	buildTypeRelease = "release"

	logDirectory     = "logs"
	logFilename      = "deej-latest-run.log"
	crashLogFilename = "deej-crash.log"

	// temporary diagnostic aid. deej only writes to the log when a slider is moved,
	// so a crash can't be located in time at all - the log simply stops somewhere
	// after the last interaction. remove this once the crash is understood
	heartbeatInterval = time.Minute * 5
)

// NewLogger provides a logger instance for the whole program
func NewLogger(buildType string) (*zap.SugaredLogger, error) {
	var loggerConfig zap.Config
	var stderrErr error

	// release: info and above, log to file only (no UI)
	if buildType == buildTypeRelease {
		if err := util.EnsureDirExists(logDirectory); err != nil {
			return nil, fmt.Errorf("ensure log directory exists: %w", err)
		}

		loggerConfig = zap.NewProductionConfig()

		loggerConfig.OutputPaths = []string{filepath.Join(logDirectory, logFilename)}
		loggerConfig.Encoding = "console"

		// a release build has no console, so a panic would otherwise be swallowed
		// whole. this is the only way a crash ever becomes visible
		stderrErr = redirectStderrToFile(filepath.Join(logDirectory, crashLogFilename))

		// development: debug and above, log to stderr only, colorful
	} else {
		loggerConfig = zap.NewDevelopmentConfig()

		// make it colorful
		loggerConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	// all build types: make it readable
	loggerConfig.EncoderConfig.EncodeCaller = nil
	loggerConfig.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.Format("2006-01-02 15:04:05.000"))
	}

	loggerConfig.EncoderConfig.EncodeName = func(s string, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(fmt.Sprintf("%-27s", s))
	}

	logger, err := loggerConfig.Build()
	if err != nil {
		return nil, fmt.Errorf("create zap logger: %w", err)
	}

	// no reason not to use the sugared logger - it's fast enough for anything we're gonna do
	sugar := logger.Sugar()

	if stderrErr != nil {
		sugar.Warnw("Failed to capture crash output to file", "error", stderrErr)
	}

	startHeartbeat(sugar)

	return sugar, nil
}

// startHeartbeat writes a marker at a fixed interval. It answers three questions a
// crash otherwise leaves open: when the process actually died, whether it died
// while idle or in the middle of something, and - if the crash log stays empty
// while these markers stop - whether it was terminated from outside rather than
// crashing on its own
func startHeartbeat(logger *zap.SugaredLogger) {
	go func() {
		started := time.Now()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for range ticker.C {
			logger.Infow("Still alive", "uptime", time.Since(started).Round(time.Second).String())
		}
	}()
}
