package observability

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
)

const (
	performanceThresholdEnv = "RAFAEL_PERF_SLOW_MS"
	defaultSlowThreshold    = 500 * time.Millisecond
)

var (
	thresholdOnce sync.Once
	slowThreshold = defaultSlowThreshold
)

// SlowThreshold is the minimum duration recorded by the lightweight
// performance diagnostics. Set RAFAEL_PERF_SLOW_MS=0 to disable these logs.
func SlowThreshold() time.Duration {
	thresholdOnce.Do(func() {
		value := os.Getenv(performanceThresholdEnv)
		if value == "" {
			return
		}
		milliseconds, err := strconv.Atoi(value)
		if err != nil || milliseconds < 0 {
			logger.Warningf("PERF ignoring invalid %s value", performanceThresholdEnv)
			return
		}
		slowThreshold = time.Duration(milliseconds) * time.Millisecond
	})
	return slowThreshold
}

// ReportSlow logs a slow operation without adding request data such as client
// emails, query strings, credentials, or raw URLs.
func ReportSlow(operation string, elapsed time.Duration, details string) {
	threshold := SlowThreshold()
	if threshold <= 0 || elapsed < threshold {
		return
	}
	logger.Warningf("PERF slow operation=%s duration=%s threshold=%s %s", operation, elapsed.Round(time.Millisecond), threshold, details)
}
