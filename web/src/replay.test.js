// replay.test.js —— replay.js 的纯函数测试（node --test，不引入依赖）
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import * as R from './replay.js'

const fixture = (name) => JSON.parse(readFileSync(new URL('./testdata/' + name, import.meta.url), 'utf8'))

test('pairTools/groupSubagents：给定固定事件 断言与快照逐字段一致', () => {
  const evs = fixture('pair-group-events.json')
  const got = R.pairTools(R.groupSubagents(evs)).map((x) => (x.subagent ? { ...x, list: R.pairTools(x.list) } : x))
  assert.deepEqual(JSON.parse(JSON.stringify(got)), fixture('pair-group-snapshot.json'))
})
