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
import { History } from 'lucide-react'
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'

import type { ChannelFormValues } from '../../../lib/channel-form'
import type { ChannelSettingViews } from '../../../lib/channel-setting-visibility'
import { ChannelSettingRow } from './channel-setting-row'
import { ChannelSettingsCard } from './channel-settings-card'

type ChannelSessionCompatSectionProps = {
  id: string
  configured: boolean
  sensitiveLocked: boolean
  settings: ChannelSettingViews
}

export function ChannelSessionCompatSection(
  props: ChannelSessionCompatSectionProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const settings = props.settings

  return (
    <ChannelSettingsCard
      id={props.id}
      title={t('Session compatibility')}
      description={t(
        'For long or resumed sessions that the upstream rejects or hangs on. Leave these off unless you see that problem.'
      )}
      icon={<History className='h-4 w-4' />}
      iconTone='neutral'
      configured={props.configured}
      sensitiveLocked={props.sensitiveLocked}
    >
      <div className='divide-border space-y-0 divide-y border-y'>
        <ChannelSettingRow
          setting={settings.responses_reasoning_fallback}
          reason={t(
            'Only acts on upstreams that speak the OpenAI Responses protocol.'
          )}
        >
          <FormField
            control={form.control}
            name='responses_reasoning_fallback'
            render={({ field }) => (
              <FormItem className='flex items-center justify-between gap-3 px-4 py-3'>
                <div className='space-y-0.5'>
                  <FormLabel>
                    {t('Responses Reasoning Compatibility')}
                  </FormLabel>
                  <FormDescription>
                    {t(
                      'Retry HTTP 400 invalid_encrypted_content once without old reasoning ciphertext and IDs. Preserve visible history, summaries and requested effort; reset hidden reasoning. Skip compacted or server-side history. Off by default.'
                    )}
                  </FormDescription>
                </div>
                <FormControl>
                  <Switch
                    checked={field.value ?? false}
                    onCheckedChange={field.onChange}
                  />
                </FormControl>
              </FormItem>
            )}
          />
        </ChannelSettingRow>

        <ChannelSettingRow
          setting={settings.claude_tool_result_images}
          reason={t(
            'Only acts on upstreams that receive Claude Messages requests.'
          )}
        >
          <FormField
            control={form.control}
            name='claude_tool_result_images'
            render={({ field }) => (
              <FormItem className='px-4 py-3'>
                <FormLabel>{t('Images in Tool Results')}</FormLabel>
                <Select
                  items={[
                    {
                      value: 'off',
                      label: t('Send as received'),
                    },
                    {
                      value: 'hoist',
                      label: t('Move after the tool results'),
                    },
                    {
                      value: 'placeholder',
                      label: t('Keep current turn only'),
                    },
                  ]}
                  onValueChange={field.onChange}
                  value={field.value ?? 'off'}
                >
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      <SelectItem value='off'>
                        {t('Send as received')}
                      </SelectItem>
                      <SelectItem value='hoist'>
                        {t('Move after the tool results')}
                      </SelectItem>
                      <SelectItem value='placeholder'>
                        {t('Keep current turn only')}
                      </SelectItem>
                    </SelectGroup>
                  </SelectContent>
                </Select>
                <FormDescription>
                  {t(
                    'For long agent sessions that hang or fail with 429/503 once screenshots pile up inside tool results. Moving keeps every image and only changes where it sits in the message. Keeping the current turn only also replaces images of earlier turns with a text note. Ignored when body passthrough is on.'
                  )}
                </FormDescription>
              </FormItem>
            )}
          />
        </ChannelSettingRow>
      </div>
    </ChannelSettingsCard>
  )
}
