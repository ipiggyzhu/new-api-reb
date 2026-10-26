package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func keyEnvironmentTestChannel(id int, family string, ttl string, enabled bool, keys string) *model.Channel {
	channelType := constant.ChannelTypeOpenAI
	if family == "claude" {
		channelType = constant.ChannelTypeAnthropic
	}
	channel := &model.Channel{Id: id, Type: channelType, Key: keys, ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	setting := channel.GetSetting()
	setting.SyntheticClientHeadersProfile = family
	setting.KeyEnvironments = enabled
	setting.KeyEnvironmentCacheTTL = ttl
	channel.SetSetting(setting)
	return channel
}

func setupKeyEnvironmentTable(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.ChannelKeyEnvironment{}, &model.Channel{}))
	require.NoError(t, model.DB.Where("1 = 1").Delete(&model.ChannelKeyEnvironment{}).Error)
	t.Cleanup(func() {
		require.NoError(t, model.DB.Where("1 = 1").Delete(&model.ChannelKeyEnvironment{}).Error)
	})
}

func reconcileInTx(channel *model.Channel, allowUnassigned bool) error {
	return model.DB.Transaction(func(tx *gorm.DB) error {
		_, err := ReconcileKeyEnvironments(tx, channel, allowUnassigned)
		return err
	})
}

// storedKeyEnvironments reads the table back as key → profile ("" = unassigned).
func storedKeyEnvironments(t *testing.T, channelId int, keys ...string) map[string]string {
	t.Helper()
	rows, err := model.LoadChannelKeyEnvironmentRows(model.DB, channelId)
	require.NoError(t, err)
	byTag := make(map[string]string, len(rows))
	for _, row := range rows {
		profile := ""
		if row.ProfileId != nil {
			profile = *row.ProfileId
		}
		byTag[row.KeyTag] = profile
	}
	stored := make(map[string]string, len(byTag))
	for _, key := range keys {
		if profile, ok := byTag[model.ChannelKeyTag(key)]; ok {
			stored[key] = profile
		}
	}
	require.Len(t, stored, len(byTag), "table holds a row for a key outside the expected list")
	return stored
}

func TestReconcileKeyEnvironmentsKeepsAssignmentsStable(t *testing.T) {
	setupKeyEnvironmentTable(t)
	const id = 7101
	all := []string{"sk-a", "sk-b", "sk-c", "sk-d"}

	steps := []struct {
		name string
		keys string
		want map[string]string
	}{
		{name: "three keys take three versions newest first", keys: "sk-a\nsk-b\nsk-c",
			want: map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64", "sk-c": "codex-0.156.1-windows-x64"}},
		{name: "deleting the middle key leaves the others", keys: "sk-a\nsk-c",
			want: map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-c": "codex-0.156.1-windows-x64"}},
		{name: "an appended key takes the freed version", keys: "sk-a\nsk-c\nsk-d",
			want: map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-c": "codex-0.156.1-windows-x64", "sk-d": "codex-0.160.0-windows-x64"}},
		{name: "reordering the textarea changes nothing", keys: "sk-d\nsk-c\nsk-a",
			want: map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-c": "codex-0.156.1-windows-x64", "sk-d": "codex-0.160.0-windows-x64"}},
	}
	for _, step := range steps {
		require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "codex", "", true, step.keys), false), step.name)
		assert.Equal(t, step.want, storedKeyEnvironments(t, id, all...), step.name)
	}
}

func TestReconcileKeyEnvironmentsRejectsBadKeyLists(t *testing.T) {
	setupKeyEnvironmentTable(t)
	owner := &model.Channel{Id: 7201, Name: "owner-env", Type: constant.ChannelTypeOpenAI, Key: "sk-owned"}
	require.NoError(t, model.DB.Create(owner).Error)
	t.Cleanup(func() { require.NoError(t, model.DB.Delete(owner).Error) })
	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(owner.Id, "codex", "", true, "sk-owned\nsk-other"), false))

	t.Run("a key repeated with a trailing carriage return", func(t *testing.T) {
		err := reconcileInTx(keyEnvironmentTestChannel(7202, "codex", "", true, "sk-x\nsk-x\r"), false)
		var duplicate *KeyEnvironmentDuplicateError
		require.ErrorAs(t, err, &duplicate)
		assert.Equal(t, KeyEnvironmentDuplicateError{Index: 1, FirstIndex: 0}, *duplicate)
	})
	t.Run("a key another environment channel holds", func(t *testing.T) {
		err := reconcileInTx(keyEnvironmentTestChannel(7202, "codex", "", true, "sk-new\nsk-owned"), false)
		var conflict *KeyEnvironmentConflictError
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, KeyEnvironmentConflictError{Index: 1, ChannelId: owner.Id, ChannelName: "owner-env"}, *conflict)
		assert.Empty(t, storedKeyEnvironments(t, 7202), "a rejected save writes no rows")
	})
}

func TestReconcileKeyEnvironmentsShortage(t *testing.T) {
	setupKeyEnvironmentTable(t)
	const id = 7301
	keys := "sk-1\nsk-2\nsk-3\nsk-4\nsk-5"

	err := reconcileInTx(keyEnvironmentTestChannel(id, "claude", "", true, keys), false)
	var shortage *KeyEnvironmentShortageError
	require.ErrorAs(t, err, &shortage)
	assert.Equal(t, KeyEnvironmentShortageError{Available: 3, Keys: 5, UnassignedIndexes: []int{3, 4}, Versions: []string{"2.1.306", "2.1.288", "2.1.282"}}, *shortage)
	assert.Empty(t, storedKeyEnvironments(t, id), "the shortage rolls the transaction back")

	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "claude", "", true, keys), true))
	assert.Equal(t, map[string]string{
		"sk-1": "claude-2.1.306-linux-x64", "sk-2": "claude-2.1.288-linux-x64", "sk-3": "claude-2.1.282-linux-x64", "sk-4": "", "sk-5": "",
	}, storedKeyEnvironments(t, id, "sk-1", "sk-2", "sk-3", "sk-4", "sk-5"))

	// The same keys saved again stay accepted only with the confirmation.
	require.ErrorAs(t, reconcileInTx(keyEnvironmentTestChannel(id, "claude", "", true, keys), false), &shortage)
}

func TestReconcileKeyEnvironmentsCacheModeMapsToVariant(t *testing.T) {
	setupKeyEnvironmentTable(t)
	const id = 7401
	all := []string{"sk-1", "sk-2", "sk-3", "sk-4"}
	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "claude", "", true, "sk-1\nsk-2\nsk-3\nsk-4"), true))
	require.Equal(t, map[string]string{
		"sk-1": "claude-2.1.306-linux-x64", "sk-2": "claude-2.1.288-linux-x64", "sk-3": "claude-2.1.282-linux-x64", "sk-4": "",
	}, storedKeyEnvironments(t, id, all...))

	// Each key keeps its version across the switch, whatever the list order.
	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "claude", "1h", true, "sk-4\nsk-3\nsk-2\nsk-1"), true))
	assert.Equal(t, map[string]string{
		"sk-1": "claude-2.1.306-linux-x64-1h", "sk-2": "claude-2.1.288-linux-x64-1h", "sk-3": "claude-2.1.282-linux-x64-1h", "sk-4": "",
	}, storedKeyEnvironments(t, id, all...))
}

func TestReconcileKeyEnvironmentsSwitchOff(t *testing.T) {
	setupKeyEnvironmentTable(t)
	const id = 7501
	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "codex", "", true, "sk-1\nsk-2"), false))

	// Off keeps the remaining keys held, so turning it back on restores them,
	// but a key deleted while off is released.
	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "codex", "", false, "sk-2\nsk-3"), false))
	assert.Equal(t, map[string]string{"sk-2": "codex-0.160.0-windows-x64"}, storedKeyEnvironments(t, id, "sk-1", "sk-2", "sk-3"))

	require.NoError(t, reconcileInTx(keyEnvironmentTestChannel(id, "codex", "", true, "sk-2\nsk-3"), false))
	assert.Equal(t, map[string]string{"sk-2": "codex-0.160.0-windows-x64", "sk-3": "codex-0.163.0-linux-x64"}, storedKeyEnvironments(t, id, "sk-2", "sk-3"))
}

func TestPlanKeyEnvironmentsPoolChanges(t *testing.T) {
	v163 := LookupCLIVersionProfile("codex-0.163.0-linux-x64")
	v160 := LookupCLIVersionProfile("codex-0.160.0-windows-x64")
	v156 := LookupCLIVersionProfile("codex-0.156.1-windows-x64")
	keys := []string{"sk-1", "sk-2", "sk-3"}
	existing := map[string]string{
		model.ChannelKeyTag("sk-1"): v156.ID,
		model.ChannelKeyTag("sk-2"): v163.ID,
		model.ChannelKeyTag("sk-3"): v160.ID,
	}
	profiles := func(plan []KeyEnvironmentAssignment) []string {
		ids := make([]string, len(plan))
		for i, a := range plan {
			ids[i] = a.ProfileId
		}
		return ids
	}
	downgrades := func(plan []KeyEnvironmentAssignment) []bool {
		flags := make([]bool, len(plan))
		for i, a := range plan {
			flags[i] = a.Downgrade
		}
		return flags
	}

	t.Run("a version leaving the pool unassigns only its key", func(t *testing.T) {
		plan, err := planKeyEnvironments(keys, existing, []*CLIVersionProfile{v163, v156}, false)
		require.NoError(t, err)
		assert.Equal(t, []string{v156.ID, v163.ID, ""}, profiles(plan))
	})
	t.Run("reassign with every version still pooled has no downgrade", func(t *testing.T) {
		plan, err := planKeyEnvironments(keys, existing, []*CLIVersionProfile{v163, v160, v156}, true)
		require.NoError(t, err)
		assert.Equal(t, []string{v156.ID, v163.ID, v160.ID}, profiles(plan))
		assert.Equal(t, []bool{false, false, false}, downgrades(plan))
	})
	t.Run("reassign after the newest version left the pool marks the downgraded key", func(t *testing.T) {
		held := map[string]string{model.ChannelKeyTag("sk-1"): v163.ID, model.ChannelKeyTag("sk-2"): v160.ID}
		plan, err := planKeyEnvironments([]string{"sk-1", "sk-2"}, held, []*CLIVersionProfile{v160, v156}, true)
		require.NoError(t, err)
		assert.Equal(t, []string{v156.ID, v160.ID}, profiles(plan))
		assert.Equal(t, []bool{true, false}, downgrades(plan))
	})
}

func TestKeyEnvironmentPoolIsTheAutoAssignCatalog(t *testing.T) {
	ids := func(pool []*CLIVersionProfile) []string {
		out := make([]string, len(pool))
		for i, p := range pool {
			out[i] = p.ID
		}
		return out
	}
	assert.Equal(t, []string{"claude-2.1.306-linux-x64", "claude-2.1.288-linux-x64", "claude-2.1.282-linux-x64"}, ids(KeyEnvironmentPool("claude", "")))
	assert.Equal(t, []string{"claude-2.1.306-linux-x64-1h", "claude-2.1.288-linux-x64-1h", "claude-2.1.282-linux-x64-1h"}, ids(KeyEnvironmentPool("claude", "1h")))
	assert.Equal(t, []string{"codex-0.163.0-linux-x64", "codex-0.160.0-windows-x64", "codex-0.156.1-windows-x64"}, ids(KeyEnvironmentPool("codex", "")))
	assert.Empty(t, KeyEnvironmentPool("openai", ""))
}

func TestConsumeLogCarriesTheCLIProfileForAdminsOnly(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	c.Set(string(constant.ContextKeyCLIProfileAudit), map[string]any{
		"cli_profile": "codex-0.163.0-linux-x64", "key_env_bound": true,
		"cli_version_mismatch": map[string]string{"client": "0.150.0", "profile": "0.163.0"},
	})
	other := GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 0, 1)
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "codex-0.163.0-linux-x64", adminInfo["cli_profile"])
	assert.Equal(t, true, adminInfo["key_env_bound"])
	assert.Equal(t, map[string]string{"client": "0.150.0", "profile": "0.163.0"}, adminInfo["cli_version_mismatch"])
	assert.NotContains(t, other, "cli_profile", "the profile is admin-only")
}
