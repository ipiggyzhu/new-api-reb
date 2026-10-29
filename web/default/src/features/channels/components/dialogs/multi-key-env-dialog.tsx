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
import { Loader2 } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import { getChannelTLSFingerprints, setMultiKeyEnv } from '../../api'
import { channelsQueryKeys, parseChannelSettings } from '../../lib'
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
  const family = parseChannelSettings(
    props.channel.setting
  ).synthetic_client_headers_profile
  const hasCLIFamily = family === 'claude' || family === 'codex'
  const savedCLIVersion = props.keyStatus.cli_version_profile ?? ''
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
    { value: 'inherit', label: t('Inherit channel setting') },
    ...(data?.cli_profiles ?? [])
      .filter((profile) => profile.family === family)
      .map((profile) => ({ value: profile.id, label: profile.label })),
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
          cli_version_profile: cliVersionChanged ? cliVersion : undefined,
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
        'Empty fields inherit the channel setting. The key keeps its own device and session identity either way.'
      )}
      contentHeight='auto'
      bodyClassName='space-y-4'
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
            {isSaving && <Loader2 className='mr-2 h-4 w-4 animate-spin' />}
            {isSaving ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <div className='space-y-2'>
        <Label htmlFor='multi-key-env-cli-version'>
          {t('CLI version (includes TLS)')}
        </Label>
        {hasCLIFamily ? (
          <Select
            items={cliItems}
            value={cliVersion || 'inherit'}
            disabled={isSaving || isPending || isError}
            onValueChange={(next) => {
              if (next === null) return
              setCLIVersion(next === 'inherit' ? '' : next)
            }}
          >
            <SelectTrigger id='multi-key-env-cli-version' className='w-full'>
              <SelectValue />
            </SelectTrigger>
            <SelectContent alignItemWithTrigger={false}>
              {cliItems.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : (
          <p className='text-muted-foreground text-xs'>
            {t(
              "A per-key CLI version needs the channel's client type to be Claude Code or Codex."
            )}
          </p>
        )}
        {isError && (
          <p className='text-destructive text-xs'>
            {t('Failed to load CLI version profiles')}
          </p>
        )}
      </div>
      <div className='space-y-2'>
        <Label htmlFor='multi-key-env-proxy'>{t('Proxy Address')}</Label>
        <Input
          id='multi-key-env-proxy'
          className='font-mono'
          placeholder={t('socks5://user:pass@host:port')}
          value={proxy}
          onChange={(event) => setProxy(event.target.value)}
          disabled={isSaving}
        />
        <p className='text-muted-foreground text-xs'>
          {t(
            'Supports http, https, socks5 and socks5h. Leave empty to use the channel proxy.'
          )}
        </p>
        {proxyKeepsMaskedPassword && (
          <p className='text-destructive text-xs'>
            {t(
              'The saved password is hidden. Enter the full proxy address, password included.'
            )}
          </p>
        )}
      </div>
    </Dialog>
  )
}
