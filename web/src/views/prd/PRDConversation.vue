<script setup>
import { ref, computed, watch, nextTick, onMounted, onUnmounted, inject } from 'vue'
import {
  api,
  UnauthorizedError,
  formatTime,
  stateLabel,
  stateTone,
  prdStateLabel,
  prdStateTone,
  prdTypeLabel,
} from '../../api'
import { confirmDialog } from '../../confirm'
import { appendCapped, createMarkdown, createPoller, MAX_LIVE_EVENTS } from '../../livelog'
import {
  STAGE_LABEL,
  SECTION_TITLE,
  REVIEW_PHASE,
  prdCaps,
  prdTitle,
  changedSections,
} from '../../prd'
import ChatComposer from '../../components/prd/ChatComposer.vue'
import ThinkingBlock from '../../components/prd/ThinkingBlock.vue'
import PRDDocPanel from '../../components/prd/PRDDocPanel.vue'

// 模糊任务会话页（docs/10-prd-template.md），豆包 / DeepSeek 式的对话形态：
//
//   中间是对话流 —— 你的话在右，智能体在左；每条智能体消息上方挂一个可
//   折叠的「思考过程」（该轮的规划事件流），下方是它的问题卡片。
//   右边是 PRD 文档面板 —— 文档逐节成形，逐节确认、复核处置都在那里签。
//   底部是输入框 + 情境化的流程动作（复核 / 提交评审 / 批准 / 生成任务）。
//
// 对话里的「一条消息」就是后端的一轮（prd_rounds）：发送 = 跑一轮。
// 状态机一条没改：能点什么严格跟着 internal/prd/state.go 的转移表走（见
// prd.js 的 prdCaps），自检闸门、逐节确认、对抗复核一个都没少。

const props = defineProps({ id: { type: String, required: true } })
const onUnauthorized = inject('onUnauthorized')
const sidebar = inject('prdSidebar', null)

const prd = ref(null)
const rounds = ref([])
const repos = ref([])
const loading = ref(true)
const loadError = ref('')
const error = ref('')
const notice = ref('')
const acting = ref(false)
const reviewing = ref(false)
const checkReport = ref(null)
const convertResult = ref(null)

// marked 出 HTML 后必须过 DOMPurify：PRD 正文是模型写的，issue 内容可以借
// 间接提示注入让它吐出带 onerror 的标记。带缓存 —— 输入框每敲一个字整页
// 重渲染，不缓存的话轮次一多输入就卡。
const md = createMarkdown()

const caps = computed(() => prdCaps(prd.value))
const state = computed(() => prd.value?.state || '')
const processing = computed(() => !!prd.value?.processing)
const repoName = computed(
  () => repos.value.find((r) => r.id === prd.value?.repoId)?.providerRepo || `#${prd.value?.repoId}`,
)

// ---------------------------------------------------------------- 读取

async function load() {
  try {
    prd.value = await api.prd(props.id)
    loadError.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    loadError.value = e.message
  } finally {
    loading.value = false
  }
}

async function loadRounds() {
  try {
    const r = await api.prdRounds(props.id)
    rounds.value = (r.rounds || []).map((x) => ({
      ...x,
      // 快照是 RawMessage，round 接口原样透出；解析一次缓存在行上
      snapshot: typeof x.documentSnapshot === 'string' ? safeParse(x.documentSnapshot) : x.documentSnapshot,
    }))
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

function safeParse(s) {
  try {
    return JSON.parse(s)
  } catch {
    return null
  }
}

// 轮次历史只在轮数变了才重拉：5 秒一次的刷新是为了盯 processing，
// 不是为了把对话全文再搬一遍
let seenRound = -1
let seenState = ''
async function refresh() {
  await load()
  if (!prd.value) return
  if (prd.value.document?.oneLiner?.body) sidebar?.setTitle?.(prd.value.id, prdTitle(prd.value))
  if (prd.value.round !== seenRound) {
    seenRound = prd.value.round
    await loadRounds()
  }
  // 状态变了（比如别处跑完一轮、被扫描器判搁置）侧栏也要跟
  if (prd.value.state !== seenState) {
    if (seenState) sidebar?.refresh()
    seenState = prd.value.state
  }
}

// ---------------------------------------------------------------- 事件流
// 与执行日志同一套增量轮询（after=lastId），内存只留最近 MAX_LIVE_EVENTS 条。

const events = ref([])
const lastId = ref(0)
const droppedEvents = ref(0)

const EVENT_PAGE = 200
async function loadEvents() {
  try {
    const resp = await api.prdEvents(props.id, lastId.value, EVENT_PAGE)
    droppedEvents.value += appendCapped(events.value, resp.events)
    if (resp.lastId != null) lastId.value = resp.lastId
    return resp.events?.length || 0
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    // 事件流只是过程展示，拉失败不打断对话
    return 0
  }
}

// 首屏一口气追平：不然长会话要每 2 秒一页慢慢追，追的过程中早期轮次的
// 思考块被裁掉又补上，「用时」数字来回跳。设上限防止事件极多时卡住首屏。
async function drainEvents() {
  for (let i = 0; i < 50; i++) {
    if ((await loadEvents()) < EVENT_PAGE) break
  }
}

const eventsByRound = computed(() => {
  const m = {}
  for (const e of events.value) {
    if (e.phase === REVIEW_PHASE || e.round == null) continue
    ;(m[e.round] ||= []).push(e)
  }
  return m
})
const reviewEvents = computed(() => lastAttempt(events.value.filter((e) => e.phase === REVIEW_PHASE)))

// 同一轮号可能跑过不止一次：解析失败的那次不推进轮次，事件却已落库，人重试
// 时沿用同一个轮号。每次 agent 运行都以一条 init 事件开头，按最后一条 init
// 切出「最后一次尝试」，失败尝试的过程不混进成功那轮的思考块。
function lastAttempt(list) {
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].kind === 'init') return list.slice(i)
  }
  return list
}
function roundEvents(n) {
  return lastAttempt(eventsByRound.value[n] || [])
}

// ---------------------------------------------------------------- 对话流

const input = ref('')
const stageOverride = ref('')
const sending = ref(false)
// 发出去、后端还没落轮次的那句话：先乐观地摆在对话里
const pendingInput = ref(null)
const sendBaseRound = ref(0)
const sendError = ref('')
const lastSent = ref('')
// 本页发起的一轮：只认发送之后落库的事件，计时从按下发送算起
const sendFloor = ref(0)
const sendStartedAt = ref(null)

const latestRound = computed(() => rounds.value.at(-1)?.round || 0)

// 「智能体在跑」有两个来源：本页刚发出去的请求，或者打开页面时别处已经在跑
// （processing）。前者以「新一轮还没落库」为准，避免请求回来前的那一下闪烁。
const running = computed(
  () => (sending.value && latestRound.value <= sendBaseRound.value) || (!sending.value && processing.value),
)
const liveRound = computed(() => (sending.value ? sendBaseRound.value : prd.value?.round || 0) + 1)
const liveEvents = computed(() => {
  const list = eventsByRound.value[liveRound.value] || []
  return lastAttempt(sending.value ? list.filter((e) => e.id > sendFloor.value) : list)
})
const liveStartedAt = computed(() =>
  sending.value ? sendStartedAt.value : prd.value?.processingStartedAt || null,
)

const timeline = computed(() => {
  const items = []
  if (!prd.value) return items
  items.push({ kind: 'user', key: 'origin', text: prd.value.originalInput, origin: true })
  let prev = null
  rounds.value.forEach((r, i) => {
    if (r.userInput) items.push({ kind: 'user', key: `u${r.id}`, text: r.userInput, at: r.createdAt })
    else if (i > 0) items.push({ kind: 'divider', key: `d${r.id}`, text: '让智能体继续推进' })
    const isLast = i === rounds.value.length - 1
    items.push({
      kind: 'agent',
      key: `a${r.id}`,
      round: r,
      events: roundEvents(r.round),
      changed: changedSections(prev, r.snapshot),
      // 待回答时，最后一轮的问题就是现在挂着的那些
      openQuestions: isLast && state.value === 'awaiting_answers',
    })
    prev = r.snapshot || prev
  })
  return items
})

function quote(q) {
  input.value = (input.value.trim() ? input.value.replace(/\s*$/, '\n\n') : '') + `> ${q}\n`
  nextTick(() => composer.value?.focusEnd())
}

function answerTemplate(qs) {
  input.value = qs.map((q, i) => `${i + 1}. `).join('\n')
  nextTick(() => composer.value?.focusEnd())
}

// ---------------------------------------------------------------- 发送 = 跑一轮

const composer = ref(null)

const canSend = computed(() => {
  if (!caps.value.round || sending.value) return false
  // 待回答时必须有内容：awaiting_answers 不能空跑过去
  return state.value !== 'awaiting_answers' || !!input.value.trim()
})

async function send() {
  if (!caps.value.round || sending.value) return
  const text = input.value.trim()
  if (state.value === 'awaiting_answers' && !text) return

  const body = {}
  if (text) body.answer = text
  if (stageOverride.value) body.stage = stageOverride.value

  sending.value = true
  sendError.value = ''
  error.value = ''
  notice.value = ''
  pendingInput.value = text
  lastSent.value = text
  sendBaseRound.value = latestRound.value
  sendFloor.value = lastId.value
  sendStartedAt.value = new Date().toISOString()
  input.value = ''
  scrollToBottom(true)
  sidebar?.refresh()

  try {
    // 跑完一轮 state/round/document/task_blocks 可能全变，后端直接给一份新的
    const r = await api.runPrdRound(props.id, body)
    if (r?.prd) prd.value = r.prd
    seenRound = -1
    await refresh()
    stageOverride.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    // 解析失败等情况这一轮等于没发生（后端不推进轮次）：把话放回输入框，
    // 一键重试，不让人重打一遍
    sendError.value = e.message
    if (!input.value.trim()) input.value = text
    await refresh()
  } finally {
    pendingInput.value = null
    sending.value = false
    sidebar?.refresh()
  }
}

// ---------------------------------------------------------------- 流程动作

// 统一外壳：确认 → 置忙 → 跑 → 重读。与 TaskDetail.act 同一形状。
async function act(fn, confirmText) {
  if (confirmText && !(await confirmDialog(confirmText))) return undefined
  acting.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await fn()
    seenRound = -1
    await refresh()
    sidebar?.refresh()
    return r
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
    throw e
  } finally {
    acting.value = false
  }
}

async function runReview() {
  reviewing.value = true
  openPanel('review')
  try {
    const r = await act(() => api.reviewPrd(props.id))
    const n = r?.findings?.length || 0
    notice.value = n
      ? `对抗复核找到 ${n} 处可疑 —— 在右侧「复核」逐条标处置之后才能提交评审。`
      : '对抗复核没找出洞 —— 「攻不动」本身是个有价值的结论。'
  } catch {
    /* act 已经把错误摆出来了 */
  } finally {
    reviewing.value = false
  }
}

async function submit() {
  acting.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.submitPrd(props.id)
    // 过了闸门也可能带警告（链长超限这类不阻塞但要人看见）
    checkReport.value = r.report?.warnings?.length ? r.report : null
    notice.value = '自检通过，已提交评审。'
    await refresh()
    sidebar?.refresh()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    // 422 带整份自检报告：阻塞项逐条摆到文档面板，可点跳节
    if (e.status === 422 && e.data?.report) {
      checkReport.value = e.data.report
      error.value = (e.data.error || e.message) + ' —— 阻塞项已列在右侧文档顶部。'
      openPanel('doc')
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
    await act(() => api.rejectPrd(props.id), '打回后这份 PRD 回到起草，可以接着对话修订。确认打回？')
    notice.value = '已打回起草 —— 把要改的写进下方输入框发出去，智能体会据此修订。'
    nextTick(() => composer.value?.focusEnd())
  } catch {
    /* 同上 */
  }
}

async function abandon() {
  menuOpen.value = false
  try {
    await act(
      () => api.abandonPrd(props.id),
      '放弃后这份 PRD 停止推进，也不会再自动流转（放久了会被判定为搁置）。' +
        '它没丢：随时可以点「继续这份 PRD」回来接着对话。确认放弃？',
    )
    notice.value = '已放弃 —— 想接着做点「继续这份 PRD」即可回来。'
  } catch {
    /* 同上 */
  }
}

// 唤醒搁置/放弃的 PRD（docs/10 §9）。终态的含义是「不再自动流转」，不是
// 死路 —— 轮次历史、已确认的节与复核报告都还在，唤醒只改状态。
async function resume() {
  try {
    await act(() => api.resumePrd(props.id))
    notice.value = '已恢复起草 —— 在下方写点什么就能接着聊。'
    nextTick(() => composer.value?.focusEnd())
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

// 逐节确认：🤖 → ✅。❓ 的节后端回 409 —— 挂着未决问题不许跳过去确认，
// 否则就绕开了 §9 的清零机制，所以面板那边按钮也不给点。
async function confirmSection(key) {
  try {
    await act(() => api.confirmPrdSection(props.id, key))
    notice.value = `${SECTION_TITLE[key] || '本节'} 已确认。`
  } catch {
    /* 同上 */
  }
}

async function saveDispositions(payload) {
  acting.value = true
  error.value = ''
  notice.value = ''
  try {
    prd.value = await api.savePrdDispositions(props.id, payload)
    notice.value = '处置已保存。'
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    acting.value = false
  }
}

// 复核待处置条数：决定「提交评审」旁边要不要提示先去处置
const unhandled = computed(
  () => (prd.value?.reviewReport?.findings || []).filter((f) => !f.disposition).length,
)
const hasReview = computed(() => !!prd.value?.reviewReport)
const pendingSections = computed(() => {
  const d = prd.value?.document || {}
  return Object.values(d).filter((s) => s && typeof s === 'object' && ['inferred', 'pending'].includes(s.status)).length
})

// ---------------------------------------------------------------- 进度条

const STEPS = [
  ...Object.entries(STAGE_LABEL).map(([k, v]) => ({ key: k, label: v })),
  { key: 'review', label: '评审' },
  { key: 'approved', label: '批准' },
  { key: 'converted', label: '生成任务' },
]
const stepIndex = computed(() => {
  const s = state.value
  if (s === 'converted') return STEPS.length
  if (s === 'approved') return STEPS.length - 1
  if (s === 'ready_for_review') return STEPS.length - 3
  const stage = rounds.value.at(-1)?.stage
  const i = STEPS.findIndex((x) => x.key === stage)
  return i < 0 ? 0 : i
})

// ---------------------------------------------------------------- 面板

const wide = () => window.matchMedia('(min-width: 1200px)').matches
const PANEL_KEY = 'lathe.prd.panel'
function readPanelPref() {
  try {
    const v = localStorage.getItem(PANEL_KEY)
    return v == null ? null : v === '1'
  } catch {
    return null
  }
}
const panelOpen = ref(false)
const panelTab = ref('doc')
const panel = ref(null)

function setPanel(open) {
  panelOpen.value = open
  // 只在宽屏记偏好：窄屏面板是浮层，记下来会让宽屏也跟着关
  if (wide()) {
    try {
      localStorage.setItem(PANEL_KEY, open ? '1' : '0')
    } catch {
      /* 存不了就算了，只是个便利 */
    }
  }
}
function openPanel(tab) {
  if (tab) panelTab.value = tab
  panelOpen.value = true
}
function gotoSection(key) {
  openPanel('doc')
  nextTick(() => panel.value?.gotoSection(key))
}

// ---------------------------------------------------------------- 菜单

const menuOpen = ref(false)
function onDocClick(e) {
  if (!e.target.closest?.('.menu-wrap')) menuOpen.value = false
}

// ---------------------------------------------------------------- 滚动

const scroller = ref(null)
function nearBottom() {
  const el = scroller.value
  return !el || el.scrollHeight - el.scrollTop - el.clientHeight < 120
}
async function scrollToBottom(force = false) {
  const stick = force || nearBottom()
  await nextTick()
  if (stick && scroller.value) scroller.value.scrollTop = scroller.value.scrollHeight
}
watch(
  () => [timeline.value.length, running.value, liveEvents.value.length],
  () => scrollToBottom(),
)

// ---------------------------------------------------------------- 生命周期

let refreshPoller = null
let evPoller = null

onMounted(async () => {
  document.addEventListener('click', onDocClick)
  api
    .repos()
    .then((r) => (repos.value = r.repos || []))
    .catch(() => {
      /* 仓库名只是显示用，拿不到就退回 repoId */
    })

  await refresh()
  await drainEvents()
  if (!prd.value) return

  const pref = readPanelPref()
  panelOpen.value = wide() && (pref ?? rounds.value.length > 0)
  scrollToBottom(true)

  // 新建页带来的 autorun：发送即跑第一轮。读完即清，刷新页面不会重跑。
  const hs = window.history.state || {}
  if (hs.autorun) {
    window.history.replaceState({ ...hs, autorun: false }, '')
    if (!rounds.value.length && state.value === 'drafting' && !processing.value) send()
  }

  // 自链式轮询：上一轮没回来不发下一轮。converted 是彻底结束；abandoned
  // 仍可能被别处改（扫描器判搁置后人在别的标签页唤醒），所以继续盯。
  refreshPoller = createPoller(async () => {
    if (state.value !== 'converted') await refresh()
  }, 5000)
  evPoller = createPoller(async () => {
    await loadEvents()
    return state.value !== 'converted'
  }, 2000)
  refreshPoller.start()
  evPoller.start()
})
onUnmounted(() => {
  document.removeEventListener('click', onDocClick)
  refreshPoller?.stop()
  evPoller?.stop()
})

// ---------------------------------------------------------------- 输入框文案

const composerPlaceholder = computed(() => {
  if (running.value) return '智能体正在思考，这一轮跑完才能继续…'
  switch (state.value) {
    case 'awaiting_answers':
      return '回答上面的问题（逐条答；答不了的写「读代码定」也行）'
    case 'drafting':
      return '补充说明、纠正理解，或直接发送让智能体继续推进'
    case 'ready_for_review':
      return '评审中 —— 要改内容先「打回起草」'
    case 'approved':
    case 'converted':
      return 'PRD 已冻结，对话到此为止'
    case 'abandoned':
      return '已搁置 —— 点「继续这份 PRD」回来接着聊'
    default:
      return ''
  }
})
const sendLabel = computed(() =>
  state.value === 'drafting' && !input.value.trim() ? '继续推进' : '发送',
)
</script>

<template>
  <div v-if="loading" class="center faint">加载中…</div>
  <div v-else-if="!prd" class="center faint">{{ loadError || '找不到这段对话。' }}</div>

  <div v-else class="conv" :class="{ 'with-panel': panelOpen }">
    <section class="chat">
      <!-- 顶栏：标题 + 状态 + 进度 + 面板开关 -->
      <header class="top">
        <div class="title-row">
          <h1 :title="prdTitle(prd)">{{ prdTitle(prd) }}</h1>
          <span class="badge" :class="prdStateTone(prd.state)">{{ prdStateLabel(prd.state) }}</span>
          <div class="top-actions">
            <button class="ghost" :class="{ on: panelOpen }" :aria-pressed="panelOpen"
              @click="setPanel(!panelOpen)">
              📄 PRD 文档<span v-if="pendingSections" class="count">{{ pendingSections }}</span>
            </button>
            <div class="menu-wrap">
              <button class="ghost icon" aria-label="更多操作" :aria-expanded="menuOpen"
                @click="menuOpen = !menuOpen">⋯</button>
              <div v-if="menuOpen" class="menu" role="menu">
                <!-- 导出（docs/10 §7）：任何状态都能导，走普通链接让浏览器自己下 -->
                <a role="menuitem" :href="`/api/prds/${prd.id}/export?format=md`" download>导出 Markdown</a>
                <a role="menuitem" :href="`/api/prds/${prd.id}/export?format=json`" download>导出 JSON</a>
                <p class="menu-note faint">导出的 Markdown 可回写进目标仓库 docs/ —— Lathe 不会自动提交或 push。</p>
                <button v-if="caps.abandon" role="menuitem" class="danger-item" :disabled="acting"
                  @click="abandon">放弃这份 PRD</button>
              </div>
            </div>
          </div>
        </div>
        <div class="meta faint">
          <span class="mono">#{{ prd.id }}</span>
          <span>{{ prdTypeLabel(prd.prdType) }}</span>
          <span class="mono">{{ repoName }}</span>
          <span>第 {{ prd.round }} 轮</span>
          <span v-if="prd.refPrdId">接续 <RouterLink :to="`/prds/${prd.refPrdId}`">#{{ prd.refPrdId }}</RouterLink></span>
        </div>
        <ol class="steps" aria-label="进度">
          <li v-for="(s, i) in STEPS" :key="s.key"
            :class="{ done: i < stepIndex, cur: i === stepIndex && state !== 'abandoned' }">
            <span class="sd"></span>{{ s.label }}
          </li>
        </ol>
      </header>

      <!-- 对话流 -->
      <div ref="scroller" class="scroll">
        <div class="thread">
          <div v-if="droppedEvents" class="faint tiny center-text">
            思考过程只保留最近 {{ MAX_LIVE_EVENTS }} 条事件，更早轮次的过程可能不完整（完整记录在库里）。
          </div>

          <template v-for="m in timeline" :key="m.key">
            <!-- 你 -->
            <div v-if="m.kind === 'user'" class="msg user">
              <div class="bubble">
                <div v-if="m.origin" class="origin-tag faint">
                  {{ prdTypeLabel(prd.prdType) }} · 原始需求（不可编辑，是一切推断的根）
                </div>
                <div class="md" v-html="md(m.text)"></div>
              </div>
            </div>

            <div v-else-if="m.kind === 'divider'" class="divider faint"><span>{{ m.text }}</span></div>

            <!-- 智能体 -->
            <div v-else class="msg agent">
              <div class="avatar" aria-hidden="true">L</div>
              <div class="body">
                <div class="who faint">
                  规划智能体 · 第 {{ m.round.round }} 轮 · {{ STAGE_LABEL[m.round.stage] || m.round.stage }}
                  <span class="time">{{ formatTime(m.round.createdAt) }}</span>
                </div>
                <ThinkingBlock :events="m.events" :md="md" />
                <div v-if="m.round.notes" class="md" v-html="md(m.round.notes)"></div>

                <div v-if="m.round.questions?.length" class="questions" :class="{ open: m.openQuestions }">
                  <div class="q-head">
                    <span>{{ m.openQuestions ? '需要你回答' : '本轮提问' }}</span>
                    <button v-if="m.openQuestions && caps.round && !input.trim()" class="link"
                      @click="answerTemplate(m.round.questions)">按序号逐条作答</button>
                  </div>
                  <button
                    v-for="(q, i) in m.round.questions"
                    :key="i"
                    class="q"
                    :disabled="!caps.round"
                    :title="caps.round ? '点击引用到输入框' : ''"
                    @click="quote(q)"
                  >
                    <span class="qn">{{ i + 1 }}</span>
                    <span>{{ q }}</span>
                  </button>
                </div>

                <div v-if="m.changed.length" class="changed">
                  <span class="faint">更新了</span>
                  <button v-for="k in m.changed" :key="k" class="chip" @click="gotoSection(k)">
                    {{ SECTION_TITLE[k] }}
                  </button>
                </div>
              </div>
            </div>
          </template>

          <!-- 发出去还没落库的那句话 -->
          <div v-if="pendingInput" class="msg user pending">
            <div class="bubble"><div class="md" v-html="md(pendingInput)"></div></div>
          </div>

          <!-- 正在跑的这一轮 -->
          <div v-if="running" class="msg agent">
            <div class="avatar" aria-hidden="true">L</div>
            <div class="body">
              <div class="who faint">规划智能体 · 第 {{ liveRound }} 轮</div>
              <ThinkingBlock
                :events="liveEvents"
                :running="true"
                :started-at="liveStartedAt"
                :md="md"
              />
            </div>
          </div>

          <!-- 这一轮没跑成 -->
          <div v-if="sendError && !running" class="msg agent">
            <div class="avatar err" aria-hidden="true">!</div>
            <div class="body">
              <div class="fail">
                <strong>这一轮没跑成</strong>
                <div class="dim">{{ sendError }}</div>
                <div class="faint tiny">轮次没有推进，你的话已放回输入框。</div>
                <button v-if="caps.round" class="retry" @click="send">重试</button>
              </div>
            </div>
          </div>

          <!-- 终局卡片：跟着状态走 -->
          <div v-if="state === 'ready_for_review'" class="sys">
            <strong>已提交评审</strong> —— 自检清单全过。看一遍右侧 PRD，没问题就批准；
            要改就打回起草，接着对话修订。
          </div>
          <div v-else-if="state === 'approved'" class="sys ok">
            <strong>已批准 · 内容冻结</strong> —— 签过字的 PRD 不可编辑。漏了要改的，开一份新 PRD 接续这份。
            接下来只剩「一键生成任务」。
          </div>
          <div v-else-if="state === 'converted'" class="sys ok">
            <strong>已生成正式任务</strong>
            <template v-if="convertResult?.flowId || prd.generatedFlowId">
              —— 编排图
              <RouterLink :to="`/flows/${convertResult?.flowId || prd.generatedFlowId}`">
                #{{ convertResult?.flowId || prd.generatedFlowId }}
              </RouterLink>，依赖已按 §8 连好，后继会等前驱合并后自动入队。
            </template>
            <ul v-if="convertResult?.tasks?.length" class="tasks">
              <li v-for="t in convertResult.tasks" :key="t.id">
                <RouterLink :to="`/tasks/${t.id}`" class="mono">#{{ t.id }}</RouterLink>
                <span class="mono faint">{{ t.issueKey }}</span>
                <span class="badge" :class="stateTone(t.state)">{{ stateLabel(t.state) }}</span>
              </li>
            </ul>
            <ul v-if="convertResult?.warnings?.length" class="tasks">
              <li v-for="(w, i) in convertResult.warnings" :key="i" class="warn-text">⚠ {{ w }}</li>
            </ul>
          </div>
          <div v-else-if="state === 'abandoned'" class="sys">
            <strong>这份 PRD 已停止推进</strong>（人放弃，或长时间无人回答被搁置）。
            对话历史与已确认的节都还在，点下方「继续这份 PRD」就能接着聊。
          </div>
        </div>
      </div>

      <!-- 底部：提示 + 流程动作 + 输入框 -->
      <footer class="foot">
        <div class="foot-inner">
          <div v-if="error" role="alert" class="toast bad">
            <span>{{ error }}</span>
            <button class="x" aria-label="关闭" @click="error = ''">✕</button>
          </div>
          <div v-else-if="notice" role="status" class="toast ok">
            <span>{{ notice }}</span>
            <button class="x" aria-label="关闭" @click="notice = ''">✕</button>
          </div>

          <!-- 有一轮在跑时整排动作都禁着（撞 store 的 CAS），淡化以示不可用，
               但按钮仍在：人要知道跑完之后能点什么 -->
          <div class="actions" :class="{ muted: processing || sending }">
            <template v-if="state === 'drafting' || state === 'awaiting_answers'">
              <button class="act" :disabled="!caps.review || acting || sending"
                title="让一个只看意图与验收标准的智能体来攻这组 AC —— 不跑就进不了评审"
                @click="runReview">
                {{ reviewing ? '对抗复核中…' : hasReview ? '重跑对抗复核' : '跑对抗复核' }}
              </button>
              <button v-if="hasReview && unhandled" class="act warn" @click="openPanel('review')">
                {{ unhandled }} 条复核待处置
              </button>
              <button v-if="pendingSections" class="act warn" @click="openPanel('doc')">
                {{ pendingSections }} 节待确认
              </button>
              <!-- 待回答时刻意留着但禁用：挂着未答问题的 PRD 不许进评审，
                   这条规则要在界面上看得见，而不是点了才被服务端拒绝 -->
              <button class="act primary" :disabled="!caps.submit || acting || sending"
                :title="state === 'awaiting_answers'
                  ? '还挂着未回答的问题：先答完跑一轮回到起草，才能提交评审'
                  : '跑自检清单；全过才进待批准'"
                @click="submit">提交评审</button>
            </template>
            <template v-else-if="state === 'ready_for_review'">
              <button class="act" :disabled="!caps.review || acting" @click="runReview">
                {{ reviewing ? '对抗复核中…' : '重跑对抗复核' }}
              </button>
              <button class="act" :disabled="!caps.reject || acting" @click="reject">打回起草</button>
              <button class="act primary" :disabled="!caps.approve || acting" @click="approve">批准</button>
            </template>
            <button v-else-if="state === 'approved'" class="act primary"
              :disabled="!caps.convert || acting" @click="convert">一键生成任务</button>
            <button v-else-if="state === 'abandoned'" class="act primary"
              :disabled="!caps.resume || acting" @click="resume">继续这份 PRD</button>
          </div>

          <ChatComposer
            ref="composer"
            v-model="input"
            :placeholder="composerPlaceholder"
            :disabled="!caps.round"
            :can-send="canSend"
            :busy="sending"
            :send-label="sendLabel"
            @send="send"
          >
            <template #tools>
              <label v-if="caps.round" class="pill" title="默认按轮次自动推进阶段；需要时可指定">
                <span>阶段</span>
                <select v-model="stageOverride" aria-label="指定阶段">
                  <option value="">自动推进</option>
                  <option v-for="(v, k) in STAGE_LABEL" :key="k" :value="k">{{ v }}</option>
                </select>
              </label>
            </template>
          </ChatComposer>
        </div>
      </footer>
    </section>

    <div v-if="panelOpen" class="panel-scrim" @click="panelOpen = false"></div>
    <PRDDocPanel
      v-if="panelOpen"
      ref="panel"
      v-model:tab="panelTab"
      class="panel"
      :prd="prd"
      :caps="caps"
      :md="md"
      :acting="acting"
      :check-report="checkReport"
      :review-events="reviewEvents"
      :reviewing="reviewing"
      @close="setPanel(false)"
      @confirm-section="confirmSection"
      @save-dispositions="saveDispositions"
      @dismiss-report="checkReport = null"
    />
  </div>
</template>

<style scoped>
.center { flex: 1; display: grid; place-items: center; }

.conv { flex: 1; display: flex; min-height: 0; position: relative; }
.chat { flex: 1; min-width: 0; display: flex; flex-direction: column; min-height: 0; }

/* ---------------------------------------------------------------- 顶栏 */
.top { padding: 10px 24px 8px 56px; border-bottom: 1px solid var(--border); }
.title-row { display: flex; align-items: center; gap: 10px; min-width: 0; }
h1 {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.top-actions { margin-left: auto; display: flex; gap: 6px; align-items: center; flex: none; }
.ghost { background: none; border-color: transparent; padding: 4px 10px; color: var(--text-dim); font-size: 13px; }
.ghost:hover:not(:disabled) { color: var(--text); border-color: var(--border); }
.ghost.on { color: var(--accent); border-color: var(--border); background: var(--surface-2); }
.ghost.icon { font-size: 18px; line-height: 1; padding: 2px 10px 6px; }
.count {
  margin-left: 5px;
  font-size: 11px;
  padding: 0 6px;
  border-radius: 999px;
  background: var(--warn-bg);
  color: var(--warn);
}
.meta { display: flex; gap: 12px; flex-wrap: wrap; font-size: 12.5px; margin-top: 2px; }

.menu-wrap { position: relative; }
.menu {
  position: absolute;
  right: 0;
  top: calc(100% + 4px);
  z-index: 30;
  width: 240px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 6px;
  box-shadow: 0 8px 28px rgba(0, 0, 0, .3);
}
.menu a, .menu button {
  display: block;
  width: 100%;
  text-align: left;
  padding: 7px 10px;
  border-radius: 6px;
  border: none;
  background: none;
  color: var(--text);
  font-size: 13.5px;
}
.menu a:hover, .menu button:hover:not(:disabled) { background: var(--surface-2); text-decoration: none; }
.menu-note { font-size: 12px; margin: 4px 10px 6px; }
.danger-item { color: var(--bad) !important; border-top: 1px solid var(--border) !important; border-radius: 0 0 6px 6px !important; }

.steps { list-style: none; display: flex; gap: 4px; margin: 8px 0 0; padding: 0; flex-wrap: wrap; font-size: 12px; color: var(--text-faint); }
.steps li { display: flex; align-items: center; gap: 5px; padding-right: 10px; }
.steps li + li::before { content: ''; width: 14px; height: 1px; background: var(--border); margin-right: 6px; }
.sd { width: 7px; height: 7px; border-radius: 50%; border: 1px solid var(--text-faint); }
.steps li.done { color: var(--text-dim); }
.steps li.done .sd { background: var(--ok); border-color: var(--ok); }
.steps li.cur { color: var(--accent); font-weight: 500; }
.steps li.cur .sd { background: var(--accent); border-color: var(--accent); box-shadow: 0 0 0 3px var(--run-bg); }

/* ---------------------------------------------------------------- 对话流 */
.scroll { flex: 1; overflow-y: auto; min-height: 0; }
.thread { max-width: 800px; margin: 0 auto; padding: 24px 24px 16px; }
.tiny { font-size: 12px; }
.center-text { text-align: center; margin-bottom: 12px; }

.msg { display: flex; gap: 12px; margin-bottom: 22px; }
.msg.user { justify-content: flex-end; }
.msg.user .bubble {
  max-width: 82%;
  background: var(--surface-2);
  border-radius: 18px 18px 4px 18px;
  padding: 10px 16px;
}
.msg.user.pending .bubble { opacity: .75; }
.origin-tag { font-size: 11.5px; margin-bottom: 2px; }

.avatar {
  flex: none;
  width: 30px;
  height: 30px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--accent-strong);
  color: #fff;
  font-weight: 600;
  font-size: 14px;
}
.avatar.err { background: var(--warn); color: #1c2024; }
.msg.agent .body { flex: 1; min-width: 0; padding-top: 3px; }
.who { font-size: 12.5px; margin-bottom: 6px; }
.time { margin-left: 6px; }

.divider { display: flex; align-items: center; gap: 10px; font-size: 12px; margin: -6px 0 18px; }
.divider::before, .divider::after { content: ''; flex: 1; height: 1px; background: var(--border); }

.questions {
  margin-top: 12px;
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 10px 12px;
}
.questions.open { border-color: var(--warn); background: var(--warn-bg); }
.q-head { display: flex; justify-content: space-between; align-items: center; font-size: 12.5px; color: var(--text-dim); margin-bottom: 6px; }
.questions.open .q-head > span { color: var(--warn); font-weight: 500; }
.q {
  display: flex;
  gap: 10px;
  width: 100%;
  text-align: left;
  border: none;
  background: none;
  padding: 6px 4px;
  border-radius: 8px;
  color: var(--text);
  line-height: 1.55;
}
.q:hover:not(:disabled) { background: var(--surface-2); }
.q:disabled { opacity: 1; cursor: default; }
.qn {
  flex: none;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  background: var(--surface-2);
  color: var(--text-dim);
  font-size: 12px;
  display: grid;
  place-items: center;
  margin-top: 1px;
}

.changed { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; margin-top: 10px; font-size: 12.5px; }
.chip {
  border-radius: 999px;
  padding: 2px 10px;
  font-size: 12px;
  background: none;
  color: var(--text-dim);
}
.chip:hover:not(:disabled) { color: var(--accent); }
.link { border: none; background: none; padding: 0; color: var(--accent); font-size: 12.5px; }
.link:hover:not(:disabled) { text-decoration: underline; }

.fail { border: 1px solid var(--bad); background: var(--bad-bg); border-radius: 12px; padding: 10px 14px; }
.retry { margin-top: 8px; }

.sys {
  border: 1px dashed var(--border);
  border-radius: 12px;
  padding: 12px 16px;
  margin: 4px 0 20px 42px;
  font-size: 13.5px;
  color: var(--text-dim);
}
.sys.ok { border-color: var(--ok); }
.sys strong { color: var(--text); }
.tasks { list-style: none; padding: 0; margin: 8px 0 0; }
.tasks li { display: flex; gap: 10px; align-items: center; padding: 3px 0; }
.warn-text { color: var(--warn); }

/* ---------------------------------------------------------------- 底部 */
.foot { padding: 0 24px 16px; }
.foot-inner { max-width: 800px; margin: 0 auto; }
.toast {
  display: flex;
  gap: 8px;
  align-items: flex-start;
  border-radius: 10px;
  padding: 8px 12px;
  margin-bottom: 8px;
  font-size: 13px;
}
.toast span { flex: 1; }
.toast.bad { background: var(--bad-bg); color: var(--bad); border: 1px solid var(--bad); }
.toast.ok { background: var(--ok-bg); color: var(--ok); border: 1px solid var(--ok); }
.x { border: none; background: none; padding: 0 4px; color: inherit; }

.actions { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 8px; }
.act { border-radius: 999px; padding: 4px 14px; font-size: 13px; }
.act.warn { color: var(--warn); border-color: var(--warn); background: var(--warn-bg); }
.actions.muted .act { opacity: .45; }

.pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--border);
  border-radius: 999px;
  padding: 1px 4px 1px 10px;
  font-size: 12.5px;
  color: var(--text-dim);
  background: var(--surface-2);
}
.pill select { border: none; background: transparent; padding: 2px 4px; font-size: 12.5px; }

/* ---------------------------------------------------------------- 面板 */
.panel { width: 460px; flex: none; }
.panel-scrim { display: none; }

@media (max-width: 1199px) {
  .panel { position: absolute; right: 0; top: 0; bottom: 0; z-index: 25; width: min(520px, 100%); box-shadow: -4px 0 24px rgba(0, 0, 0, .3); }
  .panel-scrim { display: block; position: absolute; inset: 0; z-index: 24; background: rgba(0, 0, 0, .3); }
}
@media (max-width: 640px) {
  .top { padding: 10px 12px 8px 48px; }
  .thread { padding: 16px 12px; }
  .foot { padding: 0 12px 12px; }
  .steps { display: none; }
  .msg.user .bubble { max-width: 92%; }
}

/* markdown 正文 */
.md { line-height: 1.7; overflow-wrap: anywhere; }
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) { margin: 14px 0 6px; font-size: 15px; }
.md :deep(p) { margin: 6px 0; }
.md :deep(p:first-child) { margin-top: 0; }
.md :deep(p:last-child) { margin-bottom: 0; }
.md :deep(ul), .md :deep(ol) { margin: 6px 0; padding-left: 22px; }
.md :deep(blockquote) { margin: 6px 0; padding-left: 10px; border-left: 3px solid var(--border); color: var(--text-dim); }
.md :deep(table) { width: 100%; border-collapse: collapse; margin: 8px 0; display: block; overflow-x: auto; }
.md :deep(th), .md :deep(td) { border: 1px solid var(--border); padding: 6px 8px; text-align: left; }
.md :deep(code) {
  font-family: var(--mono); font-size: 12.5px;
  background: var(--bg); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 5px;
}
.md :deep(pre) { margin: 8px 0; }
.md :deep(pre code) { background: none; border: none; padding: 0; }
.md :deep(a) { word-break: break-all; }
</style>
