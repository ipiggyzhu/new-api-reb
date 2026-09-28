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

describe('channel TLS fingerprint override', () => {
  test('a saved fingerprint reopens as an enabled override and survives saving', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        tls_fingerprint: 'chrome',
      })
    )
    assert.equal(form.tls_fingerprint_enabled, true)
    assert.equal(form.custom_client_identity_enabled, true)
    assert.equal(form.tls_fingerprint, 'chrome')
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.tls_fingerprint, 'chrome')
    assert.equal(saved.synthetic_client_headers_profile, 'claude')
  })

  test('switching the override off saves an empty fingerprint so it follows the headers', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        tls_fingerprint: 'codex-cli',
      })
    )
    form.tls_fingerprint_enabled = false
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.tls_fingerprint, '')
    assert.equal(saved.synthetic_client_headers_profile, 'claude')
  })

  test('a channel without a fingerprint opens with the override off', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(57, { synthetic_client_headers_profile: 'codex' })
    )
    assert.equal(form.tls_fingerprint_enabled, false)
    const saved = JSON.parse(
      transformFormDataToCreatePayload(form).channel.setting ?? '{}'
    )
    assert.equal(saved.tls_fingerprint, '')
  })
})

describe('custom TLS, device and session identity', () => {
  test('a new channel keeps all overrides off', () => {
    const form = transformChannelToFormDefaults(channelWithSettings(14, {}))
    assert.equal(form.custom_client_identity_enabled, false)
    const saved = JSON.parse(
      transformFormDataToCreatePayload(form).channel.setting ?? '{}'
    )
    assert.equal(saved.tls_fingerprint, '')
    assert.equal(saved.client_device_seed, '')
    assert.equal(saved.client_session_seed, '')
  })

  test('device and session configuration survives create, update and reopening', () => {
    const form = transformChannelToFormDefaults(channelWithSettings(14, {}))
    form.custom_client_identity_enabled = true
    form.tls_fingerprint_enabled = true
    form.tls_fingerprint = 'chrome'
    form.client_device_seed = ' device-profile-one '
    form.client_session_seed = ' session-profile-one '
    const created = JSON.parse(
      transformFormDataToCreatePayload(form).channel.setting ?? '{}'
    )
    const reopened = transformChannelToFormDefaults(
      channelWithSettings(14, created)
    )
    assert.equal(reopened.custom_client_identity_enabled, true)
    assert.equal(reopened.client_device_seed, 'device-profile-one')
    assert.equal(reopened.client_session_seed, 'session-profile-one')
    const updated = JSON.parse(
      transformFormDataToUpdatePayload(reopened, 25).setting ?? '{}'
    )
    assert.deepEqual(updated, created)
  })

  test('turning the master switch off resets overrides but keeps the client profile', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        tls_fingerprint: 'chrome',
        client_device_seed: 'device-profile-one',
        client_session_seed: 'session-profile-one',
      })
    )
    form.custom_client_identity_enabled = false
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.tls_fingerprint, '')
    assert.equal(saved.client_device_seed, '')
    assert.equal(saved.client_session_seed, '')
    assert.equal(saved.synthetic_client_headers_profile, 'claude')
    assert.equal(
      transformChannelToFormDefaults(channelWithSettings(14, saved))
        .custom_client_identity_enabled,
      false
    )
  })

  test('rotating a device keeps the session and TLS selections', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        tls_fingerprint: 'firefox',
        client_device_seed: 'device-profile-one',
        client_session_seed: 'session-profile-one',
      })
    )
    form.client_device_seed = 'device-profile-two'
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.client_device_seed, 'device-profile-two')
    assert.equal(saved.client_session_seed, 'session-profile-one')
    assert.equal(saved.tls_fingerprint, 'firefox')
  })
})
