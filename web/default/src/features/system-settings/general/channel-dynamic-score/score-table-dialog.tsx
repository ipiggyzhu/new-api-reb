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
import { RefreshCw } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatTimestampRelative } from '@/lib/format'

import { getDynamicScores } from './api'
import type { DynamicScoreRow, DynamicScoreSnapshot } from './types'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
}

/** Sorted so the channels the scoring has actually moved appear first. */
function sortRows(rows: DynamicScoreRow[]): DynamicScoreRow[] {
  return [...rows].sort((a, b) => {
    if (a.idle !== b.idle) return a.idle ? 1 : -1
    if (a.tier_offset !== b.tier_offset) return a.tier_offset - b.tier_offset
    if (a.channel_id !== b.channel_id) return a.channel_id - b.channel_id
    return a.model.localeCompare(b.model)
  })
}

function formatTierOffset(offset: number): string {
  if (offset > 0) return `+${offset}`
  return String(offset)
}

function successRate(row: DynamicScoreRow): string {
  if (row.total <= 0) return '-'
  return `${((row.success / row.total) * 100).toFixed(0)}%`
}

export function DynamicScoreTableDialog(props: Props) {
  const { t } = useTranslation()
  const [loading, setLoading] = useState(false)
  const [snapshot, setSnapshot] = useState<DynamicScoreSnapshot | null>(null)
  const seqRef = useRef(0)

  const load = useCallback(() => {
    const seq = ++seqRef.current
    setLoading(true)
    getDynamicScores()
      .then((res) => {
        if (seq !== seqRef.current) return
        if (res.success && res.data) setSnapshot(res.data)
        else toast.error(res.message || t('Request failed'))
      })
      .catch(() => {
        if (seq !== seqRef.current) return
        toast.error(t('Request failed'))
      })
      .finally(() => {
        if (seq !== seqRef.current) return
        setLoading(false)
      })
  }, [t])

  useEffect(() => {
    if (!props.open) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setSnapshot(null)
      return
    }
    load()
  }, [props.open, load])

  const rows = snapshot ? sortRows(snapshot.rows) : []

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Current Dynamic Scores')}
      contentClassName='sm:max-w-4xl'
      contentHeight='auto'
      bodyClassName='space-y-4'
    >
      <div className='flex items-start justify-between gap-4'>
        <p className='text-muted-foreground text-xs'>
          {t(
            'Scores live in memory only and reset when the service restarts, so the channel list keeps showing your configured priority and weight. This table is the only place the current adjustment is visible.'
          )}
        </p>
        <Button
          variant='outline'
          size='sm'
          onClick={load}
          disabled={loading}
          className='shrink-0'
        >
          <RefreshCw className={loading ? 'animate-spin' : undefined} />
          {t('Refresh')}
        </Button>
      </div>

      {snapshot && !snapshot.enabled ? (
        <Alert>
          <AlertDescription>
            {t(
              'Dynamic scoring is turned off, so these rows are not affecting routing.'
            )}
          </AlertDescription>
        </Alert>
      ) : null}

      {snapshot && snapshot.enabled && !snapshot.usable ? (
        <Alert variant='destructive'>
          <AlertDescription>
            {t(
              'Dynamic scoring is enabled but its store is unreachable, so routing is falling back to your configured values.'
            )}
          </AlertDescription>
        </Alert>
      ) : null}

      {snapshot && !snapshot.complete ? (
        <Alert>
          <AlertDescription>
            {t(
              'Redis is configured, so this is only the portion of the scoring this instance has handled. Promotion and demotion streaks are not mirrored locally and read as zero.'
            )}
          </AlertDescription>
        </Alert>
      ) : null}

      {loading && !snapshot ? (
        <div className='text-muted-foreground py-8 text-center text-sm'>
          {t('Loading...')}
        </div>
      ) : rows.length > 0 ? (
        <div className='max-h-[60vh] overflow-auto'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('Channel')}</TableHead>
                <TableHead>{t('Group')}</TableHead>
                <TableHead>{t('Model')}</TableHead>
                <TableHead className='text-right'>
                  {t('Tier Adjustment')}
                </TableHead>
                <TableHead className='text-right'>
                  {t('Weight Factor')}
                </TableHead>
                <TableHead className='text-right'>{t('Success')}</TableHead>
                <TableHead className='text-right'>{t('Streak')}</TableHead>
                <TableHead className='text-right'>{t('Updated')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow
                  key={`${row.channel_id}:${row.group}:${row.model}`}
                  className={row.idle ? 'opacity-50' : undefined}
                >
                  <TableCell className='font-medium'>
                    {row.channel_id}
                  </TableCell>
                  <TableCell className='break-all'>{row.group}</TableCell>
                  <TableCell className='break-all'>{row.model}</TableCell>
                  <TableCell className='text-right'>
                    {row.tier_offset === 0 ? (
                      <span className='text-muted-foreground'>0</span>
                    ) : (
                      <Badge
                        variant={
                          row.tier_offset > 0 ? 'secondary' : 'destructive'
                        }
                      >
                        {formatTierOffset(row.tier_offset)}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className='text-right tabular-nums'>
                    {row.weight_factor.toFixed(2)}x
                  </TableCell>
                  <TableCell className='text-right tabular-nums'>
                    {row.success}/{row.total}
                    <span className='text-muted-foreground ml-1'>
                      ({successRate(row)})
                    </span>
                  </TableCell>
                  <TableCell className='text-right tabular-nums'>
                    <span title={t('Consecutive successes')}>
                      {row.consecutive_success}
                    </span>
                    <span className='text-muted-foreground'> / </span>
                    <span title={t('Consecutive faults')}>
                      {row.fault_count}
                    </span>
                  </TableCell>
                  <TableCell className='text-muted-foreground text-right text-xs'>
                    {row.updated_at > 0
                      ? formatTimestampRelative(row.updated_at)
                      : '-'}
                    {row.idle ? (
                      <span className='ml-1'>({t('idle')})</span>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : (
        <div className='text-muted-foreground py-8 text-center text-sm'>
          {t(
            'No scoring recorded yet. A row appears once a channel has served or failed a request.'
          )}
        </div>
      )}
    </Dialog>
  )
}
