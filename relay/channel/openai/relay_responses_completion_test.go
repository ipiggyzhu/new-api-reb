package openai

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeResponsesStreamStopsAtTerminalEvent(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.done", "response.incomplete"} {
		for _, clientMode := range []string{"keep_open", "close_at_terminal"} {
			t.Run(eventType+"/"+clientMode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				status := "completed"
				if eventType == "response.incomplete" {
					status = "incomplete"
				}
				terminal := fmt.Sprintf(`{"type":%q,"response":{"id":"resp_1","status":%q,"output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3}}}}`, eventType, status)
				upstreamClosed := make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(upstreamClosed)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"OK"}`+"\n\ndata: "+terminal+"\n\n")
					w.(http.Flusher).Flush()
					// Protocol completion must release this connection without EOF.
					<-r.Context().Done()
				}))
				t.Cleanup(upstream.Close)

				_, info, _, _ := newOutputValidationTest(t, "", true, true)
				type handlerResult struct {
					usage        *dto.Usage
					apiErr       *types.NewAPIError
					transportErr error
				}
				finished := make(chan handlerResult, 1)
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					c, _ := gin.CreateTestContext(w)
					c.Request = r
					request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.URL, nil)
					if err != nil {
						finished <- handlerResult{transportErr: err}
						return
					}
					response, err := upstream.Client().Do(request)
					if err != nil {
						finished <- handlerResult{transportErr: err}
						return
					}
					usage, apiErr := OaiResponsesStreamHandler(c, info, response)
					finished <- handlerResult{usage: usage, apiErr: apiErr}
				}))
				t.Cleanup(gateway.Close)

				request, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/responses", nil)
				require.NoError(t, err)
				response, err := gateway.Client().Do(request)
				require.NoError(t, err)
				defer response.Body.Close()
				assert.Equal(t, http.StatusOK, response.StatusCode)
				scanner := bufio.NewScanner(response.Body)
				var output strings.Builder
				var terminalReceived bool
				for scanner.Scan() {
					line := scanner.Text()
					output.WriteString(line + "\n")
					if strings.HasPrefix(line, "data:") && gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "type").String() == eventType {
						terminalReceived = true
						if clientMode == "close_at_terminal" {
							require.NoError(t, response.Body.Close())
							break
						}
					}
				}
				require.NoError(t, scanner.Err(), "the gateway must finish without waiting for upstream EOF")
				require.True(t, terminalReceived)
				assert.Contains(t, output.String(), `"delta":"OK"`)
				assert.Equal(t, 1, strings.Count(output.String(), terminal))

				var result handlerResult
				select {
				case result = <-finished:
				case <-ctx.Done():
					t.Fatal("the Responses handler did not stop at protocol completion")
				}
				require.NoError(t, result.transportErr)
				require.Nil(t, result.apiErr)
				require.NotNil(t, result.usage)
				assert.True(t, result.usage.ResponseValidated)
				assert.Equal(t, 11, result.usage.PromptTokens)
				assert.Equal(t, 7, result.usage.CompletionTokens)
				assert.Equal(t, 18, result.usage.TotalTokens)
				assert.Equal(t, 3, result.usage.PromptTokensDetails.CachedTokens)
				require.NotNil(t, info.StreamStatus)
				assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
				assert.Nil(t, info.StreamStatus.FailureError())
				assert.False(t, info.StreamStatus.HasErrors())
				assert.False(t, info.StreamStatus.MissingTerminator())
				select {
				case <-upstreamClosed:
				case <-ctx.Done():
					t.Fatal("protocol completion did not release the upstream connection")
				}
			})
		}
	}
}
