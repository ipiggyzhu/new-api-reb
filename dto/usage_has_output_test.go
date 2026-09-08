package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUsageHasOutput pins the predicate that decides whether a text relay
// delivered anything. It gates both the charge and the routing verdict for a
// stream that returned 200 and no content, so a false positive bills a user for
// nothing while a false negative fails a request that did produce a reply — for
// a modality whose whole answer lives in one detail field, that is the entire
// response.
func TestUsageHasOutput(t *testing.T) {
	cases := []struct {
		name  string
		usage *Usage
		want  bool
	}{
		{name: "nil usage", usage: nil, want: false},
		{name: "zero usage", usage: &Usage{}, want: false},
		{
			name:  "prompt tokens only is the empty-response shape",
			usage: &Usage{PromptTokens: 67000, TotalTokens: 67000},
			want:  false,
		},
		{
			name:  "completion tokens",
			usage: &Usage{PromptTokens: 12, CompletionTokens: 1},
			want:  true,
		},
		{
			name:  "responses-style output tokens",
			usage: &Usage{InputTokens: 12, OutputTokens: 1},
			want:  true,
		},
		{
			name:  "text tokens only",
			usage: &Usage{CompletionTokenDetails: OutputTokenDetails{TextTokens: 1}},
			want:  true,
		},
		{
			name:  "audio answer bills through audio tokens",
			usage: &Usage{CompletionTokenDetails: OutputTokenDetails{AudioTokens: 1}},
			want:  true,
		},
		{
			name:  "image answer bills through image tokens",
			usage: &Usage{CompletionTokenDetails: OutputTokenDetails{ImageTokens: 1}},
			want:  true,
		},
		{
			name:  "reasoning-only answer bills through reasoning tokens",
			usage: &Usage{CompletionTokenDetails: OutputTokenDetails{ReasoningTokens: 1}},
			want:  true,
		},
		{
			name:  "cached prompt tokens are not output",
			usage: &Usage{PromptTokens: 100, PromptTokensDetails: InputTokenDetails{CachedTokens: 100}},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.usage.HasOutput())
		})
	}
}

func TestUsageResponseValidatedIsInternal(t *testing.T) {
	var usage Usage
	require.NoError(t, common.UnmarshalJsonStr(`{"ResponseValidated":true,"response_validated":true}`, &usage))
	assert.False(t, usage.ResponseValidated)

	usage.ResponseValidated = true
	data, err := common.Marshal(usage)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "ResponseValidated")
	assert.NotContains(t, string(data), "response_validated")
}
