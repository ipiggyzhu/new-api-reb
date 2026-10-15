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

import { Button } from '@/components/ui/button'

import { getChannelTLSFingerprints } from '../../../api'
import { channelsQueryKeys } from '../../../lib/channel-actions'
import type { ChannelFormValues } from '../../../lib/channel-form'

// Legacy overrides are displayed, never silently migrated or offered as new
// choices. CLI versions are the single selection point for complete profiles.
export function ChannelTLSFingerprintFields() {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const [family, enabled, fingerprint] = useWatch({
    control: form.control,
    name: [
      'synthetic_client_headers_profile',
      'tls_fingerprint_enabled',
      'tls_fingerprint',
    ],
  })
  const { data, isError } = useQuery({
    queryKey: channelsQueryKeys.tlsFingerprints(),
    queryFn: getChannelTLSFingerprints,
    staleTime: Infinity,
  })
  const options = data?.data ?? []
  const hasLegacy = Boolean(enabled && fingerprint)
  const effective = hasLegacy
    ? options.find((option) => option.id === fingerprint)
    : options.find((option) => option.client_family === family)
  let label = t('Loading...')
  if (hasLegacy) {
    label = effective?.label ?? fingerprint ?? ''
  } else if (data?.success) {
    label = effective?.label ?? t('Default Go TLS handshake')
  } else if (isError) {
    label = t('Failed to load TLS fingerprints')
  }

  return (
    <div className='space-y-2 px-4 py-3'>
      <p className='text-sm font-medium'>{t('Effective TLS')}</p>
      <p className='text-muted-foreground text-sm'>{label}</p>
      {hasLegacy ? (
        <>
          <p className='text-warning text-xs'>
            {t(
              'A saved legacy TLS override is still active and may not match the client type. Opening or saving this form will not migrate it. Select a CLI version or explicitly restore matching TLS below.'
            )}
          </p>
          <Button
            type='button'
            variant='outline'
            size='sm'
            onClick={() => {
              form.setValue('tls_fingerprint', '', { shouldDirty: true })
              form.setValue('tls_fingerprint_enabled', false, {
                shouldDirty: true,
              })
            }}
          >
            {t('Restore TLS matching the client type')}
          </Button>
        </>
      ) : (
        <p className='text-muted-foreground text-xs'>
          {t(
            'TLS follows the client type. Select a CLI version above to pin TLS, headers and JSON layout together.'
          )}
        </p>
      )}
    </div>
  )
}
