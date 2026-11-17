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
import type { TFunction } from 'i18next'

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

// The longer lists of channel type ids used by the rules below, each named for
// what the backend code establishes about the types on it.

/** None of their adaptors answers through the OpenAI-format response handlers. */
const NO_OPENAI_FORMAT_ANSWER = [
  4, 11, 14, 15, 16, 18, 21, 23, 33, 34, 37, 38, 39, 44, 48, 49, 56, 57,
]

/** No request path of theirs applies a channel system prompt. */
const NO_SYSTEM_PROMPT_PATH = [15, 16, 21, 23, 34, 37, 38, 44, 49, 56]

/** Their API type is neither OpenAI nor Codex. */
const NOT_OPENAI_OR_CODEX_API = [
  4, 11, 14, 15, 16, 17, 18, 20, 21, 23, 24, 25, 26, 27, 33, 34, 35, 37, 38, 39,
  40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 51, 53, 56, 58,
]

/** They have a video task adaptor. */
const VIDEO_TASK_TYPES = [1, 17, 24, 35, 41, 45, 50, 51, 52, 54, 55]

/** They can never have a Responses upstream. */
const NO_RESPONSES_UPSTREAM = [
  4, 11, 14, 15, 16, 18, 21, 23, 24, 25, 26, 33, 34, 35, 37, 38, 41, 43, 44, 46,
  49, 51,
]

/** Their ConvertClaudeRequest can return a body in the Claude Messages shape. */
const CLAUDE_BODY_TYPES = [14, 17, 25, 26, 33, 35, 41, 43, 45, ADVANCED_CUSTOM]

/**
 * The channel types each option applies to, one line per option: `only` lists
 * the types it applies to, `except` the types it does not.
 *
 * Each rule comes from where the backend reads the option, and the comment
 * above it names that code. An option is narrowed only on that evidence and
 * stays 'all' otherwise.
 *
 * Two kinds of rule mirror this editor instead. The six forwarded request
 * fields follow the payload builder (buildSettingsJSON in ./channel-form),
 * which stores those switches only for the listed types and deletes them on
 * any other. Model detection follows MODEL_FETCHABLE_TYPES, the types the
 * editor can fetch a model list for.
 *
 * force_format is deliberately wider than "OpenAI (type 1) only": the backend
 * honours it on every OpenAI-compatible type, and with the old restriction
 * the notice claimed it was unused on types where it was in fact applied.
 *
 * 1 OpenAI, 14 Anthropic, 57 Codex; CHANNEL_TYPES in ../constants names the
 * other ids.
 */
export const CHANNEL_SETTING_RULES = {
  // Read only by the handlers that answer in the OpenAI format:
  // OaiStreamHandler and OpenaiHandler in relay/channel/openai/relay-openai.go,
  // handleStream in relay/channel/gemini/relay-gemini.go.
  force_format: { except: NO_OPENAI_FORMAT_ANSWER },
  // Read by the streaming ones of those handlers: OaiStreamHandler, handleStream.
  thinking_to_content: { except: NO_OPENAI_FORMAT_ANSWER },
  pass_through_body_enabled: 'all',
  // Applied only by ClaudeHelper (relay/claude_handler.go), GeminiHelper
  // (relay/gemini_handler.go), applySystemPromptIfNeeded as called from
  // TextHelper (relay/compatible_handler.go), and ConvertOpenAIResponsesRequest
  // in relay/channel/codex/adaptor.go.
  system_prompt: { except: NO_SYSTEM_PROMPT_PATH },
  system_prompt_override: { except: NO_SYSTEM_PROMPT_PATH },
  proxy: 'all',
  // ShouldTryResponsesWebsocket in relay/channel/websocket_transport.go only
  // proceeds for the OpenAI and Codex API types (ChannelType2APIType in
  // common/api_type.go).
  websocket_transport: { except: NOT_OPENAI_OR_CODEX_API },
  // Read once, in updateVideoTasks (service/task_polling.go), which needs a
  // task adaptor from GetTaskAdaptor (relay/relay_adaptor.go).
  disable_task_polling_sleep: { only: VIDEO_TASK_TYPES },
  upstream_keepalive: 'all',
  // Read in doUpstreamRoundTrip (relay/compatible_handler.go), in the
  // Responses relay mode only.
  responses_reasoning_fallback: { except: NO_RESPONSES_UPSTREAM },
  // Read in ClaudeHelper (relay/claude_handler.go), on the body returned by
  // the adaptor's ConvertClaudeRequest.
  claude_tool_result_images: { only: CLAUDE_BODY_TYPES },
  // Read by shouldAppendClaudeBetaQuery in relay/channel/claude/adaptor.go.
  claude_beta_query: { only: [14] },
  // The six forwarded request fields, as buildSettingsJSON stores them.
  allow_service_tier: { only: [1, 14, 57] },
  allow_inference_geo: { only: [1, 14, 57] },
  disable_store: { only: [1, 57] },
  allow_safety_identifier: { only: [1, 57] },
  allow_include_obfuscation: { only: [1, 57] },
  allow_speed: { only: [14] },
  // Offered where the editor can fetch the upstream's model list.
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

/**
 * Options the payload builder (buildSettingsJSON in ./channel-form) deletes on
 * save on every channel type outside their list. One of these that is still
 * set needs no action from the admin: saving the channel clears it.
 */
export const CLEARED_ON_SAVE: ReadonlySet<ChannelSettingKey> =
  new Set<ChannelSettingKey>([
    ...FORWARDED_REQUEST_FIELDS.map((row) => row.setting),
    'claude_beta_query',
  ])

// ============================================================================
// Cards
// ============================================================================

/** The settings cards of the channel editor, in display order. */
export const CHANNEL_SETTINGS_CARDS = [
  'connection',
  'clientIdentity',
  'requestRewriting',
  'sessionCompat',
  'upstreamModelDetection',
] as const

export type ChannelSettingsCardId = (typeof CHANNEL_SETTINGS_CARDS)[number]

/** The two halves of the rewriting card, each under its own sub-heading. */
export const CHANNEL_REWRITING_OPTIONS = {
  request: [
    'pass_through_body_enabled',
    'system_prompt',
    'system_prompt_override',
    ...FORWARDED_REQUEST_FIELDS.map((row) => row.setting),
    'claude_beta_query',
  ],
  response: ['force_format', 'thinking_to_content'],
} satisfies Record<string, ChannelSettingKey[]>

/**
 * Whether the rewriting card labels its halves "Request" and "Response". It
 * does only when both have a row to show; a single half needs no label.
 */
export function showRewritingSubHeadings(
  settings: ChannelSettingViews
): boolean {
  return (
    hasVisibleChannelSetting(settings, CHANNEL_REWRITING_OPTIONS.request) &&
    hasVisibleChannelSetting(settings, CHANNEL_REWRITING_OPTIONS.response)
  )
}

/** The options on each card. Client identity is not governed by the rules. */
const CHANNEL_SETTINGS_CARD_OPTIONS = {
  connection: [
    'proxy',
    'websocket_transport',
    'disable_task_polling_sleep',
    'upstream_keepalive',
  ],
  requestRewriting: [
    ...CHANNEL_REWRITING_OPTIONS.request,
    ...CHANNEL_REWRITING_OPTIONS.response,
  ],
  sessionCompat: ['responses_reasoning_fallback', 'claude_tool_result_images'],
  upstreamModelDetection: ['upstream_model_detection'],
} satisfies Partial<Record<ChannelSettingsCardId, ChannelSettingKey[]>>

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

/** How one option is shown, and whether it is set right now. */
export type ChannelSettingView = {
  state: ChannelSettingState
  /**
   * The current value is not the default. An inactive option that has just
   * been switched off stays on screen, because it was set when the channel was
   * loaded, but it is no longer set.
   */
  isSet: boolean
  /**
   * The option does nothing on this channel type and saving the channel
   * deletes it (see CLEARED_ON_SAVE).
   */
  clearedOnSave: boolean
}

export type ChannelSettingViews = Record<ChannelSettingKey, ChannelSettingView>

type ChannelSettingsCardSummary = {
  /** The card has at least one row to show. */
  visible: boolean
  /** Something on the card is set. */
  configured: boolean
}

export type ChannelSettingsOverview = {
  settings: ChannelSettingViews
  /** Options that are set but do nothing on this channel type. */
  inactiveCount: number
  /**
   * The model check switch was on when the channel was loaded and is still on
   * in the form. The backend's scheduled scan looks only at that stored
   * switch, so this is when it keeps checking the upstream, whatever the
   * channel type.
   */
  modelCheckStillOn: boolean
  cards: Record<ChannelSettingsCardId, ChannelSettingsCardSummary>
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
  settings: ChannelSettingViews,
  keys: readonly ChannelSettingKey[]
): boolean {
  return keys.some((key) => settings[key].state !== 'hidden')
}

function summarizeCard(
  options: readonly ChannelSettingKey[],
  settings: ChannelSettingViews
): ChannelSettingsCardSummary {
  return {
    visible: hasVisibleChannelSetting(settings, options),
    configured: options.some((key) => settings[key].isSet),
  }
}

/**
 * Everything the editor derives from the rules for one channel: how each
 * option is shown, and for each card whether it has anything to show and
 * anything set.
 *
 * `loadedValues` are the values the channel was opened with; pass undefined
 * for a channel that is being created.
 */
export function getChannelSettingsOverview(
  channelType: number,
  values: ChannelSettingValues,
  loadedValues: ChannelSettingValues | undefined
): ChannelSettingsOverview {
  const settings = {} as ChannelSettingViews
  for (const key of CHANNEL_SETTING_KEYS) {
    const value = readSettingValue(key, values)
    const state = getChannelSettingState(
      key,
      channelType,
      value,
      readSettingValue(key, loadedValues)
    )
    settings[key] = {
      state,
      isSet: isNonDefaultSettingValue(key, value),
      clearedOnSave: state === 'inactive' && CLEARED_ON_SAVE.has(key),
    }
  }

  const options = CHANNEL_SETTINGS_CARD_OPTIONS
  return {
    settings,
    // Model detection is left out of the count: the backend's scheduled scan
    // does not look at the channel type, so on a type where the editor does
    // not offer it, it is still not "unused".
    inactiveCount: CHANNEL_SETTING_KEYS.filter(
      (key) =>
        key !== 'upstream_model_detection' &&
        settings[key].state === 'inactive' &&
        settings[key].isSet
    ).length,
    modelCheckStillOn:
      values.upstream_model_update_check_enabled === true &&
      loadedValues?.upstream_model_update_check_enabled === true,
    cards: {
      connection: summarizeCard(options.connection, settings),
      clientIdentity: {
        visible: true,
        configured:
          (values.synthetic_client_headers_profile ?? 'off') !== 'off' ||
          values.custom_client_identity_enabled === true,
      },
      requestRewriting: summarizeCard(options.requestRewriting, settings),
      sessionCompat: summarizeCard(options.sessionCompat, settings),
      upstreamModelDetection: summarizeCard(
        options.upstreamModelDetection,
        settings
      ),
    },
  }
}

// ============================================================================
// System prompt scope
// ============================================================================

/** The requests a channel system prompt is applied to on a channel type. */
export type SystemPromptScope = 'messages' | 'responses' | 'chatAndMessages'

// Where the backend applies the channel system prompt (the code named above
// the system_prompt rule), for the types where that is worth a line of help.

/** Anthropic, AWS Bedrock: on /v1/messages only (ClaudeHelper). */
const SYSTEM_PROMPT_ON_MESSAGES_ONLY = new Set([14, 33])

/** Codex: on /v1/responses, inside its adaptor. */
const SYSTEM_PROMPT_ON_RESPONSES = new Set([57])

/**
 * The chat types served by the OpenAI adaptor: on /v1/chat/completions
 * (TextHelper) and /v1/messages (ClaudeHelper); the Responses handler never
 * applies it.
 */
const SYSTEM_PROMPT_ON_CHAT_AND_MESSAGES = new Set([
  1, 3, 6, 7, 8, 9, 10, 12, 13, 19, 20, 22, 31, 47,
])

/**
 * Which requests the channel system prompt is applied to on this channel
 * type, where the editor says so under the option; undefined for the types it
 * has no such line for.
 */
export function getSystemPromptScope(
  channelType: number
): SystemPromptScope | undefined {
  if (SYSTEM_PROMPT_ON_MESSAGES_ONLY.has(channelType)) return 'messages'
  if (SYSTEM_PROMPT_ON_RESPONSES.has(channelType)) return 'responses'
  if (SYSTEM_PROMPT_ON_CHAT_AND_MESSAGES.has(channelType)) {
    return 'chatAndMessages'
  }
  return undefined
}

// ============================================================================
// Notices
// ============================================================================

/** What an amber notice says: its badge and the sentence next to it. */
export type ChannelSettingNotice = { badge: string; text: string }

/**
 * The notice around an option that is set on a channel type that does not use
 * it: why it does nothing there and, while it is still set, what happens to it.
 */
export function getInactiveSettingNotice(
  setting: ChannelSettingView,
  reason: string | undefined,
  t: TFunction
): ChannelSettingNotice {
  const sentences = [reason ?? t('Not normally offered for this channel type.')]
  if (setting.isSet) {
    sentences.push(
      setting.clearedOnSave
        ? t('It is still set and will be cleared when you save.')
        : t('It is still set. You can turn it off.')
    )
  }
  return {
    badge: t('Not used by this channel type'),
    text: sentences.join(' '),
  }
}

/**
 * The notice around the model detection card on a channel type the editor
 * does not offer it for. It never says "not used": the backend's scheduled
 * scan (controller/channel_upstream_update.go) looks only at the stored
 * `upstream_model_update_check_enabled` switch, not at the channel type. So
 * `checkStillOn` must be that switch (`modelCheckStillOn` of the overview),
 * not the card's composite value.
 */
export function getModelDetectionNotice(
  checkStillOn: boolean,
  t: TFunction
): ChannelSettingNotice {
  return {
    badge: t('Not offered for this channel type'),
    text: checkStillOn
      ? t(
          'Model detection is not offered for this channel type, but it is still switched on and the gateway keeps checking this upstream on schedule. Turn it off unless this upstream answers /v1/models.'
        )
      : t('Model detection is not offered for this channel type.'),
  }
}

/**
 * The line shown under "Advanced Settings" while that area is collapsed: the
 * sections that have something set and, when there are any, how many options
 * are set without doing anything on this channel type. Undefined when nothing
 * is set, so that the caller falls back to its general description.
 */
export function summarizeAdvancedSettings(
  configuredSectionTitles: string[],
  inactiveCount: number,
  t: TFunction
): string | undefined {
  if (configuredSectionTitles.length === 0) return undefined
  const summary = t('Configured: {{sections}}', {
    sections: configuredSectionTitles.join(' · '),
  })
  if (inactiveCount === 0) return summary
  const inactive = t('{{count}} not used by this channel type', {
    count: inactiveCount,
  })
  return `${summary} · ${inactive}`
}
