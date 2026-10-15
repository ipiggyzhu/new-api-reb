package openai

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesReportsReasoningEffortWithoutInventingOrRewritingIt(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, effort := range []string{"low", "none", ""} {
			t.Run(fmt.Sprintf("stream=%v/effort=%s", stream, effort), func(t *testing.T) {
				field := ""
				if effort != "" {
					field = fmt.Sprintf(`,"reasoning":{"effort":%q}`, effort)
				}
				response := `{"id":"resp_effort","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]` + field + `}`
				body := response
				if stream {
					body = `data: {"type":"response.completed","response":` + response + "}\n\n"
				}
				c, info, resp, recorder := newOutputValidationTest(t, body, true, stream)
				info.ReasoningEffort = "xhigh"
				if stream {
					_, apiErr := OaiResponsesStreamHandler(c, info, resp)
					require.Nil(t, apiErr)
				} else {
					_, apiErr := OaiResponsesHandler(c, info, resp)
					require.Nil(t, apiErr)
				}
				assert.Equal(t, "xhigh", info.ReasoningEffort)
				assert.Equal(t, effort, info.UpstreamReasoningEffort)
				if effort != "" {
					assert.Contains(t, recorder.Body.String(), `"effort":"`+effort+`"`)
				} else {
					assert.NotContains(t, recorder.Body.String(), `"effort"`)
				}
				info.ResetResponseState()
				assert.Empty(t, info.UpstreamReasoningEffort, "another channel must not inherit the reported effort")
			})
		}
	}
}

func TestResponsesReportsCompatibleReasoningEffortFields(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			response := `{"id":"resp_alias","status":"completed","reasoning_effort":"high","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`
			body := response
			if stream {
				body = `data: {"type":"response.completed","response":` + response + "}\n\n"
			}
			c, info, resp, recorder := newOutputValidationTest(t, body, true, stream)
			info.ReasoningEffort = "low"
			if stream {
				_, apiErr := OaiResponsesStreamHandler(c, info, resp)
				require.Nil(t, apiErr)
			} else {
				_, apiErr := OaiResponsesHandler(c, info, resp)
				require.Nil(t, apiErr)
			}
			assert.Equal(t, "high", info.UpstreamReasoningEffort)
			assert.Equal(t, "low", info.ReasoningEffort)
			assert.Contains(t, recorder.Body.String(), `"reasoning_effort":"high"`)
		})
	}
}

func TestResponsesStreamKeepsReportedEffortUntilAnotherValueIsReported(t *testing.T) {
	for _, tc := range []struct {
		name, field, want string
	}{
		{"missing reasoning", "", "high"},
		{"null effort", `,"reasoning":{"effort":null}`, "high"},
		{"missing effort", `,"reasoning":{"summary":"auto"}`, "high"},
		{"empty effort", `,"reasoning":{"effort":""}`, "high"},
		{"explicit none", `,"reasoning":{"effort":"none"}`, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := `{"type":"response.created","response":{"id":"resp_effort","reasoning":{"effort":"high"}}}`
			completed := `{"type":"response.completed","response":{"id":"resp_effort","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]` + tc.field + `}}`
			body := "data: " + created + "\n\ndata: " + completed + "\n\n"
			c, info, resp, recorder := newOutputValidationTest(t, body, true, true)
			_, apiErr := OaiResponsesStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			assert.Equal(t, tc.want, info.UpstreamReasoningEffort)
			assert.Contains(t, recorder.Body.String(), created)
			assert.Contains(t, recorder.Body.String(), completed)
		})
	}
}
