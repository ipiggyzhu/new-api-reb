import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'

import {
  formatOffset,
  hasTierEffect,
  hasWeightEffect,
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
  summary: ChannelScoreSummary
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
            className='cursor-pointer shrink-0'
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
        <DynamicScoreDetail summary={summary} />
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
          'Applies to this request only. The configured priority and weight are unchanged.'
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

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className='flex items-baseline justify-between gap-3'>
      <dt className='text-muted-foreground shrink-0'>{label}</dt>
      <dd className='text-right'>{value}</dd>
    </div>
  )
}
