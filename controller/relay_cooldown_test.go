package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A refusal cools the whole channel only when it has a single key. On a
// multi-key channel a rate limit cools just the key that sent the request and
// the channel stays selectable through its siblings; an overload leaves a
// multi-key channel alone, as it always did.
func TestApplyRefusalCooldownScopesMultiKeyRateLimitsToTheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalDB, originalLogDB, originalCache := model.DB, model.LOG_DB, common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.MemoryCacheEnabled = originalDB, originalLogDB, originalCache
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	// Cooldowns are process-wide; a model name of its own keeps reruns apart.
	runID := time.Now().UnixNano()

	rateLimited := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)
	overloaded := types.WithClaudeError(types.ClaudeError{Type: "overloaded_error", Message: "overloaded"}, 529)
	tests := []struct {
		name               string
		id                 int
		multiKey           bool
		err                *types.NewAPIError
		wantChannelCooling bool
		wantKeySkipped     bool
	}{
		{name: "single key 429 cools the channel", id: 7101, err: rateLimited, wantChannelCooling: true},
		{name: "single key 529 cools the channel", id: 7102, err: overloaded, wantChannelCooling: true},
		{name: "multi key 429 cools only the key", id: 7103, multiKey: true, err: rateLimited, wantKeySkipped: true},
		{name: "multi key 529 cools nothing", id: 7104, multiKey: true, err: overloaded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modelName := fmt.Sprintf("cooldown-model-%d-%d", tt.id, runID)
			channel := &model.Channel{
				Id: tt.id, Type: 1, Name: tt.name, Status: common.ChannelStatusEnabled, Key: "sk-only",
			}
			if tt.multiKey {
				channel.Key = "sk-a\nsk-b"
				channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeRandom}
			}
			require.NoError(t, db.Create(channel).Error)

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			common.SetContextKey(c, constant.ContextKeyChannelKey, strings.Split(channel.Key, "\n")[0])
			common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 0)
			// The retry loop hands over a stub channel on the first attempt.
			stub := &model.Channel{Id: channel.Id, ChannelInfo: model.ChannelInfo{IsMultiKey: tt.multiKey}}

			applyRefusalCooldown(c, stub, modelName, tt.err)

			assert.Equal(t, tt.wantChannelCooling, model.ChannelCoolingDown(channel.Id, modelName))
			if !tt.multiKey {
				return
			}
			seen := map[int]bool{}
			for i := 0; i < 40; i++ {
				_, index, apiErr := channel.GetNextEnabledKey(modelName)
				require.Nil(t, apiErr)
				seen[index] = true
			}
			assert.Equal(t, !tt.wantKeySkipped, seen[0], "refused key 0 selectable")
			assert.True(t, seen[1], "sibling key stays selectable")
		})
	}
}
