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
import { test } from 'node:test'

import { createInstance } from 'i18next'
import { renderToStaticMarkup } from 'react-dom/server'
import { I18nextProvider } from 'react-i18next'

import zh from '../../../i18n/locales/zh.json'
import { TimingMetricsCell, StreamTpsCell } from './timing-metrics-cell'

test('missing stream timing says not recorded without inventing a zero first token', async () => {
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: { en: { translation: {} } } })
  for (const frtMs of [undefined, -1000, 0, 2500]) {
    const html = renderToStaticMarkup(
      <I18nextProvider i18n={i18n}>
        <TimingMetricsCell
          isStream
          useTimeSec={10}
          completionTokens={0}
          frtMs={frtMs}
        />
      </I18nextProvider>
    )
    assert.ok(html.includes(frtMs === 2500 ? '2.5s' : 'Not recorded'))
    assert.ok(!html.includes('N/A'))
    assert.ok(html.includes('First upstream event'))
    assert.ok(
      html.includes('not client retries or the whole conversation turn')
    )
  }
  const nonStream = renderToStaticMarkup(
    <I18nextProvider i18n={i18n}>
      <TimingMetricsCell
        isStream={false}
        useTimeSec={10}
        completionTokens={0}
      />
    </I18nextProvider>
  )
  assert.ok(!nonStream.includes('First upstream event'))
})

test('thinking-only output is visible as a warning, not a transport error or a complete answer', async () => {
  const i18n = createInstance()
  await i18n.init({ lng: 'zh', resources: { zh } })
  const html = renderToStaticMarkup(
    <I18nextProvider i18n={i18n}>
      <StreamTpsCell
        isStream
        streamStatus={{
          status: 'warning',
          thinking_only: true,
          stop_reason: 'end_turn',
        }}
      />
    </I18nextProvider>
  )
  assert.ok(html.includes('仅有思考'))
  assert.ok(html.includes('未返回回答正文或工具调用'))
  assert.ok(!html.includes('text-destructive'))
  const normal = renderToStaticMarkup(
    <I18nextProvider i18n={i18n}>
      <StreamTpsCell
        isStream
        streamStatus={{ status: 'ok', stop_reason: 'end_turn' }}
      />
    </I18nextProvider>
  )
  assert.ok(!normal.includes('仅有思考'))
  const truncated = renderToStaticMarkup(
    <I18nextProvider i18n={i18n}>
      <StreamTpsCell
        isStream
        streamStatus={{
          status: 'error',
          thinking_only: true,
          end_reason: 'scanner_error',
          missing_terminator: true,
        }}
      />
    </I18nextProvider>
  )
  assert.ok(truncated.includes('text-destructive'))
  assert.ok(truncated.includes('仅有思考'))
})
