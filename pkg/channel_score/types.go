// Package channel_score keeps a short-lived success/fault record per
// (channel, group, model) and turns it into a position shift inside one
// request's candidate ranking.
//
// IMPORT ALLOWLIST — this package may import only common, pkg/cachex and
// setting/operation_setting. It must NOT import model, service or
// pkg/perf_metrics: model builds the candidate lists that call into here, and
// pkg/perf_metrics itself imports model, so pulling either one in recreates an
// import cycle through the callers.
//
// Nothing here is persisted. The admin-configured priority and weight stay the
// baseline and are never rewritten; scores only reorder candidates for the
// duration of a single selection, and a restart returns routing to the
// configured values.
package channel_score

// Candidate is the (channel, priority, weight) triple the selection paths in
// model build. It is redeclared here rather than imported so this package stays
// free of any dependency on model.
type Candidate struct {
	ChannelId int
	Priority  int64
	Weight    int
}

// Outcome is one request attempt's verdict for a channel.
type Outcome int

const (
	// OutcomeSuccess is a relay that completed. Streams that committed a 200 and
	// then died must not arrive here — the caller resolves that first.
	OutcomeSuccess Outcome = iota
	// OutcomeFault is an error that service.IsChannelFaultError accepted as the
	// channel's own fault. Rate limits, transient blips and our own
	// misconfiguration are not faults and must never be reported.
	OutcomeFault
)

// scoreState is the full per-key record. Every field moves together under one
// lock (or inside one Lua script), because promoting, resetting a streak and
// rotating the success window are a single state transition: applying them
// through separate atomic operations lets concurrent requests interleave into a
// state no single request intended.
type scoreState struct {
	// consecutiveSuccess is the streak toward the next promotion. Any fault
	// zeroes it.
	consecutiveSuccess int

	// faultCount is the streak toward the next demotion. Any success zeroes it.
	faultCount int

	// tierOffset is the accumulated movement in tiers: positive promotes,
	// negative demotes. Clamped to the configured bounds.
	tierOffset int

	// The success window is two half-buckets that rotate, so the rate reflects
	// recent behaviour instead of a channel's whole lifetime. curStart is the
	// unix second the current half opened.
	curStart    int64
	curTotal    int64
	curSuccess  int64
	prevTotal   int64
	prevSuccess int64

	// updatedAt drives idle reset.
	updatedAt int64
}

// sampleTotals returns the request volume and success count across both halves
// of the window.
func (s *scoreState) sampleTotals() (total int64, success int64) {
	return s.curTotal + s.prevTotal, s.curSuccess + s.prevSuccess
}

// rotate advances the sliding window to now, discarding data older than two
// halves. A gap longer than the whole window clears both halves rather than
// shifting stale data forward.
func (s *scoreState) rotate(now int64, halfWidth int64) {
	if halfWidth <= 0 {
		return
	}
	if s.curStart == 0 {
		s.curStart = now
		return
	}
	elapsed := now - s.curStart
	if elapsed < halfWidth {
		return
	}
	if elapsed < 2*halfWidth {
		s.prevTotal, s.prevSuccess = s.curTotal, s.curSuccess
	} else {
		s.prevTotal, s.prevSuccess = 0, 0
	}
	s.curTotal, s.curSuccess = 0, 0
	s.curStart = now
}
