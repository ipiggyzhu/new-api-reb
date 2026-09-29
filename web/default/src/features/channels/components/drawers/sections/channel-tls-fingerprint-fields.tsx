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

import { getChannelTLSFingerprints } from '../../../api'
import { channelsQueryKeys } from '../../../lib/channel-actions'
import type { ChannelFormValues } from '../../../lib/channel-form'

/**
 * TLS fingerprint override for a channel. Off, the handshake follows the
 * synthetic client header profile (Claude Code / Codex headers bring that
 * CLI's captured handshake). A Node override also updates only the runtime
 * declarations that exist in the Claude sample; body and identities stay put.
 */
export function ChannelTLSFingerprintFields() {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const headerProfile = useWatch({
    control: form.control,
    name: 'synthetic_client_headers_profile',
  })
  const enabled = useWatch({
    control: form.control,
    name: 'tls_fingerprint_enabled',
  })

  const { data } = useQuery({
    queryKey: channelsQueryKeys.tlsFingerprints(),
    queryFn: getChannelTLSFingerprints,
    staleTime: Infinity,
  })
  const fingerprint = useWatch({
    control: form.control,
    name: 'tls_fingerprint',
  })
  const options = data?.data ?? []
  const legacySelection = Boolean(
    data?.success &&
    fingerprint &&
    !options.some((option) => option.id === fingerprint)
  )
  const items = options.map((option) => ({
    value: option.id,
    label: option.label,
  }))
  if (legacySelection && fingerprint) {
    items.unshift({
      value: fingerprint,
      label: t('Saved legacy TLS preset: {{id}}', { id: fingerprint }),
    })
  }
  const followed = options.find(
    (option) =>
      option.client_family !== undefined &&
      option.client_family === headerProfile
  )

  // Until the backend list arrives nothing can say which handshake the headers
  // imply, so no description is shown rather than a wrong one.
  let followedDescription = ''
  if (followed) {
    followedDescription = t(
      'Off: follows the client headers and uses the {{label}} handshake.',
      { label: followed.label }
    )
  } else if (data?.success) {
    followedDescription = t(
      'Off: no captured handshake matches the client headers, so the default Go handshake is used.'
    )
  }

  return (
    <>
      <FormField
        control={form.control}
        name='tls_fingerprint_enabled'
        render={({ field }) => (
          <FormItem className='px-4 py-3'>
            <div className='flex items-center justify-between'>
              <div className='space-y-0.5'>
                <FormLabel>{t('Custom TLS Fingerprint')}</FormLabel>
                {followedDescription && (
                  <FormDescription>{followedDescription}</FormDescription>
                )}
              </div>
              <FormControl>
                <Switch
                  checked={field.value ?? false}
                  onCheckedChange={(checked) => {
                    field.onChange(checked)
                    if (checked && !form.getValues('tls_fingerprint')) {
                      form.setValue(
                        'tls_fingerprint',
                        followed?.id ?? options[0]?.id ?? '',
                        { shouldDirty: true }
                      )
                    }
                  }}
                />
              </FormControl>
            </div>
          </FormItem>
        )}
      />

      {enabled && (
        <FormField
          control={form.control}
          name='tls_fingerprint'
          render={({ field }) => (
            <FormItem className='px-4 py-3'>
              <FormLabel>{t('TLS Fingerprint')}</FormLabel>
              <Select
                items={items}
                onValueChange={field.onChange}
                value={field.value ?? ''}
              >
                <FormControl>
                  <SelectTrigger className='w-full sm:w-72'>
                    <SelectValue placeholder={t('Select TLS fingerprint')} />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    {items.map((option) => (
                      <SelectItem
                        key={option.value}
                        value={option.value}
                        disabled={
                          legacySelection && option.value === fingerprint
                        }
                      >
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormDescription>
                {t(
                  'TLS and existing Claude runtime headers follow the preset. Prompt, tools and identities stay unchanged. Different Node versions may share a handshake shape.'
                )}
              </FormDescription>
            </FormItem>
          )}
        />
      )}
    </>
  )
}
