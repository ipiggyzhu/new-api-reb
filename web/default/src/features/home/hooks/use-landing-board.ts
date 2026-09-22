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
import { useQuery } from '@tanstack/react-query'
import { useMemo } from 'react'

import { getUptimeStatus } from '@/features/dashboard/api'
import type { UptimeMonitor } from '@/features/dashboard/types'
import { QUOTA_TYPE_VALUES } from '@/features/pricing/constants'
import { usePricingData } from '@/features/pricing/hooks/use-pricing-data'
import { formatPrice, formatRequestPrice } from '@/features/pricing/lib/price'
import type { PricingModel } from '@/features/pricing/types'
import { useRankings } from '@/features/rankings/hooks/use-rankings'

export const LANDING_BOARD_ROWS = 7

export type LandingBoardRow = {
  modelName: string
  vendorName: string | null
  /** Per-1M-token prices from /api/pricing, or per-request price. Null when the model has no public pricing entry. */
  price: { input: string; output: string; perRequest: boolean } | null
  /** Tokens served in the last 7 days from /api/rankings. Null when rankings are empty. */
  weeklyTokens: number | null
  /** Uptime-Kuma style status (1 up, 0 down, 2 pending, 3 maintenance). Null when no monitor matches. */
  status: number | null
}

export type LandingBoardData = {
  rows: LandingBoardRow[]
  isLoading: boolean
  /** Which endpoint decided the ordering, or null when neither returned models. */
  source: 'rankings' | 'pricing' | null
  hasUsage: boolean
  hasStatus: boolean
  modelCount: number
  providerCount: number
  endpointCount: number
  weeklyTokens: number
}

function findMonitor(
  monitors: UptimeMonitor[],
  modelName: string,
  vendorName: string | null
): UptimeMonitor | undefined {
  const model = modelName.toLowerCase()
  const vendor = vendorName?.toLowerCase() ?? null
  const byModel = monitors.find((m) => m.name.toLowerCase() === model)
  if (byModel) {
    return byModel
  }
  if (!vendor) {
    return undefined
  }
  return monitors.find((m) => m.name.toLowerCase() === vendor)
}

/**
 * Real data behind the landing "model board" and the stats strip. Every
 * figure comes from a public endpoint (no login): /api/pricing for the
 * catalog and prices, /api/rankings for 7-day usage, /api/uptime/status
 * for upstream health. Nothing is synthesised when a source is empty.
 */
export function useLandingBoard(): LandingBoardData {
  const pricing = usePricingData()
  const rankings = useRankings('week')
  const uptime = useQuery({
    queryKey: ['uptime', 'status'],
    queryFn: getUptimeStatus,
    staleTime: 5 * 60 * 1000,
    retry: false,
  })

  const pricingModels = pricing.models
  const endpointMap = pricing.endpointMap
  const priceRate = pricing.priceRate
  const usdExchangeRate = pricing.usdExchangeRate
  const rankedModels = rankings.data?.data?.models
  const uptimeGroups = uptime.data?.data

  return useMemo(() => {
    const monitors: UptimeMonitor[] = []
    for (const group of uptimeGroups ?? []) {
      for (const monitor of group.monitors ?? []) {
        monitors.push(monitor)
      }
    }

    const pricingByName = new Map<string, PricingModel>()
    for (const model of pricingModels) {
      pricingByName.set(model.model_name, model)
    }

    const ranked = [...(rankedModels ?? [])].sort((a, b) => a.rank - b.rank)
    let source: LandingBoardData['source'] = null
    if (ranked.length > 0) {
      source = 'rankings'
    } else if (pricingModels.length > 0) {
      source = 'pricing'
    }

    const picked: Array<{
      modelName: string
      vendorName: string | null
      weeklyTokens: number | null
    }> = []
    if (source === 'rankings') {
      for (const item of ranked.slice(0, LANDING_BOARD_ROWS)) {
        const entry = pricingByName.get(item.model_name)
        picked.push({
          modelName: item.model_name,
          vendorName: entry?.vendor_name || item.vendor || null,
          weeklyTokens: item.total_tokens,
        })
      }
    } else if (source === 'pricing') {
      for (const model of pricingModels.slice(0, LANDING_BOARD_ROWS)) {
        picked.push({
          modelName: model.model_name,
          vendorName: model.vendor_name || null,
          weeklyTokens: null,
        })
      }
    }

    const rows: LandingBoardRow[] = picked.map((item) => {
      const entry = pricingByName.get(item.modelName)
      let price: LandingBoardRow['price'] = null
      if (entry) {
        if (entry.quota_type === QUOTA_TYPE_VALUES.REQUEST) {
          const perRequest = formatRequestPrice(
            entry,
            false,
            priceRate,
            usdExchangeRate
          )
          price = { input: perRequest, output: perRequest, perRequest: true }
        } else {
          price = {
            input: formatPrice(
              entry,
              'input',
              'M',
              false,
              priceRate,
              usdExchangeRate
            ),
            output: formatPrice(
              entry,
              'output',
              'M',
              false,
              priceRate,
              usdExchangeRate
            ),
            perRequest: false,
          }
        }
      }
      const monitor =
        monitors.length > 0
          ? findMonitor(monitors, item.modelName, item.vendorName)
          : undefined
      return {
        modelName: item.modelName,
        vendorName: item.vendorName,
        price,
        weeklyTokens: item.weeklyTokens,
        status: monitor ? monitor.status : null,
      }
    })

    const vendorNames = new Set<string>()
    for (const model of pricingModels) {
      if (model.vendor_name) {
        vendorNames.add(model.vendor_name)
      }
    }

    let weeklyTokens = 0
    for (const item of ranked) {
      weeklyTokens += Number(item.total_tokens) || 0
    }

    return {
      rows,
      isLoading: pricing.isLoading || rankings.isLoading,
      source,
      hasUsage: source === 'rankings',
      hasStatus: rows.some((row) => row.status !== null),
      modelCount: pricingModels.length,
      providerCount: vendorNames.size,
      endpointCount: Object.keys(endpointMap ?? {}).length,
      weeklyTokens,
    }
  }, [
    endpointMap,
    priceRate,
    pricing.isLoading,
    pricingModels,
    rankedModels,
    rankings.isLoading,
    uptimeGroups,
    usdExchangeRate,
  ])
}
