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
import { Cable } from 'lucide-react'
import { useFormContext } from 'react-hook-form'
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

import type { ChannelFormValues } from '../../../lib/channel-form'
import {
  hasVisibleChannelSetting,
  type ChannelSettingViews,
} from '../../../lib/channel-setting-visibility'
import { SubHeading } from './channel-section-heading'
import { ChannelSettingRow } from './channel-setting-row'
import { ChannelSettingsCard } from './channel-settings-card'
import { ChannelUpstreamKeepaliveFields } from './channel-upstream-keepalive-fields'

type ChannelConnectionSectionProps = {
  id: string
  configured: boolean
  sensitiveLocked: boolean
  settings: ChannelSettingViews
  /** The backend found that the upstream refuses the WebSocket upgrade. */
  websocketDowngraded: boolean
  upstreamKeepaliveLastTime: unknown
}

export function ChannelConnectionSection(props: ChannelConnectionSectionProps) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const settings = props.settings

  return (
    <ChannelSettingsCard
      id={props.id}
      title={t('Connection & keepalive')}
      description={t(
        'How the gateway reaches this upstream: proxy, transport and idle keepalive.'
      )}
      icon={<Cable className='h-4 w-4' />}
      iconTone='info'
      configured={props.configured}
      sensitiveLocked={props.sensitiveLocked}
    >
      <ChannelSettingRow setting={settings.proxy} layout='block'>
        <FormField
          control={form.control}
          name='proxy'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Proxy Address')}</FormLabel>
              <FormControl>
                <Input
                  placeholder={t('socks5://user:pass@host:port')}
                  {...field}
                />
              </FormControl>
              <FormDescription>
                {t('Network proxy for this channel (supports socks5 protocol)')}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
      </ChannelSettingRow>

      {hasVisibleChannelSetting(settings, [
        'websocket_transport',
        'disable_task_polling_sleep',
      ]) && (
        <div className='divide-border space-y-0 divide-y border-y'>
          <ChannelSettingRow
            setting={settings.websocket_transport}
            reason={t(
              'Only OpenAI and Codex channels can use the Responses WebSocket transport.'
            )}
          >
            <FormField
              control={form.control}
              name='websocket_transport'
              render={({ field }) => (
                <FormItem className='px-4 py-3'>
                  <div className='flex items-center justify-between'>
                    <div className='space-y-0.5'>
                      <FormLabel>{t('WebSocket Transport')}</FormLabel>
                      <FormDescription>
                        {t(
                          'Use the Responses API WebSocket transport for /v1/responses. Requires an upstream that supports WebSocket mode; pointing at an HTTP-only relay falls back to SSE automatically.'
                        )}
                      </FormDescription>
                    </div>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    </FormControl>
                  </div>
                  {props.websocketDowngraded && (
                    <p className='text-destructive mt-2 text-xs'>
                      {t(
                        'Upstream refused the WebSocket upgrade, so this channel is running on SSE. Save the channel to try again.'
                      )}
                    </p>
                  )}
                </FormItem>
              )}
            />
          </ChannelSettingRow>

          <ChannelSettingRow
            setting={settings.disable_task_polling_sleep}
            reason={t(
              'Only matters on channel types that run async video tasks.'
            )}
          >
            <FormField
              control={form.control}
              name='disable_task_polling_sleep'
              render={({ field }) => (
                <FormItem className='flex items-center justify-between px-4 py-3'>
                  <div className='space-y-0.5'>
                    <FormLabel>{t('Skip async task polling delay')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Do not wait one second between polling async tasks for this channel'
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
          </ChannelSettingRow>
        </div>
      )}

      {settings.upstream_keepalive.state !== 'hidden' && (
        <div className='space-y-4 pt-2'>
          <SubHeading title={t('Upstream keepalive')} />
          <ChannelSettingRow
            setting={settings.upstream_keepalive}
            layout='block'
          >
            <ChannelUpstreamKeepaliveFields
              lastTime={props.upstreamKeepaliveLastTime}
            />
          </ChannelSettingRow>
        </div>
      )}
    </ChannelSettingsCard>
  )
}
