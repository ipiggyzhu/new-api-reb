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
import { MODEL_FETCHABLE_TYPES } from '../constants'
import { CHANNEL_TYPE_ADVANCED_CUSTOM as ADVANCED_CUSTOM } from './advanced-custom'
import type { ChannelFormValues } from './channel-form'

/**
 * How one channel-level option is shown for the channel type being edited.
 *
 * - applicable: the option does something on this channel type.
 * - inactive: it does not, but it is set (or was when the channel was loaded),
 *   so it stays on screen, marked, until the admin turns it off.
 * - hidden: it does not and it is at its default, so it is left out.
 */
export type ChannelSettingState = 'applicable' | 'inactive' | 'hidden'

type ChannelSettingRule =
  | 'all'
  | { only: readonly number[] }
  | { except: readonly number[] }

/**
 * The channel types each option applies to, one line per option: `only` lists
 * the types it applies to, `except` the types it does not.
 *
 * These lists are the proven minimum. Narrow an option only on evidence of
 * where the backend reads it; anything unproven stays 'all'.
 *
 * 1 OpenAI, 14 Anthropic, 33 AWS Bedrock, 41 Vertex AI, 57 Codex.
 */
export const CHANNEL_SETTING_RULES = {
  force_format: { only: [1] },
  thinking_to_content: 'all',
  pass_through_body_enabled: 'all',
  system_prompt: 'all',
  system_prompt_override: 'all',
  proxy: 'all',
  websocket_transport: { except: [14, 33, 41] },
  disable_task_polling_sleep: 'all',
  upstream_keepalive: 'all',
  responses_reasoning_fallback: { except: [14, 33, 41] },
  claude_tool_result_images: { only: [14, 33, 41, ADVANCED_CUSTOM] },
  claude_beta_query: { only: [14] },
  allow_service_tier: { only: [1, 14, 57] },
  allow_inference_geo: { only: [1, 14, 57] },
  disable_store: { only: [1, 57] },
  allow_safety_identifier: { only: [1, 57] },
  allow_include_obfuscation: { only: [1, 57] },
  allow_speed: { only: [14] },
  upstream_model_detection: { only: [...MODEL_FETCHABLE_TYPES] },
} satisfies Record<string, ChannelSettingRule>

export type ChannelSettingKey = keyof typeof CHANNEL_SETTING_RULES

const CHANNEL_SETTING_KEYS = Object.keys(
  CHANNEL_SETTING_RULES
) as ChannelSettingKey[]

/**
 * Whether a value differs from the option's default: a switch that is on, a
 * choice other than 'off' (the form's stand-in for the backend's empty
 * string), or text that is not blank.
 */
function isNonDefaultSettingValue(
  key: ChannelSettingKey,
  value: unknown
): boolean {
  if (typeof value !== 'string') return value === true
  const text = value.trim()
  if (key === 'claude_tool_result_images') return text !== '' && text !== 'off'
  return text !== ''
}

export function getChannelSettingState(
  key: ChannelSettingKey,
  channelType: number,
  currentValue: unknown,
  loadedValue: unknown
): ChannelSettingState {
  const rule: ChannelSettingRule = CHANNEL_SETTING_RULES[key]
  if (rule === 'all') return 'applicable'
  if ('only' in rule && rule.only.includes(channelType)) return 'applicable'
  if ('except' in rule && !rule.except.includes(channelType)) {
    return 'applicable'
  }

  // The loaded value counts too, so that a row does not vanish the moment its
  // switch is turned off, before the change has even been saved.
  if (
    isNonDefaultSettingValue(key, currentValue) ||
    isNonDefaultSettingValue(key, loadedValue)
  ) {
    return 'inactive'
  }
  return 'hidden'
}

// ============================================================================
// Request fields forwarded upstream
// ============================================================================

/**
 * Request fields the gateway filters, in display order, each with the stored
 * setting that controls it.
 */
export const FORWARDED_REQUEST_FIELDS = [
  { field: 'service_tier', setting: 'allow_service_tier' },
  { field: 'store', setting: 'disable_store' },
  { field: 'safety_identifier', setting: 'allow_safety_identifier' },
  {
    field: 'stream_options.include_obfuscation',
    setting: 'allow_include_obfuscation',
  },
  { field: 'inference_geo', setting: 'allow_inference_geo' },
  { field: 'speed', setting: 'allow_speed' },
] as const

export type ForwardedRequestFieldSetting =
  (typeof FORWARDED_REQUEST_FIELDS)[number]['setting']

/**
 * Whether a row shows its field as forwarded. Every row reads "forward this
 * field: on/off"; `store` is the one field forwarded by default, so its stored
 * setting (`disable_store`) is the inverse of what the row shows.
 */
export function isRequestFieldForwarded(
  setting: ForwardedRequestFieldSetting,
  storedValue: boolean | undefined
): boolean {
  if (setting === 'disable_store') return storedValue !== true
  return storedValue === true
}

/** The value to store when a row is switched to `forwarded`. */
export function forwardedRequestFieldSettingValue(
  setting: ForwardedRequestFieldSetting,
  forwarded: boolean
): boolean {
  if (setting === 'disable_store') return !forwarded
  return forwarded
}

// ============================================================================
// Cards
// ============================================================================

export type ChannelSettingsCard =
  | 'connection'
  | 'clientIdentity'
  | 'requestRewriting'
  | 'sessionCompat'
  | 'upstreamModelDetection'

/** The options on each card. Client identity is not governed by the rules. */
const CHANNEL_SETTINGS_CARD_OPTIONS = {
  connection: [
    'proxy',
    'websocket_transport',
    'disable_task_polling_sleep',
    'upstream_keepalive',
  ],
  requestRewriting: [
    'pass_through_body_enabled',
    'system_prompt',
    'system_prompt_override',
    'allow_service_tier',
    'disable_store',
    'allow_safety_identifier',
    'allow_include_obfuscation',
    'allow_inference_geo',
    'allow_speed',
    'claude_beta_query',
    'force_format',
    'thinking_to_content',
  ],
  sessionCompat: ['responses_reasoning_fallback', 'claude_tool_result_images'],
  upstreamModelDetection: ['upstream_model_detection'],
} satisfies Partial<Record<ChannelSettingsCard, ChannelSettingKey[]>>

/**
 * The form values the options are read from. Every option is the form field of
 * the same name, except the two that stand for a group of fields.
 */
export type ChannelSettingValues = Partial<
  Pick<
    ChannelFormValues,
    | Exclude<
        ChannelSettingKey,
        'upstream_keepalive' | 'upstream_model_detection'
      >
    | 'upstream_keepalive_enabled'
    | 'upstream_model_update_check_enabled'
    | 'upstream_model_update_auto_sync_enabled'
    | 'upstream_model_update_ignored_models'
    | 'synthetic_client_headers_profile'
    | 'custom_client_identity_enabled'
  >
>

export type ChannelSettingStates = Record<
  ChannelSettingKey,
  ChannelSettingState
>

type ChannelSettingsCardSummary = {
  /** The card has at least one row to show. */
  visible: boolean
  /** Something on the card is set. */
  configured: boolean
}

export type ChannelSettingsOverview = {
  states: ChannelSettingStates
  /** Options that are set but do nothing on this channel type. */
  inactiveCount: number
  cards: Record<ChannelSettingsCard, ChannelSettingsCardSummary>
}

function readSettingValue(
  key: ChannelSettingKey,
  values: ChannelSettingValues | undefined
): unknown {
  if (!values) return undefined
  if (key === 'upstream_keepalive') return values.upstream_keepalive_enabled
  if (key === 'upstream_model_detection') {
    return Boolean(
      values.upstream_model_update_check_enabled ||
      values.upstream_model_update_auto_sync_enabled ||
      values.upstream_model_update_ignored_models?.trim()
    )
  }
  return values[key]
}

export function hasVisibleChannelSetting(
  states: ChannelSettingStates,
  keys: readonly ChannelSettingKey[]
): boolean {
  return keys.some((key) => states[key] !== 'hidden')
}

function summarizeCard(
  options: readonly ChannelSettingKey[],
  states: ChannelSettingStates,
  values: ChannelSettingValues
): ChannelSettingsCardSummary {
  return {
    visible: hasVisibleChannelSetting(states, options),
    configured: options.some((key) =>
      isNonDefaultSettingValue(key, readSettingValue(key, values))
    ),
  }
}

/**
 * Everything the editor derives from the rules for one channel: the state of
 * each option, and for each card whether it has anything to show and anything
 * set.
 *
 * `loadedValues` are the values the channel was opened with; pass undefined
 * for a channel that is being created.
 */
export function getChannelSettingsOverview(
  channelType: number,
  values: ChannelSettingValues,
  loadedValues: ChannelSettingValues | undefined
): ChannelSettingsOverview {
  const states = {} as ChannelSettingStates
  for (const key of CHANNEL_SETTING_KEYS) {
    states[key] = getChannelSettingState(
      key,
      channelType,
      readSettingValue(key, values),
      readSettingValue(key, loadedValues)
    )
  }

  const options = CHANNEL_SETTINGS_CARD_OPTIONS
  return {
    states,
    inactiveCount: CHANNEL_SETTING_KEYS.filter(
      (key) => states[key] === 'inactive'
    ).length,
    cards: {
      connection: summarizeCard(options.connection, states, values),
      clientIdentity: {
        visible: true,
        configured:
          (values.synthetic_client_headers_profile ?? 'off') !== 'off' ||
          values.custom_client_identity_enabled === true,
      },
      requestRewriting: summarizeCard(options.requestRewriting, states, values),
      sessionCompat: summarizeCard(options.sessionCompat, states, values),
      upstreamModelDetection: summarizeCard(
        options.upstreamModelDetection,
        states,
        values
      ),
    },
  }
}
