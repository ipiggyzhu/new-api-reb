package channel_score

// This file aggregates the per-key scores into one row per channel, for the
// channel list.
//
// The list cannot show the raw rows: a channel accumulates one key per
// (group, model) it has served, and the key's model is the request's original
// model name rather than the ability it matched, so a wildcard ability turns
// every distinct requested name into its own key. Sending that to the browser on
// a poll would be unbounded. Aggregating here keeps the payload one small object
// per channel however many keys exist.
//
// What it deliberately does NOT compute is an average tier offset. Tiers are
// ordinal and request-local — offset -1 means "one position down among the
// channels eligible for THIS request", so averaging them produces a number that
// corresponds to nothing. The list shows a count of adjusted keys and the range
// instead.

// ChannelScoreSummary is one channel's scores reduced to what a list cell can
// show without misleading the reader.
type ChannelScoreSummary struct {
	// Active is the number of keys the selection path is currently applying, i.e.
	// non-idle. Total counts those plus idle ones.
	Active int `json:"active"`
	Total  int `json:"total"`

	// Adjusted is how many active keys have a non-zero tier offset. This is the
	// honest headline: "3 of 40 keys are demoted" says something an average or a
	// bare range does not, because a range of -3..0 looks identical whether one
	// key or every key is at -3.
	Adjusted int `json:"adjusted"`

	// MinOffset and MaxOffset bound the active keys' offsets. Both zero when
	// Adjusted is zero.
	MinOffset int `json:"min_offset"`
	MaxOffset int `json:"max_offset"`

	// Weighted is how many active keys carry a weight factor other than 1.0, with
	// the range across them. Without this a channel sitting at offset 0 but
	// scaled to 0.5x weight renders as untouched, which is the case an operator
	// most needs to notice: its position is unchanged but it is being handed half
	// the traffic.
	Weighted        int     `json:"weighted"`
	MinWeightFactor float64 `json:"min_weight_factor"`
	MaxWeightFactor float64 `json:"max_weight_factor"`

	// Idle is Total-Active, carried explicitly so the cell can say "plus 12 idle"
	// rather than making the reader subtract. An idle demotion is not being
	// applied but is still evidence of what the channel did.
	Idle int `json:"idle"`
}

// weightFactorEpsilon guards the comparison against the neutral 1.0.
// successRateFactor currently returns multiples of 0.25, all exact in binary, so
// an equality test would work today; the tolerance is here so that changing the
// step to something like 0.1 does not silently start reporting every key as
// weighted.
const weightFactorEpsilon = 1e-9

// SummarizeByChannel reduces the whole store to one summary per channel id.
//
// Channels with no keys at all are absent from the map rather than present with a
// zero summary: the caller merges onto a page of channels and "no scores" is the
// same thing as "nothing to show", so materializing them would only make the
// payload proportional to the channel count instead of to what has traffic.
func SummarizeByChannel() (map[int]ChannelScoreSummary, ScoreSnapshot) {
	// Snapshot carries the enabled/usable/complete metadata the caller needs to
	// decide whether to render anything, and takes each entry's lock correctly.
	// Reimplementing the Range here would duplicate that discipline for no gain.
	snapshot := Snapshot(ScoreFilter{})
	if len(snapshot.Rows) == 0 {
		return nil, snapshot
	}

	summaries := make(map[int]ChannelScoreSummary)
	for _, row := range snapshot.Rows {
		summary := summaries[row.ChannelID]
		summary.Total++

		if row.Idle {
			// Counted in Total and Idle only. Folding an idle key's offset into the
			// range would report an adjustment the selection path is not applying,
			// which is the opposite of what this cell is for.
			summary.Idle++
			summaries[row.ChannelID] = summary
			continue
		}
		summary.Active++

		if row.TierOffset != 0 {
			if summary.Adjusted == 0 {
				summary.MinOffset = row.TierOffset
				summary.MaxOffset = row.TierOffset
			} else {
				if row.TierOffset < summary.MinOffset {
					summary.MinOffset = row.TierOffset
				}
				if row.TierOffset > summary.MaxOffset {
					summary.MaxOffset = row.TierOffset
				}
			}
			summary.Adjusted++
		}

		if row.WeightFactor < 1-weightFactorEpsilon || row.WeightFactor > 1+weightFactorEpsilon {
			if summary.Weighted == 0 {
				summary.MinWeightFactor = row.WeightFactor
				summary.MaxWeightFactor = row.WeightFactor
			} else {
				if row.WeightFactor < summary.MinWeightFactor {
					summary.MinWeightFactor = row.WeightFactor
				}
				if row.WeightFactor > summary.MaxWeightFactor {
					summary.MaxWeightFactor = row.WeightFactor
				}
			}
			summary.Weighted++
		}

		summaries[row.ChannelID] = summary
	}
	return summaries, snapshot
}
