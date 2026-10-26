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
// ============================================================================
// Model Mapping Validation Utilities
// ============================================================================

/**
 * Parse models string to array
 */
export function parseModelsString(modelsStr: string): string[] {
  return modelsStr
    ? modelsStr
        .split(',')
        .map((m) => m.trim())
        .filter(Boolean)
    : []
}

/**
 * Format models array to string
 */
export function formatModelsArray(models: string[]): string {
  return Array.from(new Set(models)).join(',')
}

/**
 * Normalize model name
 */
export function normalizeModelName(model: string): string {
  return typeof model === 'string' ? model.trim() : ''
}

/**
 * Extract source keys from model_mapping JSON
 * (the keys of the mapping object — models being remapped FROM)
 */
export function extractMappingSourceModels(modelMapping: string): string[] {
  if (typeof modelMapping !== 'string') return []
  const trimmed = modelMapping.trim()
  if (!trimmed) return []

  try {
    const parsed = JSON.parse(trimmed)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return []
    }

    const keys = Object.keys(parsed)
      .map((key) => key.trim())
      .filter(Boolean)

    return Array.from(new Set(keys))
  } catch {
    return []
  }
}

/**
 * Extract redirect models from model_mapping JSON
 */
export function extractRedirectModels(modelMapping: string): string[] {
  const mapping = modelMapping
  if (typeof mapping !== 'string') return []
  const trimmed = mapping.trim()
  if (!trimmed) return []

  try {
    const parsed = JSON.parse(trimmed)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return []
    }

    const values = Object.values(parsed)
      .map((value) => (typeof value === 'string' ? value.trim() : undefined))
      .filter((value): value is string => Boolean(value))

    return Array.from(new Set(values))
  } catch {
    return []
  }
}

/**
 * Check if model configuration has changed
 */
export function hasModelConfigChanged(
  currentModels: string[],
  currentModelMapping: string,
  initialModels: string[],
  initialModelMapping: string
): boolean {
  // Always return true if not editing (new channel)
  if (initialModels.length === 0 && !initialModelMapping) {
    return true
  }

  // Check if models array changed
  if (currentModels.length !== initialModels.length) {
    return true
  }
  for (let i = 0; i < currentModels.length; i++) {
    if (currentModels[i] !== initialModels[i]) {
      return true
    }
  }

  // Check if model_mapping changed
  const normalizedCurrent = (currentModelMapping || '').trim()
  const normalizedInitial = (initialModelMapping || '').trim()

  return normalizedCurrent !== normalizedInitial
}

/**
 * Find models in model_mapping that are missing from the models list
 */
export function findMissingModelsInMapping(
  modelMapping: string,
  currentModels: string[]
): string[] {
  if (!modelMapping || modelMapping.trim() === '') {
    return []
  }

  let parsedMapping: Record<string, unknown>
  try {
    parsedMapping = JSON.parse(modelMapping)
    if (
      !parsedMapping ||
      typeof parsedMapping !== 'object' ||
      Array.isArray(parsedMapping)
    ) {
      return []
    }
  } catch {
    return []
  }

  const modelSet = new Set(currentModels.map((m) => normalizeModelName(m)))
  const missingModels = Object.keys(parsedMapping)
    .map((key) => normalizeModelName(key))
    .filter((key) => key && !modelSet.has(key))

  return Array.from(new Set(missingModels))
}

/**
 * Validate model mapping JSON format
 */
export function validateModelMappingJson(modelMapping: string): {
  valid: boolean
  error?: string
} {
  if (!modelMapping || modelMapping.trim() === '') {
    return { valid: true }
  }

  try {
    const parsed = JSON.parse(modelMapping)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return {
        valid: false,
        error: 'Model mapping must be a valid JSON object',
      }
    }
    if (Object.values(parsed).some((value) => typeof value !== 'string')) {
      return {
        valid: false,
        error: 'Model mapping values must be strings',
      }
    }
    return { valid: true }
  } catch {
    return {
      valid: false,
      error: 'Model mapping must be valid JSON format',
    }
  }
}

/**
 * Get redirect models that are also in the models list
 * (These should be removed from models list to keep /v1/models clean)
 */
export function findExposedTargetModels(
  modelMapping: string,
  currentModels: string[]
): string[] {
  const redirectModels = extractRedirectModels(modelMapping)
  if (redirectModels.length === 0) return []

  const normalizedModels = currentModels.map((m) => normalizeModelName(m))
  const modelSet = new Set(normalizedModels)

  return redirectModels.filter((model) =>
    modelSet.has(normalizeModelName(model))
  )
}

/**
 * Resolve the upstream model a request for `model` is sent as, following
 * chained redirects exactly like the relay's ModelMappedHelper. Returns
 * undefined when the model is not remapped (or the chain is a cycle, which the
 * relay rejects).
 */
export function resolveMappedUpstreamModel(
  model: string,
  mapping: Record<string, string>
): string | undefined {
  let current = model
  const visited = new Set([current])
  let mapped = false
  for (;;) {
    const next = mapping[current]
    if (!next) break
    if (visited.has(next)) {
      if (next !== current) return undefined
      if (current === model) return undefined
      break
    }
    visited.add(next)
    current = next
    mapped = true
  }
  return mapped ? current : undefined
}

export type ChannelTestModelEntry = {
  // The name the test request is sent with (a Models entry or mapping alias)
  model: string
  // Where the relay forwards it, when model_mapping rewrites it
  upstreamModel?: string
  // False for a model_mapping alias that is missing from the Models list: it
  // can be tested, but live traffic cannot reach it until it is added.
  listed: boolean
}

/**
 * Build the channel test list: every Models entry plus every model_mapping
 * alias, each annotated with the upstream model it is forwarded to. Mapping
 * targets are deliberately not Models entries (see the drawer guardrail), so
 * without the annotation a mapped upstream model is invisible in the test
 * dialog even though testing its alias is exactly what exercises it.
 */
export function buildChannelTestModelEntries(
  models: string[],
  modelMapping: string | null | undefined
): ChannelTestModelEntry[] {
  let mapping: Record<string, string> = {}
  try {
    const parsed = JSON.parse(modelMapping?.trim() || '{}')
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      mapping = Object.fromEntries(
        Object.entries(parsed).filter(
          (entry): entry is [string, string] => typeof entry[1] === 'string'
        )
      )
    }
  } catch {
    // The relay refuses an unparsable mapping; list the models unannotated.
  }

  const listedModels = [
    ...new Set(models.map((m) => normalizeModelName(m)).filter(Boolean)),
  ]
  const listedSet = new Set(listedModels)
  const unlistedAliases = Object.keys(mapping).filter(
    (alias) =>
      alias.trim() !== '' &&
      !listedSet.has(alias) &&
      resolveMappedUpstreamModel(alias, mapping) !== undefined
  )

  return [
    ...listedModels.map((model) => ({
      model,
      upstreamModel: resolveMappedUpstreamModel(model, mapping),
      listed: true,
    })),
    ...unlistedAliases.map((model) => ({
      model,
      upstreamModel: resolveMappedUpstreamModel(model, mapping),
      listed: false,
    })),
  ]
}

/**
 * Case-insensitive search over both the tested name and its upstream target.
 */
export function filterChannelTestModelEntries(
  entries: ChannelTestModelEntry[],
  searchTerm: string
): ChannelTestModelEntry[] {
  const keyword = searchTerm.trim().toLowerCase()
  if (!keyword) return entries
  return entries.filter(
    (entry) =>
      entry.model.toLowerCase().includes(keyword) ||
      Boolean(entry.upstreamModel?.toLowerCase().includes(keyword))
  )
}

/**
 * Categorize models into different sets for UI display
 */
export function categorizeModelsWithRedirect(
  currentModels: string[],
  redirectModels: string[]
): {
  normalizedCurrentModels: Set<string>
  normalizedRedirectModels: Set<string>
  classificationSet: Set<string>
  redirectOnlySet: Set<string>
} {
  const normalizedCurrentModels = new Set(
    currentModels.map((m) => normalizeModelName(m)).filter(Boolean)
  )

  const normalizedRedirectModels = new Set(
    redirectModels.map((m) => normalizeModelName(m)).filter(Boolean)
  )

  const classificationSet = new Set([
    ...normalizedCurrentModels,
    ...normalizedRedirectModels,
  ])

  const redirectOnlySet = new Set(
    Array.from(normalizedRedirectModels).filter(
      (m) => !normalizedCurrentModels.has(m)
    )
  )

  return {
    normalizedCurrentModels,
    normalizedRedirectModels,
    classificationSet,
    redirectOnlySet,
  }
}
