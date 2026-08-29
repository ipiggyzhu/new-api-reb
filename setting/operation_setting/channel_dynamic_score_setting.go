package operation_setting

import (
	"sync/atomic"

	"github.com/QuantumNous/new-api/setting/config"
)

// ChannelDynamicScoreSetting configures dynamic per-channel priority and weight
// adjustment: a channel that keeps succeeding rises in the selection order, and
// one that faults sinks.
//
// Nothing here is persisted per channel. The admin-configured priority and weight
// on the channel remain the baseline and are never overwritten; the scores only
// shift a channel's position within one request's candidate ranking. A restart
// therefore returns routing to exactly what the admin configured.
type ChannelDynamicScoreSetting struct {
	Enabled bool `json:"enabled"`

	// SuccessesToPromote is how many consecutive successes earn one tier of
	// promotion. A single fault resets the streak, so a flapping channel never
	// accumulates one.
	SuccessesToPromote int `json:"successes_to_promote"`

	// FaultsToDemote is how many faults cost one tier of demotion. Only errors
	// that service.IsChannelFaultError accepts count: rate limits and our own
	// misconfiguration are not the channel's fault.
	FaultsToDemote int `json:"faults_to_demote"`

	// MaxPromoteTiers and MaxDemoteTiers bound the movement in tiers, not in raw
	// priority numbers, because configured priorities span arbitrary magnitudes
	// (a channel at 100 would still outrank one at 0 after any fixed offset).
	// Demotion reaches deeper than promotion: one real outage should be able to
	// push a channel below its healthy siblings, while recovery stays gradual.
	MaxPromoteTiers int `json:"max_promote_tiers"`
	MaxDemoteTiers  int `json:"max_demote_tiers"`

	// MinSampleForWeight is the request volume a channel must have inside the
	// window before its success rate is allowed to scale its weight. Below it the
	// factor stays exactly 1.0. Envoy's success_rate_request_volume exists for the
	// same reason: a success rate over three requests carries no information.
	MinSampleForWeight int `json:"min_sample_for_weight"`

	// SuccessWindowSeconds is the width of the sliding window behind the success
	// rate. Cumulative counters would make an old channel's rate progressively
	// less responsive to how it is behaving now.
	SuccessWindowSeconds int `json:"success_window_seconds"`

	// IdleResetSeconds drops a score that has seen no traffic for this long,
	// returning the channel to the admin baseline. It is the analogue of Envoy's
	// base_ejection_time: a demotion is not meant to be permanent.
	IdleResetSeconds int `json:"idle_reset_seconds"`
}

// Defaults are opt-in: Enabled is false, so an upgrade changes no routing until
// an admin turns it on.
var channelDynamicScoreSetting = ChannelDynamicScoreSetting{
	Enabled:              false,
	SuccessesToPromote:   5,
	FaultsToDemote:       1,
	MaxPromoteTiers:      1,
	MaxDemoteTiers:       3,
	MinSampleForWeight:   20,
	SuccessWindowSeconds: 300,
	IdleResetSeconds:     1800,
}

// publishedDynamicScoreSetting holds the snapshot the request path reads, for
// the same reason publishedAffinitySetting does: the config layer writes fields
// through reflection, and the selection path must never observe a half-written
// struct.
var publishedDynamicScoreSetting atomic.Pointer[ChannelDynamicScoreSetting]

func init() {
	config.GlobalConfig.Register("channel_dynamic_score_setting", &channelDynamicScoreSetting)
	RepublishChannelDynamicScoreSetting()
}

// RepublishChannelDynamicScoreSetting snapshots the mutable config into the
// atomic pointer. Called on init and after an admin save, never on the request
// path.
func RepublishChannelDynamicScoreSetting() {
	snapshot := channelDynamicScoreSetting
	publishedDynamicScoreSetting.Store(&snapshot)
}

// SetChannelDynamicScoreSettingForTest publishes a setting snapshot directly.
// Tests use it to pin thresholds instead of depending on whatever the defaults
// happen to be, so a later change to the defaults cannot silently invalidate a
// test's premise.
func SetChannelDynamicScoreSettingForTest(setting ChannelDynamicScoreSetting) {
	publishedDynamicScoreSetting.Store(&setting)
}

// ResetChannelDynamicScoreSettingForTest restores the configured snapshot.
func ResetChannelDynamicScoreSettingForTest() {
	RepublishChannelDynamicScoreSetting()
}

// GetChannelDynamicScoreSetting returns the current snapshot, with every field
// sanitized so callers never have to defend against a zero or negative value an
// admin saved. Returning usable defaults rather than an error keeps the
// selection path free of validation branches.
func GetChannelDynamicScoreSetting() ChannelDynamicScoreSetting {
	snapshot := publishedDynamicScoreSetting.Load()
	if snapshot == nil {
		return ChannelDynamicScoreSetting{}
	}
	setting := *snapshot
	if setting.SuccessesToPromote <= 0 {
		setting.SuccessesToPromote = 5
	}
	if setting.FaultsToDemote <= 0 {
		setting.FaultsToDemote = 1
	}
	if setting.MaxPromoteTiers < 0 {
		setting.MaxPromoteTiers = 0
	}
	if setting.MaxDemoteTiers < 0 {
		setting.MaxDemoteTiers = 0
	}
	if setting.MinSampleForWeight <= 0 {
		setting.MinSampleForWeight = 20
	}
	if setting.SuccessWindowSeconds <= 0 {
		setting.SuccessWindowSeconds = 300
	}
	if setting.IdleResetSeconds <= 0 {
		setting.IdleResetSeconds = 1800
	}
	return setting
}
