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
import { api } from '@/lib/api'

import type { DynamicScoreSnapshot } from './types'

/**
 * Reads the live scoring state. Scores exist only in memory, so this is the only
 * way to observe them: nothing is persisted per channel and the configured
 * priority and weight deliberately stay at their baseline in the channel list.
 */
export async function getDynamicScores(params?: {
  channel_id?: number
  group?: string
  model?: string
}): Promise<{
  success: boolean
  message?: string
  data?: DynamicScoreSnapshot
}> {
  const res = await api.get('/api/channel/dynamic_score', {
    params,
    disableDuplicate: true,
  } as Record<string, unknown>)
  return res.data
}
