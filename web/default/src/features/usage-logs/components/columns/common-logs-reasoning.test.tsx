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

import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { createInstance } from 'i18next'
import { renderToStaticMarkup } from 'react-dom/server'
import { I18nextProvider } from 'react-i18next'

import zh from '../../../../i18n/locales/zh.json'
import zhTW from '../../../../i18n/locales/zh-TW.json'
import { usageLogSchema, type UsageLog } from '../../data/schema'
import { useCommonLogsColumns } from './common-logs-columns'

function ReasoningTable({ log }: { log: UsageLog }) {
  const columns = useCommonLogsColumns(false).filter(
    (column) =>
      column.id === 'reasoning_effort' ||
      column.id === 'upstream_reasoning_effort'
  )
  const table = useReactTable({
    data: [log],
    columns,
    getCoreRowModel: getCoreRowModel(),
  })
  return (
    <table>
      <thead>
        {table.getHeaderGroups().map((group) => (
          <tr key={group.id}>
            {group.headers.map((header) => (
              <th key={header.id}>
                {flexRender(
                  header.column.columnDef.header,
                  header.getContext()
                )}
              </th>
            ))}
          </tr>
        ))}
      </thead>
      <tbody>
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id}>
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id}>
                {flexRender(cell.column.columnDef.cell, cell.getContext())}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

test('usage logs show request and reported effort independently, including absent and none', async () => {
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: { en: { translation: {} } } })
  for (const [other, request, upstream] of [
    [
      { reasoning_effort: 'xhigh', upstream_reasoning_effort: 'low' },
      'xhigh',
      'low',
    ],
    [{ reasoning_effort: 'xhigh' }, 'xhigh', 'Not reported'],
    [
      { reasoning_effort: 'none', upstream_reasoning_effort: 'none' },
      'none',
      'none',
    ],
    [{}, 'Original settings not recorded', 'Not reported'],
  ] as const) {
    const log = usageLogSchema.parse({
      id: 1,
      user_id: 1,
      created_at: 1,
      type: 2,
      content: '',
      other: JSON.stringify(other),
    })
    const html = renderToStaticMarkup(
      <I18nextProvider i18n={i18n}>
        <ReasoningTable log={log} />
      </I18nextProvider>
    )
    assert.ok(html.includes('<th>Reasoning Effort</th>'))
    assert.ok(html.includes('<th>Upstream Reasoning Effort</th>'))
    const cells = [...html.matchAll(/<td>(.*?)<\/td>/g)].map((match) =>
      match[1].replaceAll(/<[^>]+>/g, '')
    )
    assert.deepEqual(cells, [request, upstream])
  }
})

test('Chinese usage logs translate headers and absent upstream effort using shipped locale bundles', async () => {
  for (const [lng, bundle, requestLabel, upstreamLabel, missing] of [
    ['zhCN', zh, '推理强度', '上游返回强度', '上游未返回'],
    ['zhTW', zhTW, '推理強度', '上游回傳強度', '上游未回傳'],
  ] as const) {
    const i18n = createInstance()
    await i18n.init({ lng, resources: { [lng]: bundle } })
    const log = usageLogSchema.parse({
      id: 1,
      user_id: 1,
      created_at: 1,
      type: 5,
      content: '',
      other: JSON.stringify({ reasoning_effort: 'xhigh' }),
    })
    const html = renderToStaticMarkup(
      <I18nextProvider i18n={i18n}>
        <ReasoningTable log={log} />
      </I18nextProvider>
    )
    assert.ok(html.includes(`<th>${requestLabel}</th>`))
    assert.ok(html.includes(`<th>${upstreamLabel}</th>`))
    assert.ok(html.includes(missing))
    assert.ok(!html.includes('Upstream Reasoning Effort'))
    assert.ok(!html.includes('Not reported'))
  }
})

test('request settings distinguish missing effort, adaptive mode and zero budget without fabricating upstream effort', async () => {
  const i18n = createInstance()
  await i18n.init({ lng: 'zh', resources: { zh } })
  for (const settings of [{}, { mode: 'adaptive' }, { budget: 0 }, { budget: -1 }, { effort: 'max' }]) {
    const log = usageLogSchema.parse({
      id: 1, user_id: 1, created_at: 1, type: 2, content: '',
      other: JSON.stringify({ request_reasoning: settings }),
    })
    const html = renderToStaticMarkup(
      <I18nextProvider i18n={i18n}><ReasoningTable log={log} /></I18nextProvider>
    )
    assert.ok(html.includes('上游未返回'))
    assert.ok(!html.includes('未记录'))
    if ('effort' in settings) assert.ok(html.includes('max'))
    else assert.ok(html.includes('未指定强度'))
    if ('mode' in settings) assert.ok(html.includes('自适应思考'))
    if ('budget' in settings) assert.ok(html.replaceAll(/<[^>]+>/g, '').includes(`思考预算: ${settings.budget}`))
  }
})
