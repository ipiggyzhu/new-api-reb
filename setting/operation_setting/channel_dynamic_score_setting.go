package operation_setting

import (
	"sync/atomic"

	"github.com/QuantumNous/new-api/setting/config"
)

// ChannelDynamicScoreSetting configures dynamic per-channel priority and weight
// adjustment: a channel that keeps succeeding rises in the selection order, and
// one that faults sinks.
//
// The admin-configured priority and weight on the channel remain the baseline and
// are never overwritten; the scores only shift a channel's position within one
// request's candidate ranking, and every tier calculation starts from the baseline
// again rather than from the previous result.
//
// What IS written down, into channels.effective_priority and effective_weight, is
// a projection of that shift for the admin list to display — the feature was
// otherwise invisible, since both columns kept showing the configured value however
// far routing had moved. Those columns are derived, refreshed on
// ProjectionIntervalMinutes, and never read by the selection path. See
// model.RunChannelScoreProjection.
//
// Score state itself lives in Redis when configured, so it survives a restart;
// without Redis it is per-process and a restart returns routing to exactly what
// the admin configured.
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

	// IdleResetSeconds is the length of one forgiveness period for a channel that
	// has seen no traffic. A channel's sample window is cleared at that boundary,
	// because an old success rate says nothing about its behaviour now. Its tier
	// offset, however, decays ONE tier toward the configured baseline per elapsed
	// period instead of disappearing wholesale.
	//
	// The distinction is essential: a demotion is what stops traffic reaching a
	// channel, so treating that resulting silence as recovery used to return a
	// 100%-failing channel to the top tier after one period, where it failed the
	// next request and was demoted again forever. A deeper accumulated demotion
	// therefore lasts longer, while a one-tier blip is forgiven after one period —
	// the first request on return is the recovery probe, with no manual enable or
	// scheduled test required.
	IdleResetSeconds int `json:"idle_reset_seconds"`

	// ProjectionIntervalMinutes is how often the projected priority and weight
	// shown in the channel list are refreshed from the live scores.
	//
	// This is a DISPLAY cadence and nothing else. Routing recomputes the same
	// movement from the same baseline on every request, so lengthening this never
	// slows down how fast a failing channel is demoted — it only makes the number
	// in the list older.
	//
	// It should stay well under IdleResetSeconds. A projection refreshed less often
	// than scores expire would show adjustments that had already lapsed, which is
	// the one way this column can actively mislead.
	ProjectionIntervalMinutes int `json:"projection_interval_minutes"`
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
	// Ten minutes: frequent enough that the list tracks a channel going bad within
	// one poll of noticing it, and a third of the default idle window so a lapsed
	// score is cleared well before an operator could act on it. The run itself is
	// one indexed read plus a write per changed channel, so the cost of the shorter
	// cadence is not the reason to lengthen it.
	ProjectionIntervalMinutes: 10,
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
	if setting.ProjectionIntervalMinutes <= 0 {
		setting.ProjectionIntervalMinutes = 10
	}
	return setting
}
