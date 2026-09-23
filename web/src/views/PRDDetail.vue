<script setup>
import { ref, computed, onMounted, onUnmounted, inject } from 'vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import {
  api,
  UnauthorizedError,
  formatTime,
  stateLabel,
  stateTone,
  prdStateLabel,
  prdStateTone,
  prdTypeLabel,
} from '../api'
import { confirmDialog } from '../confirm'

// 模糊任务详情页（docs/10-prd-template.md）：对话区把需求问清楚，文档区
// 逐节签字，对抗复核攻验收标准，自检过闸后批准、再一键生成正式任务。
//
// 界面上「能点什么」严格跟着 internal/prd/state.go 的转移表走 —— 不是为了
// 省一次注定失败的请求（服务端本来就会拒），而是让人一眼看出这份 PRD
// 现在卡在哪一步。

const props = defineProps({ id: { type: String, required: true } })
const onUnauthorized = inject('onUnauthorized')

const prd = ref(null)
const rounds = ref([])
const repos = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')
const acting = ref(false)

// 自检不过时后端回 422 带整份报告，逐条列出来并能跳到对应节
const checkReport = ref(null)
// 一键生成的结果：生成了哪些任务、进了哪张图
const convertResult = ref(null)

// marked 出 HTML 后必须过一层消毒：PRD 正文是模型写的，
// issue 内容可以借间接提示注入让它吐出带 onerror 的标记
// （与 TaskDetail.vue / IssueDetail.vue 同一条管线）。
marked.setOptions({ breaks: true })
const md = (s) => DOMPurify.sanitize(marked.parse(s || ''))

// ---------------------------------------------------------------- 常量表

// 节的顺序与标题跟 internal/prd/document.go 的 Document.Sections() 对齐，
// 两边任何一侧改了都要一起改 —— 自检报告按 Section key 跳节，对不上就跳不动。
const SECTIONS = [
  { key: 'oneLiner', title: '§1 一句话' },
  { key: 'problem', title: '§2 问题陈述' },
  { key: 'goals', title: '§3 目标与非目标' },
  { key: 'scenarios', title: '§4 用户场景 / 复现路径 / 行为清单' },
  { key: 'codeAnalysis', title: '§5 现状分析' },
  { key: 'solution', title: '§6 方案' },
  { key: 'acceptance', title: '§7 验收标准' },
  { key: 'taskSplit', title: '§8 任务拆分与依赖' },
  { key: 'risks', title: '§9 风险与未决' },
  { key: 'decisionLogSect', title: '§10 决策记录' },
]

const SECTION_STATUS = {
  confirmed: { mark: '✅', label: '已确认', tone: 'ok' },
  inferred: { mark: '🤖', label: '推断待确认', tone: 'warn' },
  pending: { mark: '❓', label: '待答', tone: 'bad' },
}

const STAGE_LABEL = {
  understand: 'A 理解',
  explore: 'B 探索',
  converge: 'C 收敛',
  split: 'D 拆分',
  finalize: 'E 定稿',
}

const AC_CATEGORY = {
  happy_path: '正常路径',
  failure_path: '失败路径',
  boundary: '边界',
  invariant: '不变量',
  non_functional: '非功能',
  observability: '可观测',
  compatibility: '兼容性',
  behavior_preserved: '行为保持',
  structural_metric: '结构度量',
}
const VERIFY_LABEL = { auto_test: '自动化测试', script: '脚本', manual: '人工走查' }
const QUESTION_LABEL = { open: '未答', answered: '已答', accepted_unanswered: '接受不答' }
const TASK_KIND_LABEL = { fix: '缺陷修复', feature: '功能', hotfix: '热修' }

// 对抗复核发现的两类（internal/prd/review.go）与三种处置。
const FINDING_KIND = {
  malicious_compliance: { label: '恶意合规', tone: 'bad' },
  uncovered_branch: { label: '分支未覆盖', tone: 'warn' },
}
const DISPOSITIONS = [
  { value: 'fix', label: '接受 · 补 AC' },
  { value: 'reject', label: '驳回' },
  { value: 'accept', label: '认可风险不改' },
]

// ---------------------------------------------------------------- 派生状态

const state = computed(() => prd.value?.state || '')
const processing = computed(() => !!prd.value?.processing)
const isTerminal = computed(() => ['converted', 'abandoned'].includes(state.value))
// approved 之后 PRD 冻结（D10-4）：内容一律不可编辑，只能生成任务
const frozen = computed(() => ['approved', 'converted'].includes(state.value))

// 下面这组开关就是状态机本身。processing 时一切写操作禁用 ——
// 有一轮 agent 正在跑，这时候点任何东西都会撞 store 的 CAS。
const canRound = computed(
  () => ['drafting', 'awaiting_answers'].includes(state.value) && !processing.value,
)
const canSubmit = computed(() => state.value === 'drafting' && !processing.value)
// 复核在 drafting 也开着：自检清单要求「复核报告已就位且每条都标了处置」
// 才放行提交，只在 ready_for_review 才能跑的话这道门永远推不开。
const canReview = computed(
  () => ['drafting', 'ready_for_review'].includes(state.value) && !processing.value,
)
const canApprove = computed(() => state.value === 'ready_for_review' && !processing.value)
const canReject = canApprove
const canAbandon = computed(
  () =>
    ['drafting', 'awaiting_answers', 'ready_for_review'].includes(state.value) &&
    !processing.value,
)
const canConvert = computed(() => state.value === 'approved' && !processing.value)
// 逐节确认只在起草期有意义：冻结后改不了，待回答时该先答问题
const canConfirmSection = computed(() => state.value === 'drafting' && !processing.value)

const doc = computed(() => prd.value?.document || {})
const tasks = computed(() => prd.value?.taskBlocks?.tasks || [])
const review = computed(() => prd.value?.reviewReport || null)
const findings = computed(() => review.value?.findings || [])
const repoName = computed(
  () => repos.value.find((r) => r.id === prd.value?.repoId)?.providerRepo || `#${prd.value?.repoId}`,
)

const sectionsView = computed(() =>
  SECTIONS.map((s) => ({ ...s, body: doc.value[s.key] || { status: '', body: '' } })),
)

// ---------------------------------------------------------------- 读取

async function load() {
  try {
    prd.value = await api.prd(props.id)
    seedDispositions()
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function loadRounds() {
  try {
    const r = await api.prdRounds(props.id)
    rounds.value = r.rounds || []
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function loadRepos() {
  try {
    const r = await api.repos()
    repos.value = r.repos || []
  } catch {
    // 仓库名只是显示用，拿不到就退回 repoId，不打断整页
  }
}

// 轮次历史只在轮数变了才重拉：5 秒一次的刷新是为了盯 processing，
// 不是为了把对话全文再搬一遍
let seenRound = -1
async function refresh() {
  await load()
  if (prd.value && prd.value.round !== seenRound) {
    seenRound = prd.value.round
    await loadRounds()
  }
}

// ---------------------------------------------------------------- 动作

// 统一的动作外壳：确认 → 置忙 → 跑 → 重读。与 TaskDetail.act 同一形状。
async function act(fn, confirmText) {
  if (confirmText && !(await confirmDialog(confirmText))) return
  acting.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await fn()
    await refresh()
    await loadRounds()
    return r
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
    throw e
  } finally {
    acting.value = false
  }
}

const answer = ref('')
const stageOverride = ref('')

async function runRound() {
  const body = {}
  const a = answer.value.trim()
  if (a) body.answer = a
  if (stageOverride.value) body.stage = stageOverride.value
  try {
    // 跑完一轮 state/round/document/task_blocks 可能全变，后端直接给一份新的
    const r = await act(() => api.runPrdRound(props.id, body))
    answer.value = ''
    stageOverride.value = ''
    if (r) {
      notice.value = r.questions?.length
        ? `第 ${r.prd.round} 轮跑完，智能体提了 ${r.questions.length} 个问题，回答后继续。`
        : `第 ${r.prd.round} 轮跑完。`
    }
  } catch {
    // act 已经把错误摆到页面上了
  }
}

async function runReview() {
  try {
    await act(() => api.reviewPrd(props.id))
    notice.value = '对抗复核已完成 —— 逐条标处置之后才能提交评审。'
  } catch {
    /* 同上 */
  }
}

async function submit() {
  acting.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.submitPrd(props.id)
    // 过了闸门也可能带警告（链长超限这类不阻塞但要人看见），有就留着显示
    checkReport.value = r.report?.warnings?.length ? r.report : null
    notice.value = '自检通过，已提交评审。'
    await refresh()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    // 422 带整份自检报告：把阻塞项逐条摆出来，比一句「自检未过」有用
    if (e.status === 422 && e.data?.report) {
      checkReport.value = e.data.report
      error.value = e.data.error || e.message
    } else {
      error.value = e.message
    }
  } finally {
    acting.value = false
  }
}

async function approve() {
  try {
    await act(
      () => api.approvePrd(props.id),
      '批准后这份 PRD 就冻结了：文档、任务块、复核报告都不能再改，只能一键生成任务。要改内容得开一份新 PRD 引用它。确认批准？',
    )
    notice.value = '已批准 —— PRD 已冻结，接下来一键生成正式任务。'
  } catch {
    /* 同上 */
  }
}

// 打回不带理由：理由的归宿是下一轮对话的回答。prd_rounds 是 append-only 的
// 轮次记录，硬塞一条「打回」会让轮次编号跟真实对话对不上。
async function reject() {
  try {
    await act(
      () => api.rejectPrd(props.id),
      '打回后这份 PRD 回到起草，可以接着对话修订。确认打回？',
    )
    notice.value =
      '已打回起草 —— 要改什么请到上方对话区写进回答里，再跑一轮让规划智能体据此修订。'
  } catch {
    /* 同上 */
  }
}

async function abandon() {
  try {
    await act(
      () => api.abandonPrd(props.id),
      '放弃是终态：这份 PRD 之后既不能继续对话也不能生成任务，要接着做只能开新的。确认放弃？',
    )
    notice.value = '已放弃。'
  } catch {
    /* 同上 */
  }
}

async function convert() {
  try {
    const r = await act(
      () => api.convertPrd(props.id),
      '确认按 §8 的任务块生成正式任务与编排图？生成之后这份 PRD 进终态。',
    )
    if (r) {
      convertResult.value = r
      notice.value = `已生成 ${r.tasks?.length || 0} 个任务。`
    }
  } catch {
    /* 同上 */
  }
}

// 逐节确认：把 🤖 改成 ✅。❓ 的节后端回 409 —— 挂着未决问题不许跳过去
// 确认，否则就绕开了 §9 的清零机制，所以按钮那边也不给点。
async function confirmSection(key) {
  try {
    await act(() => api.confirmPrdSection(props.id, key))
    notice.value = '本节已确认。'
  } catch {
    /* 同上 */
  }
}

// ---------------------------------------------------------------- 复核处置

// 处置是人的责任，复核 agent 刻意不填（internal/prd/service.go RunReview）。
// 这里存本地编辑态，保存之后才落库；轮询刷新不覆盖没保存的输入。
//
// 发现没有 id：复核报告是整体覆盖写的，下标在一份报告内稳定，所以下标
// 既是本地编辑态的 key，也是保存时发给后端的定位方式。
const dispEdits = ref({})
const savingDisp = ref(false)

function seedDispositions() {
  const next = {}
  findings.value.forEach((f, i) => {
    next[i] = dispEdits.value[i] || {
      disposition: f.disposition || '',
      note: f.dispositionNote || '',
    }
  })
  dispEdits.value = next
}

const unhandledCount = computed(
  () => findings.value.filter((f, i) => !dispEdits.value[i]?.disposition).length,
)
// 「认可风险不改」必须写理由：不写理由的风险接受，等于没人为它负责。
// 后端同样会拦（400），这里拦一道是为了不让人白跑一趟请求。
const acceptNeedsNote = computed(() =>
  findings.value.some((f, i) => {
    const d = dispEdits.value[i]
    return d?.disposition === 'accept' && !d?.note?.trim()
  }),
)

async function saveDispositions() {
  savingDisp.value = true
  error.value = ''
  notice.value = ''
  try {
    const payload = findings.value.map((f, i) => ({
      index: i,
      disposition: dispEdits.value[i]?.disposition || '',
      dispositionNote: dispEdits.value[i]?.note || '',
    }))
    const row = await api.savePrdDispositions(props.id, payload)
    prd.value = row
    seedDispositions()
    notice.value = '处置已保存。'
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    savingDisp.value = false
  }
}

// ---------------------------------------------------------------- 跳节

const highlighted = ref('')
let highlightTimer = null

function gotoSection(key) {
  if (!key) return
  const el = document.getElementById(`sec-${key}`)
  if (!el) return
  el.scrollIntoView({ behavior: 'smooth', block: 'start' })
  highlighted.value = key
  clearTimeout(highlightTimer)
  highlightTimer = setTimeout(() => (highlighted.value = ''), 2400)
}

// ---------------------------------------------------------------- 规划事件流
// 与执行日志同一套增量轮询（after=lastId）。

const events = ref([])
const eventsError = ref('')
const lastId = ref(0)
let timer = null
let evTimer = null

// phase 分两路且刻意分开显示：plan 是作者（规划智能体）说的，plan-review 是
// 攻击者（对抗复核）说的。混在一条流里，人就分不清哪句话是来攻它的。
const PLAN_PHASE = 'plan'
const REVIEW_PHASE = 'plan-review'

const planEvents = computed(() => events.value.filter((e) => e.phase !== REVIEW_PHASE))
const reviewEvents = computed(() => events.value.filter((e) => e.phase === REVIEW_PHASE))

async function loadEvents() {
  try {
    const resp = await api.prdEvents(props.id, lastId.value)
    if (resp.events?.length) events.value.push(...resp.events)
    if (resp.lastId != null) lastId.value = resp.lastId
    eventsError.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    eventsError.value = e.message
  }
}

// ---------------------------------------------------------------- 轮次历史

// 待回答时，最后一轮的问题就是现在挂着的那些
const openQuestions = computed(() => {
  if (state.value !== 'awaiting_answers') return []
  const last = rounds.value[rounds.value.length - 1]
  return last?.questions || []
})

onMounted(() => {
  loadRepos()
  refresh()
  loadEvents()
  // 轮次在后台跑，页面要能跟着动；终态之后没有任何东西会再变
  timer = setInterval(() => {
    if (!isTerminal.value) refresh()
  }, 5000)
  evTimer = setInterval(async () => {
    await loadEvents()
    if (isTerminal.value) clearInterval(evTimer)
  }, 2000)
})
onUnmounted(() => {
  clearInterval(timer)
  clearInterval(evTimer)
  clearTimeout(highlightTimer)
})
</script>

<template>
  <div v-if="loading" class="card empty">加载中…</div>
  <div v-else-if="!prd" class="card empty">{{ error || '找不到这份 PRD。' }}</div>

  <template v-else>
    <!-- §0 元信息 + 动作 -->
    <div class="spread head">
      <div>
        <div class="row" style="gap: 10px; flex-wrap: wrap">
          <h1 class="mono">模糊任务 #{{ prd.id }}</h1>
          <span class="badge" :class="prdStateTone(prd.state)">{{ prdStateLabel(prd.state) }}</span>
          <span class="badge idle">{{ prdTypeLabel(prd.prdType) }}</span>
          <span v-if="processing" class="badge run">● 智能体进行中</span>
        </div>
        <div class="faint mono">
          {{ repoName }} · 第 {{ prd.round }} 轮 · 创建于 {{ formatTime(prd.createdAt) }}
          <template v-if="prd.refPrdId">
            · 接续 <RouterLink :to="`/prds/${prd.refPrdId}`">#{{ prd.refPrdId }}</RouterLink>
          </template>
        </div>
      </div>
      <div class="wrap">
        <RouterLink to="/prds" class="dim" style="align-self: center">← 返回列表</RouterLink>
        <button
          v-if="['drafting', 'awaiting_answers'].includes(state)"
          :disabled="!canRound || acting"
          @click="runRound"
        >
          {{ state === 'awaiting_answers' ? '提交回答并跑下一轮' : '跑下一轮' }}
        </button>
        <button
          v-if="['drafting', 'ready_for_review'].includes(state)"
          :disabled="!canReview || acting"
          title="对抗复核：让一个只看意图与验收标准的智能体来攻这组 AC —— 它是 AC 唯一的红阶段，不跑就进不了评审"
          @click="runReview"
        >跑对抗复核</button>
        <!-- 待回答时这个按钮刻意留着但禁用：还挂着未答问题的 PRD 不许进评审，
             这条规则要在界面上看得见，而不是点了才被服务端拒绝 -->
        <button
          v-if="['drafting', 'awaiting_answers'].includes(state)"
          class="primary"
          :disabled="!canSubmit || acting"
          :title="state === 'awaiting_answers'
            ? '还挂着未回答的问题：先答完跑一轮回到起草，才能提交评审'
            : '跑自检清单；全过才进待批准'"
          @click="submit"
        >提交评审</button>
        <template v-if="state === 'ready_for_review'">
          <button class="primary" :disabled="!canApprove || acting" @click="approve">批准</button>
          <button :disabled="!canReject || acting" @click="reject">打回起草</button>
        </template>
        <button
          v-if="state === 'approved'"
          class="primary"
          :disabled="!canConvert || acting"
          @click="convert"
        >一键生成任务</button>
        <button
          v-if="['drafting', 'awaiting_answers', 'ready_for_review'].includes(state)"
          class="danger"
          :disabled="!canAbandon || acting"
          @click="abandon"
        >放弃</button>
        <!-- 导出（docs/10 §7）：任何状态都能导，走普通链接让浏览器自己下 ——
             same-origin cookie 自动带上，不必绕 fetch 再造 Blob -->
        <a class="btn-link" :href="`/api/prds/${prd.id}/export?format=md`" download>导出 Markdown</a>
        <a class="btn-link" :href="`/api/prds/${prd.id}/export?format=json`" download>导出 JSON</a>
      </div>
    </div>

    <p class="faint export-hint">
      导出的 Markdown 可以回写进目标仓库的 <code class="mono">docs/</code> ——
      但 Lathe 不会自动提交、更不会 push，要不要入库是人的决定。
    </p>

    <div v-if="error" role="alert" class="error-banner">{{ error }}</div>
    <div v-if="notice" role="status" class="notice-banner">{{ notice }}</div>

    <div v-if="processing" class="card run-card">
      <div class="label">智能体正在跑这一轮</div>
      <p class="dim" style="margin: 0">
        期间所有写操作都禁用 —— 同一份 PRD 同时只能有一轮在跑。
        下方「规划事件流」在实时滚动；跑完页面自动跟进。
      </p>
    </div>

    <div v-if="state === 'approved'" class="card frozen-card">
      <div class="label">已批准 · 内容冻结</div>
      <p class="dim" style="margin: 0">
        签过字的 PRD 不可编辑（既不能回到起草，也不能放弃）。漏了要改的，
        开一份新 PRD 引用这份。接下来只剩「一键生成任务」。
      </p>
    </div>

    <!-- 自检报告：阻塞项逐条可点跳到对应节 -->
    <div v-if="checkReport" class="card fail-card">
      <div class="spread">
        <div class="label">自检未过 —— 这些拦着不让进评审</div>
        <button @click="checkReport = null">收起</button>
      </div>
      <ul class="findings">
        <li v-for="(f, i) in checkReport.blocking || []" :key="'b' + i">
          <span class="badge bad">阻塞</span>
          <span class="mono faint">{{ f.code }}</span>
          <span>{{ f.message }}</span>
          <button v-if="f.section" class="link-btn" @click="gotoSection(f.section)">跳到该节</button>
        </li>
        <li v-for="(f, i) in checkReport.warnings || []" :key="'w' + i">
          <span class="badge warn">提醒</span>
          <span class="mono faint">{{ f.code }}</span>
          <span>{{ f.message }}</span>
          <button v-if="f.section" class="link-btn" @click="gotoSection(f.section)">跳到该节</button>
        </li>
      </ul>
    </div>

    <!-- 一键生成的结果 -->
    <div v-if="convertResult" class="card ok-card">
      <div class="label">已生成正式任务</div>
      <p style="margin: 0 0 8px">
        编排图
        <RouterLink :to="`/flows/${convertResult.flowId}`">#{{ convertResult.flowId }}</RouterLink>
        —— 依赖关系已按 §8 连好，后继会等前驱合并后自动入队。
      </p>
      <table>
        <thead><tr><th>任务</th><th>Issue</th><th>状态</th></tr></thead>
        <tbody>
          <tr v-for="t in convertResult.tasks || []" :key="t.id">
            <td><RouterLink :to="`/tasks/${t.id}`" class="mono">#{{ t.id }}</RouterLink></td>
            <td class="mono dim">{{ t.issueKey }}</td>
            <td><span class="badge" :class="stateTone(t.state)">{{ stateLabel(t.state) }}</span></td>
          </tr>
        </tbody>
      </table>
      <ul v-if="convertResult.warnings?.length" class="plain" style="margin-top: 10px">
        <li v-for="(w, i) in convertResult.warnings" :key="i" class="warn-line">⚠ {{ w }}</li>
      </ul>
    </div>

    <!-- 原始输入：那句说不清的话，留着对照最后的 §1 -->
    <div class="card" style="margin-bottom: 16px">
      <div class="label">原始输入</div>
      <p class="original">{{ prd.originalInput }}</p>
    </div>

    <!-- 对话区 -->
    <div class="card" style="margin-bottom: 16px">
      <div class="label">对话（{{ rounds.length }} 轮）</div>

      <div v-if="!rounds.length" class="dim" style="padding: 4px 0 10px">
        还没跑过任何一轮。点右上角「跑下一轮」，智能体会先复述它对这件事的理解、
        判类型，并提不超过三个关键问题。
      </div>

      <div v-for="r in rounds" :key="r.id" class="round">
        <div class="round-head">
          <span class="badge idle">第 {{ r.round }} 轮</span>
          <span class="badge run">{{ STAGE_LABEL[r.stage] || r.stage }}</span>
          <span class="faint">{{ formatTime(r.createdAt) }}</span>
        </div>
        <div v-if="r.userInput" class="bubble human">
          <div class="bubble-tag">你的回答</div>
          <div class="md" v-html="md(r.userInput)"></div>
        </div>
        <div v-if="r.questions?.length" class="bubble agent">
          <div class="bubble-tag">智能体的问题</div>
          <ol class="qlist">
            <li v-for="(q, i) in r.questions" :key="i">{{ q }}</li>
          </ol>
        </div>
        <div v-if="r.notes" class="bubble agent">
          <div class="bubble-tag">本轮摘要</div>
          <div class="md" v-html="md(r.notes)"></div>
        </div>
      </div>

      <!-- 回答框：待回答时是必经之路（awaiting_answers 不能直接提交评审） -->
      <div v-if="canRound" class="answer-box">
        <div v-if="openQuestions.length" class="pending">
          <div class="label" style="margin-bottom: 6px">等你回答</div>
          <ol class="qlist">
            <li v-for="(q, i) in openQuestions" :key="i">{{ q }}</li>
          </ol>
        </div>
        <label class="field">
          <span>
            {{ state === 'awaiting_answers'
              ? '你的回答（逐条答；答不了的写「读代码定」也行）'
              : '补充说明（可留空，直接让智能体接着推进）' }}
          </span>
          <textarea v-model="answer" rows="4" placeholder="markdown"></textarea>
        </label>
        <div class="row" style="justify-content: flex-end; gap: 8px">
          <label class="row" style="gap: 6px">
            <span class="dim">阶段</span>
            <select v-model="stageOverride">
              <option value="">自动推进</option>
              <option v-for="(v, k) in STAGE_LABEL" :key="k" :value="k">{{ v }}</option>
            </select>
          </label>
          <button
            class="primary"
            :disabled="acting || (state === 'awaiting_answers' && !answer.trim())"
            @click="runRound"
          >
            {{ acting ? '跑着…' : state === 'awaiting_answers' ? '提交回答并跑下一轮' : '跑下一轮' }}
          </button>
        </div>
      </div>
      <p v-else-if="processing" class="dim" style="margin: 10px 0 0">这一轮还在跑，跑完才能继续。</p>
      <p v-else-if="frozen" class="dim" style="margin: 10px 0 0">PRD 已冻结，对话到此为止。</p>
    </div>

    <!-- 文档区 -->
    <div class="doc-head spread">
      <h3 style="margin: 0">PRD 正文</h3>
      <span class="faint">
        ✅ 已确认 · 🤖 推断待人过目 · ❓ 挂着未决问题 —— 有 ❓ 或 🤖 都进不了评审
      </span>
    </div>

    <div
      v-for="s in sectionsView"
      :key="s.key"
      :id="`sec-${s.key}`"
      class="card section"
      :class="{ flash: highlighted === s.key }"
    >
      <div class="spread sec-head">
        <div class="row" style="gap: 8px">
          <strong>{{ s.title }}</strong>
          <span v-if="SECTION_STATUS[s.body.status]" class="badge" :class="SECTION_STATUS[s.body.status].tone">
            {{ SECTION_STATUS[s.body.status].mark }} {{ SECTION_STATUS[s.body.status].label }}
          </span>
          <span v-else class="badge idle">未开始</span>
        </div>
        <button
          v-if="['inferred', 'pending'].includes(s.body.status) && canConfirmSection"
          :disabled="acting || s.body.status === 'pending'"
          :title="s.body.status === 'pending'
            ? '这一节还挂着未决问题（❓）：先在对话区把问题答掉，才能确认 —— 否则就绕开了 §9 的清零'
            : '逐节确认：看过这一节的内容，把 🤖 改成 ✅'"
          @click="confirmSection(s.key)"
        >确认本节</button>
      </div>

      <div v-if="s.body.body" class="md" v-html="md(s.body.body)"></div>
      <p v-else class="faint" style="margin: 6px 0 0">（本节还没有内容）</p>

      <ul v-if="s.body.questions?.length" class="sec-questions">
        <li v-for="(q, i) in s.body.questions" :key="i">❓ {{ q }}</li>
      </ul>

      <!-- §3：目标必须带达成判据，非目标必须带理由 -->
      <template v-if="s.key === 'goals'">
        <table v-if="doc.goalList?.length" class="sub">
          <thead><tr><th>编号</th><th>目标</th><th>怎么知道达成了</th></tr></thead>
          <tbody>
            <tr v-for="g in doc.goalList" :key="g.id">
              <td class="mono">{{ g.id }}</td><td>{{ g.text }}</td><td class="dim">{{ g.measure }}</td>
            </tr>
          </tbody>
        </table>
        <ul v-if="doc.nonGoals?.length" class="plain">
          <li v-for="(n, i) in doc.nonGoals" :key="i">
            <span class="badge idle">非目标</span> {{ n.text }}
            <span class="faint">（{{ n.reason }}）</span>
          </li>
        </ul>
      </template>

      <!-- §4：三种变体共用 Given/When/Then 形状 -->
      <table v-if="s.key === 'scenarios' && doc.scenarioList?.length" class="sub">
        <thead><tr><th>编号</th><th>场景</th><th>Given / When / Then</th><th>现有测试</th></tr></thead>
        <tbody>
          <tr v-for="sc in doc.scenarioList" :key="sc.id">
            <td class="mono">{{ sc.id }}</td>
            <td>{{ sc.title }}</td>
            <td class="dim">
              <div>Given {{ sc.given }}</div>
              <div>When {{ sc.when }}</div>
              <div>Then {{ sc.then }}</div>
            </td>
            <td class="mono faint">{{ sc.existingTest || '—' }}</td>
          </tr>
        </tbody>
      </table>

      <!-- §7：AC 表 + 系统附加的上线条件 -->
      <template v-if="s.key === 'acceptance'">
        <table v-if="doc.criteria?.length" class="sub">
          <thead>
            <tr><th>编号</th><th>类别</th><th>标准</th><th>判据</th><th>验证方式</th><th>回指</th></tr>
          </thead>
          <tbody>
            <tr v-for="c in doc.criteria" :key="c.id">
              <td class="mono">{{ c.id }}</td>
              <td class="dim">{{ AC_CATEGORY[c.category] || c.category }}</td>
              <td>
                <template v-if="c.na">
                  <span class="badge idle">N/A</span> <span class="dim">{{ c.reason }}</span>
                </template>
                <template v-else>
                  <div class="dim">Given {{ c.given }}</div>
                  <div class="dim">When {{ c.when }}</div>
                  <div>Then {{ c.then }}</div>
                  <div v-if="c.reverseConfirm" class="faint">
                    反向确认：{{ c.reverseConfirm }}
                    <span class="badge" :class="c.reverseAccepted == null ? 'bad' : 'ok'">
                      {{ c.reverseAccepted == null ? '未答复' : c.reverseAccepted ? '接受' : '不接受' }}
                    </span>
                  </div>
                </template>
              </td>
              <td class="dim">{{ c.evidence || '—' }}</td>
              <td class="dim">
                {{ VERIFY_LABEL[c.verify] || c.verify || '—' }}
                <div v-if="c.manualSteps" class="faint">{{ c.manualSteps }}</div>
              </td>
              <td class="mono faint">
                {{ [...(c.goalRefs || []), ...(c.scenarioRefs || [])].join(' ') || '—' }}
                <div v-if="c.taskKey">→ {{ c.taskKey }}</div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="doc.dod?.length" class="dod">
          <div class="label" style="margin-bottom: 4px">上线条件（系统附加，智能体不编写）</div>
          <ul class="plain"><li v-for="(d, i) in doc.dod" :key="i">{{ d }}</li></ul>
        </div>
      </template>

      <!-- §9：未决问题 Q-n -->
      <table v-if="s.key === 'risks' && doc.questions?.length" class="sub">
        <thead><tr><th>编号</th><th>问题</th><th>归属节</th><th>状态</th><th>答复</th></tr></thead>
        <tbody>
          <tr v-for="q in doc.questions" :key="q.id">
            <td class="mono">{{ q.id }}</td>
            <td>{{ q.text }}</td>
            <td class="faint">{{ q.section || '—' }}</td>
            <td>
              <span class="badge" :class="q.status === 'open' ? 'bad' : 'ok'">
                {{ QUESTION_LABEL[q.status] || q.status }}
              </span>
            </td>
            <td class="dim">{{ q.answer || '—' }}</td>
          </tr>
        </tbody>
      </table>

      <!-- §10：决策记录 -->
      <table v-if="s.key === 'decisionLogSect' && doc.decisions?.length" class="sub">
        <thead><tr><th>轮次</th><th>问的是什么</th><th>定了什么</th><th>为什么</th></tr></thead>
        <tbody>
          <tr v-for="(d, i) in doc.decisions" :key="i">
            <td class="dim">第 {{ d.round }} 轮</td>
            <td>{{ d.question }}</td>
            <td>{{ d.decision }}</td>
            <td class="dim">{{ d.reason || '—' }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 附录 B：代码证据索引。「感觉应该做」不是证据 -->
    <div v-if="doc.evidences?.length" class="card section">
      <strong>附录 B · 代码证据</strong>
      <ul class="plain">
        <li v-for="(e, i) in doc.evidences" :key="i">
          <code class="mono">{{ e.ref }}</code>
          <span v-if="e.note" class="dim"> —— {{ e.note }}</span>
        </li>
      </ul>
    </div>

    <!-- 任务块区 -->
    <div class="card section">
      <div class="spread">
        <strong>§8 任务块（一键生成的图节点）</strong>
        <span v-if="tasks.length" class="faint">{{ tasks.length }} 个任务</span>
      </div>
      <div v-if="!tasks.length" class="faint" style="margin-top: 6px">
        还没拆分。走到 D 阶段智能体才会产出任务表。
      </div>
      <table v-else class="sub">
        <thead>
          <tr><th>编号</th><th>标题</th><th>类型</th><th>依赖</th><th>估算</th><th>验证</th><th>交付 AC</th></tr>
        </thead>
        <tbody>
          <template v-for="t in tasks" :key="t.key">
            <tr>
              <td class="mono">{{ t.key }}</td>
              <td>
                {{ t.title }}
                <details v-if="t.description" class="task-desc">
                  <summary class="faint">展开描述</summary>
                  <div class="md" v-html="md(t.description)"></div>
                  <p v-if="t.filesHint?.length" class="faint mono" style="margin: 4px 0 0">
                    参考文件：{{ t.filesHint.join('、') }}
                  </p>
                </details>
              </td>
              <td class="dim">{{ TASK_KIND_LABEL[t.kind] || t.kind }}</td>
              <td class="mono dim">{{ t.dependsOn || '根节点' }}</td>
              <td class="dim">{{ t.estimate?.lines ?? '—' }} 行 / {{ t.estimate?.files ?? '—' }} 文件</td>
              <td class="dim">{{ t.verifyHint || '—' }}</td>
              <td class="mono faint">{{ (t.acceptance || []).join(' ') || '—' }}</td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <!-- 对抗复核区 -->
    <div class="card section">
      <div class="spread">
        <strong>对抗复核 —— 验收标准自己的「红阶段」</strong>
        <span v-if="review && findings.length" class="badge" :class="unhandledCount ? 'warn' : 'ok'">
          {{ unhandledCount ? `${unhandledCount} 条待处置` : '全部已处置' }}
        </span>
      </div>

      <p v-if="!review" class="faint" style="margin: 6px 0 0">
        还没跑过。一组任何实现都能过的验收标准，无权声称覆盖了意图 ——
        定稿前必须有人试着攻它（点右上角「跑对抗复核」）。
      </p>

      <template v-else>
        <p v-if="review.verdict" class="verdict">{{ review.verdict }}</p>
        <p v-if="!findings.length" class="dim" style="margin: 6px 0 0">
          复核没找出洞 —— 「攻不动」本身是个有价值的结论。
        </p>

        <div v-for="(f, i) in findings" :key="i" class="finding">
          <div class="row" style="gap: 8px; flex-wrap: wrap">
            <span class="badge" :class="FINDING_KIND[f.kind]?.tone || 'warn'">
              {{ FINDING_KIND[f.kind]?.label || f.kind }}
            </span>
            <strong>{{ f.title }}</strong>
          </div>
          <div v-if="f.detail" class="md" v-html="md(f.detail)"></div>
          <div v-if="f.acRefs?.length || f.scenarioRefs?.length" class="faint mono">
            关联：{{ [...(f.acRefs || []), ...(f.scenarioRefs || [])].join(' ') }}
          </div>

          <div v-if="dispEdits[i]" class="disp">
            <span class="dim">处置</span>
            <label v-for="d in DISPOSITIONS" :key="d.value" class="radio">
              <input
                type="radio"
                :name="`disp-${i}`"
                :value="d.value"
                :disabled="frozen || processing"
                v-model="dispEdits[i].disposition"
              />
              <span>{{ d.label }}</span>
            </label>
          </div>
          <input
            v-if="dispEdits[i]?.disposition"
            v-model="dispEdits[i].note"
            class="disp-note"
            :disabled="frozen || processing"
            :placeholder="dispEdits[i].disposition === 'accept'
              ? '认可风险必须写理由：为什么这个洞可以不补'
              : '理由（可选）'"
          />
        </div>

        <div v-if="findings.length && !frozen" class="row" style="justify-content: flex-end; margin-top: 10px">
          <span v-if="acceptNeedsNote" class="faint" style="margin-right: auto">
            「认可风险不改」必须写理由才能保存。
          </span>
          <button
            class="primary"
            :disabled="savingDisp || processing || acceptNeedsNote"
            @click="saveDispositions"
          >{{ savingDisp ? '保存中…' : '保存处置' }}</button>
        </div>

        <details v-if="review.rawText" class="raw">
          <summary class="faint">复核原文（附录 C 原样留档）</summary>
          <pre>{{ review.rawText }}</pre>
        </details>
      </template>
    </div>

    <!-- 事件流：规划与对抗复核分开摆。前者是作者说的，后者是攻击者说的，
         混在一条流里人就分不清哪句话是来攻它的 -->
    <div class="card section">
      <div class="spread">
        <strong>规划事件流</strong>
        <span v-if="processing" class="faint live">● 实时</span>
      </div>
      <div v-if="eventsError" class="faint" style="margin-top: 6px">日志拉取失败：{{ eventsError }}</div>
      <ol v-if="planEvents.length" class="log">
        <li v-for="e in planEvents" :key="e.id" class="ev">
          <span v-if="e.round != null" class="badge idle">第 {{ e.round }} 轮</span>
          <span class="mono faint">{{ e.kind }}{{ e.tool ? ` · ${e.tool}` : '' }}</span>
          <span class="ev-text">{{ e.body }}</span>
          <span class="faint">{{ formatTime(e.at) }}</span>
        </li>
      </ol>
      <div v-else class="faint" style="margin-top: 6px">
        还没有事件。跑一轮之后，智能体读了哪些文件、想了什么都会滚在这里。
      </div>
    </div>

    <div v-if="reviewEvents.length" class="card section">
      <strong>对抗复核事件流</strong>
      <p class="faint" style="margin: 4px 0 0">
        复核 agent 只看 §1–§4 与 §7，看不到现状分析与方案 —— 它要攻的是标准本身。
      </p>
      <ol class="log">
        <li v-for="e in reviewEvents" :key="e.id" class="ev">
          <span class="mono faint">{{ e.kind }}{{ e.tool ? ` · ${e.tool}` : '' }}</span>
          <span class="ev-text">{{ e.body }}</span>
          <span class="faint">{{ formatTime(e.at) }}</span>
        </li>
      </ol>
    </div>

  </template>
</template>

<style scoped>
.head { margin-bottom: 16px; flex-wrap: wrap; align-items: flex-start; }
h1 { margin: 0; font-size: 22px; }

.label { font-size: 12.5px; color: var(--text-dim); margin-bottom: 10px; font-weight: 500; }

/* 导出是链接（浏览器直接下载）而不是按钮，但站在按钮行里就得长得一样 */
.btn-link {
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--text);
  padding: 6px 14px;
  border-radius: var(--radius);
  font-size: 14px;
  line-height: 1.6;
  transition: border-color .15s;
}
.btn-link:hover { border-color: var(--accent); text-decoration: none; }

.export-hint { margin: -6px 0 16px; font-size: 12.5px; }

.notice-banner {
  background: var(--ok-bg);
  color: var(--ok);
  border: 1px solid var(--ok);
  border-radius: var(--radius);
  padding: 10px 14px;
  margin-bottom: 16px;
}

.run-card { border-color: var(--run); margin-bottom: 16px; }
.frozen-card { border-color: var(--ok); margin-bottom: 16px; }
.fail-card { border-color: var(--bad); margin-bottom: 16px; }
.ok-card { border-color: var(--ok); margin-bottom: 16px; }

.findings { list-style: none; margin: 0; padding: 0; }
.findings li {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 6px 0;
  border-bottom: 1px dashed var(--border);
  font-size: 13px;
}
.findings li:last-child { border-bottom: none; }
.link-btn {
  border: none;
  background: none;
  color: var(--accent);
  padding: 0 2px;
  font-size: 13px;
}
.link-btn:hover { text-decoration: underline; }

.original { margin: 0; white-space: pre-wrap; }

/* ---------------------------------------------------------------- 对话区 */

.round { border-top: 1px solid var(--border); padding: 10px 0; }
.round:first-of-type { border-top: none; }
.round-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 6px; }

.bubble { border-radius: 6px; padding: 8px 10px; margin: 6px 0; }
.bubble.agent { background: var(--run-bg); }
.bubble.human { background: var(--surface-2); }
.bubble-tag { font-size: 12px; color: var(--text-dim); margin-bottom: 2px; }

.qlist { margin: 4px 0; padding-left: 20px; }
.qlist li { margin: 3px 0; }

.answer-box { border-top: 1px solid var(--border); padding-top: 10px; margin-top: 10px; }
.answer-box textarea { width: 100%; box-sizing: border-box; }
.pending {
  border: 1px solid var(--warn);
  background: var(--warn-bg);
  border-radius: var(--radius);
  padding: 8px 10px;
  margin-bottom: 10px;
}

/* ---------------------------------------------------------------- 文档区 */

.doc-head { margin: 24px 0 10px; flex-wrap: wrap; gap: 8px; }
.section { margin-bottom: 12px; }
.sec-head { margin-bottom: 8px; flex-wrap: wrap; gap: 8px; }

/* 从自检报告跳过来时闪一下：不闪的话人不知道停在了哪一节 */
.section.flash { border-color: var(--accent); box-shadow: 0 0 0 2px var(--run-bg); }

.sec-questions { margin: 8px 0 0; padding-left: 20px; color: var(--warn); font-size: 13px; }

table.sub { margin-top: 10px; font-size: 13px; }
table.sub td { vertical-align: top; }

.plain { margin: 8px 0 0; padding-left: 20px; }
.plain li { margin: 3px 0; }
.warn-line { color: var(--warn); }

.dod { margin-top: 10px; padding-top: 8px; border-top: 1px dashed var(--border); }

.task-desc { margin-top: 4px; }
.task-desc > summary { cursor: pointer; font-size: 12px; }

/* ---------------------------------------------------------------- 复核区 */

.verdict { margin: 8px 0; padding: 8px 10px; background: var(--surface-2); border-radius: 6px; }

.finding { border-top: 1px solid var(--border); padding: 10px 0; }
.disp { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-top: 6px; font-size: 13px; }
.radio { display: inline-flex; align-items: center; gap: 4px; cursor: pointer; }
.disp-note { width: 100%; box-sizing: border-box; margin-top: 6px; }

.raw { margin-top: 12px; }
.raw > summary { cursor: pointer; font-size: 12.5px; }
.raw pre { margin-top: 6px; max-height: 320px; overflow-y: auto; }

/* ---------------------------------------------------------------- 事件流 */

.live { color: var(--run); font-size: 12.5px; }
.log { list-style: none; margin: 10px 0 0; padding: 0; }
.ev {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
  padding: 5px 0;
  border-bottom: 1px dashed var(--border);
  font-size: 13px;
}
.ev:last-child { border-bottom: none; }
.ev-text { flex: 1; min-width: 200px; overflow-wrap: anywhere; }

/* markdown 正文排版（与 TaskDetail / IssueDetail 同一套，作用域内自给自足） */
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) { margin: 12px 0 6px; font-size: 14.5px; }
.md :deep(p) { margin: 6px 0; }
.md :deep(ul), .md :deep(ol) { margin: 6px 0; padding-left: 22px; }
.md :deep(table) { width: 100%; border-collapse: collapse; margin: 8px 0; }
.md :deep(th), .md :deep(td) { border: 1px solid var(--border); padding: 6px 8px; text-align: left; }
.md :deep(code) {
  font-family: var(--mono); font-size: 12px;
  background: var(--bg); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 5px;
}
.md :deep(pre) { margin: 8px 0; }
.md :deep(pre code) { background: none; border: none; padding: 0; }
.md :deep(a) { word-break: break-all; }
</style>
