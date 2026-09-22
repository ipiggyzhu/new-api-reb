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

import { Link } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { Skeleton } from '@/components/ui/skeleton'
import { formatCompactNumber } from '@/lib/format'
import { cn } from '@/lib/utils'

import { LANDING_BOARD_ROWS, useLandingBoard } from '../hooks'

/* Uptime-Kuma status codes as returned by /api/uptime/status. */
const STATUS_META: Record<number, { labelKey: string; className: string }> = {
  1: { labelKey: 'Healthy', className: 'text-success' },
  0: { labelKey: 'Down', className: 'text-destructive' },
  2: { labelKey: 'Pending', className: 'text-warning' },
  3: { labelKey: 'Maintenance', className: 'text-info' },
}

const SKELETON_KEYS = Array.from(
  { length: LANDING_BOARD_ROWS },
  (_, index) => `skeleton-${index + 1}`
)

const ROW_BASE =
  'grid items-center gap-x-2 px-4 sm:gap-x-3 sm:px-5 [grid-template-columns:var(--board-cols)]'

/**
 * Landing hero visual: the gateway's public model board, fed by the same
 * endpoints as the pricing and rankings pages. Model and provider come
 * from /api/pricing (vendor from /api/rankings as fallback), prices are
 * per 1M tokens from /api/pricing, 7-day usage from /api/rankings, and the
 * status column only appears when /api/uptime/status has a matching monitor.
 */
export function RouteBoard(props: { className?: string }) {
  const { t } = useTranslation()
  const board = useLandingBoard()

  let columns = 'minmax(0,1.7fr) minmax(0,1fr) 7.5rem'
  if (board.hasUsage) {
    columns += ' 4rem'
  }
  if (board.hasStatus) {
    columns += ' 5.5rem'
  }

  let pill: string | null = null
  if (board.source === 'rankings') {
    pill = t('Top this week')
  } else if (board.source === 'pricing') {
    pill = t('Public catalog')
  }

  let body: React.ReactNode
  if (board.isLoading) {
    body = (
      <ul className='divide-border/40 divide-y' aria-busy='true'>
        {SKELETON_KEYS.map((key) => (
          <li key={key} className={cn(ROW_BASE, 'py-3')}>
            <Skeleton className='h-3 w-3/4' />
            <Skeleton className='h-3 w-1/2' />
            <Skeleton className='ml-auto h-3 w-2/3' />
            {board.hasUsage && <Skeleton className='ml-auto h-3 w-1/2' />}
            {board.hasStatus && <Skeleton className='ml-auto h-3 w-1/2' />}
          </li>
        ))}
      </ul>
    )
  } else if (board.rows.length === 0) {
    body = (
      <div className='flex flex-col items-center gap-1.5 px-6 py-12 text-center'>
        <p className='text-foreground text-sm font-medium'>
          {t('No public model data yet')}
        </p>
        <p className='text-muted-foreground max-w-xs text-xs'>
          {t(
            'Models appear here once pricing is published or usage rankings are available.'
          )}
        </p>
      </div>
    )
  } else {
    body = (
      <ul className='divide-border/40 divide-y'>
        {board.rows.map((row) => {
          const statusMeta =
            row.status === null ? undefined : STATUS_META[row.status]
          return (
            <li
              key={row.modelName}
              className={cn(
                ROW_BASE,
                'hover:bg-primary/5 py-2.5 text-xs transition-colors motion-reduce:transition-none'
              )}
            >
              <span
                className='text-foreground truncate font-mono'
                title={row.modelName}
              >
                {row.modelName}
              </span>
              <span className='text-muted-foreground truncate'>
                {row.vendorName ?? '—'}
              </span>
              <span className='text-muted-foreground truncate text-right font-mono tabular-nums'>
                {row.price === null && '—'}
                {row.price !== null && row.price.perRequest && (
                  <>
                    {row.price.input}
                    <span className='text-muted-foreground/50'>
                      {' '}
                      /{t('req')}
                    </span>
                  </>
                )}
                {row.price !== null && !row.price.perRequest && (
                  <>
                    <span className='text-foreground/90'>
                      {row.price.input}
                    </span>
                    <span className='text-muted-foreground/50'> · </span>
                    {row.price.output}
                  </>
                )}
              </span>
              {board.hasUsage && (
                <span className='text-muted-foreground text-right font-mono tabular-nums'>
                  {row.weeklyTokens === null
                    ? '—'
                    : formatCompactNumber(row.weeklyTokens)}
                </span>
              )}
              {board.hasStatus && (
                <span
                  className={cn(
                    'inline-flex items-center justify-end gap-1.5 font-mono text-[10px] tracking-wider uppercase',
                    statusMeta ? statusMeta.className : 'text-muted-foreground'
                  )}
                >
                  {statusMeta ? (
                    <>
                      <span
                        className='size-1.5 rounded-full bg-current'
                        aria-hidden='true'
                      />
                      {t(statusMeta.labelKey)}
                    </>
                  ) : (
                    '—'
                  )}
                </span>
              )}
            </li>
          )
        })}
      </ul>
    )
  }

  return (
    <div
      className={cn(
        'hero-terminal bg-card/70 overflow-hidden rounded-2xl border shadow-[0_30px_80px_-40px_rgb(0_0_0/0.8)]',
        props.className
      )}
      style={{ '--board-cols': columns } as React.CSSProperties}
      role='figure'
      aria-label={t('Model board')}
    >
      <div className='border-border/60 flex items-center justify-between border-b px-4 py-3 sm:px-5'>
        <span className='text-primary inline-flex items-center gap-2 font-mono text-[10px] font-semibold tracking-[0.18em] uppercase'>
          <span
            className='bg-primary size-1.5 rounded-full shadow-[0_0_8px_var(--primary)]'
            aria-hidden='true'
          />
          {t('Model board')}
        </span>
        {pill && (
          <span className='text-muted-foreground border-border/70 rounded-full border px-2 py-0.5 font-mono text-[10px] tracking-wider uppercase'>
            {pill}
          </span>
        )}
      </div>

      {(board.isLoading || board.rows.length > 0) && (
        <div
          className={cn(
            ROW_BASE,
            'text-muted-foreground border-border/50 border-b py-2 font-mono text-[10px] tracking-[0.14em] uppercase'
          )}
          aria-hidden='true'
        >
          <span>{t('Model')}</span>
          <span>{t('Provider')}</span>
          <span className='text-right'>
            {t('Input')} · {t('Output')}
          </span>
          {board.hasUsage && <span className='text-right'>{t('7d')}</span>}
          {board.hasStatus && <span className='text-right'>{t('Status')}</span>}
        </div>
      )}

      {body}

      {board.rows.length > 0 && (
        <div className='text-muted-foreground/60 border-border/50 flex items-center justify-between gap-3 border-t px-4 py-2.5 font-mono text-[10px] tracking-wider sm:px-5'>
          <span className='truncate'>
            {t('Prices per 1M tokens')}
            {board.modelCount > 0 && (
              <>
                {' · '}
                {t('{{count}} models', { count: board.modelCount })}
              </>
            )}
          </span>
          <Link
            to='/pricing'
            className='text-primary/80 hover:text-primary shrink-0 uppercase transition-colors'
          >
            {t('View all pricing')}
          </Link>
        </div>
      )}
    </div>
  )
}
