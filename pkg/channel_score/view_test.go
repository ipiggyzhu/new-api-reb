package channel_score

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseScoreKeyRoundTrips pins that parseScoreKey inverts scoreKey for the
// inputs the key format was designed to disambiguate. Group "a" with model "b:c"
// and group "a:b" with model "c" must stay distinguishable: before the length
// prefix existed they collapsed to one key, so two unrelated routes shared a
// streak and a tier offset.
func TestParseScoreKeyRoundTrips(t *testing.T) {
	cases := []struct {
		channelId int
		group     string
		model     string
	}{
		{1, "default", "gpt-test"},
		{42, "", "gpt-test"},
		{7, "a", "b:c"},
		{7, "a:b", "c"},
		{7, "group:with:colons", "vendor/model:v1"},
		{999, "默认分组", "claude-opus-5"},
		{3, "trailing:", ":leading"},
	}
	for _, tc := range cases {
		key := scoreKey(tc.channelId, tc.group, tc.model)
		channelId, group, model, ok := parseScoreKey(key)
		require.True(t, ok, "key %q should parse", key)
		assert.Equal(t, tc.channelId, channelId)
		assert.Equal(t, tc.group, group)
		assert.Equal(t, tc.model, model)
	}
}

// TestParseScoreKeyRejectsMalformed keeps a key this package did not produce from
// yielding a row: a mangled group or model would misattribute the score to a
// route that does not exist.
func TestParseScoreKeyRejectsMalformed(t *testing.T) {
	malformed := []string{
		"",
		"wrong-prefix:1:7:default:gpt",
		scoreKeyPrefix,
		scoreKeyPrefix + "notanumber:7:default:gpt",
		scoreKeyPrefix + "1",
		scoreKeyPrefix + "1:7",
		// group length longer than what follows
		scoreKeyPrefix + "1:99:default:gpt",
		// negative group length
		scoreKeyPrefix + "1:-1:default:gpt",
		// group length not followed by the separator
		scoreKeyPrefix + "1:7:defaultXgpt",
	}
	for _, key := range malformed {
		_, _, _, ok := parseScoreKey(key)
		assert.False(t, ok, "key %q must be rejected", key)
	}
}

// TestSnapshotReportsRecordedState checks the diagnostic view against state built
// through the ordinary Report path, so the endpoint cannot drift from what the
// selection path actually reads.
func TestSnapshotReportsRecordedState(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.MinSampleForWeight = 2
	})

	Report(11, "default", "gpt-test", OutcomeSuccess)
	Report(11, "default", "gpt-test", OutcomeSuccess)

	snap := Snapshot(ScoreFilter{})
	require.Len(t, snap.Rows, 1)
	row := snap.Rows[0]
	assert.Equal(t, 11, row.ChannelID)
	assert.Equal(t, "default", row.Group)
	assert.Equal(t, "gpt-test", row.Model)
	assert.Equal(t, int64(2), row.Total)
	assert.Equal(t, int64(2), row.Success)
	assert.Equal(t, 2, row.ConsecutiveSuccess)
	assert.Equal(t, 0, row.FaultCount)
	assert.False(t, row.Idle)
	// Two successes out of two, above the sample threshold: the top factor.
	assert.Equal(t, 1.5, row.WeightFactor)

	// No Redis in this test binary, so the local store is the whole truth.
	assert.True(t, snap.Complete)
	assert.False(t, snap.InstanceLocal)
	assert.True(t, snap.Enabled)
}

// TestSnapshotFiltersByEveryDimension pins that rows are keyed by all three
// dimensions, and that filtering matches on the decomposed fields rather than on
// a substring of the key.
func TestSnapshotFiltersByEveryDimension(t *testing.T) {
	useScoreSettingForTest(t, nil)

	Report(1, "alpha", "model-a", OutcomeSuccess)
	Report(1, "beta", "model-a", OutcomeSuccess)
	Report(2, "alpha", "model-b", OutcomeSuccess)

	assert.Len(t, Snapshot(ScoreFilter{}).Rows, 3)
	assert.Len(t, Snapshot(ScoreFilter{ChannelID: 1}).Rows, 2)
	assert.Len(t, Snapshot(ScoreFilter{Group: "alpha"}).Rows, 2)
	assert.Len(t, Snapshot(ScoreFilter{Model: "model-a"}).Rows, 2)
	assert.Len(t, Snapshot(ScoreFilter{ChannelID: 1, Group: "alpha", Model: "model-a"}).Rows, 1)
	assert.Empty(t, Snapshot(ScoreFilter{ChannelID: 3}).Rows)
	assert.Empty(t, Snapshot(ScoreFilter{Group: "gamma"}).Rows)
}

// TestSnapshotShowsTheSameDecayedIdleVerdictAsRouting pins the diagnostic
// contract: the list must show the score the selector is actually applying, not
// the stale pre-idle offset and not "nothing".
func TestSnapshotShowsTheSameDecayedIdleVerdictAsRouting(t *testing.T) {
	now := useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.IdleResetSeconds = 100
		s.MaxDemoteTiers = 3
	})

	Report(5, "default", "gpt-test", OutcomeFault)
	Report(5, "default", "gpt-test", OutcomeFault)
	Report(5, "default", "gpt-test", OutcomeFault)

	rows := Snapshot(ScoreFilter{}).Rows
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Idle)
	assert.Equal(t, -3, rows[0].TierOffset)

	*now += 100

	rows = Snapshot(ScoreFilter{}).Rows
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Idle, "the rate sample past IdleResetSeconds is idle")
	assert.Equal(t, -2, rows[0].TierOffset,
		"one idle period decays a three-tier demotion by exactly one tier")
	assert.Zero(t, rows[0].Total, "stale success-rate samples must be cleared")
	applied := lookup(scoreKey(5, "default", "gpt-test"), 100)
	require.NotNil(t, applied, "the standing demotion must still protect routing")
	assert.Equal(t, rows[0].TierOffset, applied.tierOffset,
		"the diagnostic and selector must agree on the active offset")
}

// TestSnapshotDoesNotCreateEntries is the property that makes the endpoint safe to
// poll: a diagnostic read must not materialize rows, or it would change what a
// later Reset has to clear and what a later report finds.
func TestSnapshotDoesNotCreateEntries(t *testing.T) {
	useScoreSettingForTest(t, nil)

	// Counted rather than asserted empty: ResetAll clears each entry's state but
	// keeps the entry so its generation survives, so the store legitimately holds
	// records from earlier tests in this binary.
	countEntries := func() int {
		count := 0
		localStore.Range(func(_, _ any) bool {
			count++
			return true
		})
		return count
	}

	before := countEntries()
	assert.Empty(t, Snapshot(ScoreFilter{ChannelID: 77}).Rows)
	assert.Empty(t, Snapshot(ScoreFilter{}).Rows)
	assert.Equal(t, before, countEntries(), "Snapshot must not create store entries")
}

// TestSnapshotSkipsEntriesWithNothingPublished covers the entry that exists only
// because a Redis round trip recorded its generation: it has no published
// snapshot, so it carries no observable score and must not appear as a row.
func TestSnapshotSkipsEntriesWithNothingPublished(t *testing.T) {
	useScoreSettingForTest(t, nil)

	entryGeneration(scoreKey(9, "default", "gpt-test"))

	assert.Empty(t, Snapshot(ScoreFilter{}).Rows)
}
