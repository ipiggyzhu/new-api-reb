import type { ChannelScoreSummary } from '../types'

/**
 * Presentation rules for the dynamic score badges, kept out of the components so
 * they can be tested directly.
 *
 * Priority and weight get their own badge in their own cell, and each reports only
 * its own dimension. They move independently — a channel can be demoted a tier
 * while its weight stays neutral, or be held at its configured tier while its
 * weight is halved — so folding them into one badge hides whichever the other
 * outranks. That was the first version's bug: weight only appeared when no tier
 * had moved, which is exactly the case where a reader least needs it.
 *
 * The rules below exist because the obvious renderings mislead:
 *
 * - An average tier offset is meaningless. A tier is a position among the
 *   channels eligible for ONE request, so the same offset resolves to different
 *   absolute priorities per request and a mean describes no real state.
 * - A bare range reads identically whether one route of forty moved or all forty
 *   did, so the count has to travel with it.
 */

/** formatOffset always carries an explicit sign, so +1 cannot be misread as 1. */
export function formatOffset(offset: number): string {
  return offset > 0 ? `+${offset}` : String(offset)
}

/**
 * hasTierEffect reports whether any active route's position has moved. False
 * means the priority badge must not render: a channel scored and left where the
 * admin put it is, for routing purposes, the same as one never scored, and a
 * badge reading "0" implies a measurement worth acting on.
 */
export function hasTierEffect(summary: ChannelScoreSummary): boolean {
  return summary.adjusted > 0
}

/** hasWeightEffect is the same question for the weight multiplier. */
export function hasWeightEffect(summary: ChannelScoreSummary): boolean {
  return summary.weighted > 0
}

/**
 * tierLabel is the range of active tier movement plus the count that moved. The
 * denominator is what stops one demoted route from reading like a channel-wide
 * collapse.
 */
export function tierLabel(summary: ChannelScoreSummary): string {
  const { min_offset: min, max_offset: max } = summary
  const range =
    min === max ? formatOffset(min) : `${formatOffset(min)}…${formatOffset(max)}`
  return `${range} (${summary.adjusted}/${summary.active})`
}

/**
 * weightLabel is the multiplier range. Rendered as a factor rather than as a
 * computed weight because the configured weight is what the cell's input shows,
 * and printing a second absolute number there would read as a contradiction.
 */
export function weightLabel(summary: ChannelScoreSummary): string {
  const { min_weight_factor: min, max_weight_factor: max } = summary
  const range = min === max ? `${min}x` : `${min}–${max}x`
  return `${range} (${summary.weighted}/${summary.active})`
}

export type BadgeVariant = 'danger' | 'warning' | 'success' | 'info'

/**
 * tierVariant colours by the worst active movement rather than by the range's
 * midpoint: one route demoted three tiers deserves attention even when another
 * route on the same channel was promoted.
 */
export function tierVariant(summary: ChannelScoreSummary): BadgeVariant {
  if (summary.min_offset < 0) {
    return summary.min_offset <= -2 ? 'danger' : 'warning'
  }
  return 'success'
}

/**
 * weightVariant keys off the lowest factor for the same reason. The neutral point
 * is 1.0 (the factor is 0.5 + success rate), so anything below it is a channel
 * being handed less traffic than the admin configured.
 */
export function weightVariant(summary: ChannelScoreSummary): BadgeVariant {
  if (summary.min_weight_factor < 1) {
    return summary.min_weight_factor <= 0.75 ? 'danger' : 'warning'
  }
  return 'success'
}
