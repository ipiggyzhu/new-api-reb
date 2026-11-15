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
import { Fingerprint } from 'lucide-react'
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

import type { ChannelFormValues } from '../../../lib/channel-form'
import { ChannelClientIdentityFields } from './channel-client-identity-fields'
import { ChannelSettingsCard } from './channel-settings-card'

type ChannelClientIdentitySectionProps = {
  id: string
  configured: boolean
  sensitiveLocked: boolean
  /** The channel holds (or is being created with) several keys. */
  multiKey: boolean
  /** Keys in the channel, for the key environment summary. */
  keyCount?: number
}

export function ChannelClientIdentitySection(
  props: ChannelClientIdentitySectionProps
) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()

  return (
    <ChannelSettingsCard
      id={props.id}
      title={t('Client identity')}
      description={t(
        'Which client the upstream sees when the gateway calls it.'
      )}
      icon={<Fingerprint className='h-4 w-4' />}
      iconTone='chart-4'
      configured={props.configured}
      sensitiveLocked={props.sensitiveLocked}
    >
      <div className='divide-border space-y-0 divide-y border-y'>
        <FormField
          control={form.control}
          name='synthetic_client_headers_profile'
          render={({ field }) => (
            <FormItem className='px-4 py-3'>
              <FormLabel>{t('Client type')}</FormLabel>
              <Select
                items={[
                  {
                    value: 'off',
                    label: t('Off — send no client profile'),
                  },
                  {
                    value: 'claude',
                    label: t('Claude Code CLI'),
                  },
                  {
                    value: 'openai',
                    label: t('openai-python SDK'),
                  },
                  {
                    value: 'codex',
                    label: t('Codex CLI'),
                  },
                  {
                    value: 'gemini',
                    label: t('google-genai SDK'),
                  },
                  {
                    value: 'generic',
                    label: t('Generic HTTP client'),
                  },
                ]}
                onValueChange={(next) => {
                  if (!next || next === field.value) {
                    return
                  }
                  field.onChange(next)
                  form.setValue('cli_version_profile', '', {
                    shouldDirty: true,
                  })
                  form.setValue('tls_fingerprint', '', {
                    shouldDirty: true,
                  })
                  form.setValue('tls_fingerprint_enabled', false, {
                    shouldDirty: true,
                  })
                }}
                value={field.value ?? 'off'}
              >
                <FormControl>
                  <SelectTrigger className='w-full sm:w-72'>
                    <SelectValue placeholder={t('Select client profile')} />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem value='off'>
                      {t('Off — send no client profile')}
                    </SelectItem>
                    <SelectItem value='claude'>
                      {t('Claude Code CLI')}
                    </SelectItem>
                    <SelectItem value='openai'>
                      {t('openai-python SDK')}
                    </SelectItem>
                    <SelectItem value='codex'>{t('Codex CLI')}</SelectItem>
                    <SelectItem value='gemini'>
                      {t('google-genai SDK')}
                    </SelectItem>
                    <SelectItem value='generic'>
                      {t('Generic HTTP client')}
                    </SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormDescription>
                {t(
                  'Choose the client type first, then its version below. Changing the client type resets the version and any legacy TLS override; device/session identities and requested reasoning effort stay unchanged.'
                )}
              </FormDescription>
              {(field.value === 'claude' || field.value === 'codex') && (
                <p className='text-warning mt-2 text-xs'>
                  {t(
                    'Body alignment requires the upstream {{endpoint}} protocol. Selecting a client profile does not switch endpoints: requests using other protocols only receive the selected headers. Configure a protocol converter separately if needed.',
                    {
                      endpoint:
                        field.value === 'claude'
                          ? 'Messages (/v1/messages)'
                          : 'Responses (/v1/responses)',
                    }
                  )}
                </p>
              )}
            </FormItem>
          )}
        />

        <ChannelClientIdentityFields
          multiKey={props.multiKey}
          keyCount={props.keyCount}
        />
      </div>
    </ChannelSettingsCard>
  )
}
