/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { channelSchema } from '../types'
import {
  transformChannelToFormDefaults,
  transformFormDataToUpdatePayload,
  type ChannelFormValues,
} from './channel-form'

// AWS Bedrock (33), Custom (8) and Codex (57) are not model-fetchable, so the
// editor does not offer model detection for them. The backend's scheduled scan
// does not look at the channel type: it runs wherever the check is stored on.

function storedChannel(type: number, settings: Record<string, unknown>) {
  return channelSchema.parse({
    id: 25,
    type,
    name: 'agent',
    key: 'test-key',
    status: 1,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    models: 'gpt-test',
    setting: '{}',
    settings: JSON.stringify(settings),
  })
}

/** The `settings` a save sends back, after `edit` has been made in the form. */
function savedSettings(
  type: number,
  settings: Record<string, unknown>,
  edit: Partial<ChannelFormValues> = {}
): Record<string, unknown> {
  const channel = storedChannel(type, settings)
  const payload = transformFormDataToUpdatePayload(
    { ...transformChannelToFormDefaults(channel), ...edit },
    channel.id
  )
  return JSON.parse(payload.settings ?? '')
}

function detectionKeys(settings: Record<string, unknown>): string[] {
  return Object.keys(settings)
    .filter((key) => key.startsWith('upstream_model_update_'))
    .sort()
}

// What the backend stores after a scan with auto-sync off: the DTO drops
// false booleans, so there is no auto-sync key.
const SCANNED = {
  aws_key_type: 'ak_sk',
  upstream_model_update_check_enabled: true,
  upstream_model_update_last_check_time: 1763000000,
  upstream_model_update_last_detected_models: ['model-new'],
  upstream_model_update_ignored_models: ['model-skip'],
}

// The keys every save writes, whatever the channel type.
const ALWAYS_WRITTEN = {
  disable_task_polling_sleep: false,
  upstream_keepalive_enabled: false,
  upstream_keepalive_interval_minutes: 30,
  upstream_keepalive_active_window_minutes: 120,
}

describe('model detection stored on, on a type the editor does not offer it for', () => {
  test('an untouched save sends the stored settings back as they were', () => {
    assert.deepEqual(savedSettings(33, SCANNED), {
      ...SCANNED,
      ...ALWAYS_WRITTEN,
    })
    // Nothing is added: the auto-sync key the backend left out stays out.
    assert.deepEqual(detectionKeys(savedSettings(33, SCANNED)), [
      'upstream_model_update_check_enabled',
      'upstream_model_update_ignored_models',
      'upstream_model_update_last_check_time',
      'upstream_model_update_last_detected_models',
    ])
    // The serialised text is the same too, key order included.
    const channel = storedChannel(33, SCANNED)
    assert.equal(
      transformFormDataToUpdatePayload(
        transformChannelToFormDefaults(channel),
        channel.id
      ).settings,
      JSON.stringify({ ...SCANNED, ...ALWAYS_WRITTEN })
    )

    for (const autoSync of [true, false]) {
      const stored = {
        ...SCANNED,
        upstream_model_update_auto_sync_enabled: autoSync,
      }
      assert.deepEqual(savedSettings(33, stored), {
        ...stored,
        ...ALWAYS_WRITTEN,
      })
    }
  })

  test('switching the check off is saved, and nothing else changes', () => {
    assert.deepEqual(
      savedSettings(33, SCANNED, {
        upstream_model_update_check_enabled: false,
      }),
      {
        ...SCANNED,
        ...ALWAYS_WRITTEN,
        upstream_model_update_check_enabled: false,
      }
    )
    for (const type of [8, 57]) {
      const saved = savedSettings(
        type,
        { upstream_model_update_check_enabled: true },
        { upstream_model_update_check_enabled: false }
      )
      assert.equal(saved.upstream_model_update_check_enabled, false)
      assert.deepEqual(detectionKeys(saved), [
        'upstream_model_update_check_enabled',
      ])
    }
  })

  test('auto-sync goes off with the check, as on the types that offer it', () => {
    const saved = savedSettings(
      33,
      { ...SCANNED, upstream_model_update_auto_sync_enabled: true },
      { upstream_model_update_check_enabled: false }
    )
    assert.equal(saved.upstream_model_update_check_enabled, false)
    assert.equal(saved.upstream_model_update_auto_sync_enabled, false)
  })

  test('the auto-sync switch is saved while the check stays on', () => {
    assert.equal(
      savedSettings(33, SCANNED, {
        upstream_model_update_auto_sync_enabled: true,
      }).upstream_model_update_auto_sync_enabled,
      true
    )
    assert.equal(
      savedSettings(
        33,
        { ...SCANNED, upstream_model_update_auto_sync_enabled: true },
        { upstream_model_update_auto_sync_enabled: false }
      ).upstream_model_update_auto_sync_enabled,
      false
    )
  })
})

describe('model detection not stored on, on a type the editor does not offer it for', () => {
  const storedWithoutTheCheck: Record<string, unknown>[] = [
    {},
    { upstream_model_update_check_enabled: false },
    // Leftovers without the check: nothing the scheduled scan acts on.
    {
      upstream_model_update_auto_sync_enabled: true,
      upstream_model_update_ignored_models: ['model-skip'],
    },
  ]

  test('a save adds no model detection key and changes none', () => {
    for (const stored of storedWithoutTheCheck) {
      for (const type of [33, 8, 57]) {
        const saved = savedSettings(type, stored)
        assert.deepEqual(detectionKeys(saved), detectionKeys(stored))
        for (const key of detectionKeys(stored)) {
          assert.deepEqual(saved[key], stored[key], key)
        }
      }
    }
  })

  test('not even when the switches are turned on in the form', () => {
    for (const stored of storedWithoutTheCheck) {
      const saved = savedSettings(33, stored, {
        upstream_model_update_check_enabled: true,
        upstream_model_update_auto_sync_enabled: true,
      })
      assert.deepEqual(detectionKeys(saved), detectionKeys(stored))
      for (const key of detectionKeys(stored)) {
        assert.deepEqual(saved[key], stored[key], key)
      }
    }
  })
})

describe('model detection on a type the editor offers it for', () => {
  test('is written in full, as before', () => {
    // Anthropic (14) is model-fetchable.
    const saved = savedSettings(14, {
      upstream_model_update_check_enabled: true,
      upstream_model_update_last_check_time: 1763000000,
      upstream_model_update_last_detected_models: ['model-new'],
      upstream_model_update_ignored_models: ['model-skip'],
    })
    assert.deepEqual(
      detectionKeys(saved).map((key) => [key, saved[key]]),
      [
        ['upstream_model_update_auto_sync_enabled', false],
        ['upstream_model_update_check_enabled', true],
        ['upstream_model_update_ignored_models', ['model-skip']],
        ['upstream_model_update_last_check_time', 1763000000],
        ['upstream_model_update_last_detected_models', ['model-new']],
      ]
    )

    const off = savedSettings(
      14,
      {
        upstream_model_update_check_enabled: true,
        upstream_model_update_last_detected_models: ['model-new'],
      },
      { upstream_model_update_check_enabled: false }
    )
    assert.equal(off.upstream_model_update_check_enabled, false)
    assert.deepEqual(off.upstream_model_update_last_detected_models, [])
  })
})
