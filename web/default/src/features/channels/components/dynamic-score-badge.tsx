import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'

import {
  formatOffset,
  hasProjection,
  hasTierEffect,
  hasWeightEffect,
  projectedLabel,
  projectedVariant,
  tierLabel,
  tierVariant,
  weightLabel,
  weightVariant,
} from '../lib/dynamic-score-display'
import type { Channel, ChannelScoreSummary } from '../types'

/**
 * These badges surface what dynamic scoring is doing to a channel right now,
 * beside the configured priority and weight it adjusts.
 *
 * They exist because the effect is otherwise invisible. Scoring shifts a channel
 * only within one request's candidate ranking and is deliberately never written
 * back, so both columns keep showing whatever the admin typed however much the
 * routing has moved. An operator watching them concludes the feature does nothing.
 *
 * Priority and weight get one badge each, in their own cell, because they move
 * independently: a channel can lose a tier while its weight stays neutral, or hold
 * its tier while its weight is halved. Both share one popover body, since the
 * numbers an operator needs to interpret either are the same.
 *
 * What neither may imply is arithmetic on the number in the input. An offset of -1
 * means "one position down among the channels eligible for THIS request", which
 * lands on a different absolute priority per request.
 */
export function DynamicScorePriorityBadge({ channel }: { channel: Channel }) {
  const { t } = useTranslation()
  const summary = channel.dynamic_score

  // The projected column is preferred when present because it is the concrete
  // number — `0 → -1` rather than `-1 (1/1)`. It is refreshed on an interval, so
  // the offset rendering below still covers the window between a score changing
  // and the next projection run.
  if (hasProjection(channel.priority, channel.effective_priority)) {
    const baseline = channel.priority ?? 0
    return (
      <ScoreBadge
        summary={summary}
        label={projectedLabel(baseline, channel.effective_priority)}
        variant={projectedVariant(baseline, channel.effective_priority)}
        ariaLabel={t('Dynamic scoring detail')}
      />
    )
  }

  // Absent means no scored traffic, or scoring off/unusable — the table only
  // folds summaries on when both enabled and usable hold.
  if (!summary || !hasTierEffect(summary)) {
    return null
  }

  return (
    <ScoreBadge
      summary={summary}
      label={tierLabel(summary)}
      variant={tierVariant(summary)}
      ariaLabel={t('Dynamic scoring detail')}
    />
  )
}

export function DynamicScoreWeightBadge({ channel }: { channel: Channel }) {
  const { t } = useTranslation()
  const summary = channel.dynamic_score

  if (hasProjection(channel.weight, channel.effective_weight)) {
    const baseline = channel.weight ?? 0
    return (
      <ScoreBadge
        summary={summary}
        label={projectedLabel(baseline, channel.effective_weight)}
        variant={projectedVariant(baseline, channel.effective_weight)}
        ariaLabel={t('Dynamic scoring detail')}
      />
    )
  }

  if (!summary || !hasWeightEffect(summary)) {
    return null
  }

  return (
    <ScoreBadge
      summary={summary}
      label={weightLabel(summary)}
      variant={weightVariant(summary)}
      ariaLabel={t('Dynamic scoring detail')}
    />
  )
}

function ScoreBadge({
  summary,
  label,
  variant,
  ariaLabel,
}: {
  // Optional because a projected value can outlive the summary that produced it:
  // the projection is a database column refreshed on an interval, while the
  // summary is this instance's live in-process mirror, which is empty after a
  // restart until fresh traffic repopulates it. Showing the number without the
  // route breakdown is better than hiding the number.
  // Null as well as undefined: the schema field is nullish, so an explicit JSON
  // null reaches here unchanged.
  summary?: ChannelScoreSummary | null
  label: string
  variant: 'danger' | 'warning' | 'success' | 'info'
  ariaLabel: string
}) {
  return (
    <Popover>
      <PopoverTrigger
        // Focusable and click-driven rather than hover-only: a hover tooltip is
        // unreachable by keyboard and unusable on touch, and this is the only
        // place the numbers appear. copyable={false} keeps StatusBadge's own click
        // handler from swallowing the trigger's.
        render={
          <StatusBadge
            label={label}
            variant={variant}
            size='sm'
            copyable={false}
            className='shrink-0 cursor-pointer'
            tabIndex={0}
            role='button'
            aria-label={ariaLabel}
          />
        }
        // The row has its own click behaviour; opening the detail must not also
        // trigger it.
        onClick={(e: React.MouseEvent) => e.stopPropagation()}
      />
      <PopoverContent
        className='w-80'
        align='start'
        onClick={(e) => e.stopPropagation()}
      >
        {summary ? (
          <DynamicScoreDetail summary={summary} />
        ) : (
          <DynamicScoreProjectionOnly />
        )}
      </PopoverContent>
    </Popover>
  )
}

/**
 * DynamicScoreDetail spells out the aggregate the badges compress.
 *
 * Ordered so the denominator arrives before the range: "2 of 37 routes adjusted"
 * frames "-3…0" as two outliers rather than as a channel-wide state. Idle routes
 * come last and are labelled as not applied, since a demotion past the idle window
 * still exists in the store but no longer influences selection.
 */
function DynamicScoreDetail({ summary }: { summary: ChannelScoreSummary }) {
  const { t } = useTranslation()

  return (
    <div className='space-y-2 text-sm'>
      <div className='font-medium'>{t('Dynamic scoring')}</div>
      <p className='text-muted-foreground text-xs'>
        {t(
          'The value in the input is the configured baseline, which editing writes and scoring never overwrites.'
        )}
      </p>

      <dl className='space-y-1'>
        <Row
          label={t('Scored routes')}
          value={t('{{active}} active of {{total}}', {
            active: summary.active,
            total: summary.total,
          })}
        />
        {summary.adjusted > 0 ? (
          <Row
            label={t('Tier adjusted')}
            value={t('{{adjusted}} of {{active}}, range {{min}} to {{max}}', {
              adjusted: summary.adjusted,
              active: summary.active,
              min: formatOffset(summary.min_offset),
              max: formatOffset(summary.max_offset),
            })}
          />
        ) : (
          <Row
            label={t('Tier adjusted')}
            value={t('None — every active route is at its configured tier')}
          />
        )}
        {summary.weighted > 0 ? (
          <Row
            label={t('Weight scaled')}
            value={t('{{weighted}} of {{active}}, {{min}}x to {{max}}x', {
              weighted: summary.weighted,
              active: summary.active,
              min: summary.min_weight_factor,
              max: summary.max_weight_factor,
            })}
          />
        ) : (
          <Row
            label={t('Weight scaled')}
            value={t('None — every active route keeps its configured weight')}
          />
        )}
        {summary.idle > 0 && (
          <Row
            label={t('Idle')}
            value={t('{{count}} not applied (no recent traffic)', {
              count: summary.idle,
            })}
          />
        )}
      </dl>

      <p className='text-muted-foreground text-xs'>
        {t(
          'A tier is a position among the channels eligible for a request, not a number added to priority.'
        )}
      </p>
    </div>
  )
}

/**
 * DynamicScoreProjectionOnly is the popover when the projected column is present
 * but this instance has no live summary to break it down by route — the state after
 * a restart, since the projection is stored in the database and the summary is a
 * per-process mirror.
 *
 * It says what the number means and that the detail is missing, rather than
 * rendering a zeroed breakdown that would claim the channel has one scored route at
 * neutral.
 */
function DynamicScoreProjectionOnly() {
  const { t } = useTranslation()

  return (
    <div className='space-y-2 text-sm'>
      <div className='font-medium'>{t('Dynamic scoring')}</div>
      <p className='text-muted-foreground text-xs'>
        {t(
          'The value in the input is the configured baseline, which editing writes and scoring never overwrites.'
        )}
      </p>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Per-route detail is not available on this instance yet. It reappears once the channel serves traffic again.'
        )}
      </p>
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className='flex items-baseline justify-between gap-3'>
      <dt className='text-muted-foreground shrink-0'>{label}</dt>
      <dd className='text-right'>{value}</dd>
    </div>
  )
}
