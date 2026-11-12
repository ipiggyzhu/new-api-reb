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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectGroup,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'

import { getChannelTLSFingerprints, setMultiKeyEnv } from '../../api'
import { channelsQueryKeys, parseChannelSettings } from '../../lib'
import { keyEnvironmentPool } from '../../lib/channel-key-environment'
import type { Channel, KeyStatus } from '../../types'

// The key list masks a proxy password the way Go's url.Redacted does. Saving an
// edited address that still carries the mask would make "xxxxx" the password.
const MASKED_PROXY_PASSWORD = ':xxxxx@'

type MultiKeyEnvDialogProps = {
  channel: Channel
  keyStatus: KeyStatus
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

export function MultiKeyEnvDialog(props: MultiKeyEnvDialogProps) {
  const { t } = useTranslation()
  const settings = parseChannelSettings(props.channel.setting)
  const family = settings.synthetic_client_headers_profile ?? ''
  const hasCLIFamily = family === 'claude' || family === 'codex'
  const savedCLIVersion = props.keyStatus.cli_profile ?? ''
  const savedProxy = props.keyStatus.proxy ?? ''
  const [cliVersion, setCLIVersion] = useState(savedCLIVersion)
  const [proxy, setProxy] = useState(savedProxy)
  const [isSaving, setIsSaving] = useState(false)
  const { data, isPending, isError } = useQuery({
    queryKey: channelsQueryKeys.tlsFingerprints(),
    queryFn: getChannelTLSFingerprints,
    staleTime: Infinity,
    enabled: hasCLIFamily,
  })
  const cliItems = [
    { value: 'unassigned', label: t('Unassigned') },
    ...keyEnvironmentPool(
      data?.cli_profiles ?? [],
      family,
      settings.key_environment_cache_ttl === '1h' ? '1h' : '5m'
    ).map((profile) => ({ value: profile.id, label: profile.label })),
  ]
  const cliVersionChanged = cliVersion !== savedCLIVersion
  const proxyChanged = proxy.trim() !== savedProxy
  const proxyKeepsMaskedPassword =
    proxyChanged && proxy.includes(MASKED_PROXY_PASSWORD)

  const handleSave = async () => {
    setIsSaving(true)
    try {
      // An omitted field keeps the key's value, so an untouched proxy keeps its
      // real password.
      const response = await setMultiKeyEnv(
        props.channel.id,
        props.keyStatus.index,
        {
          cli_profile: cliVersionChanged ? cliVersion : undefined,
          proxy: proxyChanged ? proxy.trim() : undefined,
        }
      )
      if (response.success) {
        toast.success(response.message || t('Operation successful'))
        props.onSaved()
        props.onOpenChange(false)
      } else {
        toast.error(response.message || t('Operation failed'))
      }
    } catch (error: unknown) {
      toast.error(
        error instanceof Error ? error.message : t('Operation failed')
      )
    } finally {
      setIsSaving(false)
    }
  }

  return (
    <Dialog
      open
      onOpenChange={props.onOpenChange}
      title={
        <>
          {t('Edit Key Environment')}
          <StatusBadge
            label={`#${props.keyStatus.index + 1}`}
            variant='neutral'
            copyable={false}
          />
        </>
      }
      titleClassName='flex items-center gap-2'
      description={t(
        'Select a CLI version for this key. An unassigned key serves no requests; an empty proxy uses the channel proxy.'
      )}
      contentHeight='auto'
      bodyClassName='py-4'
      footer={
        <>
          <Button
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={isSaving}
          >
            {t('Cancel')}
          </Button>
          <Button
            onClick={handleSave}
            disabled={
              isSaving ||
              (!cliVersionChanged && !proxyChanged) ||
              proxyKeepsMaskedPassword
            }
          >
            {isSaving && <Spinner />}
            {isSaving ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor='multi-key-env-cli-version'>
            {t('CLI version (includes TLS)')}
          </FieldLabel>
          {hasCLIFamily ? (
            <Select
              items={cliItems}
              value={cliVersion || 'unassigned'}
              disabled={isSaving || isPending || isError}
              onValueChange={(next) => {
                if (next === null) return
                setCLIVersion(next === 'unassigned' ? '' : next)
              }}
            >
              <SelectTrigger id='multi-key-env-cli-version' className='w-full'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent alignItemWithTrigger={false}>
                <SelectGroup>
                  {cliItems.map((item) => (
                    <SelectItem key={item.value} value={item.value}>
                      {item.label}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          ) : (
            <FieldDescription>
              {t(
                "A per-key CLI version needs the channel's client type to be Claude Code or Codex."
              )}
            </FieldDescription>
          )}
          {isError && (
            <p className='text-destructive text-xs'>
              {t('Failed to load CLI version profiles')}
            </p>
          )}
        </Field>
        <Field>
          <FieldLabel htmlFor='multi-key-env-proxy'>
            {t('Proxy Address')}
          </FieldLabel>
          <Input
            id='multi-key-env-proxy'
            className='font-mono'
            placeholder={t('socks5://user:pass@host:port')}
            value={proxy}
            onChange={(event) => setProxy(event.target.value)}
            disabled={isSaving}
          />
          <FieldDescription>
            {t(
              'Supports http, https, socks5 and socks5h. Leave empty to use the channel proxy.'
            )}
          </FieldDescription>
          {proxyKeepsMaskedPassword && (
            <p className='text-destructive text-xs'>
              {t(
                'The saved password is hidden. Enter the full proxy address, password included.'
              )}
            </p>
          )}
        </Field>
      </FieldGroup>
    </Dialog>
  )
}
