import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { channelSchema, type CLIVersionProfileOption } from '../types'
import {
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from './channel-form'
import {
  keyEnvironmentPool,
  keyEnvironmentRejectionMessage,
  keyEnvironmentUnavailableReason,
  parseKeyEnvironmentRejection,
  saveWithKeyEnvironmentConsent,
} from './channel-key-environment'

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

describe('independent key environments', () => {
  test('the switch and Claude cache mode survive create, update and reopening', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(14, {
        synthetic_client_headers_profile: 'claude',
        key_environments: true,
        key_environment_cache_ttl: '1h',
      })
    )
    assert.equal(form.key_environments, true)
    assert.equal(form.key_environment_cache_ttl, '1h')
    for (const setting of [
      transformFormDataToCreatePayload(form).channel.setting,
      transformFormDataToUpdatePayload(form, 25).setting,
    ]) {
      const saved = JSON.parse(setting ?? '{}')
      assert.equal(saved.key_environments, true)
      assert.equal(saved.key_environment_cache_ttl, '1h')
      const reopened = transformChannelToFormDefaults(
        channelWithSettings(14, saved)
      )
      assert.equal(reopened.key_environments, true)
      assert.equal(reopened.key_environment_cache_ttl, '1h')
    }
  })

  test('a new channel opens with key environments off in the 5m cache mode', () => {
    const form = transformChannelToFormDefaults(channelWithSettings(14, {}))
    assert.equal(form.key_environments, false)
    assert.equal(form.key_environment_cache_ttl, '5m')
    const saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.key_environments, false)
    assert.equal(saved.key_environment_cache_ttl, '')
  })

  test('the cache mode is saved only for Claude channels with key environments on', () => {
    const cases: {
      type: number
      profile: string
      keyEnvironments: boolean
      chosen: '5m' | '1h'
      ttl: string
    }[] = [
      {
        type: 14,
        profile: 'claude',
        keyEnvironments: true,
        chosen: '5m',
        ttl: '',
      },
      {
        type: 1,
        profile: 'codex',
        keyEnvironments: true,
        chosen: '1h',
        ttl: '',
      },
      {
        type: 14,
        profile: 'claude',
        keyEnvironments: false,
        chosen: '1h',
        ttl: '',
      },
    ]
    for (const c of cases) {
      const form = transformChannelToFormDefaults(
        channelWithSettings(c.type, {
          synthetic_client_headers_profile: c.profile,
          key_environments: c.keyEnvironments,
          key_environment_cache_ttl: '1h',
        })
      )
      form.key_environment_cache_ttl = c.chosen
      const saved = JSON.parse(
        transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
      )
      assert.equal(saved.key_environments, c.keyEnvironments)
      assert.equal(saved.key_environment_cache_ttl, c.ttl)
    }
  })

  test('key environments preserve the channel TLS setting and clear it when synthesis is off', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(1, {
        synthetic_client_headers_profile: 'codex',
        key_environments: true,
        tls_fingerprint: 'codex-cli',
      })
    )
    let saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.key_environments, true)
    assert.equal(saved.tls_fingerprint, 'codex-cli')

    form.synthetic_client_headers_profile = 'off'
    saved = JSON.parse(
      transformFormDataToUpdatePayload(form, 25).setting ?? '{}'
    )
    assert.equal(saved.key_environments, false)
  })

  test('key environments accept TLS and WebSocket and preserve them when saving', () => {
    const form = transformChannelToFormDefaults(
      channelWithSettings(1, {
        synthetic_client_headers_profile: 'codex',
        cli_version_profile: 'codex-test',
        key_environments: true,
        tls_fingerprint: 'claude-code',
        websocket_transport: true,
      })
    )
    assert.equal(channelFormSchema.safeParse(form).success, true)
    for (const setting of [
      transformFormDataToCreatePayload(form).channel.setting,
      transformFormDataToUpdatePayload(form, 25).setting,
    ]) {
      const saved = JSON.parse(setting ?? '{}')
      assert.equal(saved.websocket_transport, true)
      assert.equal(saved.key_environments, true)
      assert.equal(saved.tls_fingerprint, 'claude-code')
    }
  })
  test('the switch is available only where the backend accepts it', () => {
    const cases: {
      family: string
      channelType: number
      multiKey: boolean
      reason: string | undefined
    }[] = [
      { family: 'claude', channelType: 14, multiKey: true, reason: undefined },
      { family: 'codex', channelType: 1, multiKey: true, reason: undefined },
      { family: 'codex', channelType: 57, multiKey: true, reason: undefined },
      {
        family: 'off',
        channelType: 14,
        multiKey: true,
        reason:
          'Independent key environments need the Claude Code or Codex client profile',
      },
      {
        family: 'claude',
        channelType: 1,
        multiKey: true,
        reason: 'Claude key environments need an Anthropic channel',
      },
      {
        family: 'codex',
        channelType: 14,
        multiKey: true,
        reason: 'Codex key environments need an OpenAI channel',
      },
      {
        family: 'claude',
        channelType: 14,
        multiKey: false,
        reason: 'Independent key environments need a multi-key channel',
      },
    ]
    for (const c of cases) {
      assert.equal(keyEnvironmentUnavailableReason(c), c.reason)
    }
  })
})

describe('key environment version pool', () => {
  const profile = (
    id: string,
    version: string,
    extra: Partial<CLIVersionProfileOption> = {}
  ): CLIVersionProfileOption => ({
    id,
    family: id.split('-')[0],
    version,
    label: id,
    ja3_md5: '',
    auto_assign: true,
    ...extra,
  })
  const profiles = [
    profile('claude-2.1.99-linux-x64', '2.1.99'),
    profile('claude-2.1.306-linux-x64', '2.1.306'),
    profile('claude-2.1.306-linux-x64-1h', '2.1.306', { cache_ttl: '1h' }),
    profile('claude-2.1.198-windows-x64', '2.1.198', { auto_assign: false }),
    profile('codex-0.156.1-windows-x64', '0.156.1'),
    profile('codex-0.163.0-linux-x64', '0.163.0'),
  ]
  const ids = (list: CLIVersionProfileOption[]) => list.map((p) => p.id)

  test('only auto-assignable captures of the family and cache mode, newest first', () => {
    assert.deepEqual(ids(keyEnvironmentPool(profiles, 'claude', '5m')), [
      'claude-2.1.306-linux-x64',
      'claude-2.1.99-linux-x64',
    ])
    assert.deepEqual(ids(keyEnvironmentPool(profiles, 'claude', '1h')), [
      'claude-2.1.306-linux-x64-1h',
    ])
    assert.deepEqual(ids(keyEnvironmentPool(profiles, 'codex', '1h')), [
      'codex-0.163.0-linux-x64',
      'codex-0.156.1-windows-x64',
    ])
  })
})

describe('key environment save rejections', () => {
  test('a shortage reads from the axios error and names keys 1-based', () => {
    const rejection = parseKeyEnvironmentRejection({
      response: {
        status: 409,
        data: {
          success: false,
          code: 'key_environment_shortage',
          data: {
            available: 3,
            keys: 5,
            unassigned_indexes: [3, 4],
            versions: ['0.163.0', '0.160.0', '0.156.1'],
          },
        },
      },
    })
    assert.deepEqual(rejection, {
      code: 'key_environment_shortage',
      available: 3,
      keys: 5,
      unassignedIndexes: [3, 4],
      versions: ['0.163.0', '0.160.0', '0.156.1'],
    })
    assert.ok(rejection)
    assert.deepEqual(keyEnvironmentRejectionMessage(rejection).params, {
      available: 3,
      versions: '0.163.0, 0.160.0, 0.156.1',
      keys: 5,
      indexes: '4, 5',
    })
  })

  test('a key owned by another channel names that channel', () => {
    const rejection = parseKeyEnvironmentRejection({
      success: false,
      code: 'key_environment_conflict',
      data: { index: 0, channel_id: 42, channel_name: 'pool-a' },
    })
    assert.ok(rejection)
    assert.deepEqual(keyEnvironmentRejectionMessage(rejection).params, {
      index: 1,
      name: 'pool-a',
      id: 42,
    })
  })

  test('a repeated key names both lines', () => {
    const rejection = parseKeyEnvironmentRejection({
      response: {
        data: {
          code: 'key_environment_duplicate',
          data: { index: 2, first_index: 0 },
        },
      },
    })
    assert.ok(rejection)
    assert.deepEqual(keyEnvironmentRejectionMessage(rejection).params, {
      index: 3,
      first: 1,
    })
  })

  test('other failures are not mistaken for key environment rejections', () => {
    for (const error of [
      new Error('network'),
      { response: { data: { success: false, message: 'bad' } } },
      { code: 'ERR_BAD_REQUEST' },
      undefined,
    ]) {
      assert.equal(parseKeyEnvironmentRejection(error), undefined)
    }
  })
})

describe('saving a key environment channel', () => {
  const rejected = (status: number, code: string, data: unknown) =>
    Object.assign(new Error('rejected'), {
      response: { status, data: { success: false, code, data } },
    })
  const shortage = rejected(409, 'key_environment_shortage', {
    available: 1,
    keys: 2,
    unassigned_indexes: [1],
    versions: ['2.1.306'],
  })
  const translate = (key: string, params: Record<string, string | number>) =>
    `${key} ${JSON.stringify(params)}`

  for (const confirmed of [true, false]) {
    test(`a shortage asks once and ${confirmed ? 'resends with allow_unassigned' : 'stays unsaved when declined'}`, async () => {
      const sent: boolean[] = []
      const asked: unknown[] = []
      const result = await saveWithKeyEnvironmentConsent(
        async (allowUnassigned) => {
          sent.push(allowUnassigned)
          if (!allowUnassigned) throw shortage
          return 'Channel updated'
        },
        async (rejection) => {
          asked.push(rejection.unassignedIndexes)
          return confirmed
        },
        translate
      )
      assert.deepEqual(asked, [[1]])
      assert.deepEqual(sent, confirmed ? [false, true] : [false])
      assert.equal(result, confirmed ? 'Channel updated' : '')
    })
  }

  test('a conflicting key is reported without asking or resending', async () => {
    const sent: boolean[] = []
    await assert.rejects(
      saveWithKeyEnvironmentConsent(
        async (allowUnassigned) => {
          sent.push(allowUnassigned)
          throw rejected(409, 'key_environment_conflict', {
            index: 1,
            channel_id: 7,
            channel_name: 'pool-a',
          })
        },
        async () => assert.fail('a conflict must not ask for consent'),
        translate
      ),
      (error: Error) =>
        error.message.startsWith('Key #{{index}} is already used') &&
        error.message.includes('"index":2') &&
        error.message.includes('"id":7')
    )
    assert.deepEqual(sent, [false])
  })

  test('other failures pass through untouched', async () => {
    const failure = new Error('network down')
    await assert.rejects(
      saveWithKeyEnvironmentConsent(
        async () => {
          throw failure
        },
        async () => assert.fail('must not ask'),
        translate
      ),
      (error) => error === failure
    )
  })
})
