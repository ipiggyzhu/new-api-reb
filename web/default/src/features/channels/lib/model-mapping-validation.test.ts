import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  buildChannelTestModelEntries,
  filterChannelTestModelEntries,
  parseModelsString,
  resolveMappedUpstreamModel,
} from './model-mapping-validation'

// The production channel: five free aliases mapped to their upstream names.
// Four upstream names were adopted into Models before their alias existed;
// deepseek-v4.1-flash never was, because a mapped target is never proposed.
const mapping = JSON.stringify({
  'deepseek-v4-flash-free': 'deepseek-v4-flash',
  'deepseek-v4-pro-free': 'deepseek-v4-pro',
  'glm-5.2-free': 'glm-5.2',
  'step-3.7-flash-free': 'step-3.7-flash',
  'deepseek-v4.1-flash-free': 'deepseek-v4.1-flash',
})
const sources = [
  'deepseek-v4-flash-free',
  'deepseek-v4-pro-free',
  'glm-5.2-free',
  'step-3.7-flash-free',
  'deepseek-v4.1-flash-free',
]
const targets = [
  'deepseek-v4-flash',
  'deepseek-v4-pro',
  'glm-5.2',
  'step-3.7-flash',
  'deepseek-v4.1-flash',
]
const modelsField = [...sources, ...targets.slice(0, 4), 'gpt-4o'].join(',')

test('every mapped upstream model is reachable from the channel test list', () => {
  // Before: the dialog listed the Models field alone.
  const before = parseModelsString(modelsField)
  assert.equal(before.includes('deepseek-v4.1-flash'), false)
  for (const target of targets.slice(0, 4)) assert.ok(before.includes(target))

  const entries = buildChannelTestModelEntries(
    parseModelsString(modelsField),
    mapping
  )
  const names = new Set(entries.map((entry) => entry.model))
  const upstreams = new Set(entries.map((entry) => entry.upstreamModel))
  for (const source of sources) assert.ok(names.has(source), source)
  for (const target of targets) assert.ok(upstreams.has(target), target)
  assert.deepEqual(
    entries.find((entry) => entry.model === 'deepseek-v4.1-flash-free'),
    {
      model: 'deepseek-v4.1-flash-free',
      upstreamModel: 'deepseek-v4.1-flash',
      listed: true,
    }
  )
  // Listed upstream names are tested as themselves, unannotated.
  assert.equal(
    entries.find((entry) => entry.model === 'deepseek-v4-flash')?.upstreamModel,
    undefined
  )
  assert.equal(entries.length, 10)
})

test('searching an upstream model name finds the alias that exercises it', () => {
  const entries = buildChannelTestModelEntries(
    parseModelsString(modelsField),
    mapping
  )
  assert.deepEqual(
    filterChannelTestModelEntries(entries, ' DeepSeek-V4.1-Flash ').map(
      (entry) => entry.model
    ),
    ['deepseek-v4.1-flash-free']
  )
  // An alias whose name shares nothing with its target is still found.
  const azure = buildChannelTestModelEntries(
    ['gpt-4'],
    '{"gpt-4":"azure-prod-deploy"}'
  )
  assert.deepEqual(
    filterChannelTestModelEntries(azure, 'azure-prod').map((e) => e.model),
    ['gpt-4']
  )
})

test('a mapping alias missing from Models is listed for testing but flagged', () => {
  const entries = buildChannelTestModelEntries(['gpt-4o'], mapping)
  assert.deepEqual(
    entries.map((entry) => [entry.model, entry.upstreamModel, entry.listed]),
    [
      ['gpt-4o', undefined, true],
      ...sources.map((source, index) => [source, targets[index], false]),
    ]
  )
})

test('upstream resolution follows the relay mapping chain', () => {
  const chain = { a: 'b', b: 'c' }
  assert.equal(resolveMappedUpstreamModel('a', chain), 'c')
  assert.equal(resolveMappedUpstreamModel('c', chain), undefined)
  // Self-mapping is a no-op, a real cycle is rejected by the relay.
  assert.equal(resolveMappedUpstreamModel('x', { x: 'x' }), undefined)
  assert.equal(resolveMappedUpstreamModel('a', { a: 'b', b: 'b' }), 'b')
  assert.equal(resolveMappedUpstreamModel('a', { a: 'b', b: 'a' }), undefined)
  // Unparsable or empty mappings leave the list unannotated.
  for (const raw of ['', null, '{bad', '[]']) {
    assert.deepEqual(buildChannelTestModelEntries(['m'], raw), [
      { model: 'm', upstreamModel: undefined, listed: true },
    ])
  }
})
