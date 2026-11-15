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
import { useFormContext, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import {
  UPSTREAM_KEEPALIVE_DEFAULT_ACTIVE_WINDOW_MINUTES,
  UPSTREAM_KEEPALIVE_DEFAULT_INTERVAL_MINUTES,
  UPSTREAM_KEEPALIVE_MAX_ACTIVE_WINDOW_MINUTES,
  UPSTREAM_KEEPALIVE_MAX_INTERVAL_MINUTES,
  UPSTREAM_KEEPALIVE_MIN_ACTIVE_WINDOW_MINUTES,
  UPSTREAM_KEEPALIVE_MIN_INTERVAL_MINUTES,
} from '../../../constants'
import type { ChannelFormValues } from '../../../lib/channel-form'
import { formatUnixTime } from '../../../lib/channel-utils'

type ChannelUpstreamKeepaliveFieldsProps = {
  /** When the backend last sent a keepalive (Unix seconds), if it ever did. */
  lastTime: unknown
}

export function ChannelUpstreamKeepaliveFields(
  props: ChannelUpstreamKeepaliveFieldsProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const currentUpstreamKeepaliveEnabled = useWatch({
    control: form.control,
    name: 'upstream_keepalive_enabled',
  })
  const intervalMinutes = useWatch({
    control: form.control,
    name: 'upstream_keepalive_interval_minutes',
  })
  const activeWindowMinutes = useWatch({
    control: form.control,
    name: 'upstream_keepalive_active_window_minutes',
  })
  // Mirrors the backend rule: an enabled keepalive is first due one interval,
  // offset by up to +10% jitter, after the last real request and is sent on
  // the next one-minute scheduler tick, so a shorter active window closes first
  // in some quiet periods. Empty inputs fall back to the defaults.
  const upstreamKeepaliveMinActiveWindowMinutes = Math.ceil(
    ((intervalMinutes || UPSTREAM_KEEPALIVE_DEFAULT_INTERVAL_MINUTES) * 66 +
      60) /
      60
  )
  const upstreamKeepaliveWindowTooShort =
    currentUpstreamKeepaliveEnabled === true &&
    (activeWindowMinutes || UPSTREAM_KEEPALIVE_DEFAULT_ACTIVE_WINDOW_MINUTES) <
      upstreamKeepaliveMinActiveWindowMinutes

  return (
    <>
      <div className='divide-border space-y-0 divide-y border-y'>
        <FormField
          control={form.control}
          name='upstream_keepalive_enabled'
          render={({ field }) => (
            <FormItem className='flex items-center justify-between px-4 py-3'>
              <div className='space-y-0.5'>
                <FormLabel>{t('Enable upstream keepalive')}</FormLabel>
                <FormDescription>
                  {t(
                    "While the channel is in use, send a tiny request with the channel's own key whenever real requests pause, so the upstream does not go idle in the gaps. Nothing is sent while real requests are flowing or once the active window has passed. Keepalive traffic is not billed, not logged as usage, does not affect channel scoring, and never disables the channel."
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  checked={field.value}
                  onCheckedChange={field.onChange}
                />
              </FormControl>
            </FormItem>
          )}
        />
      </div>
      <FormField
        control={form.control}
        name='upstream_keepalive_interval_minutes'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Keepalive interval (minutes)')}</FormLabel>
            <FormControl>
              <Input
                type='number'
                min={UPSTREAM_KEEPALIVE_MIN_INTERVAL_MINUTES}
                max={UPSTREAM_KEEPALIVE_MAX_INTERVAL_MINUTES}
                step={1}
                placeholder={String(
                  UPSTREAM_KEEPALIVE_DEFAULT_INTERVAL_MINUTES
                )}
                {...field}
                disabled={!currentUpstreamKeepaliveEnabled}
                onChange={(e) => field.onChange(Number(e.target.value))}
              />
            </FormControl>
            <FormDescription>
              {t(
                'Between 5 and 10080 minutes. Each send is offset by up to ±10% so channels do not fire together.'
              )}
            </FormDescription>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name='upstream_keepalive_active_window_minutes'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Keepalive active window (minutes)')}</FormLabel>
            <FormControl>
              <Input
                type='number'
                min={UPSTREAM_KEEPALIVE_MIN_ACTIVE_WINDOW_MINUTES}
                max={UPSTREAM_KEEPALIVE_MAX_ACTIVE_WINDOW_MINUTES}
                step={1}
                placeholder={String(
                  UPSTREAM_KEEPALIVE_DEFAULT_ACTIVE_WINDOW_MINUTES
                )}
                {...field}
                disabled={!currentUpstreamKeepaliveEnabled}
                onChange={(e) => field.onChange(Number(e.target.value))}
              />
            </FormControl>
            <FormDescription>
              {t(
                'Between 5 and 10080 minutes. Keepalives continue for this long after the last real request, then stop until the channel is used again. Set it longer than the interval.'
              )}
            </FormDescription>
            {upstreamKeepaliveWindowTooShort && (
              <p role='alert' className='text-warning text-xs'>
                {t(
                  'With this interval the active window must be at least {{minutes}} minutes (the interval plus its 10% jitter and the one-minute scheduler tick), otherwise some quiet periods get no keepalive. Saving a changed keepalive setting like this is rejected.',
                  {
                    minutes: upstreamKeepaliveMinActiveWindowMinutes,
                  }
                )}
              </p>
            )}
            <FormMessage />
          </FormItem>
        )}
      />
      <div className='text-muted-foreground border-t pt-3 text-xs'>
        <span className='text-foreground font-medium'>
          {t('Last keepalive time')}:
        </span>{' '}
        {formatUnixTime(props.lastTime)}
      </div>
    </>
  )
}
