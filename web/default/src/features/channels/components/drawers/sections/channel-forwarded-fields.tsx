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
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import { Switch } from '@/components/ui/switch'
import { cn } from '@/lib/utils'

import type { ChannelFormValues } from '../../../lib/channel-form'
import {
  FORWARDED_REQUEST_FIELDS,
  forwardedRequestFieldSettingValue,
  isRequestFieldForwarded,
  type ChannelSettingViews,
  type ForwardedRequestFieldSetting,
} from '../../../lib/channel-setting-visibility'
import { ChannelSettingRow } from './channel-setting-row'

type ChannelForwardedFieldsProps = {
  settings: ChannelSettingViews
}

/**
 * The request fields the gateway strips unless the channel opts in, as one
 * compact table: field name, what it is, and a switch that always means
 * "forward this field".
 */
export function ChannelForwardedFields(props: ChannelForwardedFieldsProps) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()

  const rows = FORWARDED_REQUEST_FIELDS.filter(
    (row) => props.settings[row.setting].state !== 'hidden'
  )
  if (rows.length === 0) return null

  const hints: Record<ForwardedRequestFieldSetting, string> = {
    allow_service_tier: t(
      'Service tier. May change how the upstream bills the request.'
    ),
    disable_store: t(
      'Lets the upstream store the response. Forwarded by default; Codex relies on it.'
    ),
    allow_safety_identifier: t(
      'End-user identifier for abuse monitoring. Reveals who the user is to the upstream.'
    ),
    allow_include_obfuscation: t(
      'Lets the client switch off stream obfuscation padding.'
    ),
    allow_inference_geo: t(
      'Region where the request is processed (data residency).'
    ),
    allow_speed: t('Claude inference speed mode.'),
  }

  return (
    <div className='border-border/60 rounded-lg border'>
      <div className='space-y-1 px-4 py-3'>
        <h5 className='text-sm font-medium'>
          {t('Request fields forwarded upstream')}
        </h5>
        <p className='text-muted-foreground text-xs'>
          {t(
            'The gateway removes these fields from requests by default. Turn on the ones this upstream should receive. No filtering happens while body passthrough is on.'
          )}
        </p>
      </div>
      {/* From sm up the rows share three columns: name, hint, switch. */}
      <div className='divide-border grid divide-y border-t sm:grid-cols-[auto_minmax(0,1fr)_auto]'>
        {rows.map((row) => {
          // Inside the amber notice a row keeps the stacked layout: it no
          // longer sits on the shared columns.
          const aligned = props.settings[row.setting].state === 'applicable'
          return (
            <ChannelSettingRow
              key={row.setting}
              setting={props.settings[row.setting]}
              className='sm:col-span-full'
            >
              <FormField
                control={form.control}
                name={row.setting}
                render={({ field }) => (
                  <FormItem
                    className={cn(
                      'flex items-center justify-between gap-3 px-4 py-2',
                      aligned &&
                        'sm:col-span-full sm:grid sm:grid-cols-subgrid sm:gap-x-4'
                    )}
                  >
                    <div
                      className={cn(
                        'flex min-w-0 flex-col gap-1',
                        aligned && 'sm:contents'
                      )}
                    >
                      <FormLabel>
                        <code className='bg-muted rounded px-1 py-0.5 text-xs font-normal break-all'>
                          {row.field}
                        </code>
                      </FormLabel>
                      <FormDescription className='text-xs'>
                        {hints[row.setting]}
                      </FormDescription>
                    </div>
                    <FormControl>
                      <Switch
                        checked={isRequestFieldForwarded(
                          row.setting,
                          field.value
                        )}
                        onCheckedChange={(checked) =>
                          field.onChange(
                            forwardedRequestFieldSettingValue(
                              row.setting,
                              checked
                            )
                          )
                        }
                      />
                    </FormControl>
                  </FormItem>
                )}
              />
            </ChannelSettingRow>
          )
        })}
      </div>
    </div>
  )
}
