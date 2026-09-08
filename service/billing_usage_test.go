package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveBillingUsagePreservesResponsesOutputDetails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		canonical *dto.OutputTokenDetails
		legacy    dto.OutputTokenDetails
		want      dto.OutputTokenDetails
	}{
		{
			name:      "canonical details",
			canonical: &dto.OutputTokenDetails{ReasoningTokens: 6, TextTokens: 1, AudioTokens: 1, ImageTokens: 1},
			legacy:    dto.OutputTokenDetails{ReasoningTokens: 7},
			want:      dto.OutputTokenDetails{ReasoningTokens: 6, TextTokens: 1, AudioTokens: 1, ImageTokens: 1},
		},
		{
			name:      "canonical zero overrides legacy",
			canonical: &dto.OutputTokenDetails{},
			legacy:    dto.OutputTokenDetails{ReasoningTokens: 7},
		},
		{
			name:   "legacy details",
			legacy: dto.OutputTokenDetails{ReasoningTokens: 7},
			want:   dto.OutputTokenDetails{ReasoningTokens: 7},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			billingUsage := dto.NewOpenAIResponsesBillingUsage(&dto.Usage{
				InputTokens:            10,
				OutputTokens:           9,
				TotalTokens:            19,
				OutputTokensDetails:    tc.canonical,
				CompletionTokenDetails: tc.legacy,
			})
			require.NotNil(t, billingUsage)
			usage := effectiveBillingUsage(&dto.Usage{
				PromptTokens:     999,
				CompletionTokens: 999,
				TotalTokens:      1998,
				BillingUsage:     billingUsage,
			})
			require.NotNil(t, usage)
			assert.Equal(t, 10, usage.PromptTokens)
			assert.Equal(t, 10, usage.InputTokens)
			assert.Equal(t, 9, usage.CompletionTokens)
			assert.Equal(t, 9, usage.OutputTokens)
			assert.Equal(t, 19, usage.TotalTokens)
			assert.Equal(t, tc.want, usage.CompletionTokenDetails)
			assert.Equal(t, dto.BillingUsageSourceOAIResponses, usage.UsageSource)
			assert.Equal(t, dto.BillingUsageSemanticOpenAI, usage.UsageSemantic)

			if tc.canonical == nil {
				assert.Nil(t, usage.OutputTokensDetails)
				return
			}
			require.NotNil(t, usage.OutputTokensDetails)
			assert.Equal(t, tc.want, *usage.OutputTokensDetails)
			usage.OutputTokensDetails.ReasoningTokens = 99
			assert.Equal(t, tc.want, *billingUsage.OpenAIUsage.OutputTokensDetails)
			require.NotNil(t, usage.BillingUsage)
			require.NotNil(t, usage.BillingUsage.OpenAIUsage)
			require.NotNil(t, usage.BillingUsage.OpenAIUsage.OutputTokensDetails)
			assert.Equal(t, tc.want, *usage.BillingUsage.OpenAIUsage.OutputTokensDetails)
		})
	}
}
