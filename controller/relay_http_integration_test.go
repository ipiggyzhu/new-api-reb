package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const relayHTTPInitialQuota = 1_000_000

// Both listeners bind to loopback. Auth and billing use the real SQLite models;
// only the external provider is a fixture.
func setupRelayHTTPTest(t *testing.T, upstream http.Handler, retries, concurrency int) (*httptest.Server, *gorm.DB) {
	t.Helper()
	_, db := setupCrossGroupBillingTest(t, false, relayHTTPInitialQuota, 1, upstream)
	common.RetryTimes = retries
	originalRateLimit := setting.ModelRequestRateLimitEnabled
	setting.ModelRequestRateLimitEnabled = false
	t.Cleanup(func() { setting.ModelRequestRateLimitEnabled = originalRateLimit })
	prices := ratio_setting.GetModelPriceCopy()
	prices[taskRoutingModel] = 0.00002 // ten quota units per submitted request
	priceJSON, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceJSON)))
	userSetting, err := common.Marshal(dto.UserSetting{
		BillingPreference: "wallet_only", QuotaWarningThreshold: -1_000_000_000,
	})
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 991).Update("setting", string(userSetting)).Error)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", 992).Updates(map[string]any{
		"key": "gatewayhttptest", "status": common.TokenStatusEnabled, "expired_time": -1, "group": "default",
	}).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{81, 82}).Updates(map[string]any{
		"group": "default", "max_concurrency": concurrency,
	}).Error)
	model.InitChannelCache()

	router := gin.New()
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	router.Use(func(c *gin.Context) {
		defer common.CleanupBodyStorage(c)
		c.Set(common.RequestIdKey, c.GetHeader("X-Request-ID"))
		c.Next()
	}, middleware.TokenAuth(), middleware.ModelRequestRateLimit())
	router.POST("/v1/chat/completions", middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	router.POST("/suno/submit/:action", middleware.Distribute(), RelayTask)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, db
}

func newRelayHTTPRequest(ctx context.Context, serverURL, kind string, id int) (*http.Request, error) {
	label := fmt.Sprintf("%s:%d", kind, id)
	path := "/v1/chat/completions"
	payload := map[string]any{
		"model": taskRoutingModel, "max_tokens": 8,
		"messages": []map[string]string{{"role": "user", "content": label}},
	}
	if kind == "task" {
		path = "/suno/submit/lyrics"
		payload = map[string]any{"prompt": label}
	} else if kind == "stream" || kind == "broken" {
		payload["stream"] = true
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer sk-gatewayhttptest")
	request.Header.Set("X-Request-ID", label)
	return request, nil
}

func TestRelayHTTPConcurrentAccounting(t *testing.T) {
	requests, concurrency := 60, 8
	if value := os.Getenv("NEW_API_HTTP_TEST_REQUESTS"); value != "" {
		var err error
		requests, err = strconv.Atoi(value)
		require.NoError(t, err)
	}
	if value := os.Getenv("NEW_API_HTTP_TEST_CONCURRENCY"); value != "" {
		var err error
		concurrency, err = strconv.Atoi(value)
		require.NoError(t, err)
	}
	require.Positive(t, requests)
	require.Zero(t, requests%6)
	require.LessOrEqual(t, requests, 60_000)
	require.Positive(t, concurrency)
	require.LessOrEqual(t, concurrency, 128)

	var attempts sync.Map
	var upstreamCalls atomic.Int64
	server, db := setupRelayHTTPTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Prompt   string
			Messages []struct{ Content string }
		}
		if err := common.DecodeJson(r.Body, &payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		label := payload.Prompt
		if len(payload.Messages) > 0 {
			label = payload.Messages[0].Content
		}
		kind, _, _ := strings.Cut(label, ":")
		counter, _ := attempts.LoadOrStore(label, new(atomic.Int32))
		attempt := counter.(*atomic.Int32).Add(1)
		upstreamCalls.Add(1)
		if kind == "error" || (kind == "retry" && attempt == 1) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "fixture upstream unavailable")
			return
		}
		if kind == "stream" || kind == "broken" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-local\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
			if kind == "stream" {
				_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-local\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n")
			} else {
				_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-local\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if kind == "task" {
			_, _ = io.WriteString(w, "{\"code\":\"success\",\"data\":\""+label+"\"}")
		} else {
			_, _ = io.WriteString(w, "{\"id\":\"chatcmpl-local\",\"object\":\"chat.completion\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}")
		}
	}), 1, 0)
	client := server.Client()
	client.Timeout = 15 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	kinds := []string{"json", "stream", "retry", "error", "broken", "task"}
	type result struct {
		kind    string
		status  int
		body    string
		latency time.Duration
		err     error
	}
	results := make(chan result, requests)
	queue := make(chan int, requests)
	for i := 0; i < requests; i++ {
		queue <- i
	}
	close(queue)
	var workers sync.WaitGroup
	started := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range queue {
				kind := kinds[id%len(kinds)]
				start := time.Now()
				request, err := newRelayHTTPRequest(context.Background(), server.URL, kind, id)
				if err != nil {
					results <- result{kind: kind, err: err}
					continue
				}
				response, err := client.Do(request)
				if err != nil {
					results <- result{kind: kind, err: err}
					continue
				}
				body, err := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				if err == nil {
					err = closeErr
				}
				results <- result{kind: kind, status: response.StatusCode, body: string(body), latency: time.Since(start), err: err}
			}
		}()
	}
	workers.Wait()
	elapsed := time.Since(started)
	close(results)
	latencies := make([]time.Duration, 0, requests)
	taskIDs := map[string]bool{}
	for result := range results {
		require.NoError(t, result.err)
		latencies = append(latencies, result.latency)
		if result.kind == "error" {
			assert.Equal(t, http.StatusServiceUnavailable, result.status, result.body)
			assert.Contains(t, result.body, "\"error\"")
			continue
		}
		assert.Equal(t, http.StatusOK, result.status, result.body)
		if result.kind == "broken" {
			assert.Contains(t, result.body, "\"error\"", "a truncated stream must report its failure")
			assert.Contains(t, result.body, "[DONE]")
		} else if result.kind == "task" {
			var taskResponse struct{ Code, Data string }
			require.NoError(t, common.Unmarshal([]byte(result.body), &taskResponse))
			assert.Equal(t, "success", taskResponse.Code)
			assert.True(t, strings.HasPrefix(taskResponse.Data, "task_"))
			assert.False(t, taskIDs[taskResponse.Data], "public task IDs must be unique")
			taskIDs[taskResponse.Data] = true
		} else {
			assert.Contains(t, result.body, "ok")
			assert.NotContains(t, result.body, "\"error\"")
			if result.kind == "stream" {
				assert.Contains(t, result.body, "[DONE]")
			}
		}
	}
	batches := requests / len(kinds)
	require.Equal(t, int64(batches*8), upstreamCalls.Load(), "only retry and all-failure requests make two attempts")
	attempts.Range(func(key, value any) bool {
		kind, _, _ := strings.Cut(key.(string), ":")
		want := int32(1)
		if kind == "retry" || kind == "error" {
			want = 2
		}
		assert.Equal(t, want, value.(*atomic.Int32).Load(), key)
		return true
	})
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var user model.User
		var token model.Token
		require.NoError(collect, db.First(&user, 991).Error)
		require.NoError(collect, db.First(&token, 992).Error)
		// Delivered partial streams are charged; wholly failed attempts are refunded.
		assert.Equal(collect, relayHTTPInitialQuota-batches*50, user.Quota)
		assert.Equal(collect, relayHTTPInitialQuota-batches*50, token.RemainQuota)
		assert.Equal(collect, batches*50, user.UsedQuota)
		assert.Zero(collect, model.ChannelInFlight(81))
		assert.Zero(collect, model.ChannelInFlight(82))
		var total, success int64
		for _, row := range channel_score.Snapshot(channel_score.ScoreFilter{Model: taskRoutingModel}).Rows {
			total += row.Total
			success += row.Success
		}
		assert.Equal(collect, int64(batches*8), total)
		assert.Equal(collect, int64(batches*4), success)
	}, 5*time.Second, time.Millisecond)
	var tasks []model.Task
	require.NoError(t, db.Find(&tasks).Error)
	require.Len(t, tasks, batches)
	for _, task := range tasks {
		assert.True(t, taskIDs[task.TaskID], "every persisted task must have been returned to the caller")
		assert.Equal(t, 10, task.Quota)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("HTTP load: requests=%d concurrency=%d upstream_attempts=%d elapsed=%s throughput=%.2f req/s p50=%s p95=%s p99=%s; semantic_success=%d expected_failures=%d",
		requests, concurrency, upstreamCalls.Load(), elapsed, float64(requests)/elapsed.Seconds(),
		latencies[(requests*50-1)/100], latencies[(requests*95-1)/100], latencies[(requests*99-1)/100], batches*4, batches*2)
}

func TestRelayHTTPAbortReleasesQuotaAndSlots(t *testing.T) {
	for _, timedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "upstream timeout"}[timedOut], func(t *testing.T) {
			started := make(chan struct{}, 2)
			var ended atomic.Int32
			server, db := setupRelayHTTPTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				started <- struct{}{}
				<-r.Context().Done()
				ended.Add(1)
			}), 1, 1)
			upstreamClient := service.GetHttpClient()
			originalTimeout := upstreamClient.Timeout
			if timedOut {
				upstreamClient.Timeout = 50 * time.Millisecond
			}
			t.Cleanup(func() { upstreamClient.Timeout = originalTimeout })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			request, err := newRelayHTTPRequest(ctx, server.URL, "abort", 1)
			require.NoError(t, err)
			client := server.Client()
			client.Timeout = 5 * time.Second
			t.Cleanup(client.CloseIdleConnections)
			type outcome struct {
				status int
				err    error
			}
			finished := make(chan outcome, 1)
			go func() {
				response, err := client.Do(request)
				if err != nil {
					finished <- outcome{err: err}
					return
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				finished <- outcome{status: response.StatusCode, err: readErr}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach the upstream fixture")
			}
			if !timedOut {
				cancel()
			}
			var result outcome
			select {
			case result = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not finish after cancellation or upstream timeout")
			}
			wantAttempts := int32(1)
			wantFaults := int64(0)
			if timedOut {
				require.NoError(t, result.err)
				assert.Equal(t, http.StatusInternalServerError, result.status)
				wantAttempts, wantFaults = 2, 2
			} else {
				assert.ErrorIs(t, result.err, context.Canceled)
			}
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				var user model.User
				var token model.Token
				require.NoError(collect, db.First(&user, 991).Error)
				require.NoError(collect, db.First(&token, 992).Error)
				assert.Equal(collect, relayHTTPInitialQuota, user.Quota)
				assert.Equal(collect, relayHTTPInitialQuota, token.RemainQuota)
				assert.Zero(collect, user.UsedQuota)
				assert.Zero(collect, model.ChannelInFlight(81))
				assert.Zero(collect, model.ChannelInFlight(82))
				assert.Equal(collect, wantAttempts, ended.Load())
				var total, success int64
				for _, row := range channel_score.Snapshot(channel_score.ScoreFilter{Model: taskRoutingModel}).Rows {
					total += row.Total
					success += row.Success
				}
				assert.Equal(collect, wantFaults, total)
				assert.Zero(collect, success)
			}, 5*time.Second, time.Millisecond)
		})
	}
}

func TestRelayHTTPSaturationRejectsWithoutChargingAndReusesReleasedSlots(t *testing.T) {
	started := make(chan struct{}, 4)
	var calls atomic.Int32
	server, db := setupRelayHTTPTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Messages []struct{ Content string } }
		if err := common.DecodeJson(r.Body, &payload); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if len(payload.Messages) > 0 && strings.HasPrefix(payload.Messages[0].Content, "blocked:") {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"id\":\"chatcmpl-local\",\"object\":\"chat.completion\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}")
	}), 1, 2)
	client := server.Client()
	client.Timeout = 5 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	finished := make(chan error, 4)
	cancels := make([]context.CancelFunc, 0, 4)
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
	})
	for id := 0; id < 4; id++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		request, err := newRelayHTTPRequest(ctx, server.URL, "blocked", id)
		require.NoError(t, err)
		go func() {
			response, err := client.Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			finished <- err
		}()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("four channel slots were not admitted")
		}
	}
	assert.Equal(t, 2, model.ChannelInFlight(81))
	assert.Equal(t, 2, model.ChannelInFlight(82))
	probe, err := newRelayHTTPRequest(context.Background(), server.URL, "json", 9)
	require.NoError(t, err)
	response, err := client.Do(probe)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode, string(body))
	assert.Contains(t, string(body), string(types.ErrorCodeChannelsSaturated))
	assert.Equal(t, int32(4), calls.Load())
	for _, cancel := range cancels {
		cancel()
	}
	for range 4 {
		select {
		case err := <-finished:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled saturated request did not finish")
		}
	}
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var user model.User
		var token model.Token
		require.NoError(collect, db.First(&user, 991).Error)
		require.NoError(collect, db.First(&token, 992).Error)
		assert.Equal(collect, relayHTTPInitialQuota, user.Quota)
		assert.Equal(collect, relayHTTPInitialQuota, token.RemainQuota)
		assert.Zero(collect, model.ChannelInFlight(81))
		assert.Zero(collect, model.ChannelInFlight(82))
	}, 5*time.Second, time.Millisecond)
	assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{Model: taskRoutingModel}).Rows,
		"caller cancellations and rejected admission are not channel faults")

	probe, err = newRelayHTTPRequest(context.Background(), server.URL, "json", 10)
	require.NoError(t, err)
	response, err = client.Do(probe)
	require.NoError(t, err)
	body, err = io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode, string(body))
	assert.Equal(t, int32(5), calls.Load(), "released capacity must be immediately usable")
}
