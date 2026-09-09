import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { channelSchema } from '../types'
import {
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from './channel-form'

function channelWithSettings(type: number, settings: Record<string, unknown>) {
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
    setting: JSON.stringify(settings),
  })
}

describe('explicit channel client profiles', () => {
  const legacyFamilies: [number, string][] = [
    [1, 'openai'],
    [3, 'openai'],
    [14, 'claude'],
    [33, 'claude'],
    [41, 'claude'],
    [57, 'codex'],
    [24, 'gemini'],
    [20, 'generic'],
    [58, 'generic'],
    [999, 'openai'],
  ]
  for (const [type, expected] of legacyFamilies) {
    test(`opening and saving old type ${type} fixes its former profile`, () => {
      const form = transformChannelToFormDefaults(
        channelWithSettings(type, {
          synthetic_client_headers_profile: 'auto',
          synthetic_client_headers: true,
          pass_through_body_enabled: true,
        })
      )
      assert.equal(form.synthetic_client_headers_profile, expected)
      const saved = JSON.parse(
        transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
      )
      assert.equal(saved.synthetic_client_headers_profile, expected)
      assert.equal(saved.synthetic_client_headers, true)
      assert.equal(saved.pass_through_body_enabled, true)
    })
  }

  test('old boolean and unknown values retain an explicit profile', () => {
    for (const settings of [
      { synthetic_client_headers: true },
      { synthetic_client_headers_profile: 'unknown-old-profile' },
    ]) {
      const form = transformChannelToFormDefaults(
        channelWithSettings(14, settings)
      )
      assert.equal(form.synthetic_client_headers_profile, 'claude')
    }
  })

  test('a chosen profile survives changing the channel type', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(1, { synthetic_client_headers_profile: 'codex' })
    )
    form.type = 14
    const saved = JSON.parse(
      transformFormDataToCreatePayload(form).channel.setting ?? '{}'
    )
    assert.equal(saved.synthetic_client_headers_profile, 'codex')
    assert.equal(saved.synthetic_client_headers, true)
  })

  test('turning a migrated profile off clears the legacy boolean too', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(1, { synthetic_client_headers: true })
    )
    form.synthetic_client_headers_profile = 'off'
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.synthetic_client_headers_profile, '')
    assert.equal(saved.synthetic_client_headers, false)
    assert.equal(
      transformChannelToFormDefaults(channelWithSettings(1, saved))
        .synthetic_client_headers_profile,
      'off'
    )
  })

  test('new form submissions cannot select auto', () => {
    const form = transformChannelToFormDefaults(channelWithSettings(1, {}))
    const parsed = channelFormSchema.safeParse({
      ...form,
      synthetic_client_headers_profile: 'auto',
    })
    assert.equal(parsed.success, false)
    assert.ok(
      parsed.error?.issues.some(
        (issue) => issue.path[0] === 'synthetic_client_headers_profile'
      )
    )
  })
})
