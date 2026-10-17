import assert from 'node:assert/strict'
import { test } from 'node:test'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createInstance } from 'i18next'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { I18nextProvider } from 'react-i18next'

import { GlobalSettingsCard } from './global-settings-card'
import {
  formatModelUpdateExcludedChannelIDs,
  parseModelUpdateExcludedChannelIDs,
} from './model-update-exclusions'

test('channel exclusions accept pasted IDs and preserve exact positive integers', () => {
  assert.deepEqual(parseModelUpdateExcludedChannelIDs(''), [])
  assert.deepEqual(
    parseModelUpdateExcludedChannelIDs(' 1, 5，9\n5\t12 '),
    [1, 5, 9, 12]
  )
  for (const input of ['0', '-1', '1.5', '1e2', '5x', '9007199254740992']) {
    assert.equal(parseModelUpdateExcludedChannelIDs(input), null, input)
  }
})

test('saved exclusion arrays reopen as editable IDs and can be cleared', () => {
  assert.equal(formatModelUpdateExcludedChannelIDs('[1,5,9]'), '1, 5, 9')
  for (const input of ['', 'null', '[]']) {
    assert.equal(formatModelUpdateExcludedChannelIDs(input), '')
  }
  assert.equal(
    JSON.stringify(
      parseModelUpdateExcludedChannelIDs(
        formatModelUpdateExcludedChannelIDs('[1,5,9]')
      )
    ),
    '[1,5,9]'
  )
})

test('global settings expose exclusions below the scan-all switch', async () => {
  const client = new QueryClient()
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: { en: { translation: {} } } })
  const card = createElement(GlobalSettingsCard, {
    defaultValues: {
      global: {
        pass_through_request_enabled: false,
        thinking_model_blacklist: '[]',
        chat_completions_to_responses_policy: '{}',
      },
      general_setting: {
        ping_interval_enabled: false,
        ping_interval_seconds: 60,
      },
      monitor_setting: {
        upstream_model_update_enabled: false,
        upstream_model_update_interval_hours: 24,
        upstream_model_update_scan_all_channels: true,
        upstream_model_update_excluded_channel_ids: '1, 5, 9',
        upstream_model_update_validate: true,
        upstream_model_update_strip_vendor_prefix: true,
        upstream_model_update_remove_failed: true,
        upstream_model_update_remove_unavailable_models: false,
        upstream_model_update_retry_delay_minutes: 60,
        upstream_model_update_failure_threshold: 2,
        upstream_model_update_rotation_sample_size: 5,
        upstream_model_update_max_validations_per_run: 200,
        channel_test_prompts: '[]',
        channel_test_client_headers: '{}',
      },
    },
  })
  const html = renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(QueryClientProvider, { client }, card)
    )
  )
  client.clear()
  assert.ok(html.includes('Excluded Channel IDs'))
  assert.ok(
    html.indexOf('Scan All Enabled Channels') <
      html.indexOf('Excluded Channel IDs')
  )
  assert.match(
    html,
    /<textarea[^>]*name="monitor_setting\.upstream_model_update_excluded_channel_ids"[^>]*>1, 5, 9<\/textarea>/
  )
})
