package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
	"github.com/tidwall/gjson"
)

// Store only tenant/channel-scoped hashes, never prompts or ciphertext. Losing
// this bounded, process-local cache only costs another original-first attempt.
// A successful fallback retires its old snapshot; newly generated reasoning is
// still sent unchanged on subsequent turns instead of being stripped every time.
var retiredResponsesReasoning = hot.NewHotCache[string, bool](hot.LRU, 65536).Build()

func doResponsesReasoningFallback(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor, reader io.Reader) (any, func(), error) {
	original, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	var body map[string]json.RawMessage
	var input []map[string]json.RawMessage
	valid := common.Unmarshal(original, &body) == nil && body != nil &&
		common.Unmarshal(body["input"], &input) == nil
	// A gateway cannot reconstruct history hidden behind a compaction blob or a
	// server-side reference. Never remove those or silently start a new thread.
	if valid {
		for _, name := range []string{"previous_response_id", "conversation"} {
			value := gjson.ParseBytes(body[name])
			if value.Exists() && value.String() != "" {
				valid = false
			}
		}
		for _, item := range input {
			switch gjson.ParseBytes(item["type"]).String() {
			case "compaction", "item_reference":
				valid = false
			}
		}
	}
	if !valid {
		resp, err := adaptor.DoRequest(c, info, bytes.NewReader(original))
		return resp, nil, err
	}

	scope := fmt.Sprintf("%d:%d:%d:%d:%s:%s", info.UserId, info.TokenId,
		info.ChannelId, info.ChannelMultiKeyIndex, info.ChannelBaseUrl, info.ApiKey)
	pending := make(map[int]string)
	retiredCount := 0
	for i, item := range input {
		cipher := gjson.ParseBytes(item["encrypted_content"])
		if gjson.ParseBytes(item["type"]).String() != "reasoning" || cipher.Type != gjson.String || cipher.String() == "" {
			continue
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(scope+":"+cipher.String())))
		if retired, found, _ := retiredResponsesReasoning.Get(digest); found && retired {
			delete(item, "encrypted_content")
			delete(item, "id")
			retiredCount++
		} else {
			pending[i] = digest
		}
	}
	requestBytes := original
	if retiredCount > 0 {
		body["input"], err = common.Marshal(input)
		if err != nil {
			return nil, nil, err
		}
		requestBytes, err = common.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		logger.LogWarn(c, fmt.Sprintf("Responses reasoning compatibility: omitted %d retired ciphertext items; visible history and requested effort retained", retiredCount))
	}
	info.UpstreamRequestBodySize = int64(len(requestBytes))
	resp, err := adaptor.DoRequest(c, info, bytes.NewReader(requestBytes))
	if err != nil || len(pending) == 0 || c.Writer.Written() || c.Request.Context().Err() != nil {
		return resp, nil, err
	}
	httpResp, ok := resp.(*http.Response)
	if !ok || httpResp == nil || httpResp.StatusCode != http.StatusBadRequest || httpResp.Body == nil {
		return resp, nil, nil
	}

	// Inspect only a bounded JSON error, restoring the original body for every
	// non-matching response. Never retry after a 200 response has begun streaming.
	const maxErrorBytes = 64 * 1024
	errorBytes, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBytes+1))
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if readErr != nil || len(errorBytes) > maxErrorBytes ||
		common.Unmarshal(errorBytes, &envelope) != nil || envelope.Error.Code != "invalid_encrypted_content" {
		httpResp.Body = &responsesErrorReplayBody{Reader: io.MultiReader(bytes.NewReader(errorBytes), httpResp.Body), Closer: httpResp.Body}
		return resp, nil, nil
	}

	for i := range pending {
		delete(input[i], "encrypted_content")
		delete(input[i], "id")
	}
	body["input"], err = common.Marshal(input)
	if err != nil {
		_ = httpResp.Body.Close()
		return nil, nil, err
	}
	requestBytes, err = common.Marshal(body)
	if err != nil {
		_ = httpResp.Body.Close()
		return nil, nil, err
	}
	_ = httpResp.Body.Close()
	logger.LogWarn(c, fmt.Sprintf("Responses invalid_encrypted_content: retrying once without %d old ciphertext items; visible history and requested effort retained, hidden reasoning reset", len(pending)))
	info.UpstreamRequestBodySize = int64(len(requestBytes))
	resp, err = adaptor.DoRequest(c, info, bytes.NewReader(requestBytes))
	// The caller records this snapshot only after DoResponse succeeds, not just
	// on HTTP 200: an SSE response can still terminate with response.failed.
	return resp, func() {
		for _, digest := range pending {
			retiredResponsesReasoning.SetWithTTL(digest, true, 24*time.Hour)
		}
	}, err
}

type responsesErrorReplayBody struct {
	io.Reader
	io.Closer
}
