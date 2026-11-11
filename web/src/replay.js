// replay.js —— 任务详情页「执行日志」的纯函数与事件窗口加载器
// （docs/features/run-replay.md §3.3.1）。
//
// 无 DOM、无 Vue、不 import 任何包（含 livelog.js）：node --test 能直接加载，
// 换回旧实现做「先红」时也不会因依赖解析失败而假红。上限常量一律由调用方传入。

// subagent 的事件（0014）收成一个可折叠子块，插在它首次出现的位置。
//
// 为什么是「兄弟块」而不是真的挂在那条 Agent 调用下面：agentId 与父调用
// toolUseId 的对应关系只存在于 transcript 的 toolUseResult 字段里，stdout
// 这条通道给不出。与其按时间顺序猜一个父子关系，不如老实地并排放 —— 子块
// 标题就是派给它的任务描述，与父调用那行显示的 description 是同一句话，
// 人一眼能对上。（真要连线，得再读父会话的 transcript，见 docs/04 §7.4。）
export function groupSubagents(list) {
  const out = []
  const blocks = new Map() // agentId → 子块
  for (const e of list) {
    const aid = e.agentId
    if (!aid) {
      out.push(e)
      continue
    }
    let b = blocks.get(aid)
    if (!b) {
      b = { subagent: true, id: 'sub-' + aid, agentId: aid, title: '', list: [] }
      blocks.set(aid, b)
      out.push(b) // 占住首次出现的位置，保持时间顺序
    }
    // 首行是派活描述，充当子块标题而不是块内的一条
    if (e.kind === 'agent_start') {
      b.title = e.body
      continue
    }
    b.list.push(e)
  }
  return out
}

// 一次工具调用在事件流里是两条（tool_use 发起 + tool_result 结果）。平铺时
// 两行各自漂着，只看得出「调过 Bash」，看不出这步花了多久、成没成 ——
// 这是执行日志最难扫的根因。按 payload.toolUseId 把结果并进发起那条，
// 顺带用两者的 at 之差算出耗时。
//
// 老事件的 tool_use 没有 id（该字段是后来才取的），配不上的一律原样放行，
// 退回两行平铺 —— 降级而非崩溃，与后端 KindRaw 同一立场。
//
// 子块对象（groupSubagents 产出）没有 kind，会被原样放行。
export function pairTools(list) {
  const out = []
  const pending = new Map() // toolUseId → 已放进 out 的发起项
  for (const e of list) {
    const id = e.payload?.toolUseId

    if (e.kind === 'tool_use') {
      const item = { ...e, result: null, durationMs: null }
      if (id) pending.set(id, item)
      out.push(item)
      continue
    }

    if (e.kind === 'tool_result' && id && pending.has(id)) {
      const item = pending.get(id)
      pending.delete(id) // 同一 id 只认第一条结果
      item.result = e
      item.durationMs = toolDuration(item.at, e.at)
      continue
    }

    out.push(e)
  }
  return out
}

// 耗时按两条事件的落库时间差算，但这个差值是粗粒度的：at 取自入库时的
// now()，而 EventSink 是 200ms 批量刷库（eventsink.go 的 sinkFlushEvery），
// 同一批里的发起与结果拿到的是同一个时间戳。也就是说误差约 ±200ms，
// 快调用会算出 0ms。
//
// 因此只在 1s 以上才显示：那时相对误差已降到 20% 以内，数字才说得上话。
// 更低的区间与其显示一个自信的错数，不如不显示 —— 「很快」这件事，
// 从一行 ✓ 上已经看得出来了。要拿到真实的单次工具耗时，得在事件里记
// 发生时刻而不是入库时刻（需要加列，见 docs/04 §7.5）。
export const TOOL_DURATION_FLOOR_MS = 1000

function toolDuration(useAt, resultAt) {
  const dt = new Date(resultAt) - new Date(useAt)
  if (!Number.isFinite(dt) || dt < TOOL_DURATION_FLOOR_MS) return null
  return dt
}

// ---------------------------------------------------------------- 失败判定

// severityOf 返回一条事件的失败级别：'error' | 'warn' | null。
//
// 与后端 store.FailurePredicateSQL 是同一份定义，由同一张表
// （testdata/failure-cases.json）驱动两边的测试。只认现有四种形状，未知形状
// 不判失败（宁可漏判，不误报 —— 兼容 f06 的新 CLI 事件）。配对后的 tool_use
// 以它挂着的 result 为准。
export function severityOf(ev) {
  if (!ev || ev.subagent) return null
  const p = ev.payload || {}
  const truthy = (v) => v === true || v === 'true'
  switch (ev.kind) {
    case 'tool_use':
      return ev.result ? severityOf(ev.result) : null
    case 'tool_result':
      return truthy(p.isError) ? 'error' : null
    case 'verify_step':
      return p.status === 'failed' || p.status === 'error' ? 'error' : null
    case 'result':
      return truthy(p.isError) || (p.subtype != null && p.subtype !== 'success') ? 'error' : null
    case 'raw':
      return p.type === 'overflow' ? 'warn' : null
    default:
      return null
  }
}

export function isFailure(ev) {
  return severityOf(ev) !== null
}

// ---------------------------------------------------------------- 阶段

const PHASE_META = {
  triage: { label: '分诊', order: 0 },
  implement: { label: '实现', order: 1 },
  'implement-reentry': { label: '实现（新会话重入）', order: 1.5 },
  verify: { label: '验证', order: 2 },
  review: { label: '评审', order: 3 },
  push: { label: '推送', order: 4 },
}

// phaseMeta 给出阶段的标签与排序：fix-N 为「修复第 N 轮」，order 1.75、
// sub = N（按数值比，fix-2 在 fix-10 之前）；不认识的阶段原样显示、排在最后。
export function phaseMeta(phase) {
  const m = PHASE_META[phase]
  if (m) return { label: m.label, order: m.order, sub: 0 }
  const fix = /^fix-(\d+)$/.exec(phase || '')
  if (fix) {
    const n = Number(fix[1])
    return { label: `修复第 ${n} 轮`, order: 1.75, sub: n }
  }
  return { label: phase, order: 9, sub: 0 }
}

export function comparePhases(a, b) {
  const x = phaseMeta(a)
  const y = phaseMeta(b)
  return x.order - y.order || x.sub - y.sub
}

// ---------------------------------------------------------------- 过滤

const itemPhase = (x) => (x.subagent ? x.list[0]?.phase : x.phase)

// applyFilter 本地过滤（输入是 pairTools(groupSubagents(...)) 之后的列表）。
// 四个条件取交集；subagent 子块里只要有一条命中就**原样**保留整个子块
// （list 不裁剪，便于看上下文）。全部关闭时返回值逐项 === 输入。
export function applyFilter(list, { phases = [], onlyFailures = false, hideThinking = false, onlyTools = false } = {}) {
  const byPhase = phases && phases.length > 0
  if (!byPhase && !onlyFailures && !hideThinking && !onlyTools) return list.slice()
  const match = (e) => {
    if (byPhase && !phases.includes(e.phase)) return false
    if (onlyFailures && !isFailure(e)) return false
    if (hideThinking && e.kind === 'thinking') return false
    if (onlyTools && e.kind !== 'tool_use' && e.kind !== 'tool_result') return false
    return true
  }
  return list.filter((x) => (x.subagent ? x.list.some(match) : match(x)))
}

// ---------------------------------------------------------------- 时间

// 静默分隔线的阈值：回放时最直接回答「卡在哪」的信号。
export const SILENCE_GAP_MS = 30000

const itemAt = (x) => (x.subagent ? x.list[0]?.at : x.at)

// withTiming 给每一项加 offsetMs（相对 origin）与 gapMs（到下一项的间隔）。
//
// at 是**入库**时刻，EventSink 200ms 批量刷库，误差约 ±200ms；所以 gapMs 低于
// 1s 置 null，与 TOOL_DURATION_FLOOR_MS 同一理由。返回浅拷贝，不改输入。
export function withTiming(list, origin) {
  const o = Date.parse(origin)
  const ts = list.map((x) => Date.parse(itemAt(x)))
  return list.map((x, i) => {
    const offsetMs = Number.isFinite(ts[i]) && Number.isFinite(o) ? ts[i] - o : null
    let gapMs = null
    if (i + 1 < list.length) {
      const g = ts[i + 1] - ts[i]
      if (Number.isFinite(g) && g >= TOOL_DURATION_FLOOR_MS) gapMs = g
    }
    return { ...x, offsetMs, gapMs }
  })
}

const pad2 = (n) => String(n).padStart(2, '0')

// formatOffset：向下取整到秒；不足 1 小时 +MM:SS，满 1 小时 +H:MM:SS；
// null/非有限数/负数返回 ''（不显示，不伪造 +00:00）。
export function formatOffset(ms) {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return ''
  const s = Math.floor(ms / 1000)
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  return h > 0 ? `+${h}:${pad2(m)}:${pad2(sec)}` : `+${pad2(m)}:${pad2(sec)}`
}

// silenceLabel：gapMs ≥ 30s 时返回分隔线文案，否则 null（TaskDetail 只在非空时
// 渲染分隔线，自己不再比较阈值）。
export function silenceLabel(gapMs) {
  if (gapMs == null || !Number.isFinite(gapMs) || gapMs < SILENCE_GAP_MS) return null
  const s = Math.floor(gapMs / 1000)
  let d
  if (s >= 3600) d = `${Math.floor(s / 3600)}h${pad2(Math.floor((s % 3600) / 60))}m`
  else if (s >= 60) d = `${Math.floor(s / 60)}m${s % 60}s`
  else d = `${s}s`
  return `⋯ 静默 ${d}`
}

// ---------------------------------------------------------------- 运行与轮次

const LIGHT_STEPS = new Set(['build', 'lint', 'typecheck'])
const REPRO_STEPS = new Set(['repro_fail', 'repro_pass', 'regression'])

const LOG_REF_RE = /(?:^|\/)round-(\d+)\/(\d+)-[^/\s]+\.log$/

// parseLogRef 把 log_ref（可能是空格分隔的多段）解析为 {round, seqMin, seqMax}。
// 任何一段不符合 …/round-<N>/<NN>-<step>.log、或多段不在同一轮时返回 null
// （当作没有 log_ref，不猜）。
export function parseLogRef(logRef) {
  if (!logRef) return null
  const parts = String(logRef).split(/\s+/).filter(Boolean)
  if (!parts.length) return null
  let round = null
  let seqMin = Infinity
  let seqMax = -Infinity
  for (const p of parts) {
    const m = LOG_REF_RE.exec(p)
    if (!m) return null
    const r = Number(m[1])
    if (round !== null && r !== round) return null
    round = r
    seqMin = Math.min(seqMin, Number(m[2]))
    seqMax = Math.max(seqMax, Number(m[2]))
  }
  return { round, seqMin, seqMax }
}

const byAtId = (a, b) => Date.parse(a.at) - Date.parse(b.at) || a.id - b.id

// 只认这些触发原因：区间里还会夹着别的带 reason 的转移（awaiting_approval 的
// 说明、blocked_spec 的 repro_not_red），它们描述的是上一次运行怎么停下的。
const CAUSE_TEXT = {
  manual_retry: (e) => `${e.actor} 手动重试（${e.payload?.mode ?? '—'}）`,
  manual_review_retry: (e) => `${e.actor} 重来一轮 review`,
  pr_review: () => 'PR review 意见回流后重跑',
  restart_reconcile: (e) => `服务重启后恢复（中断于 ${e.payload?.interrupted_state || '—'}）`,
  rebase_followup_reverify: () => '前驱合并后 rebase 重验',
}

// runCauseOf 给出一次运行的触发原因。startIdx 是 stateEvents（按 at,id 升序）
// 里该运行起点（toState=verifying）的下标：从起点往前、到上一条 verifying 之后，
// 取最后一条带已知 reason（或 unblocked_by）的事件；没有就按起点 actor 兜底。
export function runCauseOf(stateEvents, startIdx) {
  let prev = -1
  for (let i = startIdx - 1; i >= 0; i--) {
    if (stateEvents[i].toState === 'verifying') {
      prev = i
      break
    }
  }
  if (prev < 0) return '首次运行'
  for (let i = startIdx; i > prev; i--) {
    const e = stateEvents[i]
    const reason = e.payload?.reason
    if (reason && CAUSE_TEXT[reason]) return CAUSE_TEXT[reason](e)
    if (e.payload?.unblocked_by != null) return `依赖任务 #${e.payload.unblocked_by} 恢复后重跑`
  }
  return `重新进入验证（${stateEvents[startIdx].actor}）`
}

const refSet = (logRef) => (logRef ? String(logRef).split(/\s+/).filter(Boolean) : [])

// segmentRuns 把 verifications 切成「运行 → 轮次」（docs/features/run-replay.md
// §3.3.1 规则 1–8）。返回按时间升序的运行；视图自己决定最新的放上面。
//
//   规则 2：每条 toState=verifying 的状态事件是一次新运行的起点（不枚举入口）；
//   规则 3：(round, NN) 回退无条件切出新运行（NN 在一次 runVerify 内严格递增）；
//   规则 4：运行内按 a 轮号变化 / b 非 skipped 跟在非 passed 之后 /
//           c light 跟在复现之后 / d tier 变化 切轮次；
//   规则 5：logOverwritten —— 有 id 更大的行声明了交叠的日志路径；
//   规则 6：没有验证行的运行不显示、不参与编号。
export function segmentRuns(verifications, stateEvents) {
  const rows = [...(verifications || [])].sort(byAtId)
  const evs = [...(stateEvents || [])].sort(byAtId)
  const starts = []
  evs.forEach((e, i) => {
    if (e.toState === 'verifying') starts.push(i)
  })

  // 规则 2：按 verifying 事件切段（第一条之前的行归入无事件段）
  const segs = [{ startIdx: null, rows: [] }, ...starts.map((i) => ({ startIdx: i, rows: [] }))]
  let k = 0
  for (const r of rows) {
    const t = Date.parse(r.at)
    while (k < starts.length && Date.parse(evs[starts[k]].at) <= t) k++
    segs[k].rows.push(r)
  }

  // 规则 3：段内 (round, NN) 回退 → 新运行
  const runs = []
  for (const seg of segs) {
    let cur = { startIdx: seg.startIdx, rows: [] }
    let prev = null
    for (const r of seg.rows) {
      const p = parseLogRef(r.logRef)
      if (p && prev && (p.round < prev.round || (p.round === prev.round && p.seqMin <= prev.seqMax))) {
        runs.push(cur)
        cur = { startIdx: null, rows: [] }
      }
      if (p) prev = p
      cur.rows.push(r)
    }
    runs.push(cur)
  }

  // 规则 6：空运行不显示
  const kept = runs.filter((r) => r.rows.length > 0)

  // 规则 5：日志覆盖（与后端库内判定同一条规则）
  const runOf = new Map()
  kept.forEach((run, i) => run.rows.forEach((r) => runOf.set(r.id, i + 1)))
  const overwriter = (row) => {
    const mine = refSet(row.logRef)
    if (!mine.length) return null
    let best = null
    for (const o of rows) {
      if (o.id <= row.id) continue
      if (refSet(o.logRef).some((x) => mine.includes(x)) && (best === null || o.id > best.id)) best = o
    }
    return best
  }

  return kept.map((run, i) => {
    const hasRef = run.rows.some((r) => parseLogRef(r.logRef))
    let boundary
    if (run.startIdx !== null) boundary = 'events'
    else if (run.rows.every((r) => r.status === 'skipped' || parseLogRef(r.logRef))) boundary = 'logRef'
    else boundary = 'order'

    let cause
    if (run.startIdx !== null) cause = runCauseOf(evs, run.startIdx)
    else cause = i === 0 ? '首次运行' : '重新进入验证'

    // 规则 4：运行内切轮次
    const rounds = []
    let cur = null
    let seenRound = null
    let prev = null
    for (const r0 of run.rows) {
      const p = parseLogRef(r0.logRef)
      const ow = overwriter(r0)
      const uncertain =
        hasRef && !r0.logRef && r0.status !== 'skipped' && !(r0.step === 'repro_fail' && r0.status === 'error')
      const r = { ...r0, logOverwritten: !!ow, overwrittenByRun: ow ? runOf.get(ow.id) : null, placementUncertain: uncertain }
      const split =
        cur &&
        ((p && seenRound !== null && p.round !== seenRound) ||
          (r.status !== 'skipped' && prev.status !== 'passed') ||
          (LIGHT_STEPS.has(r.step) && REPRO_STEPS.has(prev.step)) ||
          r.tier !== prev.tier)
      if (!cur || split) {
        cur = { round: rounds.length, rows: [], placementUncertain: false }
        rounds.push(cur)
        seenRound = null
      }
      if (p) seenRound = p.round
      if (uncertain) cur.placementUncertain = true
      cur.rows.push(r)
      prev = r
    }

    return {
      run: i + 1,
      startedAt: run.startIdx !== null ? evs[run.startIdx].at : run.rows[0].at,
      cause,
      boundary,
      rounds,
    }
  })
}

const STEP_NAME = { build: '构建', lint: '静态检查', typecheck: '类型检查', regression: '回归' }
const MARK = { passed: '✓', failed: '✗', error: '!', skipped: '–' }

function stepLabel(row) {
  const st = row.status
  if (row.step === 'repro_fail') {
    return {
      passed: '红：改动前确实失败 ✓',
      failed: '红未立：改动前测试已通过（bug 没复现）',
      error: '红阶段没跑起来',
      skipped: '红：跳过',
    }[st] || `红：${st}`
  }
  if (row.step === 'repro_pass') {
    return { passed: '绿：改动后通过 ✓', failed: '绿 ✗ 改动后仍失败', error: '绿阶段没跑起来', skipped: '绿：跳过' }[st] || `绿：${st}`
  }
  return `${STEP_NAME[row.step] || row.step} ${MARK[st] || st}`
}

function aggregate(rows) {
  if (!rows.length) return null
  if (rows.some((r) => r.status === 'failed' || r.status === 'error')) return 'failed'
  if (rows.every((r) => r.status === 'skipped')) return 'skipped'
  return 'passed'
}

// proofOf 给出一轮的红绿结论。**只读 status，不重新判定**：总判定沿用
// Report.Passed（skipped 不计入、全部 skipped 判未通过、error 不算通过）。
// 运行边界按顺序推断，或本轮有归属不确定的行时，总判定为 unknown ——
// 行的归属是推断的，就不拿它下结论（铁律 1）。
export function proofOf(round, run = {}) {
  const rows = round?.rows || []
  const steps = rows.map((r) => ({
    ...r,
    label: stepLabel(r),
    tone: { passed: 'ok', failed: 'bad', error: 'warn', skipped: 'idle' }[r.status] || 'idle',
  }))
  const rf = rows.find((r) => r.step === 'repro_fail')
  const rp = rows.find((r) => r.step === 'repro_pass')
  const red = rf ? { passed: 'established', failed: 'not_reproduced', error: 'not_run', skipped: 'skipped' }[rf.status] || null : null
  const green = rp ? rp.status : null
  const regression = aggregate(rows.filter((r) => r.step === 'regression'))
  const light = aggregate(rows.filter((r) => LIGHT_STEPS.has(r.step)))

  let verdict
  if (run.boundary === 'order' || round?.placementUncertain) verdict = 'unknown'
  else {
    let ran = 0
    let ok = true
    for (const r of rows) {
      if (r.status === 'skipped') continue
      ran++
      if (r.status !== 'passed') ok = false
    }
    if (!(ok && ran > 0)) verdict = 'failed'
    else verdict = rf ? 'proven' : 'passed'
  }
  return { steps, red, green, regression, light, verdict }
}

// ---------------------------------------------------------------- 事件窗口

// 实时模式内存窗口上限沿用 livelog 的 MAX_LIVE_EVENTS（由调用方以 max 传入）；
// 回放（加载更早 / 定位）时窗口硬上限 REPLAY_MAX_EVENTS。
export const REPLAY_MAX_EVENTS = 2000
const PAGE = 200
const JUMP_HALF = 100
const LIVE_PAGES_PER_TICK = 5
// before 的「∞」：比任何 bigserial 都大，后端按正整数解析
const TAIL = Number.MAX_SAFE_INTEGER

// createEventWindow 是执行日志的事件窗口加载器（取代「appendCapped + 终态即停」）。
//
//   实时模式（非终态）：首拍从尾部取最后一页，之后每拍 after=lastId 增量拉，
//     一页填满（has_more）就在同一拍里继续拉，最多 5 页；窗口尾部保留最近 max 条，
//     头部裁掉时 olderAvailable=true（可以再加载）。
//   回放模式（终态 / 加载更早）：before=∞ 取最后一页，loadOlder 以 before=firstId
//     逐页向前；窗口硬上限 REPLAY_MAX_EVENTS，超出时从离视口远的一端（新的一端）裁，
//     标 newerAvailable 并脱离实时。
//   定位：jumpTo(id) 已在窗口里直接返回；否则用 id 前后各 100 条**替换**整个窗口
//     （不拼接，避免空洞），并标记脱离实时；backToLatest 恢复尾部。
//
// 所有请求经注入的 fetchPage，串行执行（共用一条 promise 链）；替换窗口的操作
// 递增 generation，晚到的旧响应按 generation 丢弃。
export function createEventWindow({ fetchPage, max, isTerminal, state = {} }) {
  Object.assign(state, {
    events: state.events || [],
    firstId: state.firstId ?? null,
    lastId: state.lastId || 0,
    olderAvailable: !!state.olderAvailable,
    newerAvailable: !!state.newerAvailable,
    detached: !!state.detached,
    started: !!state.started,
    filter: state.filter || {},
    error: '',
  })
  let gen = 0
  let chain = Promise.resolve()

  const serial = (fn) => {
    const p = chain.then(fn, fn)
    chain = p.catch(() => {})
    return p
  }
  const params = (p) => {
    const f = state.filter
    const out = { ...p }
    if (f.phase?.length) out.phase = f.phase
    if (f.kind?.length) out.kind = f.kind
    return out
  }
  const syncBounds = () => {
    const ev = state.events
    state.firstId = ev.length ? ev[0].id : null
    if (ev.length) state.lastId = ev[ev.length - 1].id
  }

  async function loadTail() {
    const g = gen
    const resp = await fetchPage(params({ before: TAIL, limit: PAGE }))
    if (g !== gen) return
    state.events.splice(0, state.events.length, ...(resp.events || []))
    state.olderAvailable = !!resp.has_more
    state.newerAvailable = false
    state.detached = false
    state.started = true
    if (!state.events.length) state.lastId = 0
    syncBounds()
    state.error = ''
  }

  async function liveTick() {
    const g = gen
    for (let i = 0; i < LIVE_PAGES_PER_TICK; i++) {
      const resp = await fetchPage(params({ after: state.lastId, limit: PAGE }))
      if (g !== gen) return
      const items = resp.events || []
      if (items.length) {
        state.events.push(...items)
        const over = state.events.length - max
        if (over > 0) {
          state.events.splice(0, over)
          state.olderAvailable = true
        }
        syncBounds()
      }
      state.error = ''
      if (!resp.has_more) break
    }
  }

  function trimNewer() {
    const over = state.events.length - REPLAY_MAX_EVENTS
    if (over > 0) {
      state.events.splice(state.events.length - over, over)
      state.newerAvailable = true
      state.detached = true
      syncBounds()
    }
  }

  return {
    state,

    // 轮询一拍：返回 false 表示停止轮询（终态后的这一拍就是收尾）。
    tick() {
      return serial(async () => {
        if (!state.started) await loadTail()
        else if (!state.detached) await liveTick()
        return !isTerminal()
      })
    },

    // load 兼容旧调用：立即拉一拍，不关心是否继续
    load() {
      return this.tick()
    },

    loadOlder() {
      return serial(async () => {
        if (!state.olderAvailable || state.firstId == null) return
        const g = gen
        const resp = await fetchPage(params({ before: state.firstId, limit: PAGE }))
        if (g !== gen) return
        state.events.unshift(...(resp.events || []))
        state.olderAvailable = !!resp.has_more
        trimNewer()
        syncBounds()
      })
    },

    loadNewer() {
      return serial(async () => {
        if (!state.newerAvailable) return
        const g = gen
        const resp = await fetchPage(params({ after: state.lastId, limit: PAGE }))
        if (g !== gen) return
        state.events.push(...(resp.events || []))
        state.newerAvailable = !!resp.has_more
        const over = state.events.length - REPLAY_MAX_EVENTS
        if (over > 0) {
          state.events.splice(0, over)
          state.olderAvailable = true
        }
        if (!state.newerAvailable) state.detached = false
        syncBounds()
      })
    },

    // jumpTo 返回 id 是否在（替换后的）窗口里。
    jumpTo(id) {
      if (state.events.some((e) => e.id === id)) return Promise.resolve(true)
      const g = ++gen
      return serial(async () => {
        if (g !== gen) return false
        if (state.events.some((e) => e.id === id)) return true
        const before = await fetchPage(params({ before: id + 1, limit: JUMP_HALF }))
        const after = await fetchPage(params({ after: id, limit: JUMP_HALF }))
        if (g !== gen) return false
        state.events.splice(0, state.events.length, ...(before.events || []), ...(after.events || []))
        state.olderAvailable = !!before.has_more
        state.newerAvailable = !!after.has_more
        state.detached = true
        state.started = true
        syncBounds()
        return state.events.some((e) => e.id === id)
      })
    },

    backToLatest() {
      gen++
      return serial(loadTail)
    },

    // setFilter 改服务端过滤（阶段 chip），窗口重置为过滤后的最后一页。
    setFilter(filter) {
      state.filter = { ...filter }
      gen++
      return serial(loadTail)
    },

    // findFailure 以当前失败 id 为游标向服务端要下一个/上一个失败（含警告，
    // 与 summary 的 failures 同口径），不受 summary 200 条截断的约束。
    findFailure(dir, fromId) {
      return serial(async () => {
        const p = dir === 'prev' ? { before: fromId, failures: 1, limit: 1 } : { after: fromId, failures: 1, limit: 1 }
        const resp = await fetchPage(p)
        return resp.events?.[0]?.id ?? null
      })
    },
  }
}
