// replay.test.js —— replay.js 的纯函数与事件窗口测试（node --test，不引入依赖）。
//
// 运行：node --test web/src/replay.test.js（make ui-test）。
// 一律命名空间导入：导出缺失时只有用到它的用例报 `R.x is not a function`，
// 其余用例照常跑（「先红」在提交 1 的旧实现上要求这种失败形态）。
import { test, describe } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import * as R from './replay.js'

const fixture = (name) => JSON.parse(readFileSync(new URL('./testdata/' + name, import.meta.url), 'utf8'))

// 与 TaskDetail 同一组合：先收 subagent 子块，再对主流与每个子块分别配对
const prepare = (evs) =>
  R.pairTools(R.groupSubagents(evs)).map((x) => (x.subagent ? { ...x, list: R.pairTools(x.list) } : x))

// ---------------------------------------------------------------- 失败判定

describe('isFailure/severityOf：与 Go 端同一张表（testdata/failure-cases.json）', () => {
  for (const c of fixture('failure-cases.json')) {
    test(c.name, () => {
      const ev = { id: 1, kind: c.kind, payload: c.payload }
      assert.equal(R.severityOf(ev), c.severity, `severityOf got ${R.severityOf(ev)}, want ${c.severity}`)
      assert.equal(R.isFailure(ev), c.severity !== null)
    })
  }
  test('给定配对后的 tool_use 断言以 result 的判定为准', () => {
    const use = { id: 1, kind: 'tool_use', payload: { toolUseId: 'a' } }
    assert.equal(R.severityOf({ ...use, result: { id: 2, kind: 'tool_result', payload: { isError: true } } }), 'error')
    assert.equal(R.severityOf({ ...use, result: { id: 2, kind: 'tool_result', payload: {} } }), null)
  })
})

// ---------------------------------------------------------------- 分组与配对

test('pairTools/groupSubagents：给定固定事件 断言与快照逐字段一致', () => {
  const got = prepare(fixture('pair-group-events.json'))
  assert.deepEqual(JSON.parse(JSON.stringify(got)), fixture('pair-group-snapshot.json'))
})

// ---------------------------------------------------------------- 时间

describe('withTiming', () => {
  const t0 = Date.parse('2026-11-01T10:00:00Z')
  const at = (ms) => new Date(t0 + ms).toISOString()
  const list = [
    { id: 1, at: at(0) },
    { id: 2, at: at(500) },
    { id: 3, at: at(5500) },
  ]
  const cases = [
    { name: '给定相对任务起点 断言 offsetMs 正确', origin: at(0), want: [0, 500, 5500] },
    { name: '给定相对阶段起点（晚 1s）断言 offsetMs 以它为零点', origin: at(1000), want: [-1000, -500, 4500] },
  ]
  for (const c of cases) {
    test(c.name, () => {
      assert.deepEqual(R.withTiming(list, c.origin).map((x) => x.offsetMs), c.want)
    })
  }
  test('给定间隔 500ms 与 5s 断言 gapMs 低于 1000 置 null、末条为 null', () => {
    assert.deepEqual(R.withTiming(list, at(0)).map((x) => x.gapMs), [null, 5000, null])
  })
  test('给定子块 断言用其第一条的 at', () => {
    const got = R.withTiming([{ id: 1, at: at(0) }, { subagent: true, id: 'sub-a', list: [{ id: 2, at: at(2000) }] }], at(0))
    assert.deepEqual(got.map((x) => x.offsetMs), [0, 2000])
  })
})

describe('formatOffset', () => {
  const cases = [
    [0, '+00:00'], [999, '+00:00'], [42000, '+00:42'], [62000, '+01:02'], [3599999, '+59:59'],
    [3600000, '+1:00:00'], [3727000, '+1:02:07'], [36000000, '+10:00:00'], [null, ''], [-1, ''], [NaN, ''],
  ]
  for (const [ms, want] of cases) {
    test(`给定 ${ms} 断言 ${JSON.stringify(want)}`, () => assert.equal(R.formatOffset(ms), want))
  }
})

describe('silenceLabel（≥30s 静默分隔线）', () => {
  const cases = [
    [null, null], [29999, null], [30000, '⋯ 静默 30s'], [252000, '⋯ 静默 4m12s'], [3900000, '⋯ 静默 1h05m'],
  ]
  for (const [gap, want] of cases) {
    test(`给定 gapMs=${gap} 断言 ${JSON.stringify(want)}`, () => assert.equal(R.silenceLabel(gap), want))
  }
  test('给定 at 为 t, t+29.9s, t+60s 断言只有第 2 条（与第 3 条间隔 30.1s）插分隔线', () => {
    const t0 = Date.parse('2026-11-01T10:00:00Z')
    const list = [0, 29900, 60000].map((d, i) => ({ id: i + 1, at: new Date(t0 + d).toISOString() }))
    const got = R.withTiming(list, list[0].at).map((x) => R.silenceLabel(x.gapMs) !== null)
    assert.deepEqual(got, [false, true, false])
  })
})

// ---------------------------------------------------------------- 阶段

describe('phaseMeta/comparePhases', () => {
  const want = ['triage', 'implement', 'implement-reentry', 'fix-1', 'fix-2', 'fix-10', 'verify', 'review', 'push', 'bogus']
  test('给定打乱的阶段 断言排序恰为 triage…push、fix-N 按数值、未知在最后', () => {
    const shuffled = ['fix-10', 'push', 'bogus', 'verify', 'implement-reentry', 'fix-2', 'triage', 'review', 'fix-1', 'implement']
    assert.deepEqual([...shuffled].sort((a, b) => R.comparePhases(a, b)), want)
  })
  const metas = [
    ['implement-reentry', { label: '实现（新会话重入）', order: 1.5, sub: 0 }],
    ['fix-12', { label: '修复第 12 轮', order: 1.75, sub: 12 }],
    ['bogus', { label: 'bogus', order: 9, sub: 0 }],
    ['triage', { label: '分诊', order: 0, sub: 0 }],
  ]
  for (const [phase, m] of metas) {
    test(`给定 ${phase} 断言 ${JSON.stringify(m)}`, () => assert.deepEqual(R.phaseMeta(phase), m))
  }
})

// ---------------------------------------------------------------- 过滤

describe('applyFilter', () => {
  const evs = [
    { id: 1, phase: 'implement', kind: 'text', body: 't', payload: {} },
    { id: 2, phase: 'implement', kind: 'thinking', body: 'th', payload: {} },
    { id: 3, phase: 'implement', kind: 'tool_use', body: 'ok', payload: { toolUseId: 'u1' } },
    { id: 4, phase: 'implement', kind: 'tool_result', body: 'ok', payload: { toolUseId: 'u1' } },
    { id: 5, phase: 'fix-1', kind: 'tool_use', body: 'bad', payload: { toolUseId: 'u2' } },
    { id: 6, phase: 'fix-1', kind: 'tool_result', body: 'bad', payload: { toolUseId: 'u2', isError: true } },
    { id: 7, phase: 'fix-1', kind: 'tool_result', body: 'orphan', payload: {} },
    { id: 8, phase: 'fix-1', kind: 'raw', body: 'overflow', payload: { type: 'overflow' } },
    // S1：1 条 isError 的 tool_use+result 与 2 条 text
    { id: 9, phase: 'implement', kind: 'agent_start', agentId: 's1', body: 'S1', payload: {} },
    { id: 10, phase: 'implement', kind: 'tool_use', agentId: 's1', body: 'g', payload: { toolUseId: 's1u' } },
    { id: 11, phase: 'implement', kind: 'tool_result', agentId: 's1', body: 'x', payload: { toolUseId: 's1u', isError: true } },
    { id: 12, phase: 'implement', kind: 'text', agentId: 's1', body: 'a', payload: {} },
    { id: 13, phase: 'implement', kind: 'text', agentId: 's1', body: 'b', payload: {} },
    // S2：只有 text 与 thinking
    { id: 14, phase: 'implement', kind: 'agent_start', agentId: 's2', body: 'S2', payload: {} },
    { id: 15, phase: 'implement', kind: 'text', agentId: 's2', body: 'c', payload: {} },
    { id: 16, phase: 'implement', kind: 'thinking', agentId: 's2', body: 'd', payload: {} },
  ]
  const input = prepare(evs)
  const ids = (list) => list.map((x) => x.id)

  test('给定全部关闭 断言长度相同且逐项 ===', () => {
    const got = R.applyFilter(input, {})
    assert.equal(got.length, input.length)
    got.forEach((x, i) => assert.ok(x === input[i], `第 ${i} 项不是同一对象`))
  })
  test('给定只看失败 断言主流只剩 isError 的 tool_use 与 overflow，S1 整块保留且不裁剪，S2 去掉', () => {
    const got = R.applyFilter(input, { onlyFailures: true })
    assert.deepEqual(ids(got), [5, 8, 'sub-s1'])
    assert.equal(got.find((x) => x.id === 'sub-s1').list.length, 3)
  })
  test('给定隐藏思考 断言顶层无 thinking，S2 因还有 text 而保留', () => {
    const got = R.applyFilter(input, { hideThinking: true })
    assert.ok(!got.some((x) => x.kind === 'thinking'))
    assert.deepEqual(ids(got), [1, 3, 5, 7, 8, 'sub-s1', 'sub-s2'])
  })
  test('给定只看工具调用 断言只剩两个 tool_use 与未配对的 tool_result，S1 保留、S2 去掉', () => {
    assert.deepEqual(ids(R.applyFilter(input, { onlyTools: true })), [3, 5, 7, 'sub-s1'])
  })
  test("给定 phases=['fix-1'] 断言只剩 fix-1 的项；phases=[] 等同不过滤", () => {
    assert.deepEqual(ids(R.applyFilter(input, { phases: ['fix-1'] })), [5, 7, 8])
    assert.equal(R.applyFilter(input, { phases: [] }).length, input.length)
  })
  test('给定只看失败 + 只看工具调用 断言取交集', () => {
    assert.deepEqual(ids(R.applyFilter(input, { onlyFailures: true, onlyTools: true })), [5, 'sub-s1'])
  })
})

// ---------------------------------------------------------------- 运行与轮次

// v 造一条验证行；logRef 为 [round, seq] 或 [round, [seq...]] 时按真实格式生成
let vid = 0
const T0 = Date.parse('2026-11-01T10:00:00Z')
const iso = (s) => new Date(T0 + s * 1000).toISOString()
function v(step, status, at, ref, tier = 'heavy') {
  let logRef = null
  if (ref) {
    const [round, seqs] = ref
    logRef = [].concat(seqs).map((n) => `verify-logs/task-42/round-${round}/${String(n).padStart(2, '0')}-${step}.log`).join(' ')
  }
  return { id: ++vid, tier, step, status, durationMs: 1, logRef, at: iso(at) }
}
let eid = 0
function ev(fromState, toState, at, actor = 'system', payload = {}) {
  return { id: ++eid, fromState, toState, actor, payload, at: iso(at) }
}
const shape = (runs) => runs.map((r) => r.rounds.map((x) => x.rows.map((row) => row.id)))
const stripRefs = (rows) => rows.map((r) => ({ ...r, logRef: null }))

describe('parseLogRef', () => {
  const cases = [
    ['verify-logs/task-1/round-2/03-build.log', { round: 2, seqMin: 3, seqMax: 3 }],
    ['verify-logs/task-1/round-0/09-repro_fail.log verify-logs/task-1/round-0/10-repro_fail.log', { round: 0, seqMin: 9, seqMax: 10 }],
    [null, null],
    ['', null],
    ['a.log b.log', null],
    ['verify-logs/task-1/round-0/01-build.log verify-logs/task-1/round-1/02-build.log', null],
  ]
  for (const [ref, want] of cases) {
    test(`给定 ${JSON.stringify(ref)} 断言 ${JSON.stringify(want)}`, () => assert.deepEqual(R.parseLogRef(ref), want))
  }
})

describe('segmentRuns', () => {
  // 一次运行的状态事件：建单 → implementing → verifying
  const firstRun = (at) => [ev(null, 'queued', at), ev('queued', 'implementing', at + 1), ev('implementing', 'verifying', at + 2)]

  test('1 给定单次运行、有 logRef 断言 round-0、round-1 各一组', () => {
    const vs = [v('build', 'failed', 10, [0, 1]), v('lint', 'skipped', 10, null), v('build', 'passed', 20, [1, 1]), v('lint', 'passed', 20, [1, 2])]
    const runs = R.segmentRuns(vs, firstRun(0))
    assert.deepEqual(shape(runs), [[[vs[0].id, vs[1].id], [vs[2].id, vs[3].id]]])
    assert.equal(runs[0].boundary, 'events')
  })

  test('2 给定单次运行、logRef 全空 断言靠规则 4b 切成 2 轮；heavy 末尾连续 regression 不切', () => {
    const vs = [v('build', 'passed', 10, null, 'light'), v('lint', 'failed', 10, null, 'light'), v('typecheck', 'skipped', 10, null, 'light'),
      v('build', 'passed', 20, null, 'light'), v('lint', 'passed', 20, null, 'light'), v('typecheck', 'passed', 20, null, 'light')]
    const runs = R.segmentRuns(vs, firstRun(0))
    assert.equal(runs.length, 1)
    assert.equal(runs[0].rounds.length, 2)
    const hv = [v('build', 'passed', 10, null), v('repro_fail', 'passed', 10, null), v('repro_pass', 'passed', 10, null),
      v('regression', 'passed', 10, null), v('regression', 'passed', 10, null), v('regression', 'passed', 10, null)]
    assert.equal(R.segmentRuns(hv, firstRun(0))[0].rounds.length, 1)
  })

  // 用例 3/4/6 共用：手动重试后两次运行的 round-0 同名
  const retry = () => {
    const evs = [...firstRun(0), ev('verifying', 'failed', 50), ev('failed', 'queued', 100, 'user:7', { reason: 'manual_retry', mode: 'auto' }),
      ev('queued', 'implementing', 101), ev('implementing', 'verifying', 102)]
    const run1 = [v('build', 'passed', 10, [0, 1]), v('lint', 'passed', 10, [0, 2]), v('repro_fail', 'passed', 10, [0, 3]), v('repro_pass', 'failed', 10, [0, 4])]
    const run2 = [v('build', 'passed', 110, [0, 1]), v('lint', 'passed', 110, [0, 2]), v('repro_fail', 'passed', 110, [0, 3]),
      v('repro_pass', 'passed', 110, [0, 4]), v('regression', 'passed', 110, [0, 5])]
    return { evs, run1, run2 }
  }

  test('3 给定手动重试 断言 2 次运行各 1 轮、证明条不混、run2 cause 为手动重试', () => {
    const { evs, run1, run2 } = retry()
    const runs = R.segmentRuns([...run1, ...run2], evs)
    assert.deepEqual(shape(runs), [[run1.map((x) => x.id)], [run2.map((x) => x.id)]])
    assert.equal(R.proofOf(runs[0].rounds[0], runs[0]).green, 'failed')
    assert.equal(R.proofOf(runs[1].rounds[0], runs[1]).verdict, 'proven')
    assert.equal(runs[1].cause, 'user:7 手动重试（auto）')
    assert.equal(runs[0].cause, '首次运行')
  })

  test('4 给定 task_events 缺失 断言靠 (round, NN) 回退仍切成两次运行', () => {
    const { run1, run2 } = retry()
    const runs = R.segmentRuns([...run1, ...run2], [])
    assert.deepEqual(shape(runs), [[run1.map((x) => x.id)], [run2.map((x) => x.id)]])
    assert.equal(runs[1].boundary, 'logRef')
  })

  test('5 给定重试后修复回路 断言 2 次运行共 3 轮且顺序正确', () => {
    const { evs } = retry()
    const run1 = [v('build', 'failed', 10, [0, 1]), v('lint', 'skipped', 10, null), v('build', 'passed', 20, [1, 1]), v('lint', 'passed', 20, [1, 2])]
    const run2 = [v('build', 'passed', 110, [0, 1]), v('lint', 'passed', 110, [0, 2])]
    const runs = R.segmentRuns([...run1, ...run2], evs)
    assert.deepEqual(shape(runs), [[[run1[0].id, run1[1].id], [run1[2].id, run1[3].id]], [[run2[0].id, run2[1].id]]])
  })

  test('6 给定 logRef 同名 断言 run1 的行 logOverwritten、run2 不是；多段只重叠一段也算', () => {
    const { evs, run1, run2 } = retry()
    const runs = R.segmentRuns([...run1, ...run2], evs)
    assert.deepEqual(runs[0].rounds[0].rows.map((r) => r.logOverwritten), [true, true, true, true])
    assert.ok(runs[0].rounds[0].rows.every((r) => r.overwrittenByRun === 2))
    assert.deepEqual(runs[1].rounds[0].rows.map((r) => r.logOverwritten), [false, false, false, false, false])

    const a = v('repro_fail', 'passed', 10, [0, [1, 2]])
    const b = v('repro_fail', 'passed', 110, [0, 1])
    const r2 = R.segmentRuns([a, b], evs)
    assert.equal(r2[0].rounds[0].rows[0].logOverwritten, true)
    assert.equal(r2[1].rounds[0].rows[0].logOverwritten, false)
  })

  const rebase = (withRefs) => {
    const evs = [...firstRun(0), ev('verifying', 'pr_open', 50),
      ev('pr_open', 'review_feedback', 100, 'system:rebase-followup', { reason: 'rebase_followup_reverify' }),
      ev('review_feedback', 'implementing', 101, 'system:rebase-followup', { reason: 'rebase_followup_reverify' }),
      ev('implementing', 'verifying', 102, 'system:rebase-followup')]
    const heavy = (at) => [v('build', 'passed', at, [0, 1]), v('lint', 'passed', at, [0, 2]), v('repro_fail', 'passed', at, [0, 3]),
      v('repro_pass', 'passed', at, [0, 4]), v('regression', 'passed', at, [0, 5])]
    let run1 = heavy(10)
    let run2 = heavy(110)
    if (!withRefs) {
      run1 = stripRefs(run1)
      run2 = stripRefs(run2)
    }
    return { evs, run1, run2 }
  }

  test('7 给定 rebase 跟进重验不经 queued 断言切成 2 次运行、无修复轮次、cause 为 rebase 重验', () => {
    const { evs, run1, run2 } = rebase(true)
    assert.ok(!evs.slice(1).some((e) => e.toState === 'queued'))
    const runs = R.segmentRuns([...run1, ...run2], evs)
    assert.equal(runs.length, 2)
    assert.deepEqual(runs.map((r) => r.rounds.length), [1, 1])
    assert.equal(runs[1].cause, '前驱合并后 rebase 重验')
  })

  test('8 给定 rebase 重验 + logRef 全空 断言只靠规则 2 也切成 2 次运行', () => {
    const { evs, run1, run2 } = rebase(false)
    const runs = R.segmentRuns([...run1, ...run2], evs)
    assert.deepEqual(shape(runs), [[run1.map((x) => x.id)], [run2.map((x) => x.id)]])
  })

  test('9 给定 approve 后的空运行 断言只返回 1 次运行', () => {
    const evs = [...firstRun(0), ev('verifying', 'awaiting_approval', 50),
      ev('awaiting_approval', 'queued', 100, 'user:7', { reason: 'manual_approve', mode: 'approved' }),
      ev('queued', 'verifying', 101), ev('verifying', 'pr_open', 102)]
    const vs = [v('build', 'passed', 10, [0, 1]), v('lint', 'passed', 10, [0, 2])]
    const runs = R.segmentRuns(vs, evs)
    assert.equal(runs.length, 1)
    assert.equal(runs[0].cause, '首次运行')
  })

  test('10 给定任务停在 verifying 时的 rebase 重验 断言 2 次运行、cause 为 rebase 重验', () => {
    const evs = [...firstRun(0), ev('verifying', 'queued', 100, 'system:rebase-followup', { reason: 'rebase_followup_reverify' }),
      ev('queued', 'verifying', 101)]
    const vs = [v('build', 'passed', 10, null), v('lint', 'passed', 10, null), v('build', 'passed', 110, null), v('lint', 'passed', 110, null)]
    const runs = R.segmentRuns(vs, evs)
    assert.equal(runs.length, 2)
    assert.equal(runs[1].cause, '前驱合并后 rebase 重验')
  })

  test('11 给定区间里夹着 repro_not_red 断言 run2 cause 是手动重试', () => {
    const evs = [...firstRun(0), ev('verifying', 'blocked_spec', 50, 'system', { reason: 'repro_not_red' }),
      ev('blocked_spec', 'queued', 100, 'user:9', { reason: 'manual_retry', mode: 'fresh' }),
      ev('queued', 'triaging', 101), ev('triaging', 'implementing', 102), ev('implementing', 'verifying', 103)]
    const vs = [v('build', 'passed', 10, null), v('build', 'passed', 110, null)]
    assert.equal(R.segmentRuns(vs, evs)[1].cause, 'user:9 手动重试（fresh）')
  })

  test('cause 映射：给定各已知 reason 断言对应文案，未知 reason 走兜底', () => {
    const cases = [
      [{ reason: 'manual_review_retry' }, 'user:3 重来一轮 review'],
      [{ reason: 'pr_review' }, 'PR review 意见回流后重跑'],
      [{ reason: 'restart_reconcile', interrupted_state: 'verifying' }, '服务重启后恢复（中断于 verifying）'],
      [{ unblocked_by: 77 }, '依赖任务 #77 恢复后重跑'],
      [{ reason: 'something_new' }, '重新进入验证（user:3）'],
    ]
    for (const [payload, want] of cases) {
      const evs = [...firstRun(0), ev('verifying', 'failed', 50), ev('failed', 'queued', 100, 'user:3', payload), ev('queued', 'verifying', 101, 'user:3')]
      assert.equal(R.runCauseOf(evs, evs.length - 1), want, `payload=${JSON.stringify(payload)}`)
    }
  })

  // 12–16：真实 DetectLightProfile 会产出的序列，三个变体
  // (α) 事件齐全 + logRef 齐全；(β) stateEvents=[] + logRef 齐全；(γ) 事件齐全 + logRef 全 null
  const variants = (build) => {
    const { evs, vs } = build()
    return [
      ['α', vs, evs],
      ['β', vs, []],
      ['γ', stripRefs(vs), evs],
    ]
  }
  const seqRows = (steps, at, round = 0, tier = 'light', statuses = {}) => {
    let n = 0
    return steps.map((s, i) => {
      const st = statuses[i] || 'passed'
      const seqs = Array.isArray(s) ? s[1] : null
      const step = Array.isArray(s) ? s[0] : s
      if (seqs) n += seqs.length
      else n++
      return v(step, st, at, st === 'skipped' ? null : [round, seqs ? seqs.map((_, k) => n - seqs.length + 1 + k) : n], tier)
    })
  }

  for (const [name, vs, evs] of variants(() => ({ evs: firstRun(0), vs: seqRows(['build', 'build', 'lint', 'typecheck'], 10) }))) {
    test(`12${name} 给定 pnpm install 与 build 脚本同名 断言 1 次运行 1 轮`, () => {
      const runs = R.segmentRuns(vs, evs)
      assert.deepEqual(runs.map((r) => r.rounds.length), [1])
      assert.equal(runs[0].boundary, { α: 'events', β: 'logRef', γ: 'events' }[name])
    })
  }

  for (const [name, vs, evs] of variants(() => ({ evs: firstRun(0), vs: seqRows(['build', 'lint', 'build', 'lint'], 10) }))) {
    test(`13${name} 给定两个 Go 模块 断言 1 次运行 1 轮`, () => {
      assert.deepEqual(R.segmentRuns(vs, evs).map((r) => r.rounds.length), [1])
    })
  }

  const heavyFull = () => seqRows(['build', 'lint', 'build', 'lint', 'build', 'build', 'lint', 'typecheck',
    ['repro_fail', [0, 1]], 'repro_pass', 'regression', 'regression'], 10, 0, 'heavy')
  for (const [name, vs, evs] of variants(() => ({ evs: firstRun(0), vs: heavyFull() }))) {
    test(`14${name} 给定 Go 双模块 + pnpm 的 heavy 整轮 断言 1 次运行 1 轮且已证明`, () => {
      const runs = R.segmentRuns(vs, evs)
      assert.deepEqual(runs.map((r) => r.rounds.length), [1])
      if (name !== 'α') return
      assert.equal(vs[8].logRef.split(' ').length, 2)
      assert.equal(R.proofOf(runs[0].rounds[0], runs[0]).verdict, 'proven')
    })
  }

  const fixLoop = () => [
    ...seqRows(['build', 'lint', 'build', 'lint'], 10, 0, 'light', { 2: 'failed', 3: 'skipped' }),
    ...seqRows(['build', 'lint', 'build', 'lint'], 20, 1, 'light'),
  ]
  for (const [name, vs, evs] of variants(() => ({ evs: firstRun(0), vs: fixLoop() }))) {
    test(`15${name} 给定多模块的修复回路 断言 1 次运行 2 轮`, () => {
      assert.deepEqual(R.segmentRuns(vs, evs).map((r) => r.rounds.length), [2])
    })
  }
  const fixLoopRetry = () => {
    const { evs } = retry()
    return { evs, vs: [...fixLoop(), ...seqRows(['build', 'lint', 'build', 'lint'], 110, 0, 'light')] }
  }
  for (const [name, vs, evs] of variants(fixLoopRetry)) {
    test(`15${name}' 给定修复回路后再手动重试 断言 2 次运行，run1 2 轮、run2 1 轮`, () => {
      assert.deepEqual(R.segmentRuns(vs, evs).map((r) => r.rounds.length), [2, 1])
    })
  }

  test('16 给定无事件、无 logRef、两次 light 全 passed 断言承认合并：1 次运行 1 轮、boundary=order、总判定 unknown', () => {
    const vs = [v('build', 'passed', 10, null, 'light'), v('lint', 'passed', 10, null, 'light'),
      v('build', 'passed', 110, null, 'light'), v('lint', 'passed', 110, null, 'light')]
    const runs = R.segmentRuns(vs, [])
    assert.deepEqual(runs.map((r) => r.rounds.length), [1])
    assert.equal(runs[0].boundary, 'order')
    assert.equal(R.proofOf(runs[0].rounds[0], runs[0]).verdict, 'unknown')
  })
  test('16b 给定 logRef 齐全但一条非 skipped 的行为 null 断言 placementUncertain 且该轮 unknown', () => {
    const vs = seqRows(['build', 'lint', 'typecheck'], 10)
    vs[1] = { ...vs[1], logRef: null }
    const runs = R.segmentRuns(vs, firstRun(0))
    assert.equal(runs[0].rounds[0].rows[1].placementUncertain, true)
    assert.equal(runs[0].rounds[0].rows[0].placementUncertain, false)
    assert.equal(R.proofOf(runs[0].rounds[0], runs[0]).verdict, 'unknown')
  })
})

describe('proofOf', () => {
  const round = (rows) => ({ round: 0, rows })
  const run = { boundary: 'events' }
  const cases = [
    ['repro_fail passed', 'passed', 'established', '红：改动前确实失败 ✓'],
    ['repro_fail failed', 'failed', 'not_reproduced', '红未立：改动前测试已通过（bug 没复现）'],
    ['repro_fail error', 'error', 'not_run', '红阶段没跑起来'],
  ]
  for (const [name, status, red, label] of cases) {
    test(`给定 ${name} 断言 red=${red} 且文案为「${label}」`, () => {
      const p = R.proofOf(round([{ id: 1, tier: 'heavy', step: 'repro_fail', status }]), run)
      assert.equal(p.red, red)
      assert.equal(p.steps[0].label, label)
    })
  }
  const verdicts = [
    ['给定全部 skipped 断言未通过（与 Report.Passed 一致）', [['build', 'skipped']], 'failed'],
    ['给定 skipped 不计入、其余 passed 断言通过', [['build', 'passed'], ['lint', 'skipped']], 'passed'],
    ['给定一条 error 断言未通过', [['build', 'passed'], ['lint', 'error']], 'failed'],
    ['给定红立绿过 断言已证明', [['build', 'passed'], ['repro_fail', 'passed'], ['repro_pass', 'passed'], ['regression', 'passed']], 'proven'],
    ['给定红未立 断言未通过', [['repro_fail', 'failed']], 'failed'],
  ]
  for (const [name, rows, want] of verdicts) {
    test(name, () => {
      const p = R.proofOf(round(rows.map(([step, status], i) => ({ id: i + 1, tier: 'heavy', step, status }))), run)
      assert.equal(p.verdict, want)
    })
  }
})

// ---------------------------------------------------------------- 事件窗口

// fakeServer 模拟 /events 的分页语义（after/before/limit/failures，响应带 has_more）
function fakeServer(n) {
  const all = Array.from({ length: n }, (_, i) => ({
    id: i + 1, phase: 'implement', kind: i % 50 === 0 ? 'tool_result' : 'text',
    payload: i % 50 === 0 ? { isError: true } : {}, at: new Date(T0 + i * 1000).toISOString(),
  }))
  const calls = []
  async function fetchPage(p) {
    calls.push(p)
    const limit = p.limit || 200
    let src = p.failures ? all.filter((e) => R.isFailure(e)) : all
    let page
    let more
    if (p.before != null) {
      const older = src.filter((e) => e.id < p.before)
      page = older.slice(-limit)
      more = older.length > limit
    } else {
      const newer = src.filter((e) => e.id > (p.after || 0))
      page = newer.slice(0, limit)
      more = newer.length > limit
    }
    return { events: page, last_id: page.length ? page[page.length - 1].id : p.after || 0, first_id: page.length ? page[0].id : null, has_more: more }
  }
  return { all, calls, fetchPage }
}

describe('createEventWindow', () => {
  test('给定终态任务 1000 条 断言先拿到最后 200 条，loadOlder 4 次后覆盖全部且 olderAvailable=false', async () => {
    const srv = fakeServer(1000)
    const w = R.createEventWindow({ fetchPage: srv.fetchPage, max: 500, isTerminal: () => true })
    await w.tick()
    const ids = () => w.state.events.map((e) => e.id)
    assert.equal(w.state.events.length, 200)
    assert.equal(ids()[0], 801)
    assert.equal(ids()[199], 1000)
    assert.equal(w.state.olderAvailable, true)
    for (let i = 0; i < 4; i++) await w.loadOlder()
    assert.deepEqual(ids(), srv.all.map((e) => e.id))
    assert.equal(w.state.olderAvailable, false)
  })

  test('给定实时任务积压 1200 条新事件 断言一拍内最多拉 5 页、内存封顶 max 且 olderAvailable=true', async () => {
    const srv = fakeServer(1300)
    let terminal = false
    const w = R.createEventWindow({ fetchPage: srv.fetchPage, max: 500, isTerminal: () => terminal })
    w.state.lastId = 100 // 已经看到第 100 条
    w.state.started = true
    const keep = await w.tick()
    assert.equal(keep, true)
    assert.equal(srv.calls.length, 5)
    assert.ok(srv.calls.every((c) => c.after != null && c.before == null))
    assert.equal(w.state.events.length, 500)
    assert.equal(w.state.events[499].id, 1100)
    assert.equal(w.state.olderAvailable, true)
    terminal = true
    assert.equal(await w.tick(), false, '终态收尾这一拍后应停止轮询')
    assert.equal(w.state.events[w.state.events.length - 1].id, 1300)
  })

  test('给定 jumpTo 不在窗口里的 id 断言窗口被替换为前后各 100 条并标记脱离实时', async () => {
    const srv = fakeServer(1000)
    const w = R.createEventWindow({ fetchPage: srv.fetchPage, max: 500, isTerminal: () => false })
    await w.tick()
    assert.equal(await w.jumpTo(300), true)
    const ids = w.state.events.map((e) => e.id)
    assert.equal(ids[0], 201)
    assert.equal(ids[ids.length - 1], 400)
    assert.equal(ids.length, 200)
    assert.equal(w.state.detached, true)
    assert.equal(w.state.olderAvailable, true)
    // 脱离实时时 tick 不往尾部拼（否则窗口出现空洞）
    const n = srv.calls.length
    await w.tick()
    assert.equal(srv.calls.length, n)
    await w.backToLatest()
    assert.equal(w.state.detached, false)
    assert.equal(w.state.events[w.state.events.length - 1].id, 1000)
  })

  test('给定 jumpTo 已在窗口里的 id 断言不发请求', async () => {
    const srv = fakeServer(300)
    const w = R.createEventWindow({ fetchPage: srv.fetchPage, max: 500, isTerminal: () => true })
    await w.tick()
    const n = srv.calls.length
    assert.equal(await w.jumpTo(250), true)
    assert.equal(srv.calls.length, n)
  })

  test('给定 tick 在途时 jumpTo 断言旧 generation 的 tick 晚到响应被丢弃', async () => {
    const srv = fakeServer(1000)
    let release
    let hold = false
    const fetchPage = async (p) => {
      if (hold) {
        hold = false
        await new Promise((r) => (release = r))
        return { events: [{ id: 5000, phase: 'implement', kind: 'text', payload: {}, at: iso(0) }], last_id: 5000, first_id: 5000, has_more: false }
      }
      return srv.fetchPage(p)
    }
    const w = R.createEventWindow({ fetchPage, max: 500, isTerminal: () => false })
    await w.tick()
    hold = true
    const pendingTick = w.tick()
    await new Promise((r) => setImmediate(r))
    const jumped = w.jumpTo(300)
    release()
    await pendingTick
    await jumped
    const ids = w.state.events.map((e) => e.id)
    assert.ok(!ids.includes(5000), '旧 tick 的响应应被丢弃')
    assert.equal(ids[0], 201)
    assert.equal(ids[ids.length - 1], 400)
  })

  test('给定 findFailure 断言按当前 id 向服务端取下一个/上一个失败（不受 200 截断约束）', async () => {
    const srv = fakeServer(1000)
    const w = R.createEventWindow({ fetchPage: srv.fetchPage, max: 500, isTerminal: () => true })
    assert.equal(await w.findFailure('next', 1), 51)
    assert.equal(await w.findFailure('prev', 51), 1)
    assert.equal(await w.findFailure('prev', 1), null)
    const last = srv.calls[srv.calls.length - 1]
    assert.equal(last.failures, 1)
    assert.equal(last.limit, 1)
  })

  test('给定 setFilter 阶段过滤 断言后续请求带 phase 且窗口重置为最后一页', async () => {
    const srv = fakeServer(500)
    const w = R.createEventWindow({ fetchPage: srv.fetchPage, max: 500, isTerminal: () => true })
    await w.tick()
    await w.setFilter({ phase: ['fix-1'] })
    const last = srv.calls[srv.calls.length - 1]
    assert.deepEqual(last.phase, ['fix-1'])
    assert.ok(last.before != null)
  })
})
