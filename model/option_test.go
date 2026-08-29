package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupOptionTest(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&Option{}))
	require.NoError(t, DB.Exec("DELETE FROM options").Error)

	originalOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	originalModelRatio := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatio))
		common.OptionMap = originalOptionMap
		require.NoError(t, DB.AutoMigrate(&Option{}))
		require.NoError(t, DB.Exec("DELETE FROM options").Error)
	})
}

func TestUpdateOptionRejectsInvalidRatioJSONBeforePersist(t *testing.T) {
	setupOptionTest(t)

	require.NoError(t, UpdateOption("ModelRatio", `{"custom-model": 2.5}`))
	var stored Option
	require.NoError(t, DB.First(&stored, "key = ?", "ModelRatio").Error)
	require.Equal(t, `{"custom-model": 2.5}`, stored.Value)

	require.Error(t, UpdateOption("ModelRatio", `{"custom-model": `))

	// The malformed value must not reach the database, or it would break the
	// ratio map again on every restart.
	require.NoError(t, DB.First(&stored, "key = ?", "ModelRatio").Error)
	assert.Equal(t, `{"custom-model": 2.5}`, stored.Value)

	// The live ratio map must keep serving the previous value instead of being
	// wiped by the failed update.
	ratio, ok, _ := ratio_setting.GetModelRatio("custom-model")
	assert.True(t, ok)
	assert.Equal(t, 2.5, ratio)
}

func TestUpdateOptionPropagatesDatabaseWriteFailure(t *testing.T) {
	setupOptionTest(t)

	originalTopUpLink := common.TopUpLink
	t.Cleanup(func() { common.TopUpLink = originalTopUpLink })

	require.NoError(t, DB.Migrator().DropTable(&Option{}))

	err := UpdateOption("TopUpLink", "https://example.com/topup")
	require.Error(t, err)
}
