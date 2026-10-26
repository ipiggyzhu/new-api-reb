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
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { Switch } from '@/components/ui/switch'

import { getChannelTLSFingerprints } from '../../../api'
import { channelsQueryKeys } from '../../../lib/channel-actions'
import type { ChannelFormValues } from '../../../lib/channel-form'
import {
  keyEnvironmentPool,
  keyEnvironmentUnavailableReason,
} from '../../../lib/channel-key-environment'

type ChannelKeyEnvironmentFieldsProps = {
  /** The channel holds (or is being created with) several keys. */
  multiKey: boolean
  /** Keys in the channel, for the summary; undefined while unknown. */
  keyCount?: number
}

export function ChannelKeyEnvironmentFields(
  props: ChannelKeyEnvironmentFieldsProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const [family, channelType, enabled, cacheTTL] = useWatch({
    control: form.control,
    name: [
      'synthetic_client_headers_profile',
      'type',
      'key_environments',
      'key_environment_cache_ttl',
    ],
  })
  const { data } = useQuery({
    queryKey: channelsQueryKeys.tlsFingerprints(),
    queryFn: getChannelTLSFingerprints,
    staleTime: Infinity,
  })
  if (family !== 'claude' && family !== 'codex') return null

  const reason = keyEnvironmentUnavailableReason({
    family,
    channelType,
    multiKey: props.multiKey,
  })
  const pool = keyEnvironmentPool(
    data?.cli_profiles ?? [],
    family,
    cacheTTL ?? '5m'
  )

  return (
    <>
      <FormField
        control={form.control}
        name='key_environments'
        render={({ field }) => (
          <FormItem className='px-4 py-3'>
            <div className='flex items-center justify-between gap-3'>
              <div className='space-y-0.5'>
                <FormLabel>{t('Independent key environments')}</FormLabel>
                <FormDescription>
                  {t(
                    'Treat every key as its own account: each gets a different real CLI version, device and session identity. A conversation always stays on one key; when that key is rate limited or busy the request waits or returns an error instead of switching keys. Only when the key is disabled, deleted or loses its version does the conversation move to another key, once.'
                  )}
                </FormDescription>
                {reason && !field.value && (
                  <FormDescription className='text-warning'>
                    {t(reason)}
                  </FormDescription>
                )}
              </div>
              <FormControl>
                <Switch
                  checked={field.value ?? false}
                  disabled={Boolean(reason) && !field.value}
                  onCheckedChange={field.onChange}
                />
              </FormControl>
            </div>
            <FormMessage />
          </FormItem>
        )}
      />
      {enabled && family === 'claude' && (
        <FormField
          control={form.control}
          name='key_environment_cache_ttl'
          render={({ field }) => (
            <FormItem className='px-4 py-3'>
              <FormLabel>{t('Cache mode')}</FormLabel>
              <FormControl>
                <RadioGroup
                  value={field.value ?? '5m'}
                  onValueChange={field.onChange}
                  className='flex gap-4'
                >
                  <label className='flex items-center gap-2 text-sm'>
                    <RadioGroupItem value='5m' />
                    {t('5 minutes')}
                  </label>
                  <label className='flex items-center gap-2 text-sm'>
                    <RadioGroupItem value='1h' />
                    {t('1 hour')}
                  </label>
                </RadioGroup>
              </FormControl>
            </FormItem>
          )}
        />
      )}
      {enabled && (
        <p
          className='text-muted-foreground px-4 pb-3 text-xs'
          aria-live='polite'
        >
          {t(
            'Assignable real versions: {{count}} ({{versions}}) | Keys in this channel: {{keys}}',
            {
              count: pool.length,
              versions:
                pool.map((profile) => profile.version).join(', ') || '-',
              keys: props.keyCount ?? '-',
            }
          )}
        </p>
      )}
    </>
  )
}
