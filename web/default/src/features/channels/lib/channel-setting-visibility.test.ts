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

import {
  CHANNEL_SETTING_RULES,
  forwardedRequestFieldSettingValue,
  getChannelSettingState,
  getChannelSettingsOverview,
  isRequestFieldForwarded,
  type ChannelSettingKey,
  type ChannelSettingValues,
  type ChannelSettingsCardId,
} from './channel-setting-visibility'

// OpenAI, Anthropic, AWS Bedrock, Vertex AI, Codex, and Gemini as a type that
// is on none of the lists except the model-fetchable one.
const CHANNEL_TYPES = [1, 14, 33, 41, 57, 24]

// One row per option, one column per type above: Y applies, - does not.
const APPLIES: Record<ChannelSettingKey, string> = {
  force_format: 'Y-----',
  thinking_to_content: 'YYYYYY',
  pass_through_body_enabled: 'YYYYYY',
  system_prompt: 'YYYYYY',
  system_prompt_override: 'YYYYYY',
  proxy: 'YYYYYY',
  websocket_transport: 'Y---YY',
  disable_task_polling_sleep: 'YYYYYY',
  upstream_keepalive: 'YYYYYY',
  responses_reasoning_fallback: 'Y---YY',
  claude_tool_result_images: '-YYY--',
  claude_beta_query: '-Y----',
  allow_service_tier: 'YY--Y-',
  allow_inference_geo: 'YY--Y-',
  disable_store: 'Y---Y-',
  allow_safety_identifier: 'Y---Y-',
  allow_include_obfuscation: 'Y---Y-',
  allow_speed: '-Y----',
  upstream_model_detection: 'YY---Y',
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
