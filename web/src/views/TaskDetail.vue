<script setup>
import { ref, onMounted, onUnmounted, computed, inject } from 'vue'
import { useRouter } from 'vue-router'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime, formatDuration } from '../api'
import AgentEventItem from '../components/AgentEventItem.vue'

const props = defineProps({ id: { type: String, required: true } })
const router = useRouter()
const onUnauthorized = inject('onUnauthorized')

const detail = ref(null)
const error = ref('')
const acting = ref(false)
const retryPlan = ref(null)
let timer = null

const task = computed(() => detail.value?.task)
const isTerminal = computed(() =>
  ['merged', 'failed', 'cancelled'].includes(task.value?.state)
)

// 智能重试的按钮文案：预览决策直接写在按钮上，重试不是黑盒
const retryCta = computed(() => {
  const p = retryPlan.value
  if (!p) return '重试'
  if (p.fresh) return '重试 · 从头重跑'
  return `重试 · 从「${p.entryLabel}」续跑`
})

async function load() {
  try {
    detail.value = await api.task(props.id)
    error.value = ''
    // 失败任务拉取重试预览：体检现场 + 决策理由
    if (detail.value.task.state === 'failed') {
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
  if (confirmText && !confirm(confirmText)) return
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
// docs/04 §3.4：2s 增量轮询（after=last_id），进入终态后再拉一次收尾即停。

const events = ref([])
const eventsError = ref('')
const lastId = ref(0)
let evTimer = null

async function loadEvents() {
  try {
    const resp = await api.taskEvents(props.id, lastId.value)
    if (resp.events?.length) events.value.push(...resp.events)
    if (resp.last_id != null) lastId.value = resp.last_id
    eventsError.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    eventsError.value = e.message
  }
}

// 模型输出不可信：issue 内容可以借间接提示注入让模型吐出带 onerror 的
// 标记，marked 出 HTML 后必须过一层消毒。
marked.setOptions({ breaks: true })
const md = (s) => DOMPurify.sanitize(marked.parse(s || ''))

const PHASE_META = {
  triage: { label: '分诊', order: 0 },
  implement: { label: '实现', order: 1 },
  verify: { label: '验证', order: 2 },
  review: { label: '评审', order: 3 },
  push: { label: '推送', order: 4 },
}

// subagent 的事件（0014）收成一个可折叠子块，插在它首次出现的位置。
//
// 为什么是「兄弟块」而不是真的挂在那条 Agent 调用下面：agentId 与父调用
// toolUseId 的对应关系只存在于 transcript 的 toolUseResult 字段里，stdout
// 这条通道给不出。与其按时间顺序猜一个父子关系，不如老实地并排放 —— 子块
// 标题就是派给它的任务描述，与父调用那行显示的 description 是同一句话，
// 人一眼能对上。（真要连线，得再读父会话的 transcript，见 docs/04 §7.4。）
function groupSubagents(list) {
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
function pairTools(list) {
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
const TOOL_DURATION_FLOOR_MS = 1000

function toolDuration(useAt, resultAt) {
  const dt = new Date(resultAt) - new Date(useAt)
  if (!Number.isFinite(dt) || dt < TOOL_DURATION_FLOOR_MS) return null
  return dt
}

const phaseGroups = computed(() => {
  const groups = new Map()
  for (const e of events.value) {
    if (!groups.has(e.phase)) groups.set(e.phase, [])
    groups.get(e.phase).push(e)
  }
  return [...groups.entries()]
    .map(([phase, list]) => ({
      phase,
      // 先把 subagent 收成子块，再对主流与每个子块分别做工具配对
      list: pairTools(groupSubagents(list)).map((x) =>
        x.subagent ? { ...x, list: pairTools(x.list) } : x
      ),
      meta: PHASE_META[phase] || { label: phase, order: 9 },
    }))
    .sort((a, b) => a.meta.order - b.meta.order)
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

const STEP_LABEL = {
  build: '构建',
  lint: '静态检查',
  typecheck: '类型检查',
  repro_fail: '改动前复现（应失败）',
  repro_pass: '改动后复现（应通过）',
  regression: '回归测试',
}
const STATUS_META = {
  passed: { mark: '✓', tone: 'ok', label: '通过' },
  failed: { mark: '✗', tone: 'bad', label: '未通过' },
  error: { mark: '!', tone: 'warn', label: '未跑起来' },
  skipped: { mark: '–', tone: 'idle', label: '跳过' },
}

onMounted(() => {
  load()
  loadEvents()
  timer = setInterval(() => { if (!isTerminal.value) load() }, 5000)
  evTimer = setInterval(async () => {
    await loadEvents()
    // 终态后这一轮就是收尾：终态转移前 sink 已 drain，事件不会再多
    if (isTerminal.value) clearInterval(evTimer)
  }, 2000)
})
onUnmounted(() => {
  clearInterval(timer)
  clearInterval(evTimer)
})
</script>

<template>
  <div v-if="error" class="error-banner">{{ error }}</div>
  <div v-if="!detail" class="empty">加载中…</div>

  <template v-else>
    <div class="spread head">
      <div>
        <div class="row">
          <h1 class="mono">{{ task.linearIssueKey }}</h1>
          <span class="badge" :class="stateTone(task.state)">{{ stateLabel(task.state) }}</span>
        </div>
        <div class="faint mono">{{ task.providerRepo }} · 任务 #{{ task.id }}</div>
      </div>
      <div class="wrap">
        <button @click="router.push('/')">返回</button>
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
        <button
          v-if="!isTerminal"
          class="danger"
          :disabled="acting"
          @click="act(api.cancel, '确认取消这个任务？')"
        >取消</button>
      </div>
    </div>

    <div v-if="task.failureReason" class="card fail-card">
      <div class="label">失败原因</div>
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
        <span v-if="!isTerminal" class="faint live">● 实时</span>
      </div>
      <div v-if="eventsError" class="faint" style="margin-bottom: 8px">日志拉取失败：{{ eventsError }}</div>

      <template v-if="orderedGroups.length">
        <details
          v-for="g in orderedGroups"
          :key="g.phase"
          class="phase"
          :open="g.phase === activePhase"
        >
          <summary>
            <span class="phase-name">{{ g.meta.label }}</span>
            <span class="faint">{{ g.list.length }} 条</span>
            <span v-if="g.phase === activePhase && !isTerminal" class="badge run">进行中</span>
          </summary>

          <ol class="log">
            <li v-for="e in g.list" :key="e.id" class="ev" :class="e.subagent ? 'ev-sub' : 'ev-' + e.kind">
              <!-- subagent 子块（0014）：它的内部步骤 stdout 里没有，
                   来自 transcript。默认折叠，标题是派给它的任务 -->
              <details v-if="e.subagent" class="subagent">
                <summary>
                  <span class="sub-tag">子 agent</span>
                  <span class="sub-title">{{ e.title || e.agentId }}</span>
                  <span class="faint sub-count">{{ subagentToolCount(e) }} 次工具调用</span>
                </summary>
                <ol class="log sub-log">
                  <li v-for="se in e.list" :key="se.id" class="ev" :class="'ev-' + se.kind">
                    <AgentEventItem :ev="se" />
                  </li>
                </ol>
              </details>

              <AgentEventItem v-else :ev="e" />
            </li>
          </ol>
        </details>
      </template>
      <div v-else class="empty" style="padding: 24px">
        尚无执行日志。任务开始分诊后，这里会实时滚动 agent 的每一步。
      </div>
    </div>

    <div class="card">
      <div class="label">验证结果</div>
      <table v-if="detail.verifications.length">
        <thead>
          <tr><th>步骤</th><th>结果</th><th>档位</th><th>耗时</th><th>时间</th></tr>
        </thead>
        <tbody>
          <tr v-for="v in detail.verifications" :key="v.id">
            <td>{{ STEP_LABEL[v.step] || v.step }}</td>
            <td>
              <span class="badge" :class="STATUS_META[v.status]?.tone">
                {{ STATUS_META[v.status]?.mark }} {{ STATUS_META[v.status]?.label || v.status }}
              </span>
            </td>
            <td class="dim">{{ v.tier }}</td>
            <td class="dim">{{ formatDuration(v.durationMs) }}</td>
            <td class="dim">{{ formatTime(v.at) }}</td>
          </tr>
        </tbody>
      </table>
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
