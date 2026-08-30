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

/**
 * Option values arrive from the settings API as strings, so the numeric fields
 * are typed to accept either form rather than assuming a parse has happened.
 */
export interface ChannelDynamicScoreSettings {
  'channel_dynamic_score_setting.enabled': boolean
  'channel_dynamic_score_setting.successes_to_promote': number | string
  'channel_dynamic_score_setting.faults_to_demote': number | string
  'channel_dynamic_score_setting.max_promote_tiers': number | string
  'channel_dynamic_score_setting.max_demote_tiers': number | string
  'channel_dynamic_score_setting.min_sample_for_weight': number | string
  'channel_dynamic_score_setting.success_window_seconds': number | string
  'channel_dynamic_score_setting.idle_reset_seconds': number | string
}

/** One (channel, group, model) scoring row, mirroring pkg/channel_score.ScoreView. */
export interface DynamicScoreRow {
  channel_id: number
  group: string
  model: string
  /** Accumulated movement in tiers; positive promotes. */
  tier_offset: number
  total: number
  success: number
  consecutive_success: number
  fault_count: number
  updated_at: number
  weight_factor: number
  /** The selection path ignores idle rows even when a tier offset is recorded. */
  idle: boolean
}

/** Mirrors pkg/channel_score.ScoreSnapshot. */
export interface DynamicScoreSnapshot {
  /** The admin switch; usable additionally requires a reachable shared store. */
  enabled: boolean
  usable: boolean
  redis_configured: boolean
  /** With Redis these rows are only this process's mirror, so some fields read zero. */
  instance_local: boolean
  complete: boolean
  rows: DynamicScoreRow[]
}
