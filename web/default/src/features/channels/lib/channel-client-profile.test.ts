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

describe('Responses reasoning compatibility setting', () => {
  test('defaults off and survives create, reopen, update and disable', () => {
    const form = transformChannelToFormDefaults(channelWithSettings(1, {}))
    assert.equal(form.responses_reasoning_fallback, false)
    form.responses_reasoning_fallback = true
    const created = JSON.parse(
      transformFormDataToCreatePayload(form).channel.setting ?? '{}'
    )
    assert.equal(created.responses_reasoning_fallback, true)
    const reopened = transformChannelToFormDefaults(
      channelWithSettings(1, created)
    )
    assert.equal(reopened.responses_reasoning_fallback, true)
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(reopened, 25).setting ?? '{}'
    )
    assert.equal(saved.responses_reasoning_fallback, true)
    reopened.responses_reasoning_fallback = false
    const disabled = JSON.parse(
      transformFormDataToUpdatePayload(reopened, 25).setting ?? '{}'
    )
    assert.equal(disabled.responses_reasoning_fallback, false)
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
    assert.equal(form.custom_client_identity_enabled, false)
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

describe('device and session identity independent of TLS', () => {
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
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, { synthetic_client_headers_profile: 'claude' })
    )
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

  test('turning identities off resets seeds but preserves legacy TLS and the client profile', () => {
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
    assert.equal(saved.tls_fingerprint, 'chrome')
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
        synthetic_client_headers_profile: 'claude',
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

describe('independent synthesis controls', () => {
  test('enabling custom controls alone leaves default identities and TLS intact', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
      })
    )
    const before = transformFormDataToUpdatePayload(form, 25).setting
    form.custom_client_identity_enabled = true
    assert.equal(transformFormDataToUpdatePayload(form, 25).setting, before)
    form.tls_fingerprint_enabled = true
    form.tls_fingerprint = 'claude-node-22.14.0'
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.tls_fingerprint, 'claude-node-22.14.0')
    assert.equal(saved.client_device_seed, '')
    assert.equal(saved.client_session_seed, '')
  })

  test('synthesis off clears every override even with custom controls left on', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        tls_fingerprint: 'claude-node-22.14.0',
        client_device_seed: 'device-one',
        client_session_seed: 'session-one',
      })
    )
    form.synthetic_client_headers_profile = 'off'
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.synthetic_client_headers, false)
    assert.equal(saved.synthetic_client_headers_profile, '')
    assert.equal(saved.tls_fingerprint, '')
    assert.equal(saved.client_device_seed, '')
    assert.equal(saved.client_session_seed, '')
  })
})

describe('versioned CLI channel profiles', () => {
  test('version selection survives saving and reopening independently of custom identity', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(57, {
        synthetic_client_headers_profile: 'codex',
        cli_version_profile: 'codex-0.160.0-windows-x64',
      })
    )
    assert.equal(form.cli_version_profile, 'codex-0.160.0-windows-x64')
    form.custom_client_identity_enabled = false
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.cli_version_profile, 'codex-0.160.0-windows-x64')
    assert.equal(saved.tls_fingerprint, '')
    assert.equal(
      transformChannelToFormDefaults(channelWithSettings(57, saved))
        .cli_version_profile,
      form.cli_version_profile
    )
  })

  test('a complete profile cannot be saved with an independent TLS override', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        cli_version_profile: 'claude-2.1.198-windows-x64',
        tls_fingerprint: 'codex-cli',
      })
    )
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.cli_version_profile, 'claude-2.1.198-windows-x64')
    assert.equal(saved.tls_fingerprint, '')
  })

  test('switching family or disabling synthesis does not retain a mixed-family version', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        cli_version_profile: 'claude-2.1.282-windows-x64',
      })
    )
    for (const family of ['codex', 'off'] as const) {
      form.synthetic_client_headers_profile = family
      const saved = JSON.parse(
        transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
      )
      assert.equal(saved.cli_version_profile, '')
    }
  })
})

describe('channel settings preservation', () => {
  test('editing a channel preserves settings outside the form while updating managed values', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(1, {
        future_option: { enabled: true, values: [0, false, 'kept'] },
        future_flag: false,
        force_format: true,
      })
    )
    form.force_format = false
    for (const setting of [
      transformFormDataToCreatePayload(form).channel.setting,
      transformFormDataToUpdatePayload(form, 25).setting,
    ]) {
      const saved = JSON.parse(setting ?? '{}')
      assert.deepEqual(saved.future_option, {
        enabled: true,
        values: [0, false, 'kept'],
      })
      assert.equal(saved.future_flag, false)
      assert.equal(saved.force_format, false)
    }
  })
})

describe('versioned CLI transport validation', () => {
  for (const family of ['claude', 'codex'] as const) {
    test(`${family} version rejects WebSocket before submission`, () => {
      const form = transformChannelToFormDefaults(
        channelWithSettings(1, {
          synthetic_client_headers_profile: family,
          cli_version_profile: `${family}-test`,
          websocket_transport: true,
        })
      )
      const parsed = channelFormSchema.safeParse(form)
      assert.equal(parsed.success, false)
      assert.ok(
        parsed.error?.issues.some(
          (issue) => issue.path[0] === 'cli_version_profile'
        )
      )
      form.websocket_transport = false
      assert.equal(channelFormSchema.safeParse(form).success, true)
    })
  }

  test('WebSocket remains available when the version will not be saved', () => {
    for (const family of ['off', 'codex'] as const) {
      const form = transformChannelToFormDefaults(
        channelWithSettings(1, {
          synthetic_client_headers_profile: family,
          cli_version_profile: 'claude-test',
          websocket_transport: true,
        })
      )
      assert.equal(channelFormSchema.safeParse(form).success, true)
    }
  })
})
