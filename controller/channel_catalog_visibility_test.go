package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keep a nonempty pricing cache so a missing invalidation cannot pass because
// GetPricing automatically rebuilds an empty cache.
func setupChannelCatalogVisibilityTest(t *testing.T, memoryCache bool) *upstreamValidationFixture {
	t.Helper()
	f := newUpstreamValidationFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.Model{}, &model.Vendor{}))
	common.MemoryCacheEnabled = memoryCache
	t.Cleanup(func() {
		require.NoError(t, f.db.Where("1 = 1").Delete(&model.Ability{}).Error)
		require.NoError(t, f.db.Where("1 = 1").Delete(&model.Channel{}).Error)
		model.InitChannelCache()
	})
	baseline := f.createChannel(t, dto.ChannelOtherSettings{})
	baseline.Models = "gpt-4o-mini"
	require.NoError(t, f.db.Save(baseline).Error)
	require.NoError(t, baseline.AddAbilities(nil))
	model.InitChannelCache()
	require.Contains(t, pricingByModelName(model.GetPricing()), "gpt-4o-mini")
	return f
}

func TestAddChannelMakesModelImmediatelyVisible(t *testing.T) {
	for _, tc := range []struct {
		name        string
		memoryCache bool
	}{
		{"database routing", false},
		{"memory routing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupChannelCatalogVisibilityTest(t, tc.memoryCache)
			body, err := common.Marshal(AddChannelRequest{
				Mode: "single",
				Channel: &model.Channel{
					Name: "new-catalog-channel", Type: constant.ChannelTypeOpenAI,
					Key: "sk-catalog-test", Status: common.ChannelStatusEnabled,
					Models: "gpt-4o", Group: "default",
				},
			})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("id", 1)
			c.Set("role", common.RoleRootUser)
			AddChannel(c)

			require.Equal(t, http.StatusOK, recorder.Code)
			var response struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success, response.Message)
			var stored model.Channel
			require.NoError(t, f.db.Where("name = ?", "new-catalog-channel").First(&stored).Error)
			assert.Equal(t, "gpt-4o", stored.Models)
			assert.Contains(t, model.GetEnabledModels(), "gpt-4o")
			assert.Contains(t, pricingByModelName(model.GetPricing()), "gpt-4o", "the model plaza must refresh immediately after channel creation")
			picked, err := model.GetRandomSatisfiedChannel("default", "gpt-4o", 0, "", nil)
			require.NoError(t, err)
			require.NotNil(t, picked, "the newly created channel must be selectable without waiting for periodic cache sync")
			defer model.ReleaseChannelSlot(picked.Id)
			assert.Equal(t, stored.Id, picked.Id)
			cached, err := model.CacheGetChannel(stored.Id)
			require.NoError(t, err)
			assert.Equal(t, "gpt-4o", cached.Models)
		})
	}
}

func TestUpdateChannelReaddedModelImmediatelyVisibleAfterSweep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		memoryCache bool
	}{
		{"database routing", false},
		{"memory routing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupChannelCatalogVisibilityTest(t, tc.memoryCache)
			channel := &model.Channel{
				Name: "removed-catalog-channel", Type: constant.ChannelTypeOpenAI,
				Key: "sk-catalog-test", Status: common.ChannelStatusEnabled,
				Models: "gpt-4o", Group: "default",
			}
			require.NoError(t, channel.Insert())
			model.InitChannelCache()
			require.Contains(t, pricingByModelName(model.GetPricing()), "gpt-4o")

			applied, err := model.ApplyChannelModelSweep(channel, "", nil, false)
			require.NoError(t, err)
			require.True(t, applied)
			refreshChannelRuntimeCache()
			require.NotContains(t, model.GetEnabledModels(), "gpt-4o")
			require.NotContains(t, pricingByModelName(model.GetPricing()), "gpt-4o")
			picked, err := model.GetRandomSatisfiedChannel("default", "gpt-4o", 0, "", nil)
			require.NoError(t, err)
			require.Nil(t, picked)

			body, err := common.Marshal(map[string]any{"id": channel.Id, "models": "gpt-4o"})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/channel/", bytes.NewReader(body))
			c.Set("id", 1)
			c.Set("role", common.RoleRootUser)
			UpdateChannel(c)

			require.Equal(t, http.StatusOK, recorder.Code)
			var response struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success, response.Message)
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, "gpt-4o", stored.Models)
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			assert.NotContains(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth, "gpt-4o", "manual re-add must clear stale failure history")
			assert.Contains(t, model.GetEnabledModels(), "gpt-4o")
			assert.Contains(t, pricingByModelName(model.GetPricing()), "gpt-4o")
			picked, err = model.GetRandomSatisfiedChannel("default", "gpt-4o", 0, "", nil)
			require.NoError(t, err)
			require.NotNil(t, picked)
			defer model.ReleaseChannelSlot(picked.Id)
			assert.Equal(t, channel.Id, picked.Id)
			cached, err := model.CacheGetChannel(channel.Id)
			require.NoError(t, err)
			assert.Equal(t, "gpt-4o", cached.Models)
			assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
		})
	}
}

func TestUpdateChannelManualModelEditClearsStaleStateWithoutProbes(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	channel := f.createChannel(t, dto.ChannelOtherSettings{})
	channel.Models = "gpt-4o"
	channel.OtherSettings = `{"model_availability":{"missing":{"state":"unavailable"}},"upstream_model_update_last_removed_models":["missing"],"upstream_model_update_model_health":{"missing":{"failures":2},"other":{"failures":1}},"websocket_unsupported":true,"future":{"kept":true}}`
	require.NoError(t, f.db.Save(channel).Error)

	body, err := common.Marshal(map[string]any{
		"id":       channel.Id,
		"models":   "gpt-4o,missing",
		"settings": channel.OtherSettings,
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/channel/", bytes.NewReader(body))
	c.Set("id", 1)
	c.Set("role", common.RoleRootUser)

	UpdateChannel(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Empty(t, f.requested(), "manual edits must not perform billable network probes")

	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Equal(t, "gpt-4o,missing", stored.Models)
	var settings map[string]any
	require.NoError(t, common.UnmarshalJsonStr(stored.OtherSettings, &settings))
	assert.NotContains(t, settings, "model_availability")
	assert.NotContains(t, settings, "upstream_model_update_last_removed_models")
	assert.NotContains(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth, "missing")
	assert.Contains(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth, "other", "unlisted recovery candidates must survive an unrelated edit")
	assert.True(t, stored.GetOtherSettings().WebsocketUnsupported)
	assert.Contains(t, settings, "future", "unrelated and forward-compatible settings must survive")
}
