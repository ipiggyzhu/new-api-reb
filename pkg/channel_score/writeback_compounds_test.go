package channel_score

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWritingShiftedPriorityBackCompounds is the reason the adjustment is
// request-local and the priority column is never rewritten.
//
// It simulates the change an operator naturally asks for — "when scoring decides
// this channel deserves +1, store 1 in its priority" — by feeding each round's
// output back in as the next round's configured baseline, which is exactly what
// persisting it would do.
//
// The offset stays clamped at the configured maximum every round. The baseline
// does not: shiftByTiers relocates from the already-shifted value, so the channel
// walks away from every sibling without bound. MaxPromoteTiers caps the offset,
// never the accumulated priority.
func TestWritingShiftedPriorityBackCompounds(t *testing.T) {
	// The live configuration: every channel flattened to priority 0.
	const others = 0
	// The cap an operator would expect to bound this.
	const maxPromoteTiers = 1

	priority := int64(0)
	seen := []int64{priority}

	for round := 0; round < 6; round++ {
		// The candidate set as the selection path sees it this round: this channel
		// at its current stored priority, plus the flattened siblings.
		candidates := []Candidate{
			{ChannelId: 31, Priority: priority, Weight: 10},
			{ChannelId: 8, Priority: others, Weight: 10},
			{ChannelId: 5, Priority: others, Weight: 10},
		}
		tiers := distinctTiersDesc(candidates)

		// Offset is recomputed from scratch and is always within the cap.
		offset := maxPromoteTiers
		require.LessOrEqual(t, offset, maxPromoteTiers, "offset never exceeds the cap")

		priority = shiftByTiers(tiers, priority, offset)
		seen = append(seen, priority)
	}

	t.Log("stored priority per round: " + fmt.Sprint(seen))

	// Six rounds of a capped +1 produce a priority six tiers above the siblings.
	assert.Equal(t, []int64{0, 1, 2, 3, 4, 5, 6}, seen,
		"a capped offset still compounds without bound once written back")

	// The same six rounds without write-back: the baseline is untouched and the
	// effective priority is the same every time, which is the actual behaviour.
	stable := int64(0)
	for round := 0; round < 6; round++ {
		candidates := []Candidate{
			{ChannelId: 31, Priority: stable, Weight: 10},
			{ChannelId: 8, Priority: others, Weight: 10},
			{ChannelId: 5, Priority: others, Weight: 10},
		}
		effective := shiftByTiers(distinctTiersDesc(candidates), stable, maxPromoteTiers)
		assert.Equal(t, int64(1), effective,
			"without write-back the effective priority is the same every round")
	}
	assert.Equal(t, int64(0), stable, "the configured baseline never moves")
}

// TestDemotionWriteBackSinksWithoutBound is the same failure in the direction that
// actually hurts: a channel that faults during an outage would keep sinking after
// it recovered, because each demotion was measured from the previous one.
func TestDemotionWriteBackSinksWithoutBound(t *testing.T) {
	const maxDemoteTiers = 3

	priority := int64(0)
	seen := []int64{priority}

	for round := 0; round < 6; round++ {
		candidates := []Candidate{
			{ChannelId: 31, Priority: priority, Weight: 10},
			{ChannelId: 8, Priority: 0, Weight: 10},
		}
		// Even pinned at a single tier of demotion — far inside the cap — the
		// baseline runs away.
		priority = shiftByTiers(distinctTiersDesc(candidates), priority, -1)
		seen = append(seen, priority)
		require.GreaterOrEqual(t, maxDemoteTiers, 1)
	}

	t.Log("stored priority per round: " + fmt.Sprint(seen))
	assert.Equal(t, []int64{0, -1, -2, -3, -4, -5, -6}, seen,
		"one tier of demotion per round compounds past MaxDemoteTiers=3")
}
