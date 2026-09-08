package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsagePreservesOptionalResponsesOutputDetails(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want *OutputTokenDetails
	}{
		{"absent", `{}`, nil},
		{"explicit zero", `{"output_tokens_details":{"reasoning_tokens":0}}`, &OutputTokenDetails{}},
		{"reported details", `{"output_tokens_details":{"reasoning_tokens":7,"text_tokens":2,"audio_tokens":3,"image_tokens":4}}`, &OutputTokenDetails{ReasoningTokens: 7, TextTokens: 2, AudioTokens: 3, ImageTokens: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage Usage
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &usage))
			data, err := common.Marshal(usage)
			require.NoError(t, err)
			var wire struct {
				Details *OutputTokenDetails `json:"output_tokens_details"`
			}
			require.NoError(t, common.Unmarshal(data, &wire))
			assert.Equal(t, tc.want, wire.Details)
		})
	}
}

func TestUsageHasOutputRecognizesResponsesDetails(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"canonical reasoning", `{"output_tokens_details":{"reasoning_tokens":1}}`, true},
		{"canonical zero overrides legacy", `{"output_tokens_details":{"reasoning_tokens":0},"completion_tokens_details":{"reasoning_tokens":7}}`, false},
		{"legacy reasoning", `{"completion_tokens_details":{"reasoning_tokens":7}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage Usage
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &usage))
			assert.Equal(t, tc.want, usage.HasOutput())
		})
	}
}
