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

async function renderIdentity(
  settings: Partial<ChannelFormValues>,
  fieldProps: { multiKey?: boolean; keyCount?: number } = {}
) {
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
        version: '2.1.306',
        label: 'Claude version',
        ja3_md5: 'claude-ja3',
        auto_assign: true,
      },
      {
        id: 'claude-test-1h',
        family: 'claude',
        version: '2.1.306',
        label: 'Claude 1h version',
        ja3_md5: 'claude-ja3',
        cache_ttl: '1h',
        auto_assign: true,
      },
      {
        id: 'codex-test',
        family: 'codex',
        version: '0.163.0',
        label: 'Codex version',
        ja3_md5: 'codex-ja3',
        auto_assign: true,
      },
    ],
  })
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: { en: { translation: {} } } })
  function Fields() {
    const form = useForm<ChannelFormValues>({ defaultValues: settings })
    return (
      <FormProvider {...form}>
        <ChannelClientIdentityFields {...fieldProps} />
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

describe('independent key environment fields', () => {
  test('turning the switch on shows the pool and per-key TLS rule', async () => {
    const html = await renderIdentity(
      {
        type: 1,
        synthetic_client_headers_profile: 'codex',
        key_environments: true,
        websocket_transport: true,
        tls_fingerprint_enabled: true,
        tls_fingerprint: 'claude-code',
      },
      { multiKey: true, keyCount: 2 }
    )
    assert.ok(html.includes('Independent key environments'))
    assert.ok(
      html.includes(
        'Assignable real versions: 1 (0.163.0) | Keys in this channel: 2'
      )
    )
    assert.ok(!html.includes('CLI version (includes TLS)'))
    assert.equal((html.match(/role="combobox"/g) ?? []).length, 0)
    assert.ok(!html.includes('Cache mode'))
    assert.ok(html.includes('Effective TLS'))
    assert.ok(html.includes('TLS follows each key'))
    assert.ok(html.includes('Saved channel TLS override: claude-code'))
    assert.ok(html.includes('Existing connections keep their key'))
    assert.ok(html.includes('Realtime is supported on OpenAI channels'))
    assert.ok(!html.includes('A saved legacy TLS override is still active'))
  })

  test('Claude counts only the versions captured in the chosen cache mode', async () => {
    const html = await renderIdentity(
      {
        type: 14,
        synthetic_client_headers_profile: 'claude',
        key_environments: true,
        key_environment_cache_ttl: '1h',
      },
      { multiKey: true, keyCount: 3 }
    )
    assert.ok(html.includes('Cache mode'))
    assert.ok(
      html.includes(
        'Assignable real versions: 1 (2.1.306) | Keys in this channel: 3'
      )
    )
  })

  test('a single-key channel explains why the switch is unavailable', async () => {
    const html = await renderIdentity(
      { type: 14, synthetic_client_headers_profile: 'claude' },
      { multiKey: false }
    )
    assert.ok(
      html.includes('Independent key environments need a multi-key channel')
    )
    assert.ok(html.includes('CLI version (includes TLS)'))
  })
})
