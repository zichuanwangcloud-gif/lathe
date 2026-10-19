<script setup>
import { ref, computed, watch, nextTick, onUnmounted } from 'vue'

// 智能体消息上方的「思考过程」折叠块（DeepSeek 深度思考块的形态）。
//
// 内容就是该轮的规划事件流：读了哪些文件、想了什么。跑的时候展开并跟随
// 滚动，跑完自动收起成一行「已思考（用时 …）」—— 过程可查，但不喧宾夺主。
// 事件里 tool_result / result / init 对人是噪音（结果全文、计费行），不展示。

const props = defineProps({
  events: { type: Array, default: () => [] },
  running: { type: Boolean, default: false },
  // 跑的时候用来走秒；没有就取第一条事件的时间
  startedAt: { type: [String, Number, Date], default: null },
  label: { type: String, default: '思考' },
  md: { type: Function, required: true },
})

const open = ref(props.running)
watch(
  () => props.running,
  (r) => (open.value = r),
)

const SHOWN = new Set(['thinking', 'tool_use', 'text'])
const items = computed(() => props.events.filter((e) => SHOWN.has(e.kind)))
const toolCount = computed(() => items.value.filter((e) => e.kind === 'tool_use').length)

// 走秒：只在 running 时开定时器
const now = ref(Date.now())
let timer = null
watch(
  () => props.running,
  (r) => {
    clearInterval(timer)
    if (r) timer = setInterval(() => (now.value = Date.now()), 1000)
  },
  { immediate: true },
)
onUnmounted(() => clearInterval(timer))

const elapsed = computed(() => {
  const first = props.startedAt || props.events[0]?.at
  if (!first) return null
  const start = new Date(first).getTime()
  const end = props.running ? now.value : new Date(props.events.at(-1)?.at || start).getTime()
  return Math.max(0, end - start)
})

// 走秒用整秒：formatDuration 的「12.3s」每秒跳小数，读着闹心
function fmt(ms) {
  const t = Math.round(ms / 1000)
  return t < 60 ? `${t} 秒` : `${Math.floor(t / 60)} 分 ${t % 60} 秒`
}

const title = computed(() => {
  const t = elapsed.value != null ? `（用时 ${fmt(elapsed.value)}）` : ''
  if (props.running) return `${props.label}中…${elapsed.value != null ? ' ' + fmt(elapsed.value) : ''}`
  return `已${props.label}${t}`
})

// 跑的时候跟随到底；人往上翻了就不抢滚动条
const box = ref(null)
watch(
  () => items.value.length,
  async () => {
    if (!props.running || !open.value) return
    const el = box.value
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
    await nextTick()
    if (nearBottom) el.scrollTop = el.scrollHeight
  },
)

function firstLine(s) {
  const i = (s || '').indexOf('\n')
  return i >= 0 ? s.slice(0, i) : s || ''
}
</script>

<template>
  <div v-if="running || items.length" class="think" :class="{ running }">
    <button class="think-head" :aria-expanded="open" @click="open = !open">
      <span class="dot" aria-hidden="true"></span>
      <span>{{ title }}</span>
      <span v-if="toolCount" class="faint">· 调用工具 {{ toolCount }} 次</span>
      <span class="chev" :class="{ up: open }" aria-hidden="true">›</span>
    </button>
    <div v-show="open" ref="box" class="think-body">
      <div v-if="!items.length" class="faint">准备中…（检出仓库、启动智能体）</div>
      <template v-for="e in items" :key="e.id">
        <div v-if="e.kind === 'tool_use'" class="t-tool mono" :title="e.body">
          <span class="t-name">{{ e.tool || '工具' }}</span>
          <span class="t-arg">{{ firstLine(e.body) }}</span>
        </div>
        <div v-else-if="e.kind === 'thinking'" class="t-think">{{ e.body }}</div>
        <div v-else class="t-text md" v-html="md(e.body)"></div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.think { margin: 2px 0 10px; }
.think-head {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: none;
  background: var(--surface-2);
  color: var(--text-dim);
  border-radius: 999px;
  padding: 4px 12px;
  font-size: 13px;
}
.think-head:hover:not(:disabled) { color: var(--text); border-color: transparent; }
.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--text-faint); }
.running .dot { background: var(--run); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: .3; } }
.chev { display: inline-block; transform: rotate(90deg); transition: transform .15s; }
.chev.up { transform: rotate(-90deg); }

.think-body {
  margin: 8px 0 0 3px;
  padding: 2px 0 2px 14px;
  border-left: 2px solid var(--border);
  color: var(--text-dim);
  font-size: 13px;
  max-height: 360px;
  overflow-y: auto;
}
.t-tool {
  display: flex;
  gap: 8px;
  padding: 2px 0;
  font-size: 12px;
  min-width: 0;
}
.t-name { color: var(--accent); flex: none; }
.t-arg { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--text-faint); }
.t-think { white-space: pre-wrap; padding: 4px 0; color: var(--text-faint); overflow-wrap: anywhere; }
.t-text { padding: 2px 0; }
.md :deep(p) { margin: 4px 0; }
.md :deep(code) { font-family: var(--mono); font-size: 12px; }
</style>
