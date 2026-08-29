package aws

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gin.SetMode writes a package global. These tests run in parallel, so setting
// it per test made every -race run report a write/write race and drown out any
// real one.
func init() {
	gin.SetMode(gin.TestMode)
}

func TestDoAwsClientRequest_AppliesRuntimeHeaderOverrideToAnthropicBeta(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName:           "claude-3-5-sonnet-20240620",
		IsStream:                  false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"anthropic-beta": "computer-use-2025-01-24",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            "access-key|secret-key|us-east-1",
			UpstreamModelName: "claude-3-5-sonnet-20240620",
		},
	}

	requestBody := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":128}`)
	adaptor := &Adaptor{}

	_, err := doAwsClientRequest(ctx, info, adaptor, requestBody)
	require.NoError(t, err)

	awsReq, ok := adaptor.AwsReq.(*bedrockruntime.InvokeModelInput)
	require.True(t, ok)

	var payload map[string]any
	require.NoError(t, common.Unmarshal(awsReq.Body, &payload))

	anthropicBeta, exists := payload["anthropic_beta"]
	require.True(t, exists)

	values, ok := anthropicBeta.([]any)
	require.True(t, ok)
	require.Equal(t, []any{"computer-use-2025-01-24"}, values)
}

// stubBedrockHTTPClient returns a canned Bedrock reply so the Nova handler can be
// driven end to end without a network call.
type stubBedrockHTTPClient struct {
	body string
}

func (s stubBedrockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(s.body)),
		Request:    req,
	}, nil
}

func TestHandleNovaRequest_UpstreamContent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		upstreamBody string
		wantErr      bool
		wantText     string
		wantUsage    dto.Usage
	}{
		{
			name:         "empty content array is rejected",
			upstreamBody: `{"output":{"message":{"content":[]}},"usage":{"inputTokens":7,"outputTokens":0,"totalTokens":7}}`,
			wantErr:      true,
		},
		{
			name:         "missing output is rejected",
			upstreamBody: `{"usage":{"inputTokens":7,"outputTokens":0,"totalTokens":7}}`,
			wantErr:      true,
		},
		{
			name:         "first content block is relayed",
			upstreamBody: `{"output":{"message":{"content":[{"text":"hello"}]}},"usage":{"inputTokens":7,"outputTokens":3,"totalTokens":10}}`,
			wantText:     "hello",
			wantUsage:    dto.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			adaptor := &Adaptor{
				AwsClient: bedrockruntime.New(bedrockruntime.Options{
					Region:      "us-east-1",
					Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("access-key", "secret-key", "")),
					HTTPClient:  stubBedrockHTTPClient{body: tc.upstreamBody},
				}),
				AwsReq: &bedrockruntime.InvokeModelInput{
					ModelId:     aws.String("amazon.nova-lite-v1:0"),
					Accept:      aws.String("application/json"),
					ContentType: aws.String("application/json"),
					Body:        []byte(`{"messages":[]}`),
				},
			}
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "nova-lite"},
			}

			apiErr, usage := handleNovaRequest(ctx, info, adaptor)

			if tc.wantErr {
				require.NotNil(t, apiErr)
				assert.Nil(t, usage)
				assert.Equal(t, types.ErrorCodeBadResponseBody, apiErr.GetErrorCode())
				return
			}

			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tc.wantUsage, *usage)

			var relayed dto.OpenAITextResponse
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &relayed))
			require.Len(t, relayed.Choices, 1)
			assert.Equal(t, tc.wantText, relayed.Choices[0].Message.StringContent())
			assert.Equal(t, "stop", relayed.Choices[0].FinishReason)
		})
	}
}
