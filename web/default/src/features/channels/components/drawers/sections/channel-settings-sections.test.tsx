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
import type { ReactNode } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { FormProvider, useForm, type FieldErrors } from 'react-hook-form'
import { I18nextProvider } from 'react-i18next'

import {
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToUpdatePayload,
  type ChannelFormValues,
} from '../../../lib/channel-form'
import { hasAdvancedSettingsErrors } from '../../../lib/channel-form-errors'
import { getChannelSettingsOverview } from '../../../lib/channel-setting-visibility'
import { channelSchema } from '../../../types'
import { ChannelRequestRewritingSection } from './channel-request-rewriting-section'
import { ChannelSettingRow } from './channel-setting-row'
import {
  ChannelSettingsSections,
  type ChannelSettingsSectionsProps,
} from './channel-settings-sections'

const SECTION_IDS = {
  connection: 'card-connection',
  clientIdentity: 'card-client-identity',
  requestRewriting: 'card-request-rewriting',
  sessionCompat: 'card-session-compat',
  upstreamModelDetection: 'card-upstream-model-detection',
}

const INACTIVE_BADGE = 'Not used by this channel type'

/** A stored channel: `setting` and `settings` are its two settings JSON blobs. */
function storedChannel(
  type: number,
  setting: Record<string, unknown> = {},
  settings: Record<string, unknown> = {}
) {
  return channelSchema.parse({
    id: 25,
    type,
    name: 'agent',
    key: 'test-key',
    status: 1,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    models: 'gpt-test',
    setting: JSON.stringify(setting),
    settings: JSON.stringify(settings),
  })
}

async function renderInForm(
  values: ChannelFormValues,
  children: ReactNode,
  errors?: FieldErrors<ChannelFormValues>
) {
  const client = new QueryClient()
  client.setQueryData(['channels', 'tls-fingerprints'], {
    success: true,
    data: [{ id: 'codex-cli', label: 'Codex TLS', client_family: 'codex' }],
    cli_profiles: [
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
    const form = useForm<ChannelFormValues>({ defaultValues: values, errors })
    return <FormProvider {...form}>{children}</FormProvider>
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

/** The settings cards as the drawer shows them for a channel being edited. */
function renderSections(
  channel: ReturnType<typeof storedChannel>,
  props: Partial<ChannelSettingsSectionsProps> = {},
  errors?: FieldErrors<ChannelFormValues>
) {
  return renderInForm(
    transformChannelToFormDefaults(channel),
    <ChannelSettingsSections
      sensitiveLocked={false}
      isEditing
      multiKey={false}
      websocketDowngraded={false}
      upstreamModelInfo={{ lastCheckTime: 0, detectedModels: [] }}
      upstreamKeepaliveLastTime={0}
      sectionIds={SECTION_IDS}
      {...props}
    />,
    errors
  )
}

function count(html: string, text: string): number {
  return html.split(text).length - 1
}

/** The markup of one card: from its id up to the next card. */
function cardMarkup(html: string, card: keyof typeof SECTION_IDS): string {
  const start = html.indexOf(`id="${SECTION_IDS[card]}"`)
  assert.notEqual(start, -1, `${card} card is rendered`)
  const next = html.indexOf('id="card-', start + 1)
  return html.slice(start, next === -1 ? undefined : next)
}

/** The request fields listed in the forwarded-fields block, in order. */
function forwardedFields(html: string): string[] {
  return [...html.matchAll(/<code[^>]*>([^<]+)<\/code>/g)].map(
    (match) => match[1]
  )
}

/** Whether the first switch after `marker` is on. */
function switchIsOn(html: string, marker: string): boolean {
  const start = html.indexOf(marker)
  assert.notEqual(start, -1, `${marker} is rendered`)
  const state = html
    .slice(start)
    .match(/role="switch"[^>]*aria-checked="(true|false)"/)
  assert.ok(state, `a switch follows ${marker}`)
  return state[1] === 'true'
}

describe('options follow the channel type', () => {
  test('an option set on a type that ignores it stays visible, marked, with the reason', async () => {
    const html = await renderSections(
      storedChannel(14, { responses_reasoning_fallback: true })
    )
    const card = cardMarkup(html, 'sessionCompat')
    assert.ok(card.includes('Responses Reasoning Compatibility'))
    assert.equal(count(html, INACTIVE_BADGE), 1)
    assert.ok(
      card.includes(
        'Only acts on upstreams that speak the OpenAI Responses protocol. It is still set. You can turn it off.'
      )
    )
    assert.equal(switchIsOn(card, 'Responses Reasoning Compatibility'), true)
    // The option this type does use sits next to it, unmarked.
    assert.ok(card.includes('Images in Tool Results'))
  })

  test('the same option at its default is left out', async () => {
    const html = await renderSections(
      storedChannel(14, { responses_reasoning_fallback: false })
    )
    assert.ok(!html.includes('Responses Reasoning Compatibility'))
    assert.ok(!html.includes(INACTIVE_BADGE))
    assert.ok(
      cardMarkup(html, 'sessionCompat').includes('Images in Tool Results')
    )
  })

  test('an OpenAI channel offers the Responses option and not the tool-result images', async () => {
    const html = await renderSections(storedChannel(1))
    assert.ok(html.includes('Responses Reasoning Compatibility'))
    assert.ok(!html.includes('Images in Tool Results'))
    assert.ok(!html.includes(INACTIVE_BADGE))

    const stillSet = await renderSections(
      storedChannel(1, { claude_tool_result_images: 'hoist' })
    )
    assert.ok(stillSet.includes('Images in Tool Results'))
    assert.ok(
      stillSet.includes(
        'Only acts on upstreams that receive Claude Messages requests. It is still set.'
      )
    )
  })

  test('WebSocket transport is not offered on Anthropic unless it is on', async () => {
    const anthropic = await renderSections(storedChannel(14))
    assert.ok(!anthropic.includes('WebSocket Transport'))

    const stillOn = await renderSections(
      storedChannel(14, { websocket_transport: true })
    )
    const card = cardMarkup(stillOn, 'connection')
    assert.ok(card.includes('WebSocket Transport'))
    assert.ok(card.includes(INACTIVE_BADGE))
    assert.ok(
      card.includes(
        'Only OpenAI and Codex channels can use the Responses WebSocket transport.'
      )
    )

    const openai = await renderSections(storedChannel(1))
    assert.ok(openai.includes('WebSocket Transport'))
    assert.ok(!openai.includes(INACTIVE_BADGE))
  })

  test('options that apply to every type are always there', async () => {
    // AWS Bedrock is on none of the "only" lists.
    const html = await renderSections(storedChannel(33))
    for (const label of [
      'Proxy Address',
      'Skip async task polling delay',
      'Enable upstream keepalive',
      'Client type',
      'Pass Through Body',
      'System Prompt',
      'Also prepend to requests that have their own',
      'Thinking to Content',
      'Images in Tool Results',
    ]) {
      assert.ok(html.includes(label), label)
    }
    for (const label of [
      'Force Format',
      'WebSocket Transport',
      'Responses Reasoning Compatibility',
      'Append ?beta=true to Messages requests',
      'Request fields forwarded upstream',
      'Upstream Model Detection Settings',
      INACTIVE_BADGE,
    ]) {
      assert.ok(!html.includes(label), label)
    }
  })
})

describe('request fields forwarded upstream', () => {
  const rowsByType: [number, string[]][] = [
    [
      1,
      [
        'service_tier',
        'store',
        'safety_identifier',
        'stream_options.include_obfuscation',
        'inference_geo',
      ],
    ],
    [
      57,
      [
        'service_tier',
        'store',
        'safety_identifier',
        'stream_options.include_obfuscation',
        'inference_geo',
      ],
    ],
    [14, ['service_tier', 'inference_geo', 'speed']],
    [33, []],
  ]
  for (const [type, fields] of rowsByType) {
    test(`type ${type} lists ${fields.join(', ') || 'no fields'}`, async () => {
      const html = await renderSections(storedChannel(type))
      assert.deepEqual(forwardedFields(html), fields)
      assert.equal(
        html.includes('Request fields forwarded upstream'),
        fields.length > 0
      )
    })
  }

  test('store is forwarded by default, the other fields are not', async () => {
    const html = await renderSections(storedChannel(1))
    assert.equal(switchIsOn(html, '>store</code>'), true)
    assert.equal(switchIsOn(html, '>service_tier</code>'), false)
    assert.equal(switchIsOn(html, '>safety_identifier</code>'), false)
  })

  test('each switch shows whether the field is forwarded, whatever its stored name', async () => {
    const html = await renderSections(
      storedChannel(1, {}, { disable_store: true, allow_service_tier: true })
    )
    assert.equal(switchIsOn(html, '>store</code>'), false)
    assert.equal(switchIsOn(html, '>service_tier</code>'), true)
  })

  test('a field set on a type that does not take it is listed, marked', async () => {
    const html = await renderSections(
      storedChannel(33, {}, { allow_safety_identifier: true })
    )
    assert.deepEqual(forwardedFields(html), ['safety_identifier'])
    assert.equal(count(html, INACTIVE_BADGE), 1)
    assert.ok(html.includes('Not normally offered for this channel type.'))
  })

  test('the Anthropic beta query sits with the request options, reworded', async () => {
    const html = await renderSections(
      storedChannel(14, {}, { claude_beta_query: true })
    )
    const card = cardMarkup(html, 'requestRewriting')
    assert.equal(
      switchIsOn(card, 'Append ?beta=true to Messages requests'),
      true
    )
    assert.ok(!card.includes(INACTIVE_BADGE))

    const openai = await renderSections(
      storedChannel(1, {}, { claude_beta_query: true })
    )
    assert.ok(
      cardMarkup(openai, 'requestRewriting').includes(
        'Only Anthropic channels use this query parameter. It is still set.'
      )
    )
  })
})

describe('cards', () => {
  test('the five cards come in order, each under its own id', async () => {
    const html = await renderSections(storedChannel(1))
    const cards: [keyof typeof SECTION_IDS, string, string][] = [
      [
        'connection',
        'Connection &amp; keepalive',
        'How the gateway reaches this upstream: proxy, transport and idle keepalive.',
      ],
      [
        'clientIdentity',
        'Client identity',
        'Which client the upstream sees when the gateway calls it.',
      ],
      [
        'requestRewriting',
        'Request &amp; response rewriting',
        'What the gateway changes before forwarding a request and before returning the answer.',
      ],
      [
        'sessionCompat',
        'Session compatibility',
        'For long or resumed sessions that the upstream rejects or hangs on. Leave these off unless you see that problem.',
      ],
      [
        'upstreamModelDetection',
        'Upstream Model Detection Settings',
        'Check the upstream&#x27;s model list on a schedule and optionally sync it.',
      ],
    ]
    const positions = cards.map(([card]) =>
      html.indexOf(`id="${SECTION_IDS[card]}"`)
    )
    assert.ok(
      positions.every(
        (position, index) => index === 0 || position > positions[index - 1]
      ),
      'cards are in display order'
    )
    for (const [card, title, description] of cards) {
      const markup = cardMarkup(html, card)
      assert.ok(markup.includes(`${title}</h3>`), title)
      assert.ok(markup.includes(`${description}</p>`), description)
    }

    const connection = cardMarkup(html, 'connection')
    for (const label of [
      'Proxy Address',
      'WebSocket Transport',
      'Skip async task polling delay',
      'Upstream keepalive</h4>',
      'Enable upstream keepalive',
      'Last keepalive time',
    ]) {
      assert.ok(connection.includes(label), label)
    }
    assert.ok(cardMarkup(html, 'clientIdentity').includes('Client type'))
    const rewriting = cardMarkup(html, 'requestRewriting')
    for (const label of [
      'Request</h4>',
      'Pass Through Body',
      'Request fields forwarded upstream',
      'Response</h4>',
      'Force Format',
      'Thinking to Content',
    ]) {
      assert.ok(rewriting.includes(label), label)
    }
  })

  test('model detection is a card of its own only where models can be fetched', async () => {
    const aws = await renderSections(storedChannel(33))
    assert.ok(!aws.includes(SECTION_IDS.upstreamModelDetection))

    const stillSet = await renderSections(
      storedChannel(33, {}, { upstream_model_update_check_enabled: true })
    )
    const card = cardMarkup(stillSet, 'upstreamModelDetection')
    assert.ok(card.includes(INACTIVE_BADGE))
    assert.equal(switchIsOn(card, 'Upstream Model Update Check'), true)
  })

  test('a card with something set is outlined', async () => {
    const html = await renderSections(
      storedChannel(1, { proxy: 'socks5://127.0.0.1:1080' })
    )
    const outlined = (card: keyof typeof SECTION_IDS) =>
      html.includes(`id="${SECTION_IDS[card]}" class="`) &&
      cardMarkup(html, card).split('"')[3].includes('ring-1')
    assert.equal(outlined('connection'), true)
    assert.equal(outlined('requestRewriting'), false)
  })

  test('the Response heading goes when no response option is shown', async () => {
    const form = transformChannelToFormDefaults(storedChannel(14))
    const settings = getChannelSettingsOverview(14, form, form).settings
    const render = (hideResponseOptions: boolean) =>
      renderInForm(
        form,
        <ChannelRequestRewritingSection
          id='card-request-rewriting'
          configured={false}
          sensitiveLocked={false}
          settings={
            hideResponseOptions
              ? {
                  ...settings,
                  force_format: { state: 'hidden', isSet: false },
                  thinking_to_content: { state: 'hidden', isSet: false },
                }
              : settings
          }
        />
      )
    assert.ok((await render(false)).includes('>Response</h4>'))
    const html = await render(true)
    assert.ok(!html.includes('>Response</h4>'))
    assert.ok(html.includes('>Request</h4>'))
  })

  test('without the sensitive-write permission the cards are locked under one notice', async () => {
    const html = await renderSections(storedChannel(1), {
      sensitiveLocked: true,
    })
    assert.equal(count(html, 'No permission to perform this action'), 1)
    assert.ok(
      html.indexOf('No permission to perform this action') <
        html.indexOf(`id="${SECTION_IDS.connection}"`)
    )
    assert.equal(count(html, '<fieldset disabled=""'), 5)

    const unlocked = await renderSections(storedChannel(1))
    assert.ok(!unlocked.includes('No permission to perform this action'))
    assert.equal(count(unlocked, '<fieldset disabled=""'), 0)
  })
})

describe('an option switched off on a type that ignores it', () => {
  test('keeps its row, without claiming it is still set', async () => {
    const form = transformChannelToFormDefaults(storedChannel(14))
    const render = (isSet: boolean) =>
      renderInForm(
        form,
        <ChannelSettingRow
          setting={{ state: 'inactive', isSet }}
          reason='Only acts on upstreams that speak the OpenAI Responses protocol.'
        >
          <span>the control</span>
        </ChannelSettingRow>
      )
    const set = await render(true)
    assert.ok(set.includes('It is still set. You can turn it off.'))

    const switchedOff = await render(false)
    assert.ok(switchedOff.includes(INACTIVE_BADGE))
    assert.ok(
      switchedOff.includes(
        'Only acts on upstreams that speak the OpenAI Responses protocol.'
      )
    )
    assert.ok(switchedOff.includes('the control'))
    assert.ok(!switchedOff.includes('It is still set'))
  })
})

describe('saving', () => {
  test('a channel carrying options its type does not use is saved back unchanged', async () => {
    // What the backend stores for an Anthropic channel that still has two
    // OpenAI-only options on, plus a key this form knows nothing about.
    const setting = {
      force_format: false,
      thinking_to_content: false,
      proxy: '',
      pass_through_body_enabled: false,
      responses_reasoning_fallback: true,
      claude_tool_result_images: '',
      websocket_transport: true,
      synthetic_client_headers: false,
      synthetic_client_headers_profile: '',
      cli_version_profile: '',
      key_environments: false,
      key_environment_cache_ttl: '',
      tls_fingerprint: '',
      client_device_seed: '',
      client_session_seed: '',
      system_prompt: '',
      system_prompt_override: false,
      future_option: 'kept',
    }
    const settings = {
      allow_service_tier: false,
      allow_inference_geo: false,
      allow_speed: false,
      claude_beta_query: false,
      disable_task_polling_sleep: false,
      upstream_model_update_check_enabled: false,
      upstream_model_update_auto_sync_enabled: false,
      upstream_model_update_ignored_models: [],
      upstream_model_update_last_detected_models: [],
      upstream_model_update_last_check_time: 0,
      upstream_keepalive_enabled: false,
      upstream_keepalive_interval_minutes: 30,
      upstream_keepalive_active_window_minutes: 120,
    }
    const channel = storedChannel(14, setting, settings)

    const html = await renderSections(channel)
    assert.equal(count(html, INACTIVE_BADGE), 2)

    const payload = transformFormDataToUpdatePayload(
      transformChannelToFormDefaults(channel),
      channel.id
    )
    assert.deepEqual(JSON.parse(payload.setting ?? ''), setting)
    assert.deepEqual(JSON.parse(payload.settings ?? ''), settings)
  })
})

describe('validation errors', () => {
  // The drawer opens the advanced area when hasAdvancedSettingsErrors is true,
  // and the form then scrolls to the message, so the message has to be inside
  // the card that holds the field.
  test('a keepalive interval out of range is reported inside the connection card', async () => {
    const channel = storedChannel(
      1,
      {},
      {
        upstream_keepalive_enabled: true,
        upstream_keepalive_interval_minutes: 2,
      }
    )
    const parsed = channelFormSchema.safeParse(
      transformChannelToFormDefaults(channel)
    )
    assert.deepEqual(
      parsed.error?.issues.map((issue) => issue.path[0]),
      ['upstream_keepalive_interval_minutes']
    )
    const message = parsed.error?.issues[0].message ?? ''
    const errors = {
      upstream_keepalive_interval_minutes: { type: 'custom', message },
    }
    assert.equal(hasAdvancedSettingsErrors(errors), true)

    const html = await renderSections(channel, {}, errors)
    assert.equal(count(html, message), 1)
    assert.ok(cardMarkup(html, 'connection').includes(message))
  })

  test('a pinned CLI version with WebSocket on is reported inside the client identity card', async () => {
    const channel = storedChannel(1, {
      websocket_transport: true,
      synthetic_client_headers_profile: 'codex',
      cli_version_profile: 'codex-test',
    })
    const parsed = channelFormSchema.safeParse(
      transformChannelToFormDefaults(channel)
    )
    assert.deepEqual(
      parsed.error?.issues.map((issue) => issue.path[0]),
      ['cli_version_profile']
    )
    const message = parsed.error?.issues[0].message ?? ''
    const errors = { cli_version_profile: { type: 'custom', message } }
    assert.equal(hasAdvancedSettingsErrors(errors), true)

    const html = await renderSections(channel, {}, errors)
    assert.ok(cardMarkup(html, 'clientIdentity').includes(message))
  })
})
