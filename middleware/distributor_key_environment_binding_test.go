package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const keyEnvironmentBindingChannel = 1601

type keyEnvironmentBindingFixture struct {
	db      *gorm.DB
	model   string
	channel *model.Channel
}

// setupKeyEnvironmentBindingFixture serves a fresh model from one independent
// key environment channel with keys sk-a and sk-b, through the memory channel
// cache and the in-memory binding store. Each run gets its own model name
// because cooldowns are process-wide.
func setupKeyEnvironmentBindingFixture(t *testing.T, maxConcurrency int) *keyEnvironmentBindingFixture {
	t.Helper()
	db := setupDistributorTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Ability{}))
	fixture := &keyEnvironmentBindingFixture{db: db, model: fmt.Sprintf("gpt-kenv-%d", time.Now().UnixNano())}

	originalMemoryCache, originalRedis := common.MemoryCacheEnabled, common.RedisEnabled
	affinity := operation_setting.GetChannelAffinitySetting()
	originalAffinity := *affinity
	t.Cleanup(func() {
		*affinity = originalAffinity
		common.MemoryCacheEnabled, common.RedisEnabled = originalMemoryCache, originalRedis
		service.ResetKeyEnvironmentBindingCacheForTest()
		model.InitChannelCache()
		for model.ChannelInFlight(keyEnvironmentBindingChannel) > 0 {
			model.ReleaseChannelSlot(keyEnvironmentBindingChannel)
		}
	})
	common.MemoryCacheEnabled, common.RedisEnabled = true, false
	*affinity = operation_setting.ChannelAffinitySetting{MaxEntries: 100, KeyEnvironmentBindingTTLSeconds: 3600}
	service.ResetKeyEnvironmentBindingCacheForTest()

	fixture.channel = createKeyEnvironmentChannel(t, db, keyEnvironmentBindingChannel, "sk-a\nsk-b", map[string]string{
		"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64",
	})
	updates := map[string]any{"models": fixture.model, "group": "default"}
	if maxConcurrency > 0 {
		updates["max_concurrency"] = maxConcurrency
	}
	require.NoError(t, db.Model(fixture.channel).Updates(updates).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: fixture.model, ChannelId: keyEnvironmentBindingChannel, Enabled: true}).Error)
	model.InitChannelCache()
	return fixture
}

// distribute runs one request of tokenId's conversation through Distribute and
// returns the key it was served from ("" when refused).
func (f *keyEnvironmentBindingFixture) distribute(t *testing.T, tokenId int) (string, *httptest.ResponseRecorder) {
	t.Helper()
	return f.distributeModel(t, tokenId, f.model)
}

func (f *keyEnvironmentBindingFixture) distributeModel(t *testing.T, tokenId int, modelName string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := newJSONTestContext(t, http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"input":"hi"}`, modelName))
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenId, tokenId)
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	Distribute()(c)
	service.ReleaseHeldChannelSlot(c)
	if c.IsAborted() {
		return "", w
	}
	return common.GetContextKeyString(c, constant.ContextKeyChannelKey), w
}

func (f *keyEnvironmentBindingFixture) binding(t *testing.T, tokenId int) (service.KeyEnvironmentBinding, bool) {
	t.Helper()
	binding, found, err := service.GetKeyEnvironmentBinding(fmt.Sprintf("kenv:token:%d:default", tokenId))
	require.NoError(t, err)
	return binding, found
}

func TestKeyEnvironmentConversationStaysOnItsKey(t *testing.T) {
	f := setupKeyEnvironmentBindingFixture(t, 0)

	first, w := f.distribute(t, 71)
	require.NotEmpty(t, first, w.Body.String())
	binding, found := f.binding(t, 71)
	require.True(t, found, "the first request binds the conversation")
	assert.Equal(t, service.KeyEnvironmentBinding{ChannelId: keyEnvironmentBindingChannel, KeyTag: model.ChannelKeyTag(first)}, binding)
	for round := 0; round < 5; round++ {
		key, _ := f.distribute(t, 71)
		assert.Equal(t, first, key, "polling never rotates a bound conversation (round %d)", round)
	}
}

func TestKeyEnvironmentSoftSignalsWaitInsteadOfSwitching(t *testing.T) {
	t.Run("a cooling key answers 429 with its own window", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 0)
		key, _ := f.distribute(t, 72)
		require.NotEmpty(t, key)
		before, _ := f.binding(t, 72)
		keys := f.channel.GetKeys()
		index := 0
		if keys[1] == key {
			index = 1
		}
		model.MarkChannelKeyCooldown(keyEnvironmentBindingChannel, f.model, index, key, 30*time.Second, time.Now())

		served, w := f.distribute(t, 72)
		assert.Empty(t, served, "the other key is never tried")
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Contains(t, []string{"30", "29"}, w.Header().Get("Retry-After"))
		after, _ := f.binding(t, 72)
		assert.Equal(t, before, after)
	})
	t.Run("a cooling channel answers 429", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 0)
		key, _ := f.distribute(t, 73)
		require.NotEmpty(t, key)
		model.MarkChannelCooldown(keyEnvironmentBindingChannel, f.model, time.Now().Add(time.Minute))

		served, w := f.distribute(t, 73)
		assert.Empty(t, served)
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, "5", w.Header().Get("Retry-After"))
	})
	t.Run("a full channel answers 503 and keeps the binding", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 1)
		key, _ := f.distribute(t, 74)
		require.NotEmpty(t, key)
		before, _ := f.binding(t, 74)
		require.True(t, model.AcquireChannelSlot(keyEnvironmentBindingChannel, 1))

		served, w := f.distribute(t, 74)
		assert.Empty(t, served)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "5", w.Header().Get("Retry-After"))
		after, _ := f.binding(t, 74)
		assert.Equal(t, before, after)
	})
}

func TestKeyEnvironmentHardSignalMigratesOnce(t *testing.T) {
	f := setupKeyEnvironmentBindingFixture(t, 0)
	original, _ := f.distribute(t, 75)
	require.NotEmpty(t, original)
	other := "sk-a"
	if original == "sk-a" {
		other = "sk-b"
	}

	require.True(t, model.UpdateChannelStatus(keyEnvironmentBindingChannel, original, common.ChannelStatusAutoDisabled, "quota exhausted"))
	moved, w := f.distribute(t, 75)
	require.Equal(t, other, moved, w.Body.String())
	binding, _ := f.binding(t, 75)
	assert.Equal(t, model.ChannelKeyTag(other), binding.KeyTag, "the migration is stored")

	require.True(t, model.UpdateChannelStatus(keyEnvironmentBindingChannel, original, common.ChannelStatusEnabled, ""))
	for round := 0; round < 3; round++ {
		key, _ := f.distribute(t, 75)
		assert.Equal(t, other, key, "re-enabling the old key does not pull the conversation back (round %d)", round)
	}
}

func TestKeyEnvironmentBindingStoreFailsClosed(t *testing.T) {
	f := setupKeyEnvironmentBindingFixture(t, 0)
	server := withMiniRedis(t)
	service.ResetKeyEnvironmentBindingCacheForTest()
	server.Close()

	served, w := f.distribute(t, 76)
	assert.Empty(t, served, "an unreadable binding is not treated as no binding")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "5", w.Header().Get("Retry-After"))
}

// A key added to the channel takes new conversations only; every conversation
// bound before stays on its key, even those a fresh placement would now put on
// the new key.
func TestKeyEnvironmentAddedKeyLeavesBoundConversationsAlone(t *testing.T) {
	f := setupKeyEnvironmentBindingFixture(t, 0)
	bound := map[int]string{}
	for tokenId := 200; tokenId < 216; tokenId++ {
		key, w := f.distribute(t, tokenId)
		require.NotEmpty(t, key, w.Body.String())
		bound[tokenId] = key
	}

	require.NoError(t, f.db.Model(&model.Channel{}).Where("id = ?", keyEnvironmentBindingChannel).Update("key", "sk-a\nsk-b\nsk-c").Error)
	require.NoError(t, f.db.Create(&model.ChannelKeyEnvironment{
		ChannelId: keyEnvironmentBindingChannel, KeyTag: model.ChannelKeyTag("sk-c"), ProfileId: common.GetPointer("codex-0.156.1-windows-x64"),
	}).Error)
	model.InitChannelCache()
	grown, err := model.CacheGetChannel(keyEnvironmentBindingChannel)
	require.NoError(t, err)
	movedByPlacement := 0
	for tokenId := range bound {
		if key, _, _ := grown.GetEnabledKeyForSeed(fmt.Sprintf("kenv:token:%d:default", tokenId), f.model); key == "sk-c" {
			movedByPlacement++
		}
	}
	require.Positive(t, movedByPlacement, "some bound conversations would land on the new key if placed again")

	for tokenId, key := range bound {
		served, _ := f.distribute(t, tokenId)
		assert.Equal(t, key, served, "token %d keeps its key after a key is added", tokenId)
	}
	newKeyServed := false
	for tokenId := 300; tokenId < 316 && !newKeyServed; tokenId++ {
		served, _ := f.distribute(t, tokenId)
		newKeyServed = served == "sk-c"
	}
	assert.True(t, newKeyServed, "new conversations are placed on the added key")
}

// A caller with no session id is one conversation per token: under the default
// token fallback rule, which pins channels per model, switching models keeps
// the key.
func TestKeyEnvironmentModelSwitchKeepsTheKey(t *testing.T) {
	f := setupKeyEnvironmentBindingFixture(t, 0)
	models := []string{"gpt-kenv-switch-a", "gpt-kenv-switch-b"}
	require.NoError(t, f.db.Model(&model.Channel{}).Where("id = ?", keyEnvironmentBindingChannel).Update("models", models[0]+","+models[1]).Error)
	for _, name := range models {
		require.NoError(t, f.db.Create(&model.Ability{Group: "default", Model: name, ChannelId: keyEnvironmentBindingChannel, Enabled: true}).Error)
	}
	model.InitChannelCache()
	affinity := operation_setting.GetChannelAffinitySetting()
	*affinity = operation_setting.ChannelAffinitySetting{
		Enabled: true, StickyMultiKey: true, MaxEntries: 100, DefaultTTLSeconds: 3600, KeyEnvironmentBindingTTLSeconds: 3600,
		Rules: []operation_setting.ChannelAffinityRule{{
			Name: "token fallback", ModelRegex: []string{".*"}, PathRegex: []string{"/v1/responses"},
			KeySources:        []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}},
			IncludeUsingGroup: true, IncludeModelName: true, IncludeRuleName: true,
		}},
	}
	service.ClearChannelAffinityCacheAll()
	t.Cleanup(func() { service.ClearChannelAffinityCacheAll() })

	for tokenId := 400; tokenId < 416; tokenId++ {
		first, w := f.distributeModel(t, tokenId, models[0])
		require.NotEmpty(t, first, w.Body.String())
		for round := 0; round < 3; round++ {
			for _, name := range models {
				served, _ := f.distributeModel(t, tokenId, name)
				assert.Equal(t, first, served, "token %d keeps its key on %s (round %d)", tokenId, name, round)
			}
		}
	}
}

// startFirstRequest takes tokenId's conversation through selection on
// channelId the way a first request does, holding the channel's slot, up to
// the point where it claims the binding.
func (f *keyEnvironmentBindingFixture) startFirstRequest(t *testing.T, tokenId int, channelId int) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := newJSONTestContext(t, http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"input":"hi"}`, f.model))
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenId, tokenId)
	service.SetKeyEnvironmentBindingState(c, &service.KeyEnvironmentBindingState{Key: service.KeyEnvironmentBindingKey(c, "default")})
	channel, err := model.CacheGetChannel(channelId)
	require.NoError(t, err)
	require.True(t, model.AcquireChannelSlot(channelId, 0))
	service.HoldChannelSlot(c, channelId)
	require.Nil(t, SetupContextForSelectedChannel(c, channel, f.model))
	return c, w
}

// addKeyEnvironmentChannel adds a second independent key environment channel
// serving the fixture's model, with its own keys.
func (f *keyEnvironmentBindingFixture) addKeyEnvironmentChannel(t *testing.T, id int, assignments map[string]string, keys string) {
	t.Helper()
	channel := createKeyEnvironmentChannel(t, f.db, id, keys, assignments)
	require.NoError(t, f.db.Model(channel).Updates(map[string]any{"models": f.model, "group": "default"}).Error)
	require.NoError(t, f.db.Create(&model.Ability{Group: "default", Model: f.model, ChannelId: id, Enabled: true}).Error)
	model.InitChannelCache()
	t.Cleanup(func() {
		for model.ChannelInFlight(id) > 0 {
			model.ReleaseChannelSlot(id)
		}
	})
}

// Two first requests of one conversation race for its binding. The loser
// follows the winner instead of serving the conversation from a second key.
func TestKeyEnvironmentConcurrentFirstRequestFollowsTheWinner(t *testing.T) {
	const otherChannel = 1602
	t.Run("winner on another key of the same channel", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 0)
		c, _ := f.startFirstRequest(t, 81, keyEnvironmentBindingChannel)
		selected := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
		winnerKey := map[string]string{"sk-a": "sk-b", "sk-b": "sk-a"}[selected]
		winner := service.KeyEnvironmentBinding{ChannelId: keyEnvironmentBindingChannel, KeyTag: model.ChannelKeyTag(winnerKey)}
		claimed, err := service.ClaimKeyEnvironmentBinding("kenv:token:81:default", nil, winner)
		require.NoError(t, err)
		require.True(t, claimed)

		served, apiErr := BindKeyEnvironment(c, f.channel, f.model)
		require.Nil(t, apiErr)
		assert.Equal(t, keyEnvironmentBindingChannel, served.Id)
		assert.Equal(t, winnerKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), "the loser takes the winner's key")
		assert.Equal(t, winner.KeyTag, common.GetContextKeyString(c, constant.ContextKeyChannelKeyTag))
		assert.Equal(t, 1, model.ChannelInFlight(keyEnvironmentBindingChannel), "the slot taken at selection is the one used")
		assert.Equal(t, &winner, service.GetKeyEnvironmentBindingState(c).Bound)
		stored, _ := f.binding(t, 81)
		assert.Equal(t, winner, stored, "the winner's binding is not overwritten")
	})
	t.Run("winner on another channel", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 0)
		f.addKeyEnvironmentChannel(t, otherChannel, map[string]string{
			"sk-x": "codex-0.163.0-linux-x64", "sk-y": "codex-0.160.0-windows-x64",
		}, "sk-x\nsk-y")
		c, _ := f.startFirstRequest(t, 82, keyEnvironmentBindingChannel)
		winner := service.KeyEnvironmentBinding{ChannelId: otherChannel, KeyTag: model.ChannelKeyTag("sk-y")}
		claimed, err := service.ClaimKeyEnvironmentBinding("kenv:token:82:default", nil, winner)
		require.NoError(t, err)
		require.True(t, claimed)

		served, apiErr := BindKeyEnvironment(c, f.channel, f.model)
		require.Nil(t, apiErr)
		require.NotNil(t, served)
		assert.Equal(t, otherChannel, served.Id, "the loser moves to the winner's channel")
		assert.Equal(t, "sk-y", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
		assert.Equal(t, otherChannel, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
		setting, _ := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
		assert.Equal(t, "codex-0.160.0-windows-x64", setting.CLIVersionProfile, "and wears the winner key's version")
		assert.Zero(t, model.ChannelInFlight(keyEnvironmentBindingChannel), "the candidate slot is given back")
		assert.Equal(t, 1, model.ChannelInFlight(otherChannel), "the winner's channel slot is taken instead")
		service.ReleaseHeldChannelSlot(c)
		assert.Zero(t, model.ChannelInFlight(otherChannel), "the request ends holding exactly that slot")
	})
	t.Run("winner on a cooling key answers 429 without switching", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 0)
		f.addKeyEnvironmentChannel(t, otherChannel, map[string]string{
			"sk-x": "codex-0.163.0-linux-x64", "sk-y": "codex-0.160.0-windows-x64",
		}, "sk-x\nsk-y")
		model.MarkChannelKeyCooldown(otherChannel, f.model, 1, "sk-y", 30*time.Second, time.Now())
		c, w := f.startFirstRequest(t, 83, keyEnvironmentBindingChannel)
		selected := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
		winner := service.KeyEnvironmentBinding{ChannelId: otherChannel, KeyTag: model.ChannelKeyTag("sk-y")}
		claimed, err := service.ClaimKeyEnvironmentBinding("kenv:token:83:default", nil, winner)
		require.NoError(t, err)
		require.True(t, claimed)

		served, apiErr := BindKeyEnvironment(c, f.channel, f.model)
		require.NotNil(t, apiErr)
		assert.Nil(t, served)
		assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
		assert.Contains(t, []string{"30", "29"}, w.Header().Get("Retry-After"))
		assert.Equal(t, selected, common.GetContextKeyString(c, constant.ContextKeyChannelKey), "no other key is tried")
		assert.Zero(t, model.ChannelInFlight(otherChannel))
		stored, _ := f.binding(t, 83)
		assert.Equal(t, winner, stored)
	})
	t.Run("winner whose channel cannot serve the request", func(t *testing.T) {
		f := setupKeyEnvironmentBindingFixture(t, 0)
		c, _ := f.startFirstRequest(t, 84, keyEnvironmentBindingChannel)
		selected := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
		winner := service.KeyEnvironmentBinding{ChannelId: otherChannel, KeyTag: model.ChannelKeyTag("sk-gone")}
		claimed, err := service.ClaimKeyEnvironmentBinding("kenv:token:84:default", nil, winner)
		require.NoError(t, err)
		require.True(t, claimed)

		served, apiErr := BindKeyEnvironment(c, f.channel, f.model)
		require.Nil(t, apiErr)
		assert.Equal(t, keyEnvironmentBindingChannel, served.Id, "served once from the channel it selected")
		assert.Equal(t, selected, common.GetContextKeyString(c, constant.ContextKeyChannelKey))
		assert.Nil(t, service.GetKeyEnvironmentBindingState(c).Bound, "unbound, so nothing renews or overwrites the winner")
		assert.Equal(t, 1, model.ChannelInFlight(keyEnvironmentBindingChannel))
		stored, _ := f.binding(t, 84)
		assert.Equal(t, winner, stored)
	})
}
