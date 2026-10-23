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
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToUpdatePayload,
} from './channel-form'

function channelWithOtherSettings(settings: Record<string, unknown>) {
  return channelSchema.parse({
    id: 25,
    type: 1,
    name: 'keepalive',
    key: 'test-key',
    status: 1,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    models: 'gpt-test',
    settings: JSON.stringify(settings),
  })
}

describe('upstream keepalive settings', () => {
  test('defaults off with the backend default interval and active window', () => {
    const form = transformChannelToFormDefaults(channelWithOtherSettings({}))
    assert.equal(form.upstream_keepalive_enabled, false)
    assert.equal(form.upstream_keepalive_interval_minutes, 30)
    assert.equal(form.upstream_keepalive_active_window_minutes, 120)
  })

  // The last send time is written by the backend task; an admin save must not
  // drop it, or every saved channel would be due again immediately.
  test('saving keeps the switch, interval and active window and preserves the last send time', () => {
    const form = transformChannelToFormDefaults(
      channelWithOtherSettings({
        upstream_keepalive_enabled: true,
        upstream_keepalive_interval_minutes: 45,
        upstream_keepalive_active_window_minutes: 240,
        upstream_keepalive_last_time: 1_700_000_000,
      })
    )
    assert.equal(form.upstream_keepalive_enabled, true)
    assert.equal(form.upstream_keepalive_interval_minutes, 45)
    assert.equal(form.upstream_keepalive_active_window_minutes, 240)

    form.upstream_keepalive_enabled = false
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).settings ?? '{}'
    )
    assert.equal(saved.upstream_keepalive_enabled, false)
    assert.equal(saved.upstream_keepalive_interval_minutes, 45)
    assert.equal(saved.upstream_keepalive_active_window_minutes, 240)
    assert.equal(saved.upstream_keepalive_last_time, 1_700_000_000)
  })

  test('the interval and active window are bounded like the backend', () => {
    const form = transformChannelToFormDefaults(channelWithOtherSettings({}))
    for (const field of [
      'upstream_keepalive_interval_minutes',
      'upstream_keepalive_active_window_minutes',
    ] as const) {
      for (const [minutes, valid] of [
        [4, false],
        [5, true],
        [10_080, true],
        [10_081, false],
        [7.5, false],
      ] as const) {
        const parsed = channelFormSchema.safeParse({
          ...form,
          [field]: minutes,
        })
        assert.equal(parsed.success, valid, `${field} minutes=${minutes}`)
        if (!valid) {
          assert.ok(
            parsed.error?.issues.some((issue) => issue.path[0] === field)
          )
        }
      }
    }
  })
})
