package controller

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClassifyChannelModelSweepResult pins the three safety-critical, settings-
// independent verdicts the sweep must never get wrong: a clean test keeps the
// model, an explicit upstream "model does not exist" removes it, and an error we
// raised ourselves before ever reaching the upstream leaves the model untouched.
// A rate limit must also never remove a model. Cases whose classification depends
// on operator-configured disable codes/keywords are covered by service tests, not
// re-litigated here.
func TestClassifyChannelModelSweepResult(t *testing.T) {
	cases := []struct {
		name   string
		result testResult
		want   channelModelSweepVerdict
	}{
		{
			name:   "clean test passes",
			result: testResult{newAPIError: nil, localErr: nil},
			want:   sweepVerdictPass,
		},
		{
			name:   "explicit model_not_found on 404 is removed",
			result: testResult{newAPIError: upstreamJSONError(404, "model_not_found", "The model `gpt-9` does not exist")},
			want:   sweepVerdictFail,
		},
		{
			name:   "chinese unsupported-model prose on 404 is removed",
			result: testResult{newAPIError: upstreamTextError(404, "当前 API 不支持所选模型 claude-3-5-haiku-20241022")},
			want:   sweepVerdictFail,
		},
		{
			name:   "rate limit is inconclusive, never removed",
			result: testResult{newAPIError: upstreamJSONError(429, "rate_limit_exceeded", "Rate limit reached for requests")},
			want:   sweepVerdictInconclusive,
		},
		{
			name:   "local request-build error never removes the model",
			result: testResult{newAPIError: nil, localErr: errors.New("dial tcp: timeout")},
			want:   sweepVerdictInconclusive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyChannelModelSweepResult(tc.result))
		})
	}
}

// TestComputeSweptModels covers the pruning contract: keep passing and inconclusive
// models, remove confirmed failures (including models already on the channel), add
// newly passing upstream candidates, leave untested models exactly as they were,
// and never empty a channel whose every model failed.
func TestComputeSweptModels(t *testing.T) {
	t.Run("keep passing, keep inconclusive, remove failing existing", func(t *testing.T) {
		next, added, removed, outage := computeSweptModels(
			[]string{"a", "b", "c"}, nil,
			map[string]channelModelSweepVerdict{"a": sweepVerdictPass, "b": sweepVerdictFail, "c": sweepVerdictInconclusive},
		)
		assert.Equal(t, []string{"a", "c"}, next)
		assert.Empty(t, added)
		assert.Equal(t, []string{"b"}, removed)
		assert.False(t, outage)
	})

	t.Run("add a passing candidate, drop a failing one", func(t *testing.T) {
		next, added, removed, outage := computeSweptModels(
			[]string{"a"}, []string{"b", "c"},
			map[string]channelModelSweepVerdict{"a": sweepVerdictPass, "b": sweepVerdictPass, "c": sweepVerdictFail},
		)
		assert.Equal(t, []string{"a", "b"}, next)
		assert.Equal(t, []string{"b"}, added)
		assert.Empty(t, removed)
		assert.False(t, outage)
	})

	t.Run("untested existing kept, untested candidate not added", func(t *testing.T) {
		next, added, removed, outage := computeSweptModels(
			[]string{"a", "b"}, []string{"c"},
			map[string]channelModelSweepVerdict{"a": sweepVerdictPass},
		)
		assert.Equal(t, []string{"a", "b"}, next)
		assert.Empty(t, added)
		assert.Empty(t, removed)
		assert.False(t, outage)
	})

	t.Run("candidate duplicating an existing model is not added twice", func(t *testing.T) {
		next, added, _, _ := computeSweptModels(
			[]string{"a"}, []string{"a", "b"},
			map[string]channelModelSweepVerdict{"a": sweepVerdictPass, "b": sweepVerdictPass},
		)
		assert.Equal(t, []string{"a", "b"}, next)
		assert.Equal(t, []string{"b"}, added)
	})

	t.Run("all existing fail keeps the channel unchanged and reports outage", func(t *testing.T) {
		next, added, removed, outage := computeSweptModels(
			[]string{"a", "b"}, nil,
			map[string]channelModelSweepVerdict{"a": sweepVerdictFail, "b": sweepVerdictFail},
		)
		assert.Equal(t, []string{"a", "b"}, next)
		assert.Empty(t, added)
		assert.Empty(t, removed)
		assert.True(t, outage)
	})

	t.Run("a passing candidate rescues a channel whose existing models all fail", func(t *testing.T) {
		next, added, removed, outage := computeSweptModels(
			[]string{"a", "b"}, []string{"c"},
			map[string]channelModelSweepVerdict{"a": sweepVerdictFail, "b": sweepVerdictFail, "c": sweepVerdictPass},
		)
		assert.Equal(t, []string{"c"}, next)
		assert.Equal(t, []string{"c"}, added)
		assert.ElementsMatch(t, []string{"a", "b"}, removed)
		assert.False(t, outage)
	})
}
