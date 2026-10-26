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
import type { CLIVersionProfileOption } from '../types'

const ANTHROPIC_CHANNEL_TYPE = 14
const OPENAI_CHANNEL_TYPE = 1
const CODEX_CHANNEL_TYPE = 57

/**
 * Why a channel cannot run independent key environments, as an i18n key, or
 * undefined when it can. Mirrors the backend's save validation.
 */
export function keyEnvironmentUnavailableReason(params: {
  family: string | undefined
  channelType: number | undefined
  multiKey: boolean
}): string | undefined {
  if (params.family !== 'claude' && params.family !== 'codex') {
    return 'Independent key environments need the Claude Code or Codex client profile'
  }
  if (
    params.family === 'claude' &&
    params.channelType !== ANTHROPIC_CHANNEL_TYPE
  ) {
    return 'Claude key environments need an Anthropic channel'
  }
  if (
    params.family === 'codex' &&
    params.channelType !== OPENAI_CHANNEL_TYPE &&
    params.channelType !== CODEX_CHANNEL_TYPE
  ) {
    return 'Codex key environments need an OpenAI channel'
  }
  if (!params.multiKey) {
    return 'Independent key environments need a multi-key channel'
  }
  return undefined
}

/**
 * The captured versions the backend draws key environments from, newest
 * first: auto-assignable profiles of the family, and for Claude only the ones
 * captured in the chosen cache mode.
 */
export function keyEnvironmentPool(
  profiles: CLIVersionProfileOption[],
  family: string,
  cacheTTL: '5m' | '1h'
): CLIVersionProfileOption[] {
  const ttl = cacheTTL === '1h' ? '1h' : ''
  return profiles
    .filter(
      (profile) =>
        profile.auto_assign === true &&
        profile.family === family &&
        (family !== 'claude' || (profile.cache_ttl ?? '') === ttl)
    )
    .sort((a, b) => compareVersions(b.version, a.version))
}

function compareVersions(a: string, b: string): number {
  const left = a.split('.').map(Number)
  const right = b.split('.').map(Number)
  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const diff = (left[i] ?? 0) - (right[i] ?? 0)
    if (diff !== 0) return diff
  }
  return 0
}

export type KeyEnvironmentRejection =
  | { code: 'key_environment_duplicate'; index: number; firstIndex: number }
  | {
      code: 'key_environment_conflict'
      index: number
      channelId: number
      channelName: string
    }
  | {
      code: 'key_environment_shortage'
      available: number
      keys: number
      unassignedIndexes: number[]
      versions: string[]
    }

export type KeyEnvironmentShortage = Extract<
  KeyEnvironmentRejection,
  { code: 'key_environment_shortage' }
>

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

/**
 * Reads a key environment rejection (400 duplicate, 409 conflict or shortage)
 * out of a failed channel save, from either the axios error or the body.
 * Indexes stay 0-based as the backend sends them.
 */
export function parseKeyEnvironmentRejection(
  error: unknown
): KeyEnvironmentRejection | undefined {
  const body =
    isRecord(error) && isRecord(error.response) ? error.response.data : error
  if (!isRecord(body) || typeof body.code !== 'string') return undefined
  const data = isRecord(body.data) ? body.data : {}
  const num = (value: unknown) => (typeof value === 'number' ? value : 0)
  switch (body.code) {
    case 'key_environment_duplicate':
      return {
        code: body.code,
        index: num(data.index),
        firstIndex: num(data.first_index),
      }
    case 'key_environment_conflict':
      return {
        code: body.code,
        index: num(data.index),
        channelId: num(data.channel_id),
        channelName:
          typeof data.channel_name === 'string' ? data.channel_name : '',
      }
    case 'key_environment_shortage':
      return {
        code: body.code,
        available: num(data.available),
        keys: num(data.keys),
        unassignedIndexes: Array.isArray(data.unassigned_indexes)
          ? data.unassigned_indexes.filter(
              (value): value is number => typeof value === 'number'
            )
          : [],
        versions: Array.isArray(data.versions)
          ? data.versions.filter(
              (value): value is string => typeof value === 'string'
            )
          : [],
      }
  }
  return undefined
}

/** The i18n key and parameters that explain a rejection to the admin. */
export function keyEnvironmentRejectionMessage(
  rejection: KeyEnvironmentRejection
): { key: string; params: Record<string, string | number> } {
  switch (rejection.code) {
    case 'key_environment_duplicate':
      return {
        key: 'Key #{{index}} repeats key #{{first}}. Each key can appear only once in an independent key environment channel.',
        params: { index: rejection.index + 1, first: rejection.firstIndex + 1 },
      }
    case 'key_environment_conflict':
      return {
        key: 'Key #{{index}} is already used by the independent key environment channel "{{name}}" (#{{id}}). A key can belong to only one independent environment; remove it from that channel first, or use a different key.',
        params: {
          index: rejection.index + 1,
          name: rejection.channelName,
          id: rejection.channelId,
        },
      }
    case 'key_environment_shortage':
      return {
        key: 'This client has only {{available}} real captured versions ({{versions}}), and this channel has {{keys}} keys. Key #{{indexes}} will stay "Unassigned" and serve no requests until a version frees up or a new real captured version is added. Versions are never reused across keys. Save anyway?',
        params: {
          available: rejection.available,
          versions: rejection.versions.join(', '),
          keys: rejection.keys,
          indexes: rejection.unassignedIndexes.map((i) => i + 1).join(', '),
        },
      }
  }
}

/**
 * Saves a channel; when the backend reports that keys would be left without a
 * version, asks the admin and resends with allow_unassigned only if they
 * agree. Resolves '' when they decline, so the caller keeps the form open.
 * Duplicate and conflict rejections become a translated error.
 */
export async function saveWithKeyEnvironmentConsent(
  save: (allowUnassigned: boolean) => Promise<string>,
  confirmShortage: (rejection: KeyEnvironmentShortage) => Promise<boolean>,
  translate: (key: string, params: Record<string, string | number>) => string
): Promise<string> {
  try {
    return await save(false)
  } catch (error) {
    const rejection = parseKeyEnvironmentRejection(error)
    if (!rejection) throw error
    if (rejection.code === 'key_environment_shortage') {
      return (await confirmShortage(rejection)) ? await save(true) : ''
    }
    const message = keyEnvironmentRejectionMessage(rejection)
    throw new Error(translate(message.key, message.params))
  }
}
