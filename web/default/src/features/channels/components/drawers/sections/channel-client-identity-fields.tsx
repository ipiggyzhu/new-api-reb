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
import { nanoid } from 'nanoid'
import { useFormContext, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import { Switch } from '@/components/ui/switch'

import type { ChannelFormValues } from '../../../lib/channel-form'
import { ChannelTLSFingerprintFields } from './channel-tls-fingerprint-fields'

export function ChannelClientIdentityFields() {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const enabled = useWatch({
    control: form.control,
    name: 'custom_client_identity_enabled',
  })
  const websocket = useWatch({
    control: form.control,
    name: 'websocket_transport',
  })

  return (
    <>
      <FormField
        control={form.control}
        name='custom_client_identity_enabled'
        render={({ field }) => (
          <FormItem className='px-4 py-3'>
            <div className='flex items-center justify-between gap-3'>
              <div className='space-y-0.5'>
                <FormLabel>
                  {t('Custom TLS, Device and Session Identity')}
                </FormLabel>
                <FormDescription>
                  {t(
                    'Choose a TLS preset and manually regenerate device/session identities. Save to apply. Off restores the current client profile defaults.'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  checked={field.value ?? false}
                  onCheckedChange={(checked) => {
                    field.onChange(checked)
                    if (checked) {
                      for (const name of [
                        'client_device_seed',
                        'client_session_seed',
                      ] as const) {
                        if (!form.getValues(name)) {
                          form.setValue(name, nanoid(), { shouldDirty: true })
                        }
                      }
                    }
                  }}
                />
              </FormControl>
            </div>
          </FormItem>
        )}
      />
      {enabled && (
        <div className='border-border mx-4 mb-3 rounded-lg border'>
          <ChannelTLSFingerprintFields />
          <FormField
            control={form.control}
            name='client_device_seed'
            render={({ field, fieldState }) => (
              <FormItem className='px-4 py-3'>
                <div className='flex flex-wrap items-center justify-between gap-2'>
                  <FormLabel>{t('Device Identity')}</FormLabel>
                  <FormControl>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() => field.onChange(nanoid())}
                    >
                      {t('Regenerate Device Identity')}
                    </Button>
                  </FormControl>
                </div>
                <FormDescription aria-live='polite'>
                  {field.value
                    ? t('Saved configuration stays stable until regenerated.')
                    : t('Using the default isolated identity.')}
                  {fieldState.isDirty && (
                    <span className='text-warning block'>
                      {t('New identity configuration pending save.')}
                    </span>
                  )}
                </FormDescription>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='client_session_seed'
            render={({ field, fieldState }) => (
              <FormItem className='px-4 py-3'>
                <div className='flex flex-wrap items-center justify-between gap-2'>
                  <FormLabel>{t('Session Identity')}</FormLabel>
                  <FormControl>
                    <Button
                      type='button'
                      variant='outline'
                      size='sm'
                      onClick={() => field.onChange(nanoid())}
                    >
                      {t('Regenerate Session Identity')}
                    </Button>
                  </FormControl>
                </div>
                <FormDescription aria-live='polite'>
                  {field.value
                    ? t('Saved configuration stays stable until regenerated.')
                    : t('Using the default isolated identity.')}
                  {fieldState.isDirty && (
                    <span className='text-warning block'>
                      {t('New identity configuration pending save.')}
                    </span>
                  )}
                </FormDescription>
              </FormItem>
            )}
          />
          <p className='text-muted-foreground px-4 pb-3 text-xs'>
            {t(
              'Device/session overrides apply to matching Claude Code or Codex requests only. Users and conversations remain isolated. Rotating sessions changes cache keys; TLS can be configured independently.'
            )}
          </p>
          {websocket && (
            <p className='text-warning px-4 pb-3 text-xs'>
              {t(
                'TLS presets apply to HTTPS requests; WebSocket keeps its existing handshake.'
              )}
            </p>
          )}
        </div>
      )}
    </>
  )
}
