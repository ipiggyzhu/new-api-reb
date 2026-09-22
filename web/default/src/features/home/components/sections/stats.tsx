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

import { useRef, useEffect, useCallback } from 'react'
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

import { useLandingBoard } from '../../hooks'

interface CounterProps {
  end: number
  suffix?: string
  prefix?: string
  duration?: number
  decimals?: number
}

function Counter(props: CounterProps) {
  const { end, suffix = '', prefix = '', duration = 1600, decimals = 0 } = props
  const ref = useRef<HTMLSpanElement>(null)
  const startedRef = useRef(false)

  const formatValue = useCallback(
    (v: number) =>
      decimals > 0 ? v.toFixed(decimals) : Math.round(v).toLocaleString(),
    [decimals]
  )

  const animate = useCallback(() => {
    const el = ref.current
    if (!el) return
    const start = performance.now()
    const step = (now: number) => {
      const progress = Math.min((now - start) / duration, 1)
      const eased = 1 - Math.pow(1 - progress, 3)
      el.textContent = `${prefix}${formatValue(eased * end)}${suffix}`
      if (progress < 1) requestAnimationFrame(step)
    }
    requestAnimationFrame(step)
  }, [end, duration, prefix, suffix, formatValue])

  useEffect(() => {
    const el = ref.current
    if (!el) return

    const mq = window.matchMedia('(prefers-reduced-motion: reduce)')
    if (mq.matches) {
      el.textContent = `${prefix}${formatValue(end)}${suffix}`
      return
    }

    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting && !startedRef.current) {
          startedRef.current = true
          animate()
          observer.unobserve(el)
        }
      },
      { threshold: 0.5 }
    )

    observer.observe(el)
    return () => observer.disconnect()
  }, [animate, end, prefix, suffix, formatValue])

  return (
    <span ref={ref} className='tabular-nums'>
      {prefix}0{suffix}
    </span>
  )
}

interface StatsProps {
  className?: string
}

interface StatItem {
  key: string
  end: number
  suffix: string
  label: string
  decimals?: number
}

const COLUMN_CLASS: Record<number, string> = {
  1: 'grid-cols-1',
  2: 'grid-cols-2',
  3: 'grid-cols-2 md:grid-cols-3',
  4: 'grid-cols-2 md:grid-cols-4',
}

/**
 * Stats strip: real figures from the public catalog and rankings on a
 * ruled band, separated by hairlines rather than boxed into cards. A
 * figure with no data behind it is dropped rather than padded; the strip
 * disappears entirely when nothing public is available.
 */
export function Stats(props: StatsProps) {
  const { t } = useTranslation()
  const board = useLandingBoard()

  if (board.isLoading) {
    return null
  }

  const stats: StatItem[] = []
  if (board.modelCount > 0) {
    stats.push({
      key: 'models',
      end: board.modelCount,
      suffix: '',
      label: t('models in catalog'),
    })
  }
  if (board.providerCount > 0) {
    stats.push({
      key: 'providers',
      end: board.providerCount,
      suffix: '',
      label: t('providers'),
    })
  }
  if (board.endpointCount > 0) {
    stats.push({
      key: 'endpoints',
      end: board.endpointCount,
      suffix: '',
      label: t('API endpoint types'),
    })
  }
  if (board.weeklyTokens > 0) {
    let value = board.weeklyTokens
    let unit = ''
    if (value >= 1e9) {
      value /= 1e9
      unit = 'B'
    } else if (value >= 1e6) {
      value /= 1e6
      unit = 'M'
    } else if (value >= 1e3) {
      value /= 1e3
      unit = 'K'
    }
    stats.push({
      key: 'tokens',
      end: value,
      suffix: unit,
      decimals: unit && value < 100 ? 1 : 0,
      label: t('tokens routed this week'),
    })
  }

  if (stats.length === 0) {
    return null
  }

  return (
    <div className={cn('relative z-10 px-5 sm:px-8', props.className)}>
      <div className='border-border/60 mx-auto max-w-6xl border-y'>
        <dl className={cn('grid', COLUMN_CLASS[stats.length])}>
          {stats.map((s) => (
            <div
              key={s.key}
              className='border-border/60 px-5 py-7 sm:px-7 sm:py-9 md:[&:not(:first-child)]:border-l [&:nth-child(even)]:border-l [&:nth-child(n+3)]:border-t md:[&:nth-child(n+3)]:border-t-0'
            >
              <dd className='font-heading text-primary text-4xl font-semibold tracking-tight md:text-5xl'>
                <Counter end={s.end} suffix={s.suffix} decimals={s.decimals} />
              </dd>
              <dt className='text-muted-foreground mt-3 text-[11px] font-medium tracking-[0.14em] uppercase'>
                {s.label}
              </dt>
            </div>
          ))}
        </dl>
      </div>
    </div>
  )
}
