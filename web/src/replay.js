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

// createEventWindow 是执行日志的事件加载器。
//
// fetchPage(params) 由调用方注入（生产里是 api.taskEvents，测试里是假实现）；
// state 可传入一个响应式对象（Vue 的 reactive），本模块只就地改它的字段。
//
// 这一版与 base 行为完全一致：after=lastId 增量拉一页、内存封顶 max 条、
// 终态拉完这一 tick 即停（docs/04 §3.4）。
export function createEventWindow({ fetchPage, max, isTerminal, state = {} }) {
  state.events = state.events || []
  state.lastId = state.lastId || 0
  state.dropped = state.dropped || 0
  state.error = ''

  function appendCapped(items) {
    if (!items?.length) return 0
    state.events.push(...items)
    const over = state.events.length - max
    if (over > 0) {
      state.events.splice(0, over)
      return over
    }
    return 0
  }

  async function load() {
    const resp = await fetchPage({ after: state.lastId })
    state.dropped += appendCapped(resp.events)
    if (resp.last_id != null) state.lastId = resp.last_id
    state.error = ''
  }

  return {
    state,
    load,
    // 轮询一拍：返回 false 表示停止（终态后这一轮就是收尾：终态转移前 sink 已
    // drain，事件不会再多）
    async tick() {
      await load()
      return !isTerminal()
    },
  }
}
