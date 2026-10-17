package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestClassifyChannelModelSweepResult pins the three safety-critical, settings-
// independent verdicts the sweep must never get wrong: a clean test keeps the
// model, an explicit upstream "model does not exist" removes it, and an error we
// raised ourselves before ever reaching the upstream leaves the model untouched.
// A rate limit must also never remove a model. This policy is independent of
// the automatic channel-disable status-code configuration.
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
			want:   sweepVerdictTemporary,
		},
		{
			name:   "local request-build error never removes the model",
			result: testResult{newAPIError: nil, localErr: errors.New("dial tcp: timeout")},
			want:   sweepVerdictInconclusive,
		},
		{
			name:   "account permission failure is unavailable on this channel",
			result: testResult{newAPIError: upstreamJSONError(403, "permission_error", "forbidden")},
			want:   sweepVerdictInconclusive,
		},
		{
			name:   "explicit missing model on 400 is unavailable",
			result: testResult{newAPIError: upstreamJSONError(400, "model_not_found", "model does not exist")},
			want:   sweepVerdictFail,
		},
		{
			name:   "bad probe parameter must not delete a model",
			result: testResult{newAPIError: upstreamJSONError(400, "missing_required_parameter", "tools[0].description is required")},
			want:   sweepVerdictInconclusive,
		},
		{
			name:   "invalid Google API key on 400 is unavailable on this channel",
			result: testResult{newAPIError: upstreamJSONError(400, "INVALID_ARGUMENT", "API key not valid. Please pass a valid API key.")},
			want:   sweepVerdictAccount,
		},
		{
			name:   "retired free model must not remain routable or switch to paid",
			result: testResult{newAPIError: upstreamTextError(404, "This model is unavailable for free. The paid version is available now - use this slug instead: qwen/qwen3.8-27b")},
			want:   sweepVerdictFail,
		},
		{
			name:   "server outage stays unverified",
			result: testResult{newAPIError: upstreamJSONError(503, "model_not_found", "no available upstream")},
			want:   sweepVerdictTemporary,
		},
		{
			name:   "explicit retired model on 410 is unavailable",
			result: testResult{newAPIError: upstreamTextError(410, "The model 'retired-model' has reached its end of life and is no longer available.")},
			want:   sweepVerdictFail,
		},
		{
			name:   "gone endpoint alone is inconclusive",
			result: testResult{newAPIError: upstreamTextError(410, "This endpoint is gone")},
			want:   sweepVerdictInconclusive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyChannelModelSweepResult(tc.result))
		})
	}
}

// Exercise the task handler, real HTTP relay, persisted models and routing rows
// together; helper-only verdict tests missed the upstream/local error mix-up.
func TestModelUpdateHandlerFullAvailabilitySync(t *testing.T) {
	for _, payload := range []string{`{}`, `{"manual":true,"auto_apply":true}`, `{"full_sweep":true}`} {
		t.Run(payload, func(t *testing.T) {
			f := newUpstreamValidationFixture(t)
			t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
			t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_SIZE", "1")
			t.Setenv("CHANNEL_MODEL_SWEEP_MAX_TESTS_PER_RUN", "0")
			t.Cleanup(operation_setting.SetMonitorSettingForTest(operation_setting.MonitorSetting{
				UpstreamModelUpdateEnabled: true, UpstreamModelUpdateStripVendorPrefix: true,
				UpstreamModelUpdateRotationSampleSize: 0, UpstreamModelUpdateMaxValidationsPerRun: 1,
			}))
			var probes, disabledRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer disabled-test" {
					disabledRequests.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[{"id":"openai/gpt-4o"},{"id":"broken"}]}`)
					return
				}
				probes.Add(1)
				body, _ := io.ReadAll(r.Body)
				if gjson.GetBytes(body, "model").String() == "broken" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"model does not exist"}}`)
					return
				}
				assert.Equal(t, "openai/gpt-4o", gjson.GetBytes(body, "model").String())
				_, _ = io.WriteString(w, `{"id":"probe","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			t.Cleanup(upstream.Close)
			channel := f.createChannel(t, dto.ChannelOtherSettings{UpstreamModelUpdateIgnoredModels: []string{"gpt-4o"}})
			channel.Models, channel.BaseURL = "broken", &upstream.URL
			// A stale mapping must not hide a recovered model from candidate discovery.
			channel.ModelMapping = common.GetPointer(`{"gpt-4o":"openai/gpt-4o"}`)
			require.NoError(t, f.db.Save(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			disabled := *channel
			disabled.Id, disabled.Status, disabled.Key = 32, common.ChannelStatusManuallyDisabled, "disabled-test"
			require.NoError(t, f.db.Create(&disabled).Error)
			require.NoError(t, disabled.AddAbilities(nil))
			before, err := model.GetChannelById(disabled.Id, true)
			require.NoError(t, err)
			require.NoError(t, f.db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))
			task := &model.SystemTask{TaskID: "sweep", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusRunning, Payload: payload, LockedBy: "test-runner"}
			require.NoError(t, f.db.Create(task).Error)
			require.NoError(t, f.db.Create(&model.SystemTaskLock{
				Type: model.SystemTaskTypeModelUpdate, TaskID: task.TaskID, LockedBy: "test-runner", LockedUntil: common.GetTimestamp() + 600,
			}).Error)
			modelUpdateHandler{}.Run(context.Background(), task, "test-runner")
			var persisted model.SystemTask
			require.NoError(t, f.db.First(&persisted, task.ID).Error)
			assert.Equal(t, model.SystemTaskStatusSucceeded, persisted.Status, persisted.Error)
			var summary channelModelSweepSummary
			require.NoError(t, common.UnmarshalJsonStr(persisted.Result, &summary))
			assert.True(t, summary.Complete)
			assert.Equal(t, 1, summary.CheckedChannels)
			assert.Equal(t, 2, summary.TestedModels, "full sync ignores the old 1-test budget and 0-model sampling settings")
			assert.Equal(t, 1, summary.AddedModels)
			assert.Equal(t, 1, summary.RemovedModels)
			assert.EqualValues(t, 2, probes.Load())
			assert.Zero(t, disabledRequests.Load())
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, "gpt-4o", stored.Models)
			assert.Equal(t, channel.GetModelMapping(), stored.GetModelMapping())
			var routes []model.Ability
			require.NoError(t, f.db.Where("channel_id = ?", channel.Id).Find(&routes).Error)
			require.Len(t, routes, 1)
			assert.Equal(t, "gpt-4o", routes[0].Model)
			assert.True(t, routes[0].Enabled)
			after, err := model.GetChannelById(disabled.Id, true)
			require.NoError(t, err)
			assert.Equal(t, before, after, "disabled channel must be byte-for-byte unchanged")
		})
	}
}

func TestSweepRetriesRateLimitAndRestoresRemovedModel(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"concurrency limit"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"probe","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{UpstreamModelUpdateModelHealth: map[string]dto.ModelHealthState{"gpt-4o": {Failures: 1}}})
	channel.Models, channel.BaseURL = "", &upstream.URL
	require.NoError(t, f.db.Save(channel).Error)
	budget := 10
	result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
	require.NoError(t, err)
	assert.EqualValues(t, 2, probes.Load())
	assert.Equal(t, []string{"gpt-4o"}, result.added)
	assert.Empty(t, result.removed)
	assert.Empty(t, result.unverified)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", stored.Models)
	assert.Empty(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth)
}

func TestSweepMultiKeyModelRemovalRequiresAllEnabledKeysToReject(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newUpstreamValidationFixture(t)
			t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[]}`)
					return
				}
				resultStatus := http.StatusNotFound
				if r.Header.Get("Authorization") == "Bearer first-key" {
					resultStatus = status
				}
				w.WriteHeader(resultStatus)
				if resultStatus == http.StatusOK {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				} else if resultStatus == http.StatusUnauthorized {
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"invalid api key"}}`)
				} else {
					_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"model does not exist"}}`)
				}
			}))
			t.Cleanup(upstream.Close)
			channel := f.createChannel(t, dto.ChannelOtherSettings{})
			channel.Models, channel.BaseURL, channel.Key = "gpt-4o", &upstream.URL, "first-key\nsecond-key"
			channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
			require.NoError(t, f.db.Save(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			budget := 10
			result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
			require.NoError(t, err)
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			if status == http.StatusNotFound {
				assert.Empty(t, stored.Models)
				assert.Equal(t, []string{"gpt-4o"}, result.removed)
			} else {
				assert.Equal(t, "gpt-4o", stored.Models)
				assert.Empty(t, result.removed, "a single key's rejection must not delete a model for the whole channel")
			}
		})
	}
}

func TestSweepFetchFailureStillTestsExistingAndReportsIncomplete(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"invalid api key"}}`)
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Models, channel.BaseURL = "gpt-4o", &upstream.URL
	require.NoError(t, f.db.Save(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	summary := runChannelModelSweepTaskOnce(context.Background(), nil)
	assert.False(t, summary.Complete)
	assert.Equal(t, 1, summary.FailedChannels)
	assert.Equal(t, 1, summary.TestedModels)
	assert.Zero(t, summary.RemovedModels)
	assert.Zero(t, summary.EmptiedChannels)
	assert.NotEmpty(t, summary.ScanError)
	require.Len(t, summary.Channels, 1)
	assert.True(t, summary.Channels[0].FetchFailed)
	assert.Equal(t, []string{"gpt-4o"}, summary.Channels[0].Unverified)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", stored.Models)
	assert.Contains(t, model.GetEnabledModels(), "gpt-4o", "an account error must not hide a listed model")
}

func TestSweepRemovesExplicitRejectionsAndRetainsRecoveryCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"invalid key", 400, `{"error":{"code":"INVALID_ARGUMENT","message":"API key not valid. Please pass a valid API key."}}`},
		{"free model retired", 404, `{"error":{"message":"This model is unavailable for free. The paid version is available now - use this slug instead: vendor/paid-model"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpstreamValidationFixture(t)
			t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
			var probes atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o"}]}`)
					return
				}
				probes.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(upstream.Close)
			channel := f.createChannel(t, dto.ChannelOtherSettings{})
			channel.Models, channel.BaseURL = "gpt-4o", &upstream.URL
			require.NoError(t, f.db.Save(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			budget := 10
			result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
			require.NoError(t, err)
			assert.EqualValues(t, 1, probes.Load(), "definitive rejections must not waste retry budget")
			if tc.status == 400 {
				assert.Empty(t, result.removed)
				assert.Equal(t, []string{"gpt-4o"}, result.unverified)
			} else {
				assert.Equal(t, []string{"gpt-4o"}, result.removed)
				assert.Empty(t, result.unverified)
			}
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			var activeRoutes int64
			require.NoError(t, f.db.Model(&model.Ability{}).Where("channel_id = ? AND enabled = ?", channel.Id, true).Count(&activeRoutes).Error)
			if tc.status == 400 {
				// An account error says nothing about the model: it stays listed and routable.
				assert.Equal(t, "gpt-4o", stored.Models)
				assert.EqualValues(t, 1, activeRoutes)
			} else {
				assert.Empty(t, stored.Models)
				assert.Contains(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth, "gpt-4o")
				assert.Zero(t, activeRoutes)
			}
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			assert.Equal(t, channel.GetModelMapping(), stored.GetModelMapping(), "do not adopt paid replacements")
		})
	}
}

func TestSweepKeepsTemporaryFailuresListedAndRoutable(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		probes.Add(1)
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"temporary"}}`)
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Models, channel.BaseURL = "gpt-4o", &upstream.URL
	require.NoError(t, f.db.Save(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	budget := 10
	result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
	require.NoError(t, err)
	assert.EqualValues(t, 3, probes.Load())
	assert.Empty(t, result.removed)
	assert.Equal(t, []string{"gpt-4o"}, result.unverified)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", stored.Models)
	assert.Contains(t, model.GetEnabledModels(), "gpt-4o")
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
}

func TestSweepExplicitBudgetIsIncompleteNotSuccessful(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	t.Setenv("CHANNEL_MODEL_SWEEP_MAX_TESTS_PER_RUN", "1")
	t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Models = "gpt-4o"
	require.NoError(t, f.db.Save(channel).Error)
	summary := runChannelModelSweepTaskOnce(context.Background(), nil)
	assert.False(t, summary.Complete)
	assert.NotEmpty(t, summary.ScanError)
	assert.Equal(t, 1, summary.TestedModels)
	assert.Zero(t, summary.RemovedModels)
	require.Len(t, summary.Channels, 1)
	assert.NotEmpty(t, summary.Channels[0].Unverified)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Contains(t, strings.Split(stored.Models, ","), "gpt-4o")
}

// TestComputeSweptModels covers the pruning contract: keep passing and inconclusive
// models, remove confirmed failures (including models already on the channel), add
// newly passing upstream candidates, leave untested models exactly as they were,
// and allow an empty catalogue when every existing model is confirmed unavailable.
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

	t.Run("all existing fail empties the channel", func(t *testing.T) {
		next, added, removed, outage := computeSweptModels(
			[]string{"a", "b"}, nil,
			map[string]channelModelSweepVerdict{"a": sweepVerdictFail, "b": sweepVerdictFail},
		)
		assert.Empty(t, next)
		assert.Empty(t, added)
		assert.Equal(t, []string{"a", "b"}, removed)
		assert.False(t, outage)
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

func TestSweepRemovesRealUpstream404AndItsAbilities(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"model does not exist"}}`)
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.BaseURL = &upstream.URL
	channel.Models = "gpt-4o"
	require.NoError(t, f.db.Model(channel).Updates(map[string]any{"base_url": upstream.URL, "models": channel.Models}).Error)
	require.NoError(t, channel.AddAbilities(nil))

	probe := testChannel(context.Background(), channel, 1, channel.Models, "", false)
	require.NotNil(t, probe.newAPIError)
	require.Equal(t, http.StatusNotFound, probe.newAPIError.StatusCode, "%v", probe.localErr)
	assert.Equal(t, sweepVerdictFail, classifyChannelModelSweepResult(probe), "a real upstream 404 must reach the deletion classifier")
	budget := 10
	result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
	require.NoError(t, err)
	assert.Equal(t, []string{"gpt-4o"}, result.removed)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Empty(t, stored.Models)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	var abilities []model.Ability
	require.NoError(t, f.db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	assert.Empty(t, abilities, "an empty model list must not leave a model=empty-string route")
	// Removed candidates are still probed on later runs.
	skipped, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
	require.NoError(t, err)
	assert.False(t, skipped.skipped)
	assert.Equal(t, 1, skipped.tested)
}

func TestSweepDisablesOnlyConfirmedEmptyChannels(t *testing.T) {
	for _, tc := range []struct {
		name            string
		existing        string
		catalogue       string
		fetchStatus     int
		candidateStatus int
		wantDisabled    bool
	}{
		{"all models unavailable", "gpt-4o", `{"data":[{"id":"gpt-4o"},{"id":"broken"}]}`, 200, 404, false},
		{"empty catalogue and no configured models", "", `{"data":[]}`, 200, 404, false},
		{"catalogue fetch failed is not proof of emptiness", "", `{"error":"temporary failure"}`, 503, 404, false},
		{"candidate rate limited is not proof of emptiness", "gpt-4o", `{"data":[{"id":"gpt-4o"},{"id":"broken"}]}`, 200, 429, false},
		{"one passing candidate keeps channel enabled", "gpt-4o", `{"data":[{"id":"gpt-4o"},{"id":"broken"}]}`, 200, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpstreamValidationFixture(t)
			t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.WriteHeader(tc.fetchStatus)
					_, _ = io.WriteString(w, tc.catalogue)
					return
				}
				body, _ := io.ReadAll(r.Body)
				status := http.StatusNotFound
				if gjson.GetBytes(body, "model").String() == "broken" {
					status = tc.candidateStatus
				}
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = io.WriteString(w, `{"id":"probe","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				} else {
					_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"model unavailable"}}`)
				}
			}))
			t.Cleanup(upstream.Close)
			channel := f.createChannel(t, dto.ChannelOtherSettings{})
			channel.Models, channel.BaseURL = tc.existing, &upstream.URL
			require.NoError(t, f.db.Save(channel).Error)
			summary := runChannelModelSweepTaskOnce(context.Background(), nil)
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			if tc.wantDisabled {
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.Empty(t, stored.Models)
				assert.NotEmpty(t, stored.GetOtherInfo()["status_reason"])
				assert.Equal(t, 1, summary.DisabledChannels)
			} else {
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.Zero(t, summary.DisabledChannels)
			}
			assert.Equal(t, 1, summary.CheckedChannels)
			require.Len(t, summary.Channels, 1)
			assert.Equal(t, tc.wantDisabled, summary.Channels[0].Disabled)
		})
	}
}

func TestSweepCodexProfileUsesResponsesInsteadOfDeletingWorkingModel(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	var responseRequests, wrongEndpointRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o"}]}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			wrongEndpointRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"当前 API 不支持所选模型 gpt-4o","code":"model_not_found"}}`)
			return
		}
		responseRequests.Add(1)
		body, _ := io.ReadAll(r.Body)
		assert.True(t, gjson.GetBytes(body, "stream").Bool(), "automatic Codex probes must use the CLI streaming protocol")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_probe\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-4o\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Models, channel.BaseURL = "gpt-4o", &upstream.URL
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})
	require.NoError(t, f.db.Save(channel).Error)
	budget := 10
	result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
	require.NoError(t, err)
	assert.Zero(t, wrongEndpointRequests.Load())
	assert.EqualValues(t, 1, responseRequests.Load())
	assert.Empty(t, result.removed)
	assert.Zero(t, result.inconclusive)
	assert.False(t, result.disabled)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, "gpt-4o", stored.Models)
}

func TestCodexProfileTestFallsBackOnlyForUnsupportedAutomaticResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		message  string
		endpoint string
		fallback bool
	}{
		{"missing responses route", 404, "openai_error", "", true},
		{"wrong method", 405, "method not allowed", "", true},
		{"unimplemented responses", 501, "not implemented", "", true},
		{"provider reports unimplemented as 500", 500, "not implemented", "", true},
		{"rate limit is not an endpoint failure", 429, "rate limited", "", false},
		{"permission failure is not retried", 403, "forbidden", "", false},
		{"server outage is not an endpoint failure", 500, "temporary outage", "", false},
		{"bad tools remain an error", 400, "tools[0].description is required", "", false},
		{"explicit responses stays explicit", 404, "openai_error", "openai-response", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpstreamValidationFixture(t)
			var chatRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/responses" {
					w.WriteHeader(tc.status)
					body, err := common.Marshal(map[string]any{"error": map[string]string{"message": tc.message}})
					assert.NoError(t, err)
					_, _ = w.Write(body)
					return
				}
				chatRequests.Add(1)
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				body, _ := io.ReadAll(r.Body)
				assert.True(t, gjson.GetBytes(body, "messages").IsArray())
				assert.Equal(t, "gpt-4o", gjson.GetBytes(body, "model").String())
				assert.True(t, gjson.GetBytes(body, "stream").Bool(), "fallback must preserve the requested streaming mode")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"probe\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"probe\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(upstream.Close)
			channel := f.createChannel(t, dto.ChannelOtherSettings{})
			channel.Models, channel.BaseURL = "gpt-4o", &upstream.URL
			channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})
			require.NoError(t, f.db.Save(channel).Error)
			result := testChannel(context.Background(), channel, 1, "gpt-4o", tc.endpoint, true)
			if tc.fallback {
				assert.Equal(t, sweepVerdictPass, classifyChannelModelSweepResult(result), "%v / %v", result.localErr, result.newAPIError)
				assert.EqualValues(t, 1, chatRequests.Load())
			} else {
				assert.NotNil(t, result.newAPIError)
				assert.Zero(t, chatRequests.Load())
			}
		})
	}
}

func TestClaudeCompactNamedModelUsesNativeAutomaticTest(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	ratios := ratio_setting.GetModelRatioCopy()
	ratios["gpt-4o-openai-compact"] = 1
	ratioJSON, err := common.Marshal(ratios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(ratioJSON)))
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/v1/messages", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		assert.Equal(t, "gpt-4o-openai-compact", gjson.GetBytes(body, "model").String())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"permission_error","message":"forbidden"}}`)
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Type = constant.ChannelTypeAnthropic
	channel.Models, channel.BaseURL = "gpt-4o-openai-compact", &upstream.URL
	require.NoError(t, f.db.Save(channel).Error)
	result := testChannel(context.Background(), channel, 1, channel.Models, "", false)
	require.Nil(t, result.localErr)
	require.NotNil(t, result.newAPIError)
	assert.Equal(t, http.StatusForbidden, result.newAPIError.StatusCode)
	assert.Equal(t, sweepVerdictInconclusive, classifyChannelModelSweepResult(result))
	assert.EqualValues(t, 1, requests.Load())
	// Explicit compaction is still unsupported on a Claude channel: do not turn
	// an explicitly requested protocol into a different successful operation.
	result = testChannel(context.Background(), channel, 1, channel.Models, "openai-response-compact", false)
	assert.NotNil(t, result.localErr)
	assert.EqualValues(t, 1, requests.Load())
}

func TestSweepKeepsModelWhenAlternativeProtocolIsRateLimited(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	t.Setenv("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", "0")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o"}]}`)
		} else if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"model not supported on this endpoint"}}`)
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"try later"}}`)
		}
	}))
	t.Cleanup(upstream.Close)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Models, channel.BaseURL = "gpt-4o", &upstream.URL
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})
	require.NoError(t, f.db.Save(channel).Error)
	budget := 10
	result, err := sweepChannelModels(context.Background(), channel.Id, 1, &budget)
	require.NoError(t, err)
	assert.Empty(t, result.removed)
	assert.Equal(t, 1, result.inconclusive)
	assert.False(t, result.disabled)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", stored.Models)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
}
