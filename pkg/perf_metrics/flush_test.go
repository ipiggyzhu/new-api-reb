package perfmetrics

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFlushPassRecoversPanic pins the reason flushPass carries its own recover:
// Init spawns flushLoop once from main.go and there is no watchdog, so a panic in
// the flush cycle would end that goroutine for the life of the process. The
// consequence is worse than lost metrics — Record keeps calling LoadOrStore on
// hotBuckets for every relay, and with no flush left to Delete the keys the map
// grows without bound.
//
// The panic is triggered through the real path rather than a test seam:
// flushCompletedBuckets type-asserts every key with key.(bucketKey), so a
// wrong-typed key in the map panics exactly where a future refactor storing the
// wrong key type would.
func TestFlushPassRecoversPanic(t *testing.T) {
	const bogusKey = "not-a-bucketKey"
	hotBuckets.Store(bogusKey, &atomicBucket{})
	t.Cleanup(func() { hotBuckets.Delete(bogusKey) })

	require.NotPanics(t, flushPass,
		"a panic inside the flush cycle must be contained; propagating it kills flushLoop and hotBuckets then grows unbounded")

	// The pass must remain callable — a recover that ran once is worthless if the
	// surrounding loop cannot make another attempt.
	require.NotPanics(t, flushPass,
		"flushPass must stay usable after recovering, since flushLoop calls it on every interval")
}
