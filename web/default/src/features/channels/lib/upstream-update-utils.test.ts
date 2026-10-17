import assert from 'node:assert/strict'
import { test } from 'node:test'

import { parseUpstreamUpdateMeta } from './upstream-update-utils'

test('legacy removal queues never become selectable model updates', () => {
  const settings = {
    upstream_model_update_check_enabled: true,
    upstream_model_update_last_detected_models: [' gpt-4o ', 'gpt-4o'],
    upstream_model_update_last_removed_models: ['gpt-4o'],
  }
  for (const input of [settings, JSON.stringify(settings)]) {
    assert.deepEqual(parseUpstreamUpdateMeta(input), {
      enabled: true,
      pendingAddModels: ['gpt-4o'],
    })
  }
})
