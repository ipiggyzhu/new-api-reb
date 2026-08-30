import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { ChannelScoreSummary } from '../types'
import {
  formatOffset,
  hasTierEffect,
  hasWeightEffect,
  tierLabel,
  tierVariant,
  weightLabel,
  weightVariant,
} from './dynamic-score-display'

function summary(
  overrides: Partial<ChannelScoreSummary> = {}
): ChannelScoreSummary {
  return {
    active: 1,
    total: 1,
    adjusted: 0,
    min_offset: 0,
    max_offset: 0,
    weighted: 0,
    min_weight_factor: 0,
    max_weight_factor: 0,
    idle: 0,
    ...overrides,
  }
}

describe('tier and weight are reported independently', () => {
  test('a tier change and a weight change are both visible at once', () => {
    // The bug this guards: folding both into one badge showed weight only when no
    // tier had moved, so a channel demoted AND throttled reported just the
    // demotion — hiding half of what routing was doing to it.
    const both = summary({
      active: 10,
      adjusted: 2,
      min_offset: -1,
      max_offset: -1,
      weighted: 3,
      min_weight_factor: 0.75,
      max_weight_factor: 0.75,
    })
    assert.equal(hasTierEffect(both), true)
    assert.equal(hasWeightEffect(both), true)
    assert.equal(tierLabel(both), '-1 (2/10)')
    assert.equal(weightLabel(both), '0.75x (3/10)')
  })

  test('a scored channel left at its configured position shows neither', () => {
    // Rendering "0" here would invite treating an untouched channel as penalised.
    const neutral = summary({ active: 40, total: 40 })
    assert.equal(hasTierEffect(neutral), false)
    assert.equal(hasWeightEffect(neutral), false)
  })

  test('a weight-only change shows no tier badge', () => {
    const weightOnly = summary({
      active: 4,
      weighted: 1,
      min_weight_factor: 0.5,
      max_weight_factor: 0.5,
    })
    assert.equal(hasTierEffect(weightOnly), false)
    assert.equal(hasWeightEffect(weightOnly), true)
  })

  test('a tier-only change shows no weight badge', () => {
    const tierOnly = summary({
      active: 4,
      adjusted: 1,
      min_offset: -2,
      max_offset: -2,
    })
    assert.equal(hasTierEffect(tierOnly), true)
    assert.equal(hasWeightEffect(tierOnly), false)
  })
})

describe('tierLabel', () => {
  test('carries the denominator so one moved route cannot read as a collapse', () => {
    const one = tierLabel(
      summary({ active: 40, adjusted: 1, min_offset: -3, max_offset: -3 })
    )
    const all = tierLabel(
      summary({ active: 40, adjusted: 40, min_offset: -3, max_offset: -3 })
    )
    assert.equal(one, '-3 (1/40)')
    assert.equal(all, '-3 (40/40)')
    assert.notEqual(one, all)
  })

  test('shows a spread when routes moved in both directions', () => {
    assert.equal(
      tierLabel(
        summary({ active: 9, adjusted: 4, min_offset: -3, max_offset: 1 })
      ),
      '-3…+1 (4/9)'
    )
  })
})

describe('weightLabel', () => {
  test('collapses an identical range and keeps the denominator', () => {
    assert.equal(
      weightLabel(
        summary({
          active: 8,
          weighted: 2,
          min_weight_factor: 0.75,
          max_weight_factor: 0.75,
        })
      ),
      '0.75x (2/8)'
    )
  })

  test('shows a spread across differing factors', () => {
    assert.equal(
      weightLabel(
        summary({
          active: 8,
          weighted: 3,
          min_weight_factor: 0.5,
          max_weight_factor: 1.5,
        })
      ),
      '0.5–1.5x (3/8)'
    )
  })
})

describe('tierVariant', () => {
  test('colours by the worst route, not by the midpoint of the range', () => {
    // One route down three tiers and another up one is not healthy on average;
    // the demotion is the operational signal.
    assert.equal(
      tierVariant(
        summary({ active: 9, adjusted: 4, min_offset: -3, max_offset: 1 })
      ),
      'danger'
    )
  })

  test('one tier down is a warning, two or more is danger', () => {
    assert.equal(
      tierVariant(summary({ adjusted: 1, min_offset: -1, max_offset: -1 })),
      'warning'
    )
    assert.equal(
      tierVariant(summary({ adjusted: 1, min_offset: -2, max_offset: -2 })),
      'danger'
    )
  })

  test('promotion only is success', () => {
    assert.equal(
      tierVariant(summary({ adjusted: 1, min_offset: 1, max_offset: 1 })),
      'success'
    )
  })
})

describe('weightVariant', () => {
  test('below the neutral 1.0 is a penalty', () => {
    // The factor is 0.5 + success rate, so 1.0 is neutral: a 50% success rate
    // changes nothing. Anything under it draws less traffic than configured.
    assert.equal(
      weightVariant(
        summary({ weighted: 1, min_weight_factor: 0.75, max_weight_factor: 0.75 })
      ),
      'danger'
    )
    assert.equal(
      weightVariant(
        summary({ weighted: 1, min_weight_factor: 0.5, max_weight_factor: 0.5 })
      ),
      'danger'
    )
  })

  test('a boost above neutral is success', () => {
    assert.equal(
      weightVariant(
        summary({ weighted: 1, min_weight_factor: 1.5, max_weight_factor: 1.5 })
      ),
      'success'
    )
  })

  test('a mixed range colours by the lowest factor', () => {
    assert.equal(
      weightVariant(
        summary({ weighted: 3, min_weight_factor: 0.5, max_weight_factor: 1.5 })
      ),
      'danger'
    )
  })
})

describe('formatOffset', () => {
  test('signs positive values so +1 cannot be read as 1', () => {
    assert.equal(formatOffset(1), '+1')
    assert.equal(formatOffset(-1), '-1')
    assert.equal(formatOffset(0), '0')
  })
})
