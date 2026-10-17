export function parseModelUpdateExcludedChannelIDs(
  value: string
): number[] | null {
  const parts = value
    .trim()
    .split(/[,，\s]+/u)
    .filter(Boolean)
  const ids: number[] = []
  for (const part of parts) {
    const id = Number(part)
    if (!/^\d+$/.test(part) || !Number.isSafeInteger(id) || id <= 0) return null
    ids.push(id)
  }
  return [...new Set(ids)]
}

export function formatModelUpdateExcludedChannelIDs(value: string): string {
  try {
    const ids: unknown = JSON.parse(value)
    if (Array.isArray(ids)) return ids.join(', ')
  } catch {
    // Older servers do not expose this setting yet.
  }
  return ''
}
