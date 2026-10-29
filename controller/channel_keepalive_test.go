package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const channelKeepaliveTestPrompt = "keepalive test question"

// keepaliveTestMaxChannelId bounds the channel ids these tests use, so the
// fixture can clear their activity records.
const keepaliveTestMaxChannelId = 20

type capturedKeepaliveRequest struct {
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// setupChannelKeepaliveTest pins the shared prompt pool to one entry and turns
// on everything a stray keepalive side effect would show up in: consume logs,
// channel scoring, and auto-disable on 401.
func setupChannelKeepaliveTest(t *testing.T) *gorm.DB {
	t.Helper()
	originalDB, originalLogDB := model.DB, model.LOG_DB
	originalMode, originalRedis, originalCache := gin.Mode(), common.RedisEnabled, common.MemoryCacheEnabled
	originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
	originalDisable, originalLogConsume := common.AutomaticDisableChannelEnabled, common.LogConsumeEnabled
	originalDisableCodes := operation_setting.AutomaticDisableStatusCodesToString()
	originalScores := operation_setting.GetChannelDynamicScoreSetting()

	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Log{}, &model.User{}, &model.SystemTask{}, &model.SystemTaskLock{}))
	model.DB, model.LOG_DB = db, db

	common.AutomaticDisableChannelEnabled, common.LogConsumeEnabled = true, true
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("401"))
	scores := originalScores
	scores.Enabled = true
	operation_setting.SetChannelDynamicScoreSettingForTest(scores)
	channel_score.ResetAll()
	monitor := *operation_setting.GetMonitorSetting()
	monitor.ChannelTestPrompts = []string{channelKeepaliveTestPrompt}
	restoreMonitor := operation_setting.SetMonitorSettingForTest(monitor)
	service.InitHttpClient()
	// The activity record is process-wide; start every test with no traffic seen.
	for id := 1; id <= keepaliveTestMaxChannelId; id++ {
		service.ForgetChannelKeysForTest(id)
	}

	t.Cleanup(func() {
		for id := 1; id <= keepaliveTestMaxChannelId; id++ {
			service.ForgetChannelKeysForTest(id)
		}
		restoreMonitor()
		channel_score.ResetAll()
		operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
		require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString(originalDisableCodes))
		common.AutomaticDisableChannelEnabled, common.LogConsumeEnabled = originalDisable, originalLogConsume
		model.DB, model.LOG_DB = originalDB, originalLogDB
		common.SetDatabaseTypes(originalMainType, originalLogType)
		common.RedisEnabled, common.MemoryCacheEnabled = originalRedis, originalCache
		gin.SetMode(originalMode)
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func newKeepaliveUpstream(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []capturedKeepaliveRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedKeepaliveRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedKeepaliveRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(upstream.Close)
	return upstream, func() []capturedKeepaliveRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedKeepaliveRequest(nil), captured...)
	}
}

const (
	keepaliveChatReply       = `{"id":"chatcmpl-keepalive","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	keepaliveResponsesReply  = `{"id":"resp_keepalive","object":"response","created_at":1,"status":"completed","model":"gpt-5-codex","output":[{"type":"message","id":"msg_keepalive","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`
	keepaliveResponsesStream = "event: response.completed\ndata: " +
		`{"type":"response.completed","response":{"id":"resp_keepalive","object":"response","status":"completed","model":"gpt-6.1-sol",` +
		`"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}` + "\n\n"
	keepaliveClaudeStream = "event: message_start\ndata: " +
		`{"type":"message_start","message":{"id":"msg_keepalive","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
)

// TestChannelKeepaliveRequestShape pins what a keepalive sends upstream from a
// channel without a client profile: the channel's own key, the test model
// (else the first model), one user message made of a question from the shared
// channel-test pool plus the reply instruction, a 16-token cap, and no
// streaming.
func TestChannelKeepaliveRequestShape(t *testing.T) {
	wantPrompt := channelKeepaliveTestPrompt + "\n" + channelKeepaliveInstruction
	for _, tc := range []struct {
		name        string
		channelType int
		models      string
		testModel   string
		reply       string
		wantPath    string
		wantAuth    [2]string
		wantBody    map[string]string
	}{
		{
			name: "test model wins", channelType: constant.ChannelTypeOpenAI,
			models: "gpt-4o,gpt-4o-mini", testModel: "gpt-4o-mini", reply: keepaliveChatReply,
			wantPath: "/v1/chat/completions", wantAuth: [2]string{"Authorization", "Bearer sk-keepalive"},
			wantBody: map[string]string{
				"model": "gpt-4o-mini", "messages.#": "1", "messages.0.role": "user",
				"messages.0.content": wantPrompt, "max_tokens": "16",
			},
		},
		{
			name: "first model without a test model", channelType: constant.ChannelTypeOpenAI,
			models: "gpt-4o,gpt-4o-mini", reply: keepaliveChatReply,
			wantPath: "/v1/chat/completions", wantAuth: [2]string{"Authorization", "Bearer sk-keepalive"},
			wantBody: map[string]string{"model": "gpt-4o", "messages.0.content": wantPrompt, "max_tokens": "16"},
		},
		{
			name: "codex model uses the responses endpoint", channelType: constant.ChannelTypeOpenAI,
			models: "gpt-5-codex", reply: keepaliveResponsesReply,
			wantPath: "/v1/responses", wantAuth: [2]string{"Authorization", "Bearer sk-keepalive"},
			wantBody: map[string]string{
				"model": "gpt-5-codex", "input.#": "1", "input.0.role": "user",
				"input.0.content": wantPrompt, "max_output_tokens": "16",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupChannelKeepaliveTest(t)
			upstream, requests := newKeepaliveUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.reply)
			})
			channel := &model.Channel{
				Id: 3, Type: tc.channelType, Key: "sk-keepalive", Status: common.ChannelStatusEnabled,
				Models: tc.models, Group: "default", BaseURL: &upstream.URL,
			}
			if tc.testModel != "" {
				channel.TestModel = &tc.testModel
			}

			require.NoError(t, sendChannelKeepalive(context.Background(), channel, "0", channel.Key, 0))

			captured := requests()
			require.Len(t, captured, 1)
			request := captured[0]
			assert.Equal(t, tc.wantPath, request.Path)
			assert.Equal(t, tc.wantAuth[1], request.Header.Get(tc.wantAuth[0]))
			for path, want := range tc.wantBody {
				assert.Equal(t, want, gjson.GetBytes(request.Body, path).String(), "body field %s: %s", path, request.Body)
			}
			assert.False(t, gjson.GetBytes(request.Body, "stream").Bool(), "keepalive must not stream: %s", request.Body)
		})
	}
}

// TestChannelKeepaliveWearsClientProfile pins that a keepalive on a channel
// with a CLI profile goes out as that channel's own CLI: the CLI version's
// headers and TLS fingerprint, the CLI's endpoint, streaming, and no output
// limit the CLI would not send. With a real session recorded on the channel it
// continues that session: the same model and reasoning, and on every keepalive
// the same client and session ids, derived from that channel's seeds and no
// other channel's.
func TestChannelKeepaliveWearsClientProfile(t *testing.T) {
	setupChannelKeepaliveTest(t)
	originalSkipVerify := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		common.TLSInsecureSkipVerify = originalSkipVerify
		service.ResetProxyClientCache()
	})
	var mu sync.Mutex
	var captured []capturedKeepaliveRequest
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedKeepaliveRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_, _ = io.WriteString(w, keepaliveClaudeStream)
			return
		}
		_, _ = io.WriteString(w, keepaliveResponsesStream)
	}))
	t.Cleanup(upstream.Close)
	send := func(channel *model.Channel) capturedKeepaliveRequest {
		mu.Lock()
		captured = nil
		mu.Unlock()
		require.NoError(t, sendChannelKeepalive(context.Background(), channel, "0", channel.Key, 0))
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, captured, 1)
		return captured[0]
	}

	t.Run("codex", func(t *testing.T) {
		version := service.LookupCLIVersionProfile("codex-0.156.1-windows-x64")
		require.NotNil(t, version)
		newChannel := func(id int, seed string) *model.Channel {
			t.Cleanup(func() { service.ForgetChannelKeysForTest(id) })
			channel := &model.Channel{
				Id: id, Type: constant.ChannelTypeOpenAI, Key: "sk-codex", Status: common.ChannelStatusEnabled,
				Models: "gpt-6-astra,gpt-6.1-sol", Group: "default", BaseURL: &upstream.URL,
			}
			channel.SetSetting(dto.ChannelSettings{
				SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex, CLIVersionProfile: version.ID,
				ClientDeviceSeed: "device-" + seed, ClientSessionSeed: "session-" + seed,
			})
			return channel
		}
		first, second := newChannel(11, "a"), newChannel(12, "b")
		installation := func(request capturedKeepaliveRequest) string {
			return gjson.GetBytes(request.Body, "client_metadata.x-codex-installation-id").String()
		}

		fresh := send(first)
		assert.Equal(t, "/v1/responses", fresh.Path)
		assert.Equal(t, "Bearer sk-codex", fresh.Header.Get("Authorization"))
		for name, value := range version.Headers {
			assert.Equal(t, value, fresh.Header.Get(name), name)
		}
		assert.Equal(t, "gpt-6-astra", gjson.GetBytes(fresh.Body, "model").String(), "no session yet: the first model")
		assert.True(t, gjson.GetBytes(fresh.Body, "stream").Bool())
		assert.False(t, gjson.GetBytes(fresh.Body, "max_output_tokens").Exists(), "the Codex CLI sends no output limit: %s", fresh.Body)
		require.NotEmpty(t, fresh.Header.Get("session-id"))
		assert.Equal(t, fresh.Header.Get("session-id"), gjson.GetBytes(fresh.Body, "prompt_cache_key").String())
		require.NotEmpty(t, installation(fresh))

		session := service.ChannelCLISession{
			ModelName: "gpt-6.1-sol", UserId: 42, TokenId: 7, Seed: "real-session",
			Reasoning: []byte(`{"effort":"xhigh"}`),
		}
		service.RecordChannelCLISession(first.Id, "0", session)
		service.RecordChannelCLISession(second.Id, "0", session)
		continued := send(first)
		assert.Equal(t, "gpt-6.1-sol", gjson.GetBytes(continued.Body, "model").String())
		assert.Equal(t, "xhigh", gjson.GetBytes(continued.Body, "reasoning.effort").String())
		assert.NotEqual(t, fresh.Header.Get("session-id"), continued.Header.Get("session-id"), "the real session, not a new one")

		again := send(first)
		assert.Equal(t, continued.Header.Get("session-id"), again.Header.Get("session-id"))
		assert.Equal(t, installation(continued), installation(again))
		assert.NotEqual(t, continued.Header.Get("x-client-request-id"), again.Header.Get("x-client-request-id"), "each keepalive is a new turn")

		other := send(second)
		assert.NotEqual(t, continued.Header.Get("session-id"), other.Header.Get("session-id"))
		assert.NotEqual(t, installation(continued), installation(other))
	})

	t.Run("claude", func(t *testing.T) {
		version := service.LookupCLIVersionProfile("claude-2.1.306-linux-x64")
		require.NotNil(t, version)
		t.Cleanup(func() { service.ForgetChannelKeysForTest(13) })
		channel := &model.Channel{
			Id: 13, Type: constant.ChannelTypeAnthropic, Key: "sk-claude", Status: common.ChannelStatusEnabled,
			Models: "claude-opus-5-5", Group: "default", BaseURL: &upstream.URL,
		}
		channel.SetSetting(dto.ChannelSettings{
			SyntheticClientHeadersProfile: constant.ClientHeaderFamilyClaude, CLIVersionProfile: version.ID,
		})

		request := send(channel)
		assert.Equal(t, "/v1/messages", request.Path)
		assert.Equal(t, "beta=true", request.Query)
		assert.Equal(t, "sk-claude", request.Header.Get("X-Api-Key"))
		assert.Equal(t, version.Headers["user-agent"], request.Header.Get("User-Agent"))
		assert.NotEmpty(t, request.Header.Get("X-Claude-Code-Session-Id"))
		assert.True(t, gjson.GetBytes(request.Body, "stream").Bool())
		assert.Equal(t, int64(version.MaxTokens), gjson.GetBytes(request.Body, "max_tokens").Int(), "the profile limit, not a keepalive cap: %s", request.Body)
		assert.NotEmpty(t, gjson.GetBytes(request.Body, "metadata.user_id").String())
	})
}

// TestChannelKeepalivePassIsolation pins the scheduler's selection and the
// keepalive's isolation from everything a real request or channel test touches.
// Only enabled, switched-on channels whose key's real traffic stopped an
// interval ago and is still inside the active window are sent: not one whose
// key has a request in flight, one last used past the window, one kept alive
// less than an interval ago, or one never used since startup even if task10
// stamped a send time on it. A 401 with auto-disable on is logged and nothing
// more: the channel stays enabled, no consume or error log is written, no
// channel score is recorded, and the test time and response time do not move.
// The keepalive is not activity either, so the keys' activity records are where
// the seed put them. The send time is stamped for success and failure alike, so
// a pass right after finds nothing due and sends nothing.
func TestChannelKeepalivePassIsolation(t *testing.T) {
	db := setupChannelKeepaliveTest(t)
	upstream, requests := newKeepaliveUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer sk-revoked" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		_, _ = io.WriteString(w, keepaliveChatReply)
	})
	now := common.GetTimestamp()
	recent := now - 60
	// Outside the ±10% jitter of the default 30-minute interval, inside the
	// default 120-minute window.
	quiet := now - 40*60
	keepalive := func(lastTime int64) dto.ChannelOtherSettings {
		return dto.ChannelOtherSettings{UpstreamKeepaliveEnabled: true, UpstreamKeepaliveLastTime: lastTime}
	}
	for _, seed := range []struct {
		id            int
		key           string
		status        int
		settings      *dto.ChannelOtherSettings
		raw           string
		lastActive    int64
		lastKeepalive int64
	}{
		{id: 1, key: "sk-revoked", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(0)), lastActive: quiet},
		{id: 2, key: "sk-live", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(0)), lastActive: quiet},
		{id: 3, key: "sk-recent", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(recent)), lastActive: quiet, lastKeepalive: recent},
		// Switched off but still matching the scan's LIKE.
		{id: 4, key: "sk-off", status: common.ChannelStatusEnabled, raw: `{"upstream_keepalive_enabled":false}`, lastActive: quiet},
		{id: 5, key: "sk-disabled", status: common.ChannelStatusManuallyDisabled, settings: lo.ToPtr(keepalive(0)), lastActive: quiet},
		// Used a minute ago: the interval has not passed since real traffic.
		{id: 6, key: "sk-active", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(0)), lastActive: recent},
		// Last used past the active window: the keepalive has stopped.
		{id: 7, key: "sk-stale", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(0)), lastActive: now - 121*60},
		// Sent unconditionally by task10 long ago, no real traffic since startup.
		{id: 8, key: "sk-unused", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(now - 600*60))},
		// A request is being served right now, so the key is in use.
		{id: 10, key: "sk-busy", status: common.ChannelStatusEnabled, settings: lo.ToPtr(keepalive(0)), lastActive: quiet},
	} {
		channel := &model.Channel{
			Id: seed.id, Type: constant.ChannelTypeOpenAI, Key: seed.key, Status: seed.status,
			Models: "gpt-4o", Group: "default", BaseURL: &upstream.URL, AutoBan: lo.ToPtr(1),
			OtherSettings: seed.raw,
		}
		if seed.settings != nil {
			channel.SetOtherSettings(*seed.settings)
		}
		require.NoError(t, db.Create(channel).Error)
		service.SetChannelKeyActivityForTest(seed.id, "0", seed.lastActive, seed.lastKeepalive)
	}
	// Bound the way the distributor binds a request: the slot, then the key.
	busy, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, model.AcquireChannelSlot(10, 0))
	service.HoldChannelSlot(busy, 10)
	service.HoldChannelKey(busy, 10, "0")
	t.Cleanup(func() { service.ReleaseHeldChannelSlot(busy) })

	summary, err := runChannelKeepalivePass(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, summary.Due)
	assert.Equal(t, 1, summary.Succeeded)
	assert.Equal(t, 1, summary.Failed)
	require.Len(t, summary.Failures, 1)
	assert.Equal(t, 1, summary.Failures[0].ChannelId)
	assert.Nil(t, summary.Failures[0].KeyIndex, "a single-key channel's failure names no key")
	assert.Contains(t, summary.Failures[0].Error, "401")
	captured := requests()
	require.Len(t, captured, 2)
	for _, request := range captured {
		assert.Contains(t, []string{"Bearer sk-revoked", "Bearer sk-live"}, request.Header.Get("Authorization"))
	}

	lastTimes := map[int]int64{}
	for _, id := range []int{1, 2, 3, 4, 5, 6, 7, 8, 10} {
		stored, err := model.GetChannelById(id, true)
		require.NoError(t, err)
		lastTimes[id] = stored.GetOtherSettings().UpstreamKeepaliveLastTime
		assert.Zero(t, stored.TestTime, "channel %d: a keepalive is not a channel test", id)
		assert.Zero(t, stored.ResponseTime, "channel %d: a keepalive is not a channel test", id)
		if id != 5 {
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status, "channel %d: a keepalive never disables", id)
			assert.NotContains(t, stored.OtherInfo, "status_reason", "channel %d", id)
		}
	}
	assert.Positive(t, lastTimes[1], "a failed keepalive still waits a full interval")
	assert.Positive(t, lastTimes[2])
	assert.Equal(t, recent, lastTimes[3])
	assert.Zero(t, lastTimes[4])
	assert.Zero(t, lastTimes[5])
	assert.Zero(t, lastTimes[6], "a channel in active use needs no keepalive")
	assert.Zero(t, lastTimes[7], "past the active window the keepalive stops")
	assert.Equal(t, now-600*60, lastTimes[8], "a task10 send stamp alone is no activity")
	assert.Zero(t, lastTimes[10], "a request in flight is activity")
	// A sent keepalive leaves the activity record alone, so it cannot keep its
	// own window open.
	for _, id := range []int{1, 2} {
		assert.Equal(t, quiet, service.ChannelKeyActivityAt(id, "0", common.GetTimestamp()).LastActive, "channel %d: a keepalive is not channel activity", id)
	}

	var logCount int64
	require.NoError(t, db.Model(&model.Log{}).Count(&logCount).Error)
	assert.Zero(t, logCount, "a keepalive writes no consume or error log")
	for id := 1; id <= 2; id++ {
		assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{ChannelID: id}).Rows, "channel %d", id)
	}
	// Positive control: with this setup a reported outcome does land in the store.
	channel_score.Report(2, "default", "gpt-4o", channel_score.OutcomeSuccess)
	assert.NotEmpty(t, channel_score.Snapshot(channel_score.ScoreFilter{ChannelID: 2}).Rows)

	again, err := runChannelKeepalivePass(context.Background())
	require.NoError(t, err)
	assert.Zero(t, again.Due)
	assert.Len(t, requests(), 2, "a pass right after must not send again")
	assert.False(t, channelKeepaliveHandler{}.Enabled(), "no task row is scheduled while nothing is due")

	// A real request on the stale channel's key, bound the way the distributor
	// binds one, brings it back into the active window: due again once it has
	// been quiet for an interval, not before.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, model.AcquireChannelSlot(7, 0))
	service.HoldChannelSlot(c, 7)
	service.HoldChannelKey(c, 7, "0")
	assert.Equal(t, now+999, service.ChannelKeyActivityAt(7, "0", now+999).LastActive, "a request in flight is activity as of now")
	service.ReleaseHeldChannelSlot(c)
	activity := service.ChannelKeyActivityAt(7, "0", common.GetTimestamp())
	reentered := activity.LastActive
	require.GreaterOrEqual(t, reentered, now)
	stored, err := model.GetChannelById(7, true)
	require.NoError(t, err)
	settings := stored.GetOtherSettings()
	assert.False(t, settings.UpstreamKeepaliveDue(7, "0", reentered, activity.LastKeepalive, reentered+20*60))
	assert.True(t, settings.UpstreamKeepaliveDue(7, "0", reentered, activity.LastKeepalive, reentered+40*60))
}

// TestChannelKeepaliveKeepsEachKeyApart pins that the keys of a multi-key CLI
// channel are kept alive as the separate clients they are to the upstream. Each
// key is judged on its own activity: the one serving a request is skipped while
// the others are sent, and a disabled one is never sent. Each goes out with its
// own key, wearing its own CLI version where it has one and the channel's where
// it does not, and continues its own last session under its own identity: with
// the same user, token and conversation seed, two keys are still two sessions
// and two installations. A failing key is named by its index and stays enabled.
func TestChannelKeepaliveKeepsEachKeyApart(t *testing.T) {
	db := setupChannelKeepaliveTest(t)
	originalSkipVerify := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		common.TLSInsecureSkipVerify = originalSkipVerify
		service.ResetProxyClientCache()
	})
	var mu sync.Mutex
	sent := map[string]capturedKeepaliveRequest{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		sent[key] = capturedKeepaliveRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
		mu.Unlock()
		if key == "sk-key-d" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, keepaliveResponsesStream)
	}))
	t.Cleanup(upstream.Close)
	channelVersion := service.LookupCLIVersionProfile("codex-0.156.1-windows-x64")
	keyVersion := service.LookupCLIVersionProfile("codex-0.160.0-windows-x64")
	require.NotNil(t, channelVersion)
	require.NotNil(t, keyVersion)

	keys := []string{"sk-key-a", "sk-key-b", "sk-key-c", "sk-key-d", "sk-key-e"}
	channel := &model.Channel{
		Id: 14, Type: constant.ChannelTypeOpenAI, Key: strings.Join(keys, "\n"), Status: common.ChannelStatusEnabled,
		Models: "gpt-6-astra,gpt-6.1-sol", Group: "default", BaseURL: &upstream.URL, AutoBan: lo.ToPtr(1),
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true, MultiKeySize: len(keys), MultiKeyMode: constant.MultiKeyModeRandom,
			MultiKeyOriginFingerprint: model.KeyFingerprint("sk-key-a"),
			MultiKeyStatusList:        map[int]int{4: common.ChannelStatusManuallyDisabled},
			MultiKeyEnvs:              map[string]model.MultiKeyEnv{model.KeyFingerprint("sk-key-b"): {CLIVersionProfile: keyVersion.ID}},
		},
	}
	channel.SetSetting(dto.ChannelSettings{
		SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex, CLIVersionProfile: channelVersion.ID,
		ClientDeviceSeed: "device", ClientSessionSeed: "session",
	})
	channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamKeepaliveEnabled: true})
	require.NoError(t, db.Create(channel).Error)
	quiet := common.GetTimestamp() - 40*60
	slots := make(map[string]string, len(keys))
	for _, key := range keys {
		slots[key] = channel.KeyIdentitySlot(key)
		service.SetChannelKeyActivityForTest(channel.Id, slots[key], quiet, 0)
	}
	service.RecordChannelCLISession(channel.Id, slots["sk-key-a"], service.ChannelCLISession{
		ModelName: "gpt-6-astra", UserId: 42, TokenId: 7, Seed: "conversation",
	})
	service.RecordChannelCLISession(channel.Id, slots["sk-key-b"], service.ChannelCLISession{
		ModelName: "gpt-6.1-sol", UserId: 42, TokenId: 7, Seed: "conversation", Reasoning: []byte(`{"effort":"xhigh"}`),
	})
	// sk-key-c is serving a request right now.
	busy, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, model.AcquireChannelSlot(channel.Id, 0))
	service.HoldChannelSlot(busy, channel.Id)
	service.HoldChannelKey(busy, channel.Id, slots["sk-key-c"])
	t.Cleanup(func() { service.ReleaseHeldChannelSlot(busy) })

	summary, err := runChannelKeepalivePass(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 3, summary.Due)
	assert.Equal(t, 2, summary.Succeeded)
	require.Len(t, summary.Failures, 1)
	assert.Equal(t, channel.Id, summary.Failures[0].ChannelId)
	assert.Equal(t, lo.ToPtr(3), summary.Failures[0].KeyIndex, "a multi-key channel's failure names the key")
	mu.Lock()
	captured := make(map[string]capturedKeepaliveRequest, len(sent))
	for key, request := range sent {
		captured[key] = request
	}
	mu.Unlock()
	require.Len(t, captured, 3)
	require.Contains(t, captured, "sk-key-d")
	a, b := captured["sk-key-a"], captured["sk-key-b"]
	require.NotNil(t, a.Header, "sk-key-a is kept alive")
	require.NotNil(t, b.Header, "sk-key-b is kept alive")

	assert.Equal(t, channelVersion.Headers["user-agent"], a.Header.Get("User-Agent"), "a key without a CLI version of its own wears the channel's")
	assert.Equal(t, keyVersion.Headers["user-agent"], b.Header.Get("User-Agent"), "a key wears its own CLI version")
	assert.Equal(t, "gpt-6-astra", gjson.GetBytes(a.Body, "model").String(), "each key continues its own session")
	assert.Equal(t, "gpt-6.1-sol", gjson.GetBytes(b.Body, "model").String())
	assert.Equal(t, "xhigh", gjson.GetBytes(b.Body, "reasoning.effort").String())
	installation := func(request capturedKeepaliveRequest) string {
		return gjson.GetBytes(request.Body, "client_metadata.x-codex-installation-id").String()
	}
	require.NotEmpty(t, a.Header.Get("session-id"))
	require.NotEmpty(t, installation(a))
	assert.NotEqual(t, a.Header.Get("session-id"), b.Header.Get("session-id"), "one conversation on two keys is two sessions")
	assert.NotEqual(t, installation(a), installation(b), "two keys are two installations")

	now := common.GetTimestamp()
	for _, key := range []string{"sk-key-a", "sk-key-b", "sk-key-d"} {
		assert.Positive(t, service.ChannelKeyActivityAt(channel.Id, slots[key], now).LastKeepalive, "%s: its own keepalive is stamped", key)
	}
	for _, key := range []string{"sk-key-c", "sk-key-e"} {
		assert.Zero(t, service.ChannelKeyActivityAt(channel.Id, slots[key], now).LastKeepalive, "%s: another key's keepalive is not its own", key)
	}
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, map[int]int{4: common.ChannelStatusManuallyDisabled}, stored.ChannelInfo.MultiKeyStatusList, "a failed keepalive disables no key")

	again, err := runChannelKeepalivePass(context.Background())
	require.NoError(t, err)
	assert.Zero(t, again.Due, "each key that was sent waits its own interval")
}

// A real relay request through the distributor is what opens the keepalive's
// active window on the channel that served it.
func TestRelayRequestIsChannelActivity(t *testing.T) {
	server, _ := setupRelayHTTPTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, keepaliveChatReply)
	}), 0, 0)
	for _, id := range []int{81, 82} {
		service.ForgetChannelKeysForTest(id)
	}
	t.Cleanup(func() {
		for _, id := range []int{81, 82} {
			service.ForgetChannelKeysForTest(id)
		}
	})
	before := common.GetTimestamp()

	request, err := newRelayHTTPRequest(context.Background(), server.URL, "json", 1)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	// The distributor gives the slot back after the response is written. Wait
	// for it, so the verdict comes from the record and not the in-flight gauge.
	require.Eventually(t, func() bool {
		return model.ChannelInFlight(81) == 0 && model.ChannelInFlight(82) == 0
	}, 5*time.Second, time.Millisecond)

	now := common.GetTimestamp()
	served := 0
	for _, id := range []int{81, 82} {
		if lastActive := service.ChannelKeyActivityAt(id, "0", now).LastActive; lastActive > 0 {
			assert.GreaterOrEqual(t, lastActive, before, "channel %d", id)
			served++
		}
	}
	assert.Equal(t, 1, served, "exactly the channel that served the request is active")
}

// A channel test reaches the upstream with the channel's key, but it is not
// real traffic, so it must not open the keepalive's active window.
func TestChannelTestIsNotChannelActivity(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	channel := f.createChannel(t, dto.ChannelOtherSettings{UpstreamKeepaliveEnabled: true})
	service.ForgetChannelKeysForTest(channel.Id)
	t.Cleanup(func() { service.ForgetChannelKeysForTest(channel.Id) })

	testChannel(context.Background(), channel, 1, "gpt-4o", "", false)

	f.mu.Lock()
	reached := len(f.requestedModel)
	f.mu.Unlock()
	require.Positive(t, reached, "the channel test must reach the upstream for this to mean anything")
	assert.Empty(t, service.ChannelKeyActivities(common.GetTimestamp())[channel.Id], "no key of the channel is recorded as used")
}

// The task row is where an admin sees keepalive failures, so a pass with a
// failing channel is recorded as failed and names the channel, while the
// channel itself is left enabled.
func TestChannelKeepaliveHandlerRecordsFailures(t *testing.T) {
	db := setupChannelKeepaliveTest(t)
	upstream, _ := newKeepaliveUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	})
	channel := &model.Channel{
		Id: 9, Type: constant.ChannelTypeOpenAI, Key: "sk-revoked", Status: common.ChannelStatusEnabled,
		Models: "gpt-4o", Group: "default", BaseURL: &upstream.URL,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamKeepaliveEnabled: true})
	require.NoError(t, db.Create(channel).Error)
	require.False(t, channelKeepaliveHandler{}.Enabled(), "no real traffic seen, nothing to keep alive")
	service.SetChannelKeyActivityForTest(channel.Id, "0", common.GetTimestamp()-40*60, 0)
	require.True(t, channelKeepaliveHandler{}.Enabled())

	const runnerID = "keepalive-runner"
	now := common.GetTimestamp()
	task := &model.SystemTask{
		TaskID: "systask_keepalive", Type: model.SystemTaskTypeChannelKeepalive, Status: model.SystemTaskStatusRunning,
		LockedBy: runnerID, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, db.Create(&model.SystemTaskLock{
		Type: model.SystemTaskTypeChannelKeepalive, TaskID: task.TaskID, LockedBy: runnerID, LockedUntil: now + 600, UpdatedAt: now,
	}).Error)

	channelKeepaliveHandler{}.Run(context.Background(), task, runnerID)

	var persisted model.SystemTask
	require.NoError(t, db.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.SystemTaskStatusFailed, persisted.Status)
	assert.Contains(t, persisted.Error, "#9")
	assert.Equal(t, int64(1), gjson.Get(persisted.Result, "failed").Int())
	assert.Equal(t, int64(9), gjson.Get(persisted.Result, "failures.0.channel_id").Int())
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
}
