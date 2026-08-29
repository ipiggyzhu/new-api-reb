package perfmetrics

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
)

func flushLoop() {
	for {
		interval := perf_metrics_setting.GetFlushIntervalMinutes()
		time.Sleep(time.Duration(interval) * time.Minute)
		flushPass()
	}
}

// flushPass runs one flush cycle with its own recover. Without it a panic ends
// flushLoop for good: Init starts the goroutine once from main and there is no
// watchdog, so nothing would ever drain hotBuckets again. That is not just lost
// metrics — Record keeps calling LoadOrStore on every relay, and with no flush
// to Delete the keys the map grows without bound for the life of the process.
func flushPass() {
	defer func() {
		if r := recover(); r != nil {
			common.SysError(fmt.Sprintf("perf metrics flush pass panic recovered: panic=%v\n%s", r, debug.Stack()))
		}
	}()

	setting := perf_metrics_setting.GetSetting()
	if !setting.Enabled {
		return
	}
	flushCompletedBuckets()
	cleanupExpiredMetrics(setting.RetentionDays)
}

func flushCompletedBuckets() {
	currentBucket := bucketStart(time.Now().Unix())
	hotBuckets.Range(func(key, value any) bool {
		k := key.(bucketKey)
		if k.bucketTs >= currentBucket {
			return true
		}

		bucket := value.(*atomicBucket)
		drained := bucket.drain()
		if drained.requestCount == 0 {
			deleteOldEmptyBucket(k, key)
			return true
		}

		err := model.UpsertPerfMetric(&model.PerfMetric{
			ModelName:      k.model,
			Group:          k.group,
			BucketTs:       k.bucketTs,
			RequestCount:   drained.requestCount,
			SuccessCount:   drained.successCount,
			TotalLatencyMs: drained.totalLatencyMs,
			TtftSumMs:      drained.ttftSumMs,
			TtftCount:      drained.ttftCount,
			OutputTokens:   drained.outputTokens,
			GenerationMs:   drained.generationMs,
		})
		if err != nil {
			bucket.addCounters(drained)
			common.SysError(fmt.Sprintf("failed to flush perf metric bucket model=%s group=%s bucket=%d: %s", k.model, k.group, k.bucketTs, err.Error()))
			return true
		}

		deleteOldEmptyBucket(k, key)
		return true
	})
}

func deleteOldEmptyBucket(k bucketKey, rawKey any) {
	if k.bucketTs < bucketStart(time.Now().Add(-24*time.Hour).Unix()) {
		hotBuckets.Delete(rawKey)
	}
}

func cleanupExpiredMetrics(retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	if err := model.DeletePerfMetricsBefore(cutoff); err != nil {
		common.SysError("failed to cleanup expired perf metrics: " + err.Error())
	}
}

func redisCounters(values map[string]string) counters {
	return counters{
		requestCount:   parseRedisInt(values["req"]),
		successCount:   parseRedisInt(values["ok"]),
		totalLatencyMs: parseRedisInt(values["lat"]),
		ttftSumMs:      parseRedisInt(values["ttft"]),
		ttftCount:      parseRedisInt(values["ttft_n"]),
		outputTokens:   parseRedisInt(values["out"]),
		generationMs:   parseRedisInt(values["gen_ms"]),
	}
}

func parseRedisInt(value string) int64 {
	if value == "" {
		return 0
	}
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}
