package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A task resubmitted on an independent key environment channel goes back to the
// key that created the upstream task while that key can serve; otherwise it
// falls back to the channel's selectors, which never pick an unassigned key.
func TestResolveOriginTaskOnKeyEnvironmentsUsesTheOriginKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Task{}, &model.Channel{}, &model.ChannelKeyEnvironment{}))
	originalDB, originalMemoryCache := model.DB, common.MemoryCacheEnabled
	model.DB, common.MemoryCacheEnabled = db, false
	t.Cleanup(func() { model.DB, common.MemoryCacheEnabled = originalDB, originalMemoryCache })

	const channelId = 1801
	channel := &model.Channel{
		Id: channelId, Type: constant.ChannelTypeOpenAI, Name: "task-keys", Status: common.ChannelStatusEnabled,
		Key:         "sk-a\nsk-b\nsk-c",
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling},
	}
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex, KeyEnvironments: true})
	require.NoError(t, db.Create(channel).Error)
	for key, profile := range map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64"} {
		require.NoError(t, db.Create(&model.ChannelKeyEnvironment{ChannelId: channelId, KeyTag: model.ChannelKeyTag(key), ProfileId: common.GetPointer(profile)}).Error)
	}
	require.NoError(t, db.Create(&model.ChannelKeyEnvironment{ChannelId: channelId, KeyTag: model.ChannelKeyTag("sk-c")}).Error)

	for _, tc := range []struct {
		name          string
		originKey     string
		selectedId    int
		selectedKey   string
		wantKeys      []string
		wantKeyIndex  int
		checkKeyIndex bool
	}{
		{name: "same channel, the origin key replaces the selected one", originKey: "sk-b", selectedId: channelId, selectedKey: "sk-a",
			wantKeys: []string{"sk-b"}, wantKeyIndex: 1, checkKeyIndex: true},
		{name: "another channel selected, the origin key is used", originKey: "sk-b", selectedId: 1802, selectedKey: "sk-other",
			wantKeys: []string{"sk-b"}, wantKeyIndex: 1, checkKeyIndex: true},
		{name: "an unassigned origin key falls back to the assigned keys", originKey: "sk-c", selectedId: 1802, selectedKey: "sk-other",
			wantKeys: []string{"sk-a", "sk-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskId := "task-" + tc.originKey + "-" + tc.name
			require.NoError(t, db.Create(&model.Task{
				TaskID: taskId, UserId: 1, ChannelId: channelId, PrivateData: model.TaskPrivateData{Key: tc.originKey},
				Properties: model.Properties{OriginModelName: "sora-2"},
			}).Error)
			for round := 0; round < 4; round++ {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
				common.SetContextKey(c, constant.ContextKeyChannelKey, tc.selectedKey)
				info := &relaycommon.RelayInfo{
					UserId:        1,
					ChannelMeta:   &relaycommon.ChannelMeta{ChannelId: tc.selectedId, ApiKey: tc.selectedKey},
					TaskRelayInfo: &relaycommon.TaskRelayInfo{OriginTaskID: taskId},
				}

				require.Nil(t, ResolveOriginTask(c, info))
				assert.Equal(t, channelId, info.ChannelId)
				assert.Contains(t, tc.wantKeys, info.ApiKey, "round %d", round)
				assert.Equal(t, info.ApiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey))
				if tc.checkKeyIndex {
					assert.Equal(t, tc.wantKeyIndex, info.ChannelMultiKeyIndex)
					assert.Equal(t, model.ChannelKeyTag(tc.originKey), common.GetContextKeyString(c, constant.ContextKeyChannelKeyTag))
				}
			}
		})
	}
}
