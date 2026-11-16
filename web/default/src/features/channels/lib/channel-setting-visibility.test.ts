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

import { createInstance } from 'i18next'

import { channelSchema } from '../types'
import {
  transformChannelToFormDefaults,
  transformFormDataToUpdatePayload,
} from './channel-form'
import {
  CHANNEL_SETTING_RULES,
  CLEARED_ON_SAVE,
  forwardedRequestFieldSettingValue,
  getChannelSettingState,
  getChannelSettingsOverview,
  getInactiveSettingNotice,
  getModelDetectionNotice,
  isRequestFieldForwarded,
  summarizeAdvancedSettings,
  type ChannelSettingKey,
  type ChannelSettingValues,
  type ChannelSettingsCardId,
} from './channel-setting-visibility'

// OpenAI, Custom (an OpenAI-compatible relay), Anthropic, Ali, Gemini, AWS
// Bedrock, Vertex AI, Jimeng, Codex, and Baidu as a type whose adaptor has none
// of the code paths the narrowed options need.
const CHANNEL_TYPES = [1, 8, 14, 17, 24, 33, 41, 51, 57, 15]

// One row per option, one column per type above: Y applies, - does not.
const APPLIES: Record<ChannelSettingKey, string> = {
  force_format: 'YY-YY-YY--',
  thinking_to_content: 'YY-YY-YY--',
  pass_through_body_enabled: 'YYYYYYYYYY',
  system_prompt: 'YYYYYYYYY-',
  system_prompt_override: 'YYYYYYYYY-',
  proxy: 'YYYYYYYYYY',
  websocket_transport: 'YY------Y-',
  disable_task_polling_sleep: 'Y--YY-YY--',
  upstream_keepalive: 'YYYYYYYYYY',
  responses_reasoning_fallback: 'YY-Y----Y-',
  claude_tool_result_images: '--YY-YY---',
  claude_beta_query: '--Y-------',
  allow_service_tier: 'Y-Y-----Y-',
  allow_inference_geo: 'Y-Y-----Y-',
  disable_store: 'Y-------Y-',
  allow_safety_identifier: 'Y-------Y-',
  allow_include_obfuscation: 'Y-------Y-',
  allow_speed: '--Y-------',
  upstream_model_detection: 'Y-YYY-----',
}

function setValue(key: ChannelSettingKey): unknown {
  if (key === 'claude_tool_result_images') return 'hoist'
  if (key === 'proxy' || key === 'system_prompt') return 'value'
  return true
}

function defaultValue(key: ChannelSettingKey): unknown {
  if (key === 'claude_tool_result_images') return 'off'
  if (key === 'proxy' || key === 'system_prompt') return ''
  return false
}

describe('channel setting rules', () => {
  for (const key of Object.keys(CHANNEL_SETTING_RULES) as ChannelSettingKey[]) {
    test(`${key} applies to the expected channel types`, () => {
      const on = setValue(key)
      const off = defaultValue(key)
      CHANNEL_TYPES.forEach((channelType, column) => {
        const applies = APPLIES[key][column] === 'Y'
        const whenSet = applies ? 'applicable' : 'inactive'
        const note = `type ${channelType}`

        assert.equal(
          getChannelSettingState(key, channelType, off, off),
          applies ? 'applicable' : 'hidden',
          note
        )
        assert.equal(
          getChannelSettingState(key, channelType, on, on),
          whenSet,
          note
        )
        // Switched on in this session.
        assert.equal(
          getChannelSettingState(key, channelType, on, off),
          whenSet,
          note
        )
        // Switched off but not saved yet: the row must stay.
        assert.equal(
          getChannelSettingState(key, channelType, off, on),
          whenSet,
          note
        )
      })
    })
  }

  test('an advanced custom channel offers both old-session options', () => {
    assert.equal(
      getChannelSettingState('claude_tool_result_images', 58, 'off', 'off'),
      'applicable'
    )
    assert.equal(
      getChannelSettingState('responses_reasoning_fallback', 58, false, false),
      'applicable'
    )
    // Its routes can answer in the OpenAI format, but it never takes the
    // Responses WebSocket path.
    assert.equal(
      getChannelSettingState('force_format', 58, false, false),
      'applicable'
    )
    assert.equal(
      getChannelSettingState('websocket_transport', 58, false, false),
      'hidden'
    )
  })

  test('the task platforms that share the OpenAI API type keep WebSocket transport', () => {
    // Nothing in the backend rules them out, so they are not narrowed.
    for (const channelType of [2, 5, 36, 50, 52, 54, 55]) {
      assert.equal(
        getChannelSettingState(
          'websocket_transport',
          channelType,
          false,
          false
        ),
        'applicable',
        `type ${channelType}`
      )
    }
  })

  test('values that are absent or blank count as the default', () => {
    for (const value of [undefined, null, false, '', 'off']) {
      assert.equal(
        getChannelSettingState('claude_tool_result_images', 1, value, value),
        'hidden'
      )
    }
    for (const value of ['hoist', 'placeholder']) {
      assert.equal(
        getChannelSettingState('claude_tool_result_images', 1, value, 'off'),
        'inactive'
      )
    }
  })
})

describe('forwarded request fields', () => {
  test('store is forwarded unless disable_store is set', () => {
    assert.equal(isRequestFieldForwarded('disable_store', undefined), true)
    assert.equal(isRequestFieldForwarded('disable_store', false), true)
    assert.equal(isRequestFieldForwarded('disable_store', true), false)
    assert.equal(
      forwardedRequestFieldSettingValue('disable_store', true),
      false
    )
    assert.equal(
      forwardedRequestFieldSettingValue('disable_store', false),
      true
    )
  })

  test('every other field is forwarded only when its setting is on', () => {
    for (const setting of [
      'allow_service_tier',
      'allow_safety_identifier',
      'allow_include_obfuscation',
      'allow_inference_geo',
      'allow_speed',
    ] as const) {
      assert.equal(isRequestFieldForwarded(setting, undefined), false)
      assert.equal(isRequestFieldForwarded(setting, false), false)
      assert.equal(isRequestFieldForwarded(setting, true), true)
      assert.equal(forwardedRequestFieldSettingValue(setting, true), true)
      assert.equal(forwardedRequestFieldSettingValue(setting, false), false)
    }
  })
})

describe('channel settings overview', () => {
  test('counts the options that are set on a type that ignores them', () => {
    const loaded: ChannelSettingValues = {
      responses_reasoning_fallback: true,
      websocket_transport: true,
      claude_tool_result_images: 'hoist',
    }
    const onAnthropic = getChannelSettingsOverview(14, loaded, loaded)
    assert.equal(onAnthropic.inactiveCount, 2)
    assert.deepEqual(onAnthropic.settings.responses_reasoning_fallback, {
      state: 'inactive',
      isSet: true,
      clearedOnSave: false,
    })
    assert.equal(onAnthropic.settings.websocket_transport.state, 'inactive')
    assert.equal(
      onAnthropic.settings.claude_tool_result_images.state,
      'applicable'
    )

    const onOpenAI = getChannelSettingsOverview(1, loaded, loaded)
    assert.equal(onOpenAI.inactiveCount, 1)
    assert.equal(onOpenAI.settings.claude_tool_result_images.state, 'inactive')
  })

  test('an option switched off stays on screen but is no longer counted as set', () => {
    const loaded: ChannelSettingValues = { responses_reasoning_fallback: true }
    const current: ChannelSettingValues = {
      responses_reasoning_fallback: false,
    }

    const editing = getChannelSettingsOverview(14, current, loaded)
    assert.deepEqual(editing.settings.responses_reasoning_fallback, {
      state: 'inactive',
      isSet: false,
      clearedOnSave: false,
    })
    assert.equal(editing.inactiveCount, 0)
    assert.equal(editing.cards.sessionCompat.visible, true)
    assert.equal(editing.cards.sessionCompat.configured, false)

    // Nothing is stored for a channel that is still being created.
    const creating = getChannelSettingsOverview(14, current, undefined)
    assert.equal(creating.settings.responses_reasoning_fallback.state, 'hidden')
  })

  test('a card is shown when it has a row and hidden when it has none', () => {
    const defaults = getChannelSettingsOverview(33, {}, {})
    assert.deepEqual(
      Object.entries(defaults.cards).map(([card, summary]) => [
        card,
        summary.visible,
      ]),
      [
        ['connection', true],
        ['clientIdentity', true],
        ['requestRewriting', true],
        ['sessionCompat', true],
        ['upstreamModelDetection', false],
      ]
    )

    const detectionSet: ChannelSettingValues = {
      upstream_model_update_check_enabled: true,
    }
    const withDetection = getChannelSettingsOverview(
      33,
      detectionSet,
      detectionSet
    )
    assert.equal(withDetection.cards.upstreamModelDetection.visible, true)
    assert.equal(
      withDetection.settings.upstream_model_detection.state,
      'inactive'
    )
    assert.equal(
      getChannelSettingsOverview(14, {}, {}).cards.upstreamModelDetection
        .visible,
      true
    )
  })

  const configuredBy: [ChannelSettingsCardId, ChannelSettingValues][] = [
    ['connection', { proxy: 'socks5://127.0.0.1:1080' }],
    ['connection', { websocket_transport: true }],
    ['connection', { disable_task_polling_sleep: true }],
    ['connection', { upstream_keepalive_enabled: true }],
    ['clientIdentity', { synthetic_client_headers_profile: 'codex' }],
    ['clientIdentity', { custom_client_identity_enabled: true }],
    ['requestRewriting', { pass_through_body_enabled: true }],
    ['requestRewriting', { system_prompt: 'Answer briefly.' }],
    ['requestRewriting', { system_prompt_override: true }],
    ['requestRewriting', { allow_service_tier: true }],
    ['requestRewriting', { disable_store: true }],
    ['requestRewriting', { allow_safety_identifier: true }],
    ['requestRewriting', { allow_include_obfuscation: true }],
    ['requestRewriting', { allow_inference_geo: true }],
    ['requestRewriting', { allow_speed: true }],
    ['requestRewriting', { claude_beta_query: true }],
    ['requestRewriting', { force_format: true }],
    ['requestRewriting', { thinking_to_content: true }],
    ['sessionCompat', { responses_reasoning_fallback: true }],
    ['sessionCompat', { claude_tool_result_images: 'placeholder' }],
    ['upstreamModelDetection', { upstream_model_update_check_enabled: true }],
    [
      'upstreamModelDetection',
      { upstream_model_update_auto_sync_enabled: true },
    ],
    [
      'upstreamModelDetection',
      { upstream_model_update_ignored_models: 'gpt-test' },
    ],
  ]
  for (const [card, values] of configuredBy) {
    test(`${Object.keys(values)[0]} marks only ${card} as configured`, () => {
      const cards = getChannelSettingsOverview(1, values, values).cards
      assert.deepEqual(
        Object.entries(cards)
          .filter(([, summary]) => summary.configured)
          .map(([name]) => name),
        [card]
      )
    })
  }

  test('defaults and blank text leave every card unconfigured', () => {
    const cards = getChannelSettingsOverview(
      1,
      {
        proxy: '  ',
        system_prompt: '',
        claude_tool_result_images: 'off',
        synthetic_client_headers_profile: 'off',
        upstream_model_update_ignored_models: ' ',
        disable_store: false,
      },
      undefined
    ).cards
    assert.deepEqual(
      Object.values(cards).map((summary) => summary.configured),
      [false, false, false, false, false]
    )
  })
})

// The same options as the app: keys are whole English sentences, and the
// section titles are inserted as they are.
async function translator(lng: string) {
  const i18n = createInstance()
  await i18n.init({
    lng,
    nsSeparator: false,
    interpolation: { escapeValue: false },
    resources: {
      zh: {
        translation: {
          'Configured: {{sections}}': '已配置：{{sections}}',
          '{{count}} not used by this channel type':
            '{{count}} 项对此渠道类型无效',
        },
      },
    },
  })
  return i18n.t
}

/** Baidu: on none of the lists, so every narrowed option is unused on it. */
const UNLISTED_TYPE = 15

/** The options kept in the `settings` blob; the others are in `setting`. */
const IN_SETTINGS_BLOB = new Set<ChannelSettingKey>([
  ...CLEARED_ON_SAVE,
  'disable_task_polling_sleep',
])

/** What an untouched save sends back for a channel that has `key` set. */
function savedValue(key: ChannelSettingKey, channelType: number): unknown {
  const stored = { [key]: setValue(key) }
  const inSettings = IN_SETTINGS_BLOB.has(key)
  const channel = channelSchema.parse({
    id: 25,
    type: channelType,
    name: 'agent',
    key: 'test-key',
    status: 1,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    models: 'gpt-test',
    setting: JSON.stringify(inSettings ? {} : stored),
    settings: JSON.stringify(inSettings ? stored : {}),
  })
  const payload = transformFormDataToUpdatePayload(
    transformChannelToFormDefaults(channel),
    channel.id
  )
  const blob = inSettings ? payload.settings : payload.setting
  return JSON.parse(blob ?? '{}')[key]
}

describe('options the editor clears on save', () => {
  const narrowedKeys = (
    Object.keys(CHANNEL_SETTING_RULES) as ChannelSettingKey[]
  ).filter(
    (key) =>
      key !== 'upstream_model_detection' &&
      getChannelSettingState(key, UNLISTED_TYPE, undefined, undefined) ===
        'hidden'
  )

  test('are the six forwarded request fields and the beta query', () => {
    assert.deepEqual([...CLEARED_ON_SAVE].sort(), [
      'allow_include_obfuscation',
      'allow_inference_geo',
      'allow_safety_identifier',
      'allow_service_tier',
      'allow_speed',
      'claude_beta_query',
      'disable_store',
    ])
    for (const key of CLEARED_ON_SAVE) assert.ok(narrowedKeys.includes(key))
  })

  // The sentence "will be cleared when you save" is only true where the
  // payload builder really deletes the option, and "you can turn it off" is
  // only needed where it does not.
  for (const key of narrowedKeys) {
    const cleared = CLEARED_ON_SAVE.has(key)
    test(`${key} set on a type that does not use it is ${cleared ? 'deleted' : 'kept'} by a save`, () => {
      assert.equal(
        savedValue(key, UNLISTED_TYPE),
        cleared ? undefined : setValue(key)
      )

      const values: ChannelSettingValues = { [key]: setValue(key) }
      assert.deepEqual(
        getChannelSettingsOverview(UNLISTED_TYPE, values, values).settings[key],
        { state: 'inactive', isSet: true, clearedOnSave: cleared }
      )
    })
  }

  test('nothing is marked as cleared on a type that uses the option', () => {
    const values: ChannelSettingValues = {
      allow_service_tier: true,
      claude_beta_query: true,
    }
    const settings = getChannelSettingsOverview(14, values, values).settings
    assert.equal(settings.allow_service_tier.clearedOnSave, false)
    assert.equal(settings.claude_beta_query.clearedOnSave, false)
    assert.equal(savedValue('claude_beta_query', 14), true)
  })
})

describe('notice around an option the channel type does not use', () => {
  const reason = 'Only Anthropic channels use this query parameter.'

  test('says an option the save deletes will be cleared', async () => {
    const t = await translator('en')
    assert.deepEqual(
      getInactiveSettingNotice(
        { state: 'inactive', isSet: true, clearedOnSave: true },
        reason,
        t
      ),
      {
        badge: 'Not used by this channel type',
        text: `${reason} It is still set and will be cleared when you save.`,
      }
    )
  })

  test('says any other option can be turned off', async () => {
    const t = await translator('en')
    assert.equal(
      getInactiveSettingNotice(
        { state: 'inactive', isSet: true, clearedOnSave: false },
        reason,
        t
      ).text,
      `${reason} It is still set. You can turn it off.`
    )
  })

  test('says neither once the option has been switched off', async () => {
    const t = await translator('en')
    for (const clearedOnSave of [true, false]) {
      assert.equal(
        getInactiveSettingNotice(
          { state: 'inactive', isSet: false, clearedOnSave },
          reason,
          t
        ).text,
        reason
      )
    }
  })

  test('falls back to a general reason', async () => {
    const t = await translator('en')
    assert.equal(
      getInactiveSettingNotice(
        { state: 'inactive', isSet: false, clearedOnSave: true },
        undefined,
        t
      ).text,
      'Not normally offered for this channel type.'
    )
  })
})

describe('notice around model detection on a type it is not offered for', () => {
  test('warns that the gateway keeps checking while the check is on', async () => {
    assert.deepEqual(getModelDetectionNotice(true, await translator('en')), {
      badge: 'Not offered for this channel type',
      text: 'Model detection is not offered for this channel type, but it is still switched on and the gateway keeps checking this upstream on schedule. Turn it off unless this upstream answers /v1/models.',
    })
  })

  test('only says it is not offered once the check is off', async () => {
    assert.deepEqual(getModelDetectionNotice(false, await translator('en')), {
      badge: 'Not offered for this channel type',
      text: 'Model detection is not offered for this channel type.',
    })
  })

  test('counts the check as still on only when it was stored on and still is', () => {
    const on: ChannelSettingValues = {
      upstream_model_update_check_enabled: true,
    }
    const off: ChannelSettingValues = {
      upstream_model_update_check_enabled: false,
    }
    // A leftover ignore list keeps the card on screen with the check off.
    const leftover: ChannelSettingValues = {
      upstream_model_update_check_enabled: false,
      upstream_model_update_auto_sync_enabled: true,
      upstream_model_update_ignored_models: 'gpt-test',
    }
    const stillOn = (
      values: ChannelSettingValues,
      loaded: ChannelSettingValues | undefined
    ) => getChannelSettingsOverview(33, values, loaded).modelCheckStillOn

    assert.equal(stillOn(on, on), true)
    // Switched off in the form: the save will store it off.
    assert.equal(stillOn(off, on), false)
    assert.equal(stillOn(leftover, leftover), false)
    // Switched on in the form on a type that does not offer it: the backend
    // has it off, and the save does not store it.
    assert.equal(stillOn(on, off), false)
    assert.equal(stillOn(on, leftover), false)
    // A channel that is being created has nothing stored.
    assert.equal(stillOn(on, undefined), false)
  })

  test('is not counted among the options the channel type does not use', () => {
    // AWS Bedrock: not model-fetchable, but the backend scans it all the same.
    const values: ChannelSettingValues = {
      upstream_model_update_check_enabled: true,
      responses_reasoning_fallback: true,
    }
    const overview = getChannelSettingsOverview(33, values, values)
    assert.equal(overview.settings.upstream_model_detection.state, 'inactive')
    assert.equal(overview.settings.upstream_model_detection.isSet, true)
    assert.equal(overview.inactiveCount, 1)
  })
})

describe('collapsed advanced settings summary', () => {
  test('lists the configured sections in order', async () => {
    const t = await translator('en')
    assert.equal(
      summarizeAdvancedSettings(
        ['Routing Strategy', 'Connection & keepalive', 'Client identity'],
        0,
        t
      ),
      'Configured: Routing Strategy · Connection & keepalive · Client identity'
    )
  })

  test('adds how many options do nothing on this channel type', async () => {
    assert.equal(
      summarizeAdvancedSettings(
        ['Client identity', 'Session compatibility'],
        1,
        await translator('en')
      ),
      'Configured: Client identity · Session compatibility · 1 not used by this channel type'
    )
    assert.equal(
      summarizeAdvancedSettings(
        ['客户端身份', '旧会话兼容'],
        2,
        await translator('zh')
      ),
      '已配置：客户端身份 · 旧会话兼容 · 2 项对此渠道类型无效'
    )
  })

  test('is absent when nothing is configured', async () => {
    assert.equal(
      summarizeAdvancedSettings([], 0, await translator('en')),
      undefined
    )
  })
})
