package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetMonitorSetting_ChannelTestEnabledEnvOverridesEnabledConfig(t *testing.T) {
	orig := monitorSetting
	t.Cleanup(func() { monitorSetting = orig })

	t.Setenv("CHANNEL_TEST_ENABLED", "false")
	t.Setenv("CHANNEL_TEST_FREQUENCY", "5")
	monitorSetting = MonitorSetting{
		AutoTestChannelEnabled: true,
		AutoTestChannelMinutes: 20,
	}

	setting := GetMonitorSetting()

	require.NotNil(t, setting)
	assert.False(t, setting.AutoTestChannelEnabled)
	assert.Equal(t, float64(5), setting.AutoTestChannelMinutes)
}

func TestGetMonitorSetting_ChannelTestEnabledEnvCanEnableDisabledConfig(t *testing.T) {
	orig := monitorSetting
	t.Cleanup(func() { monitorSetting = orig })

	t.Setenv("CHANNEL_TEST_ENABLED", "true")
	monitorSetting = MonitorSetting{
		AutoTestChannelEnabled: false,
		AutoTestChannelMinutes: 12,
	}

	setting := GetMonitorSetting()

	require.NotNil(t, setting)
	assert.True(t, setting.AutoTestChannelEnabled)
	assert.Equal(t, float64(12), setting.AutoTestChannelMinutes)
}

func TestPickChannelTestPromptFallsBackToBuiltin(t *testing.T) {
	orig := monitorSetting
	t.Cleanup(func() { monitorSetting = orig })

	monitorSetting = MonitorSetting{
		ChannelTestPrompts: nil,
	}

	for i := 0; i < 50; i++ {
		prompt := PickChannelTestPrompt()
		require.NotEmpty(t, prompt)
		assert.Contains(t, builtinChannelTestPrompts, prompt)
	}
}

func TestPickChannelTestPromptUsesConfiguredPool(t *testing.T) {
	orig := monitorSetting
	t.Cleanup(func() { monitorSetting = orig })

	configured := []string{"custom prompt A", "custom prompt B"}
	monitorSetting = MonitorSetting{
		ChannelTestPrompts: configured,
	}

	seen := make(map[string]int)
	for i := 0; i < 50; i++ {
		prompt := PickChannelTestPrompt()
		require.NotEmpty(t, prompt)
		seen[prompt]++
	}

	assert.Len(t, seen, 2, "only configured prompts should be picked")
	for _, expected := range configured {
		assert.Contains(t, seen, expected)
	}
}

func TestPickChannelTestPromptSkipsWhitespace(t *testing.T) {
	orig := monitorSetting
	t.Cleanup(func() { monitorSetting = orig })

	monitorSetting = MonitorSetting{
		ChannelTestPrompts: []string{"  ", "", "valid prompt", "   "},
	}

	for i := 0; i < 20; i++ {
		prompt := PickChannelTestPrompt()
		assert.Equal(t, "valid prompt", prompt)
	}
}

func TestMonitorSettingGettersReturnDefaultOnZero(t *testing.T) {
	s := &MonitorSetting{
		UpstreamModelUpdateIntervalHours:        0,
		UpstreamModelUpdateRetryDelayMinutes:    0,
		UpstreamModelUpdateFailureThreshold:     0,
		UpstreamModelUpdateRotationSampleSize:   -1,
		UpstreamModelUpdateMaxValidationsPerRun: 0,
	}

	assert.Equal(t, 24, s.GetUpstreamModelUpdateIntervalHours())
	assert.Equal(t, 60, s.GetUpstreamModelUpdateRetryDelayMinutes())
	assert.Equal(t, 2, s.GetUpstreamModelUpdateFailureThreshold())
	assert.Equal(t, 0, s.GetUpstreamModelUpdateRotationSampleSize(), "negative becomes 0, 0 is valid")
	assert.Equal(t, 200, s.GetUpstreamModelUpdateMaxValidationsPerRun())
}

func TestMonitorSettingGettersPreserveNonZero(t *testing.T) {
	s := &MonitorSetting{
		UpstreamModelUpdateIntervalHours:        48,
		UpstreamModelUpdateRetryDelayMinutes:    120,
		UpstreamModelUpdateFailureThreshold:     3,
		UpstreamModelUpdateRotationSampleSize:   10,
		UpstreamModelUpdateMaxValidationsPerRun: 500,
	}

	assert.Equal(t, 48, s.GetUpstreamModelUpdateIntervalHours())
	assert.Equal(t, 120, s.GetUpstreamModelUpdateRetryDelayMinutes())
	assert.Equal(t, 3, s.GetUpstreamModelUpdateFailureThreshold())
	assert.Equal(t, 10, s.GetUpstreamModelUpdateRotationSampleSize())
	assert.Equal(t, 500, s.GetUpstreamModelUpdateMaxValidationsPerRun())
}
