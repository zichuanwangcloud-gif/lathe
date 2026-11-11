<script setup>
import { ref, reactive, onMounted, onUnmounted, computed, inject, nextTick, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime, formatDuration } from '../api'
import AgentEventItem from '../components/AgentEventItem.vue'
import VerifyLogViewer from '../components/VerifyLogViewer.vue'
import { confirmDialog } from '../confirm'
import { createMarkdown, createPoller, MAX_LIVE_EVENTS } from '../livelog'
import {
  applyFilter, comparePhases, createEventWindow, formatOffset, groupSubagents, pairTools,
  phaseMeta, proofOf, segmentRuns, severityOf, silenceLabel, withTiming,
} from '../replay'

const props = defineProps({ id: { type: String, required: true } })
const router = useRouter()
const onUnauthorized = inject('onUnauthorized')

const detail = ref(null)
const error = ref('')
const acting = ref(false)
const retryPlan = ref(null)
let detailPoller = null

const task = computed(() => detail.value?.task)
const isTerminal = computed(() =>
  ['merged', 'failed', 'cancelled'].includes(task.value?.state)
)

// 智能重试的按钮文案：预览决策直接写在按钮上，重试不是黑盒
const retryCta = computed(() => {
  const p = retryPlan.value
  if (!p) return '重试'
  if (p.fresh) return '重试 · 从头重跑'
  if (p.rebuild) return '重来一轮 review · 重挂现场'
  return `重试 · 从「${p.entryLabel}」续跑`
})

async function load() {
  try {
    detail.value = await api.task(props.id)
    error.value = ''
    // 失败任务、以及停在 review_feedback 的任务都拉重试预览：体检现场 + 决策理由
    if (detail.value.task.state === 'failed' || detail.value.task.state === 'review_feedback') {
      try {
        retryPlan.value = await api.retryPlan(props.id)
      } catch {
        retryPlan.value = null // 预览失败不挡着重试本身
      }
    } else {
      retryPlan.value = null
    }
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function act(fn, confirmText) {
  if (confirmText && !(await confirmDialog(confirmText))) return
  acting.value = true
  try {
    await fn(props.id)
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    acting.value = false
  }
}

// ---------------------------------------------------------------- 执行日志
// docs/04 §3.4 + docs/features/run-replay.md §3.3：实时任务 2s 增量轮询；
// 终态任务进入回放模式 —— 先取最后一页，「加载更早」按需向前翻页。

const evWindow = createEventWindow({
  fetchPage: ({ after, limit = 200, ...opts }) => api.taskEvents(props.id, after, limit, opts),
  max: MAX_LIVE_EVENTS,
  isTerminal: () => isTerminal.value,
  state: reactive({}),
})
const ws = evWindow.state
const events = computed(() => ws.events)
const eventsError = computed(() => ws.error)
let evPoller = null

async function loadEvents() {
  try {
    await evWindow.tick()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    ws.error = e.message
  }
}

async function guarded(fn) {
  try {
    await fn()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    ws.error = e.message
  }
}

// summary：不受内存窗口影响的全局视图（阶段条数/失败数、首末失败）
const summary = ref(null)
async function loadSummary() {
  try {
    summary.value = await api.taskEventsSummary(props.id)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
  }
}

// 模型输出不可信：issue 内容可以借间接提示注入让模型吐出带 onerror 的
// 标记，marked 出 HTML 后必须过一层消毒。
const md = createMarkdown()

// ---- 工具栏状态
const selectedPhases = ref([])
const onlyFailures = ref(false)
const hideThinking = ref(false)
const onlyTools = ref(false)

const TIME_MODES = { task: '相对任务起点', phase: '相对阶段起点', abs: '绝对时间' }
const TIME_KEY = 'lathe.taskDetail.timeMode'
function readTimeMode() {
  try {
    const v = localStorage.getItem(TIME_KEY)
    return v in TIME_MODES ? v : 'task'
  } catch {
    return 'task'
  }
}
const timeMode = ref(readTimeMode())
watch(timeMode, (v) => {
  try {
    localStorage.setItem(TIME_KEY, v)
  } catch {
    // 隐私窗口等场景读写会抛，偏好只是便利，不影响渲染
  }
})

function togglePhase(phase) {
  const cur = selectedPhases.value
  selectedPhases.value = cur.includes(phase) ? cur.filter((p) => p !== phase) : [...cur, phase]
  guarded(() => evWindow.setFilter({ phase: selectedPhases.value }))
}
function clearPhases() {
  selectedPhases.value = []
  guarded(() => evWindow.setFilter({}))
}

// ---- 受控展开：工具输出 / thinking / 子块 / 阶段，按 id 记在集合里
const openSet = ref(new Set())
function toggleOpen(id) {
  const s = new Set(openSet.value)
  s.has(id) ? s.delete(id) : s.add(id)
  openSet.value = s
}
const openPhases = ref(new Set())
const closedPhases = ref(new Set())

const phaseGroups = computed(() => {
  const groups = new Map()
  for (const e of events.value) {
    if (!groups.has(e.phase)) groups.set(e.phase, [])
    groups.get(e.phase).push(e)
  }
  const taskOrigin = summary.value?.firstAt || events.value[0]?.at
  return [...groups.entries()]
    .map(([phase, list]) => {
      // 先把 subagent 收成子块，再对主流与每个子块分别做工具配对
      const paired = pairTools(groupSubagents(list)).map((x) =>
        x.subagent ? { ...x, list: pairTools(x.list) } : x
      )
      const filtered = applyFilter(paired, {
        onlyFailures: onlyFailures.value,
        hideThinking: hideThinking.value,
        onlyTools: onlyTools.value,
      })
      const ps = summary.value?.phases?.find((p) => p.phase === phase)
      const origin = timeMode.value === 'phase' ? ps?.firstAt || list[0]?.at : taskOrigin
      return { phase, list: withTiming(filtered, origin), total: list.length, meta: phaseMeta(phase), stat: ps }
    })
    .sort((a, b) => comparePhases(a.phase, b.phase))
})

function timeLabel(e) {
  const at = e.subagent ? e.list[0]?.at : e.at
  if (timeMode.value === 'abs') return at ? new Date(at).toLocaleTimeString() : ''
  return formatOffset(e.offsetMs)
}
function timeTitle(e) {
  const at = e.subagent ? e.list[0]?.at : e.at
  return at ? `${new Date(at).toLocaleString()}（入库时间，粒度约 ±200ms）` : ''
}
function rowSeverity(e) {
  if (e.subagent) return e.list.some((x) => severityOf(x) === 'error') ? 'error' : null
  return severityOf(e)
}

// ---- 失败定位
const highlightId = ref(null)
const currentFailure = ref(null)
const jumpError = ref('')

function containerOf(id) {
  for (const g of phaseGroups.value) {
    for (const x of g.list) {
      if (x.id === id) return { phase: g.phase, sub: null, item: x }
      if (x.subagent && x.list.some((y) => y.id === id)) return { phase: g.phase, sub: x.id, item: x.list.find((y) => y.id === id) }
    }
    // 配对后的 tool_result 并进了发起那条
    for (const x of g.list) {
      if (x.result?.id === id) return { phase: g.phase, sub: null, item: x }
      if (x.subagent) {
        const y = x.list.find((z) => z.result?.id === id)
        if (y) return { phase: g.phase, sub: x.id, item: y }
      }
    }
  }
  return null
}

async function jumpTo(id) {
  if (id == null) return
  jumpError.value = ''
  let ok = false
  await guarded(async () => {
    ok = await evWindow.jumpTo(id)
  })
  if (!ok) {
    // 阶段过滤之外的事件：清掉过滤再找一次
    if (selectedPhases.value.length) {
      selectedPhases.value = []
      await guarded(() => evWindow.setFilter({}))
      await guarded(async () => {
        ok = await evWindow.jumpTo(id)
      })
    }
    if (!ok) {
      jumpError.value = `事件 #${id} 不存在或已被清理`
      return
    }
  }
  currentFailure.value = id
  await nextTick()
  let c = containerOf(id)
  if (!c && (onlyFailures.value || hideThinking.value || onlyTools.value)) {
    // 本地开关把目标藏起来了（例如「只看工具调用」藏掉了 verify_step）：放开再找
    onlyFailures.value = hideThinking.value = onlyTools.value = false
    await nextTick()
    c = containerOf(id)
  }
  if (c) {
    const op = new Set(openPhases.value).add(c.phase)
    openPhases.value = op
    const cp = new Set(closedPhases.value)
    cp.delete(c.phase)
    closedPhases.value = cp
    const s = new Set(openSet.value)
    if (c.sub) s.add(c.sub)
    s.add(c.item.id)
    openSet.value = s
  }
  const anchor = c ? c.item.id : id
  highlightId.value = anchor
  try {
    history.replaceState(history.state, '', `#ev-${id}`)
  } catch {
    // 沙箱里 replaceState 可能被拒，定位本身不受影响
  }
  await nextTick()
  document.getElementById(`ev-${anchor}`)?.scrollIntoView({ block: 'center' })
  setTimeout(() => {
    if (highlightId.value === anchor) highlightId.value = null
  }, 1500)
}

async function jumpFailure(dir) {
  const from = currentFailure.value ?? (dir === 'next' ? 0 : Number.MAX_SAFE_INTEGER)
  let id = null
  await guarded(async () => {
    id = await evWindow.findFailure(dir, from)
  })
  if (id == null) {
    jumpError.value = dir === 'next' ? '后面没有更多失败了' : '前面没有更多失败了'
    return
  }
  await jumpTo(id)
}

function expandAll(open) {
  const s = new Set()
  const ph = new Set()
  if (open) {
    for (const g of phaseGroups.value) {
      ph.add(g.phase)
      for (const x of g.list) {
        s.add(x.id)
        if (x.subagent) for (const y of x.list) s.add(y.id)
      }
    }
  }
  openSet.value = s
  openPhases.value = ph
  closedPhases.value = open ? new Set() : new Set(phaseGroups.value.map((g) => g.phase))
}

function phaseOpen(g) {
  if (closedPhases.value.has(g.phase)) return false
  return openPhases.value.has(g.phase) || g.phase === activePhase.value
}
function onPhaseToggle(g, e) {
  const want = e.target.open
  if (want === phaseOpen(g)) return
  const op = new Set(openPhases.value)
  const cp = new Set(closedPhases.value)
  if (want) {
    op.add(g.phase)
    cp.delete(g.phase)
  } else {
    op.delete(g.phase)
    cp.add(g.phase)
  }
  openPhases.value = op
  closedPhases.value = cp
}

function phaseDuration(stat) {
  if (!stat) return ''
  const d = new Date(stat.lastAt) - new Date(stat.firstAt)
  return Number.isFinite(d) && d >= 1000 ? formatDuration(d) : ''
}

// summary 里有、窗口里还没有的阶段也要出 chip（条数来自 summary）
const phaseChips = computed(() => {
  const list = summary.value?.phases || []
  return [...list].sort((a, b) => comparePhases(a.phase, b.phase)).map((p) => ({ ...p, meta: phaseMeta(p.phase) }))
})

// 子块头上的计数：显示它调了多少次工具，比"多少条事件"更能说明它干了多少活
function subagentToolCount(block) {
  return block.list.filter((e) => e.kind === 'tool_use').length
}

// 当前阶段：进行中按任务状态推断，终态取最后一个有事件的阶段
const activePhase = computed(() => {
  const g = phaseGroups.value
  if (!g.length) return null
  if (isTerminal.value) return g[g.length - 1].phase
  const byState = {
    triaging: 'triage',
    implementing: 'implement',
    verifying: 'verify',
    review_feedback: 'review',
  }
  let cur = byState[task.value?.state]
  // 推送复用 verifying 状态（状态机无独立推送态）：一旦出现推送阶段
  // 事件（如网络抖动重试），说明验证已完、正在推送，置顶推送组
  if (cur === 'verify' && g.some((x) => x.phase === 'push')) cur = 'push'
  return g.some((x) => x.phase === cur) ? cur : g[g.length - 1].phase
})

// 当前进行中的阶段置顶展开，其余按分诊→实现→验证的序
const orderedGroups = computed(() => {
  const g = phaseGroups.value
  const cur = g.find((x) => x.phase === activePhase.value)
  return cur ? [cur, ...g.filter((x) => x !== cur)] : g
})

// ---------------------------------------------------------------- 验证区
// 证明条只读 detail.verifications（证据本体），不从事件流推断：事件流可能因
// 溢出丢事件，verifications 是可审计的那一份（docs/features/run-replay.md §3.3.4）。

const runs = computed(() => {
  const d = detail.value
  if (!d) return []
  return segmentRuns(d.verifications || [], d.events || [])
    .map((run) => ({
      ...run,
      rounds: run.rounds.map((r) => ({ ...r, proof: proofOf(r, run) })).reverse(),
    }))
    .reverse() // 最新一次运行在最上面
})

const VERDICT = {
  proven: { mark: '✓', tone: 'ok', label: '已证明' },
  passed: { mark: '✓', tone: 'ok', label: '通过' },
  failed: { mark: '✗', tone: 'bad', label: '未通过' },
  unknown: { mark: '?', tone: 'idle', label: '归属为推断，不下结论' },
}

function roundTitle(run, round) {
  if (run.boundary === 'order') return `第 ${round.round + 1} 段（按顺序推断）`
  return round.round === 0 ? '第 1 轮' : `第 ${round.round + 1} 轮（修复第 ${round.round} 轮之后）`
}
function runTitle(run) {
  return `第 ${run.run} 次运行 · ${run.cause} · ${formatTime(run.startedAt)}`
}
function stepTitle(row) {
  const t = []
  if (row.logOverwritten) t.push(`该运行的日志已被第 ${row.overwrittenByRun} 次运行覆盖`)
  if (row.placementUncertain) t.push('该步日志落盘失败，归属的轮次是按顺序推断的')
  t.push(`${row.tier} · ${formatDuration(row.durationMs)} · ${formatTime(row.at)}`)
  return t.join('\n')
}

const openRuns = ref(new Set())
const viewing = ref(null) // 正在看完整日志的 verification 行
function openLog(row) {
  viewing.value = viewing.value?.id === row.id ? null : row
}
// 时间线里 verify_step 的「查看完整日志」：按 step 找同名、最新的、有日志的验证行
function openLogForEvent(ev) {
  const step = ev.payload?.step
  for (const run of runs.value) {
    for (const r of run.rounds) {
      const row = [...r.rows].reverse().find((x) => x.step === step && x.logRef && !x.logOverwritten)
      if (row) {
        viewing.value = row
        openRuns.value = new Set(openRuns.value).add(run.run)
        nextTick(() => document.getElementById('verify-card')?.scrollIntoView({ block: 'start' }))
        return
      }
    }
  }
}

// 终态切换时刷新 summary；实时模式下每 15 秒刷一次（不放进 5 秒的详情轮询）
watch(isTerminal, (t, was) => {
  if (t && !was) loadSummary()
})
let summaryPoller = null

onMounted(async () => {
  load()
  loadSummary()
  summaryPoller = createPoller(async () => {
    if (isTerminal.value) return false
    await loadSummary()
  }, 15000)
  summaryPoller.start()
  await loadEvents()
  // 深链接：#ev-<id> 先定位再滚动过去
  const m = /^#ev-(\d+)$/.exec(location.hash || '')
  if (m) jumpTo(Number(m[1]))
  // 自链式轮询：上一轮没回来不发下一轮（慢网络下 setInterval 会叠加请求）
  detailPoller = createPoller(async () => {
    if (!isTerminal.value) await load()
  }, 5000)
  evPoller = createPoller(async () => {
    await loadEvents()
    // 终态后这一轮就是收尾：终态转移前 sink 已 drain，事件不会再多；
    // 历史部分由「加载更早」按需翻页（createEventWindow 的回放模式）
    return !isTerminal.value
  }, 2000)
  detailPoller.start()
  evPoller.start()
})
onUnmounted(() => {
  detailPoller?.stop()
  evPoller?.stop()
  summaryPoller?.stop()
})
</script>

<template>
  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>
  <div v-if="!detail" class="empty">加载中…</div>

  <template v-else>
    <div class="spread head">
      <div>
        <div class="row">
          <h1 class="mono">{{ task.externalKey }}</h1>
          <span class="badge" :class="stateTone(task.state)">{{ stateLabel(task.state) }}</span>
        </div>
        <div class="faint mono">
          {{ task.providerRepo }} · 任务 #{{ task.id }}
          <template v-if="task.trackerProvider === 'internal' && task.issueId">
            · 工单 <RouterLink :to="`/issues/${task.issueId}`">{{ task.externalKey }}</RouterLink>
          </template>
        </div>
      </div>
      <div class="wrap">
        <button @click="router.push('/')">返回</button>
        <!-- 人工闸门放行：只在任务真的停在闸门上时出现 -->
        <button
          v-if="task.state === 'awaiting_approval'"
          class="primary"
          :disabled="acting"
          title="验证已通过。确认后才会推分支并开 PR"
          @click="act(api.approve, '确认推分支并开 PR？')"
        >确认开 PR</button>
        <template v-if="task.state === 'failed'">
          <button
            class="primary"
            :disabled="acting"
            :title="retryPlan ? retryPlan.reasons.join('\n') : ''"
            @click="act((id) => api.retry(id, 'auto'))"
          >{{ retryCta }}</button>
          <button
            v-if="retryPlan && !retryPlan.fresh"
            :disabled="acting"
            @click="act((id) => api.retry(id, 'fresh'), '确认丢弃现有工作区与分支，从分诊从头重跑？')"
          >从头重跑</button>
        </template>
        <!-- parked 的人工出口：通用重试会丢弃现场（第一轮分支已在远端，
             推送必被非快进拒绝），所以只给这一个按钮 -->
        <button
          v-if="task.state === 'review_feedback'"
          class="primary"
          :disabled="acting"
          :title="retryPlan ? retryPlan.reasons.join('\n') : ''"
          @click="act(api.reviewRetry, '确认重来一轮 review？会重建工作区现场（远端分支原样检出），并按 review 模式派发二轮。')"
        >{{ retryCta }}</button>
        <button
          v-if="!isTerminal"
          class="danger"
          :disabled="acting"
          @click="act(api.cancel, '确认取消这个任务？')"
        >取消</button>
      </div>
    </div>

    <div v-if="task.state === 'awaiting_approval'" class="card gate-card">
      <div class="label">等待人工放行</div>
      <p>
        验证已经通过，但这个仓库配了人工闸门（<span class="mono">gate_mode=manual</span>），
        所以平台停在这里没有推分支、也没有开 PR。
      </p>
      <p class="dim">
        看过改动之后点右上角「确认开 PR」；平台只补推送与开 PR 两步，
        不会重跑实现与验证。不想要这个改动就直接取消任务。
      </p>
    </div>

    <div v-if="task.state === 'review_feedback'" class="card gate-card">
      <div class="label">待处理的评审意见</div>
      <p>
        PR 上收到了评审意见，任务已经停在这里等人接手。
        它不会自动派发 —— 要么没有可续跑的 agent 会话，要么工作区现场已不可用，
        两种情况都会让二轮白烧一轮（第一轮的提交早已推到远端，从头重建的分支
        在推送时会被拒）。
      </p>
      <p v-if="retryPlan" class="dim">
        {{ retryPlan.rebuild ? '点「重来一轮 review」会把远端分支原样检出到本地工作区，再按 review 模式派发二轮。'
          : '点「重来一轮 review」会按 review 模式派发二轮，逐条处理 PR 上的意见后重新走一遍完整验证。' }}
      </p>
      <p class="dim">
        不想要这个改动就直接取消任务。也可以直接进现场手工处理，再点同一个按钮让平台接手收尾。
      </p>
    </div>

    <div v-if="task.failureReason" class="card fail-card">
      <div class="spread">
        <div class="label">失败原因</div>
        <!-- 跳到最后一个失败事件：数据源是 summary.lastFailure（不受 200 条截断影响） -->
        <button v-if="summary?.lastFailure" @click="jumpTo(summary.lastFailure.id)">在日志中定位</button>
      </div>
      <pre>{{ task.failureReason }}</pre>
      <p v-if="task.worktreePath" class="dim tip">
        现场已保留，可直接进去接手：
        <code class="mono">cd {{ task.worktreePath }}</code>
      </p>
      <div v-if="retryPlan" class="retry-plan">
        <div class="label">重试计划（智能决策）</div>
        <p>
          <template v-if="retryPlan.fresh">丢弃旧现场，从分诊从头重跑。</template>
          <template v-else>
            从「{{ retryPlan.entryLabel }}」阶段续跑{{ retryPlan.resumeSession ? '，resume 原 agent 会话' : '' }}。
          </template>
        </p>
        <ul class="reasons">
          <li v-for="(r, i) in retryPlan.reasons" :key="i">{{ r }}</li>
        </ul>
        <p v-if="retryPlan.worktree && retryPlan.worktree.exists" class="dim">
          现场：分支 {{ retryPlan.worktree.commits }} 个提交
          <template v-if="retryPlan.worktree.dirty">，有未提交改动</template>
          <template v-if="retryPlan.worktree.remoteBranch">，远端已有分支</template>
        </p>
      </div>
    </div>

    <!-- 交付摘要卡（docs/04 §3.5）：实现阶段终局自述 + 成本/耗时徽章 -->
    <div v-if="task.agentSummary" class="card summary-card">
      <div class="spread">
        <div class="label">交付摘要</div>
        <div class="row badges">
          <span v-if="task.agentNumTurns != null" class="badge idle">{{ task.agentNumTurns }} 轮</span>
          <span v-if="task.agentDurationMs != null" class="badge idle">{{ formatDuration(task.agentDurationMs) }}</span>
          <span v-if="task.agentCostUsd != null" class="badge idle">${{ Number(task.agentCostUsd).toFixed(4) }}</span>
        </div>
      </div>
      <div class="md" v-html="md(task.agentSummary)"></div>
    </div>

    <div class="grid">
      <div class="card">
        <div class="label">基本信息</div>
        <dl>
          <dt>类型</dt><dd>{{ task.taskKind || '—' }}</dd>
          <dt>验证档位</dt><dd>{{ task.verifyTier || '—' }}</dd>
          <dt>分支</dt><dd class="mono">{{ task.branchName || '—' }}</dd>
          <dt>PR</dt>
          <dd>
            <a v-if="task.prUrl" :href="task.prUrl" target="_blank" rel="noopener">{{ task.prUrl }}</a>
            <span v-else>—</span>
          </dd>
          <dt>工作区</dt><dd class="mono break">{{ task.worktreePath || '—' }}</dd>
          <dt>会话</dt><dd class="mono break">{{ task.agentSessionId || '—' }}</dd>
          <dt>创建</dt><dd>{{ formatTime(task.createdAt) }}</dd>
          <dt>更新</dt><dd>{{ formatTime(task.updatedAt) }}</dd>
        </dl>
      </div>

      <div class="card">
        <div class="label">状态轨迹</div>
        <ol class="timeline">
          <li v-for="e in detail.events" :key="e.id">
            <span class="badge" :class="stateTone(e.toState)">{{ stateLabel(e.toState) }}</span>
            <span class="faint">{{ e.actor }}</span>
            <span class="faint">{{ formatTime(e.at) }}</span>
            <div v-if="e.payload && Object.keys(e.payload).length" class="faint mono payload">
              {{ JSON.stringify(e.payload) }}
            </div>
          </li>
        </ol>
      </div>
    </div>

    <!-- 执行日志面板（docs/04 §3.4）：按阶段分节，当前阶段置顶展开 -->
    <div class="card">
      <div class="spread">
        <div class="label">执行日志</div>
        <span v-if="!isTerminal && !ws.detached" class="faint live">● 实时</span>
      </div>

      <!-- 工具栏（吸顶）：阶段 chip、开关、时间显示、失败导航、全部展开/折叠 -->
      <div class="toolbar">
        <div v-if="phaseChips.length" class="chips">
          <button class="chip" :class="{ on: !selectedPhases.length }" @click="clearPhases">全部</button>
          <button
            v-for="p in phaseChips"
            :key="p.phase"
            class="chip"
            :class="{ on: selectedPhases.includes(p.phase) }"
            @click="togglePhase(p.phase)"
          >
            {{ p.meta.label }} {{ p.count }}<span v-if="p.failures" class="chip-bad"> ✗{{ p.failures }}</span>
          </button>
        </div>
        <div class="tools">
          <label><input v-model="onlyFailures" type="checkbox" /> 只看失败</label>
          <label><input v-model="hideThinking" type="checkbox" /> 隐藏思考</label>
          <label><input v-model="onlyTools" type="checkbox" /> 只看工具调用</label>
          <select v-model="timeMode" aria-label="时间显示">
            <option v-for="(label, k) in TIME_MODES" :key="k" :value="k">{{ label }}</option>
          </select>
          <span v-if="summary" class="fail-nav">
            <span :class="summary.failureCount ? 'bad-text' : 'faint'">✗ {{ summary.failureCount }} 个失败</span>
            <button :disabled="!summary.firstFailure" @click="jumpTo(summary.firstFailure?.id)">第一个</button>
            <button :disabled="!summary.failures.length" title="上一个失败" @click="jumpFailure('prev')">↑</button>
            <button :disabled="!summary.failures.length" title="下一个失败" @click="jumpFailure('next')">↓</button>
            <span v-if="summary.failuresTruncated" class="faint">（列表仅列前 200 个，↑/↓ 仍可继续）</span>
          </span>
          <button @click="expandAll(true)">全部展开</button>
          <button @click="expandAll(false)">全部折叠</button>
        </div>
      </div>

      <div v-if="eventsError" class="faint" style="margin-bottom: 8px">日志拉取失败：{{ eventsError }}</div>
      <div v-if="jumpError" class="faint" style="margin-bottom: 8px">{{ jumpError }}</div>
      <div class="faint window-info">
        <template v-if="summary">共 {{ summary.total }} 条，</template>已加载 {{ events.length }} 条
        <button v-if="ws.olderAvailable" @click="guarded(() => evWindow.loadOlder())">加载更早的 200 条</button>
        <button v-if="ws.newerAvailable" @click="guarded(() => evWindow.loadNewer())">加载更新的 200 条</button>
        <button v-if="ws.detached" @click="guarded(() => evWindow.backToLatest())">回到最新</button>
      </div>

      <template v-if="orderedGroups.length">
        <details
          v-for="g in orderedGroups"
          :key="g.phase"
          class="phase"
          :open="phaseOpen(g)"
          @toggle="onPhaseToggle(g, $event)"
        >
          <summary>
            <span class="phase-name">{{ g.meta.label }}</span>
            <span class="faint">{{ g.stat ? g.stat.count : g.total }} 条</span>
            <span v-if="g.stat?.failures" class="bad-text small">✗ {{ g.stat.failures }}</span>
            <span v-if="g.stat" class="faint small" :title="`${new Date(g.stat.firstAt).toLocaleString()} → ${new Date(g.stat.lastAt).toLocaleString()}`">
              {{ phaseDuration(g.stat) }}
            </span>
            <span v-if="g.phase === activePhase && !isTerminal" class="badge run">进行中</span>
          </summary>

          <ol v-if="phaseOpen(g)" class="log">
            <template v-for="e in g.list" :key="e.id">
              <li
                :id="'ev-' + e.id"
                class="ev"
                :class="[
                  e.subagent ? 'ev-sub' : 'ev-' + e.kind,
                  rowSeverity(e) === 'error' ? 'ev-failed' : rowSeverity(e) === 'warn' ? 'ev-warn' : '',
                  highlightId === e.id ? 'ev-flash' : '',
                ]"
              >
                <span class="ev-time mono" :title="timeTitle(e)">{{ timeLabel(e) }}</span>
                <div class="ev-body">
                  <!-- subagent 子块（0014）：它的内部步骤 stdout 里没有，
                       来自 transcript。默认折叠，标题是派给它的任务 -->
                  <details v-if="e.subagent" class="subagent" :open="openSet.has(e.id)" @toggle="($event.target.open !== openSet.has(e.id)) && toggleOpen(e.id)">
                    <summary>
                      <span class="sub-tag">子 agent</span>
                      <span class="sub-title">{{ e.title || e.agentId }}</span>
                      <span class="faint sub-count">{{ subagentToolCount(e) }} 次工具调用</span>
                    </summary>
                    <ol v-if="openSet.has(e.id)" class="log sub-log">
                      <li
                        v-for="se in e.list"
                        :id="'ev-' + se.id"
                        :key="se.id"
                        class="ev"
                        :class="['ev-' + se.kind, severityOf(se) === 'error' ? 'ev-failed' : '', highlightId === se.id ? 'ev-flash' : '']"
                      >
                        <AgentEventItem :ev="se" :open="openSet" :toggle="toggleOpen" @full-log="openLogForEvent" />
                      </li>
                    </ol>
                  </details>

                  <AgentEventItem v-else :ev="e" :open="openSet" :toggle="toggleOpen" @full-log="openLogForEvent" />
                </div>
              </li>
              <li v-if="silenceLabel(e.gapMs)" class="silence faint">{{ silenceLabel(e.gapMs) }}</li>
            </template>
          </ol>
        </details>
      </template>
      <div v-else class="empty" style="padding: 24px">
        尚无执行日志。任务开始分诊后，这里会实时滚动 agent 的每一步。
      </div>
    </div>

    <!-- 验证区：按运行、再按轮次分块的红绿证明条；只读 detail.verifications -->
    <div id="verify-card" class="card">
      <div class="label">验证结果</div>
      <template v-if="runs.length">
        <!-- 只有一次运行时不显示运行这一层（component 换成 div，不留空 summary） -->
        <component
          :is="runs.length > 1 ? 'details' : 'div'"
          v-for="(run, ri) in runs"
          :key="run.run"
          class="run"
          :open="runs.length > 1 ? ri === 0 || openRuns.has(run.run) : undefined"
        >
          <summary v-if="runs.length > 1">
            <span class="phase-name">{{ runTitle(run) }}</span>
            <span v-if="run.boundary === 'order'" class="faint small">运行边界按顺序推断，可能把相邻两次运行并在一起</span>
          </summary>
          <div
            v-for="(round, k) in run.rounds"
            :key="round.round"
            class="round"
            :class="{ 'round-old': k > 0 }"
          >
            <div class="row round-head">
              <span class="round-name">{{ roundTitle(run, round) }}</span>
              <span class="faint small">{{ round.rows[0]?.tier }}</span>
              <span class="badge" :class="VERDICT[round.proof.verdict].tone">
                {{ VERDICT[round.proof.verdict].mark }} {{ VERDICT[round.proof.verdict].label }}
              </span>
            </div>
            <div class="proof">
              <template v-for="(st, i) in round.proof.steps" :key="st.id">
                <span v-if="i > 0" class="arrow faint">→</span>
                <span
                  class="step"
                  :class="['st-' + st.tone, { 'step-bad': st.status === 'failed' || st.status === 'error' }]"
                  :title="stepTitle(st)"
                >
                  {{ st.label }}
                  <span v-if="st.logOverwritten" class="faint small">（日志已覆盖）</span>
                  <button
                    v-else-if="st.logRef"
                    class="link-btn"
                    :data-log-ref="st.logRef"
                    @click="openLog(st)"
                  >完整日志</button>
                </span>
              </template>
            </div>
            <VerifyLogViewer
              v-if="viewing && round.rows.some((r) => r.id === viewing.id)"
              :task-id="props.id"
              :row="viewing"
              @close="viewing = null"
              @unauthorized="onUnauthorized"
            />
          </div>
        </component>
      </template>
      <div v-else class="empty" style="padding: 24px">
        尚无验证记录。任务走到验证阶段后这里会显示逐步结果。
      </div>
    </div>
  </template>
</template>

<style scoped>
.head { margin-bottom: 16px; flex-wrap: wrap; }
h1 { margin: 0; font-size: 22px; }

.label {
  font-size: 12.5px;
  color: var(--text-dim);
  margin-bottom: 10px;
  font-weight: 500;
}

.fail-card { border-color: var(--bad); margin-bottom: 16px; }
.gate-card { border-color: var(--warn); margin-bottom: 16px; }

.tip { margin: 10px 0 0; font-size: 13px; }

.retry-plan {
  margin-top: 12px;
  padding-top: 12px;
  border-top: 1px dashed var(--border, #444);
  font-size: 13px;
}
.retry-plan p { margin: 6px 0; }
.retry-plan .reasons { margin: 6px 0; padding-left: 18px; color: var(--text-dim); }
.retry-plan .reasons li { margin: 2px 0; }

.summary-card { margin-bottom: 16px; }
.badges { gap: 8px; flex-wrap: wrap; }

.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
  gap: 16px;
  margin-bottom: 16px;
}

dl {
  display: grid;
  grid-template-columns: 84px 1fr;
  gap: 7px 12px;
  margin: 0;
}
dt { color: var(--text-dim); font-size: 13px; }
dd { margin: 0; overflow-wrap: anywhere; }
.break { word-break: break-all; }

.timeline { list-style: none; margin: 0; padding: 0; }
.timeline li {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  padding: 7px 0;
  border-bottom: 1px solid var(--border);
  font-size: 13px;
}
.timeline li:last-child { border-bottom: none; }
.payload { width: 100%; font-size: 11.5px; }
@media (max-width: 600px) {
  .ev-time { width: 48px; }
}

/* ---------------------------------------------------------------- 执行日志 */

.card:has(.phase) { margin-bottom: 16px; }
.live { color: var(--run); font-size: 12.5px; }

.phase { border-top: 1px solid var(--border); padding: 4px 0; }
.phase:first-of-type { border-top: none; }
.phase > summary {
  cursor: pointer;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 0;
  list-style: none;
  user-select: none;
}
.phase > summary::-webkit-details-marker { display: none; }
.phase > summary::before {
  content: '▸';
  color: var(--text-faint);
  transition: transform .15s;
}
.phase[open] > summary::before { transform: rotate(90deg); }
.phase-name { font-weight: 500; }

.log { list-style: none; margin: 0; padding: 0 0 8px 18px; }
.ev { padding: 5px 0; border-bottom: 1px dashed var(--border); font-size: 13px; }
.ev:last-child { border-bottom: none; }

/* 回放：时间列 + 失败高亮 + 静默分隔线（docs/features/run-replay.md §3.3.2） */
.log > .ev { display: flex; gap: 10px; align-items: baseline; }
.ev-time { flex: none; width: 64px; color: var(--text-faint); font-size: 11.5px; text-align: right; }
.ev-body { flex: 1; min-width: 0; }
.ev-failed { box-shadow: inset 3px 0 0 var(--bad); background: var(--bad-bg); padding-left: 6px; }
.ev-warn { box-shadow: inset 3px 0 0 var(--warn); background: var(--warn-bg); padding-left: 6px; }
.ev-flash { animation: ev-flash 1.5s ease-out; }
@keyframes ev-flash { from { background: var(--run-bg); } to { background: transparent; } }
.silence { list-style: none; text-align: center; font-size: 11.5px; padding: 4px 0; border-bottom: 1px dashed var(--border); }

.toolbar {
  position: sticky;
  top: 0;
  z-index: 2;
  background: var(--surface);
  padding: 6px 0 8px;
  border-bottom: 1px solid var(--border);
  margin-bottom: 8px;
}
.chips, .tools { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.chips { margin-bottom: 6px; }
.chip { font-size: 12px; padding: 2px 10px; border-radius: 999px; }
.chip.on { border-color: var(--accent); color: var(--accent); }
.chip-bad { color: var(--bad); }
.tools label { font-size: 12.5px; display: inline-flex; gap: 4px; align-items: center; }
.tools select, .tools button { font-size: 12px; padding: 2px 8px; }
.fail-nav { display: inline-flex; gap: 4px; align-items: center; font-size: 12.5px; }
.window-info { font-size: 12.5px; margin-bottom: 6px; display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.window-info button { font-size: 12px; padding: 2px 8px; }
.bad-text { color: var(--bad); }
.small { font-size: 12px; }

/* 验证区：证明条 */
.run { border-top: 1px solid var(--border); padding: 4px 0; }
.run:first-of-type { border-top: none; }
.run > summary { cursor: pointer; padding: 8px 0; display: flex; gap: 10px; flex-wrap: wrap; align-items: baseline; }
.round { padding: 8px 0; }
.round-old { opacity: .85; }
.round-head { gap: 10px; margin-bottom: 6px; }
.round-name { font-weight: 500; font-size: 13px; }
.proof { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.arrow { font-size: 12px; }
.step {
  font-size: 12.5px;
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 2px 8px;
  display: inline-flex;
  gap: 6px;
  align-items: center;
}
.step.st-ok { color: var(--ok); }
.step.st-bad { color: var(--bad); }
.step.st-warn { color: var(--warn); }
.step.st-idle { color: var(--idle); }
.step-bad { border: 2px solid var(--bad); font-weight: 600; }
.link-btn { border: none; background: none; padding: 0; color: var(--accent); font-size: 12px; cursor: pointer; }

/* 单条事件的排版在 components/AgentEventItem.vue 里（作用域样式不跨组件，
   两边各管自己那部分）。这里只留容器与 subagent 子块。 */

/* subagent 子块（0014）：默认折叠，避免它的几十步把主时间线冲垮；
   左侧竖线表明这是一层缩进，而不是主 agent 的步骤 */
.subagent { margin: 2px 0; }
.subagent > summary {
  cursor: pointer;
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 3px 0;
  list-style: none;
  user-select: none;
  font-size: 12.5px;
}
.subagent > summary::-webkit-details-marker { display: none; }
.subagent > summary::before {
  content: '▸';
  color: var(--text-faint);
  flex: none;
  transition: transform .15s;
}
.subagent[open] > summary::before { transform: rotate(90deg); }

.sub-tag {
  flex: none;
  font-size: 11px;
  padding: 1px 6px;
  border: 1px solid var(--border);
  border-radius: 999px;
  color: var(--text-dim);
}
.sub-title {
  color: var(--text-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub-count { flex: none; margin-left: auto; font-size: 11.5px; white-space: nowrap; }

.sub-log {
  margin-left: 6px;
  padding-left: 12px;
  border-left: 2px solid var(--border);
}

/* markdown 正文的基本排版（全局没有 md 样式，作用域内自给自足） */
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) {
  margin: 12px 0 6px;
  font-size: 14.5px;
}
.md :deep(p) { margin: 6px 0; }
.md :deep(ul), .md :deep(ol) { margin: 6px 0; padding-left: 22px; }
.md :deep(code) {
  font-family: var(--mono);
  font-size: 12px;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 4px;
  padding: 1px 5px;
}
.md :deep(pre) { margin: 8px 0; }
.md :deep(pre code) { background: none; border: none; padding: 0; }
.md :deep(a) { word-break: break-all; }
</style>
