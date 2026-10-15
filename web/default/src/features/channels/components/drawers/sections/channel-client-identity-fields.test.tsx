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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createInstance } from 'i18next'
import { renderToStaticMarkup } from 'react-dom/server'
import { FormProvider, useForm } from 'react-hook-form'
import { I18nextProvider } from 'react-i18next'

import type { ChannelFormValues } from '../../../lib/channel-form'
import { ChannelClientIdentityFields } from './channel-client-identity-fields'

async function renderIdentity(settings: Partial<ChannelFormValues>) {
  const client = new QueryClient()
  client.setQueryData(['channels', 'tls-fingerprints'], {
    success: true,
    data: [
      { id: 'claude-code', label: 'Claude TLS', client_family: 'claude' },
      { id: 'codex-cli', label: 'Codex TLS', client_family: 'codex' },
    ],
    cli_profiles: [
      {
        id: 'claude-test',
        family: 'claude',
        label: 'Claude version',
        ja3_md5: 'claude-ja3',
      },
      {
        id: 'codex-test',
        family: 'codex',
        label: 'Codex version',
        ja3_md5: 'codex-ja3',
      },
    ],
  })
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: { en: { translation: {} } } })
  function Fields() {
    const form = useForm<ChannelFormValues>({ defaultValues: settings })
    return (
      <FormProvider {...form}>
        <ChannelClientIdentityFields />
      </FormProvider>
    )
  }
  const markup = renderToStaticMarkup(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <Fields />
      </QueryClientProvider>
    </I18nextProvider>
  )
  client.clear()
  return markup
}

describe('single version/TLS selection', () => {
  for (const family of ['claude', 'codex'] as const) {
    test(`${family} exposes one version selector and read-only default TLS`, async () => {
      const html = await renderIdentity({
        synthetic_client_headers_profile: family,
      })
      assert.equal((html.match(/role="combobox"/g) ?? []).length, 1)
      assert.ok(html.includes('CLI version (includes TLS)'))
      assert.ok(html.includes(family === 'claude' ? 'Claude TLS' : 'Codex TLS'))
      assert.ok(!html.includes('Custom TLS Fingerprint'))
    })
    test(`${family} selected version shows its JA3 without a second TLS selector`, async () => {
      const html = await renderIdentity({
        synthetic_client_headers_profile: family,
        cli_version_profile: `${family}-test`,
      })
      assert.equal((html.match(/role="combobox"/g) ?? []).length, 1)
      assert.ok(html.includes(`JA3: ${family}-ja3`))
      assert.ok(!html.includes('Effective TLS'))
    })
  }
  test('a saved mixed-family TLS override is visible even when identity overrides are off', async () => {
    const html = await renderIdentity({
      synthetic_client_headers_profile: 'claude',
      tls_fingerprint_enabled: true,
      tls_fingerprint: 'codex-cli',
      custom_client_identity_enabled: false,
    })
    assert.ok(html.includes('Codex TLS'))
    assert.ok(html.includes('A saved legacy TLS override is still active'))
    assert.ok(html.includes('Restore TLS matching the client type'))
    assert.equal((html.match(/role="combobox"/g) ?? []).length, 1)
  })
})
