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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import { getChannelTLSFingerprints } from '../../../api'
import { channelsQueryKeys } from '../../../lib/channel-actions'
import type { ChannelFormValues } from '../../../lib/channel-form'

export function ChannelCLIVersionFields() {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const family = useWatch({
    control: form.control,
    name: 'synthetic_client_headers_profile',
  })
  const { data, isPending, isError } = useQuery({
    queryKey: channelsQueryKeys.tlsFingerprints(),
    queryFn: getChannelTLSFingerprints,
    staleTime: Infinity,
  })
  if (family !== 'claude' && family !== 'codex') return null
  const profiles = (data?.cli_profiles ?? []).filter(
    (profile) => profile.family === family
  )
  const items = [
    { value: 'default', label: t('Keep existing client defaults') },
    ...profiles.map((profile) => ({ value: profile.id, label: profile.label })),
  ]

  return (
    <FormField
      control={form.control}
      name='cli_version_profile'
      render={({ field }) => {
        const value = field.value?.startsWith(`${family}-`) ? field.value : ''
        const selected = profiles.find((profile) => profile.id === value)
        return (
          <FormItem className='px-4 py-3'>
            <FormLabel>{t('CLI version (includes TLS)')}</FormLabel>
            <Select
              items={items}
              value={value || 'default'}
              disabled={isPending || isError}
              onValueChange={(next) => {
                field.onChange(next === 'default' ? '' : next)
                if (next && next !== 'default') {
                  form.setValue('tls_fingerprint', '', { shouldDirty: true })
                  form.setValue('tls_fingerprint_enabled', false, {
                    shouldDirty: true,
                  })
                }
              }}
            >
              <FormControl>
                <SelectTrigger className='w-full'>
                  <SelectValue />
                </SelectTrigger>
              </FormControl>
              <SelectContent alignItemWithTrigger={false}>
                {items.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <FormDescription>
              {t(
                'The selected version binds TLS, headers and JSON layout together. TLS cannot be selected separately. HTTPS HTTP/1.1 only; disable WebSocket. Requested reasoning effort is preserved. Different versions may share the same JA3.'
              )}
              {selected && (
                <span className='block'>JA3: {selected.ja3_md5}</span>
              )}
            </FormDescription>
            {isError && (
              <FormDescription>
                {t('Failed to load CLI version profiles')}
              </FormDescription>
            )}
            <FormMessage />
          </FormItem>
        )
      }}
    />
  )
}
