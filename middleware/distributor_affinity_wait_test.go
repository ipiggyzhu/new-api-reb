package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	affinityWaitPinned   = 1501
	affinityWaitFallback = 1502
)

type affinityWaitFixture struct {
	model string
	// withFallback is false when the pinned channel is the only one serving
	// the model.
	withFallback bool
}

// setupAffinityWaitFixture creates a pinned channel with one slot (which the
// fixture occupies) and, optionally, an unlimited fallback, then pins the
// "wait-session" conversation to the former. Each run gets its own model name
// because channel cooldowns are process-wide.
func setupAffinityWaitFixture(t *testing.T, withFallback bool, waitSeconds int, maxWaiters int) *affinityWaitFixture {
	t.Helper()
	fixture := &affinityWaitFixture{
		model:        fmt.Sprintf("claude-wait-%d", time.Now().UnixNano()),
		withFallback: withFallback,
	}
	db := setupDistributorTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Ability{}))

	originalMemoryCache, originalRedis := common.MemoryCacheEnabled, common.RedisEnabled
	affinity := operation_setting.GetChannelAffinitySetting()
	originalAffinity := *affinity
	general := operation_setting.GetGeneralSetting()
	originalPingEnabled, originalPingSeconds := general.PingIntervalEnabled, general.PingIntervalSeconds
	t.Cleanup(func() {
		service.ClearChannelAffinityCacheAll()
		*affinity = originalAffinity
		general.PingIntervalEnabled, general.PingIntervalSeconds = originalPingEnabled, originalPingSeconds
		common.MemoryCacheEnabled, common.RedisEnabled = originalMemoryCache, originalRedis
		model.InitChannelCache()
	})
	common.MemoryCacheEnabled, common.RedisEnabled = true, false
	general.PingIntervalEnabled = false
	*affinity = operation_setting.ChannelAffinitySetting{
		Enabled: true, MaxEntries: 100, DefaultTTLSeconds: 120,
		SaturatedWaitSeconds: waitSeconds, SaturatedMaxWaiters: maxWaiters,
		Rules: []operation_setting.ChannelAffinityRule{{
			Name: "wait", ModelRegex: []string{"^claude-wait-"}, PathRegex: []string{"^/v1/messages$"},
			KeySources:        []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Wait-Session"}},
			IncludeUsingGroup: true, IncludeModelName: true, IncludeRuleName: true,
		}},
	}
	service.ClearChannelAffinityCacheAll()

	channels := []model.Channel{{
		Id: affinityWaitPinned, Type: constant.ChannelTypeAnthropic, Name: "pinned", Key: "sk-pinned",
		Status: common.ChannelStatusEnabled, Models: fixture.model, Group: "default",
		MaxConcurrency: common.GetPointer(1),
	}}
	if withFallback {
		channels = append(channels, model.Channel{
			Id: affinityWaitFallback, Type: constant.ChannelTypeAnthropic, Name: "fallback", Key: "sk-fallback",
			Status: common.ChannelStatusEnabled, Models: fixture.model, Group: "default",
		})
	}
	for i := range channels {
		require.NoError(t, db.Create(&channels[i]).Error)
	}
	model.InitChannelCache()

	seed, _ := fixture.newContext(t, "")
	_, found := service.GetPreferredChannelByAffinity(seed, fixture.model, "default")
	require.False(t, found)
	service.SetChannelAffinityRelayOutcome(seed, true)
	service.RecordChannelAffinity(seed, affinityWaitPinned)

	require.True(t, model.AcquireChannelSlot(affinityWaitPinned, 1), "the fixture holds the pinned channel's only slot")
	t.Cleanup(func() {
		for model.ChannelInFlight(affinityWaitPinned) > 0 {
			model.ReleaseChannelSlot(affinityWaitPinned)
		}
		for model.ChannelInFlight(affinityWaitFallback) > 0 {
			model.ReleaseChannelSlot(affinityWaitFallback)
		}
	})
	return fixture
}

func (f *affinityWaitFixture) newContext(t *testing.T, extraBody string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := newJSONTestContext(t, http.MethodPost, "/v1/messages",
		fmt.Sprintf(`{"model":%q,"messages":[]%s}`, f.model, extraBody))
	c.Request.Header.Set("X-Wait-Session", "wait-session")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	return c, w
}

// distributeAsync runs Distribute in the background; the returned channel is
// closed when it returns.
func distributeAsync(c *gin.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Distribute()(c)
	}()
	return done
}

func waitForDistribute(t *testing.T, done <-chan struct{}, within time.Duration, why string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		require.FailNow(t, why)
	}
}

// A pinned channel that is merely full keeps its conversation: the request
// waits, takes the slot the moment it frees up, and the fallback is untouched.
func TestAffinityWaitKeepsThePinnedChannelOnceASlotFrees(t *testing.T) {
	fixture := setupAffinityWaitFixture(t, true, 30, 3)
	c, _ := fixture.newContext(t, "")

	done := distributeAsync(c)
	require.Eventually(t, func() bool { return model.ChannelSlotWaiters(affinityWaitPinned) == 1 },
		5*time.Second, time.Millisecond, "the request must park on the pinned channel")
	model.ReleaseChannelSlot(affinityWaitPinned)
	waitForDistribute(t, done, 5*time.Second, "a freed slot must end the wait")

	require.False(t, c.IsAborted())
	assert.Equal(t, affinityWaitPinned, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	assert.Equal(t, 0, model.ChannelSlotWaiters(affinityWaitPinned), "the waiter must be unregistered")
	assert.Equal(t, 0, model.ChannelInFlight(affinityWaitPinned), "Distribute returned, so its slot is released")
	assert.Equal(t, 0, model.ChannelInFlight(affinityWaitFallback))
}

// Waiting only happens when it is enabled, there is waiter room, and the
// pinned channel is not cooling down; otherwise the request falls back at once
// exactly as before. The configured wait (30s) is far longer than the guard, so
// a request that waited anyway fails the test.
func TestAffinityWaitFallsBackImmediatelyWhenItMustNotWait(t *testing.T) {
	cases := []struct {
		name        string
		waitSeconds int
		maxWaiters  int
		prepare     func(fixture *affinityWaitFixture)
	}{
		{name: "wait disabled", waitSeconds: 0, maxWaiters: 3},
		{name: "no waiter room configured", waitSeconds: 30, maxWaiters: 0},
		{
			name: "waiter cap reached", waitSeconds: 30, maxWaiters: 1,
			prepare: func(*affinityWaitFixture) {
				require.True(t, model.EnterChannelSlotWait(affinityWaitPinned, 1))
				t.Cleanup(func() { model.LeaveChannelSlotWait(affinityWaitPinned) })
			},
		},
		{
			name: "pinned channel cooling down", waitSeconds: 30, maxWaiters: 3,
			prepare: func(fixture *affinityWaitFixture) {
				model.MarkChannelCooldown(affinityWaitPinned, fixture.model, time.Now().Add(time.Minute))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupAffinityWaitFixture(t, true, tc.waitSeconds, tc.maxWaiters)
			if tc.prepare != nil {
				tc.prepare(fixture)
			}
			waitersBefore := model.ChannelSlotWaiters(affinityWaitPinned)
			c, _ := fixture.newContext(t, "")

			waitForDistribute(t, distributeAsync(c), 3*time.Second, "the request must not wait")

			require.False(t, c.IsAborted())
			assert.Equal(t, affinityWaitFallback, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
			assert.Equal(t, waitersBefore, model.ChannelSlotWaiters(affinityWaitPinned))
		})
	}
}

// Ordinary selection never waits: a conversation without an affinity record
// passes over the full channel at once, exactly as before, even with waiting
// configured.
func TestNonAffinitySelectionDoesNotWaitForAFullChannel(t *testing.T) {
	fixture := setupAffinityWaitFixture(t, true, 30, 3)
	c, _ := fixture.newContext(t, "")
	c.Request.Header.Set("X-Wait-Session", "another-session")

	waitForDistribute(t, distributeAsync(c), 3*time.Second, "a request without affinity must not wait")

	require.False(t, c.IsAborted())
	assert.Equal(t, affinityWaitFallback, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	assert.Equal(t, 0, model.ChannelSlotWaiters(affinityWaitPinned))
	assert.Equal(t, 1, model.ChannelInFlight(affinityWaitPinned), "only the fixture's slot remains")
}

// The wait is bounded: a slot that never frees sends the request to the
// fallback once SaturatedWaitSeconds runs out, and a 429 that cools the pinned
// channel mid-wait ends it early.
func TestAffinityWaitEndsInFallback(t *testing.T) {
	cases := []struct {
		name        string
		waitSeconds int
		during      func(fixture *affinityWaitFixture)
		within      time.Duration
	}{
		{name: "wait runs out", waitSeconds: 1, within: 5 * time.Second},
		{
			name: "pinned channel starts cooling down", waitSeconds: 30, within: 3 * time.Second,
			during: func(fixture *affinityWaitFixture) {
				model.MarkChannelCooldown(affinityWaitPinned, fixture.model, time.Now().Add(time.Minute))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupAffinityWaitFixture(t, true, tc.waitSeconds, 3)
			c, _ := fixture.newContext(t, "")

			done := distributeAsync(c)
			require.Eventually(t, func() bool { return model.ChannelSlotWaiters(affinityWaitPinned) == 1 },
				5*time.Second, time.Millisecond)
			if tc.during != nil {
				tc.during(fixture)
			}
			waitForDistribute(t, done, tc.within, "the wait must end")

			require.False(t, c.IsAborted())
			assert.Equal(t, affinityWaitFallback, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
			assert.Equal(t, 0, model.ChannelSlotWaiters(affinityWaitPinned))
			assert.Equal(t, 1, model.ChannelInFlight(affinityWaitPinned), "only the fixture's slot remains")
		})
	}
}

// A client that disconnects while waiting ends the request right there: no
// fallback channel is picked for no one, and neither the waiter count nor a
// slot is left behind.
func TestAffinityWaitAbortsWhenTheClientGoesAway(t *testing.T) {
	fixture := setupAffinityWaitFixture(t, true, 30, 3)
	c, _ := fixture.newContext(t, "")
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)

	done := distributeAsync(c)
	require.Eventually(t, func() bool { return model.ChannelSlotWaiters(affinityWaitPinned) == 1 },
		5*time.Second, time.Millisecond)
	cancel()
	waitForDistribute(t, done, 3*time.Second, "a cancelled request must stop waiting")

	assert.True(t, c.IsAborted())
	_, picked := common.GetContextKey(c, constant.ContextKeyChannelId)
	assert.False(t, picked, "no channel is set up for a request nobody is waiting on")
	assert.Equal(t, 0, model.ChannelSlotWaiters(affinityWaitPinned))
	assert.Equal(t, 1, model.ChannelInFlight(affinityWaitPinned), "only the fixture's slot remains")
	assert.Equal(t, 0, model.ChannelInFlight(affinityWaitFallback))
}

// With PingIntervalEnabled a streaming request is kept alive with the relay's
// ping frame while it waits, and the frames are counted for the retry guard;
// a non-streaming request gets nothing, since a ping would commit a 200
// event stream it never asked for.
func TestAffinityWaitPingsOnlyStreamingRequests(t *testing.T) {
	cases := []struct {
		name      string
		extraBody string
		wantPings bool
	}{
		{name: "streaming", extraBody: `,"stream":true`, wantPings: true},
		{name: "non-streaming", extraBody: "", wantPings: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupAffinityWaitFixture(t, true, 2, 3)
			general := operation_setting.GetGeneralSetting()
			general.PingIntervalEnabled, general.PingIntervalSeconds = true, 1
			c, w := fixture.newContext(t, tc.extraBody)

			waitForDistribute(t, distributeAsync(c), 5*time.Second, "the wait must run out")

			assert.Equal(t, affinityWaitFallback, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
			pings := common.GetContextKeyInt(c, constant.ContextKeyAffinityWaitPings)
			if !tc.wantPings {
				assert.Zero(t, pings)
				assert.False(t, c.Writer.Written(), "nothing may reach a non-streaming client")
				return
			}
			require.Positive(t, pings)
			assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
			assert.Equal(t, strings.Repeat(": PING\n\n", pings), w.Body.String())
		})
	}
}

// Once wait pings have committed a 200 event stream, a selection failure has
// to arrive as an SSE error event; a JSON body appended to the stream is a
// line every SSE parser drops, leaving the client with an empty stream.
func TestAffinityWaitFailureAfterPingsIsAnSSEErrorEvent(t *testing.T) {
	require.NoError(t, i18n.Init())
	fixture := setupAffinityWaitFixture(t, false, 2, 3)
	general := operation_setting.GetGeneralSetting()
	general.PingIntervalEnabled, general.PingIntervalSeconds = true, 1
	c, w := fixture.newContext(t, `,"stream":true`)

	waitForDistribute(t, distributeAsync(c), 5*time.Second, "the wait must run out")

	require.True(t, c.IsAborted(), "the only channel is full, so selection fails")
	require.Positive(t, common.GetContextKeyInt(c, constant.ContextKeyAffinityWaitPings))
	body := w.Body.String()
	errorAt := strings.Index(body, "event: error\ndata: ")
	require.GreaterOrEqual(t, errorAt, 0, "the failure must be an SSE error event: %q", body)
	assert.Equal(t, strings.Repeat(": PING\n\n", strings.Count(body, ": PING\n\n")), body[:errorAt],
		"only pings precede the error event")
	assert.Contains(t, body[errorAt:], `"type":"error"`)
	assert.Contains(t, body[errorAt:], string(types.ErrorCodeChannelsSaturated))
}
