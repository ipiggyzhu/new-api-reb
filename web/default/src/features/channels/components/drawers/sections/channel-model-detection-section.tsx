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

import type { ChannelFormValues } from '../../../lib/channel-form'
import type { ChannelSettingView } from '../../../lib/channel-setting-visibility'
import { formatUnixTime } from '../../../lib/channel-utils'
import { ChannelSettingRow } from './channel-setting-row'
import { ChannelSettingsCard } from './channel-settings-card'

const UPSTREAM_DETECTED_MODEL_PREVIEW_LIMIT = 8

type ChannelModelDetectionSectionProps = {
  id: string
  configured: boolean
  sensitiveLocked: boolean
  /** The whole card is one option as far as the channel type is concerned. */
  setting: ChannelSettingView
  /** What the backend recorded on its last check of the upstream. */
  upstreamModelInfo: { lastCheckTime: unknown; detectedModels: string[] }
}

export function ChannelModelDetectionSection(
  props: ChannelModelDetectionSectionProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const upstreamModelUpdateCheckEnabled = useWatch({
    control: form.control,
    name: 'upstream_model_update_check_enabled',
  })
  const upstreamDetectedModelsPreview =
    props.upstreamModelInfo.detectedModels.slice(
      0,
      UPSTREAM_DETECTED_MODEL_PREVIEW_LIMIT
    )
  const upstreamDetectedModelsOmittedCount =
    props.upstreamModelInfo.detectedModels.length -
    upstreamDetectedModelsPreview.length

  return (
    <ChannelSettingsCard
      id={props.id}
      title={t('Upstream Model Detection Settings')}
      description={t(
        "Check the upstream's model list on a schedule and optionally sync it."
      )}
      icon={<RefreshCw className='h-4 w-4' />}
      iconTone='info'
      configured={props.configured}
      sensitiveLocked={props.sensitiveLocked}
    >
      <ChannelSettingRow setting={props.setting} layout='block'>
        <div className='divide-border space-y-0 divide-y border-y'>
          <FormField
            control={form.control}
            name='upstream_model_update_check_enabled'
            render={({ field }) => (
              <FormItem className='flex items-center justify-between px-4 py-3'>
                <div className='space-y-0.5'>
                  <FormLabel>{t('Upstream Model Update Check')}</FormLabel>
                  <FormDescription>
                    {t('Periodically check for upstream model changes')}
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
          <FormField
            control={form.control}
            name='upstream_model_update_auto_sync_enabled'
            render={({ field }) => (
              <FormItem className='flex items-center justify-between px-4 py-3'>
                <div className='space-y-0.5'>
                  <FormLabel>{t('Auto Sync Upstream Models')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Automatically sync model list when upstream changes are detected'
                    )}
                  </FormDescription>
                </div>
                <FormControl>
                  <Switch
                    checked={field.value}
                    disabled={!upstreamModelUpdateCheckEnabled}
                    onCheckedChange={field.onChange}
                  />
                </FormControl>
              </FormItem>
            )}
          />
        </div>
        <FormField
          control={form.control}
          name='upstream_model_update_ignored_models'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Ignored upstream models')}</FormLabel>
              <FormControl>
                <Input
                  placeholder={t(
                    'e.g., gpt-4.1-nano,regex:^claude-.*$,regex:^sora-.*$'
                  )}
                  {...field}
                />
              </FormControl>
              <FormDescription>
                {t(
                  'Comma-separated exact model names. Prefix with regex: to ignore by regular expression.'
                )}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
        <div className='text-muted-foreground space-y-2 border-t pt-3 text-xs'>
          <div>
            <span className='text-foreground font-medium'>
              {t('Last check time')}:
            </span>{' '}
            {formatUnixTime(props.upstreamModelInfo.lastCheckTime)}
          </div>
          <div>
            <span className='text-foreground font-medium'>
              {t('Last detected addable models')}:
            </span>{' '}
            {props.upstreamModelInfo.detectedModels.length === 0 ? (
              t('None')
            ) : (
              <>
                <span className='break-all'>
                  {upstreamDetectedModelsPreview.join(', ')}
                </span>
                {upstreamDetectedModelsOmittedCount > 0 && (
                  <span className='ml-1'>
                    {t('({{total}} total, {{omit}} omitted)', {
                      total: props.upstreamModelInfo.detectedModels.length,
                      omit: upstreamDetectedModelsOmittedCount,
                    })}
                  </span>
                )}
              </>
            )}
          </div>
        </div>
      </ChannelSettingRow>
    </ChannelSettingsCard>
  )
}
