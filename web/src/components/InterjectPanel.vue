<script setup>
// 运行中插话面板（docs/features/interject.md §3.5）。
//
// 全部 UI 逻辑都在这里：TaskDetail.vue 只加 import 与一行组件，不碰执行日志
// 的分组/时间线（那是 f04 运行回放的地盘）。插话不合并进事件时间线 ——
// 它的数据源是 task_interjections，状态会在事后变化（queued → delivered/failed），
// 用独立的 rev 游标增量轮询。
//
// 正文用 {{ }} 插值，不过 markdown、不走原始 HTML 绑定：这是人写的话，减少 XSS 面。
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { api, formatTime } from '../api'
import { createPoller } from '../livelog'

const props = defineProps({
  taskId: { type: Number, required: true },
  state: { type: String, required: true },
})

// 与后端判定第 1 条对齐：只在实现与验证阶段可插话。
const canInterject = computed(() => props.state === 'implementing' || props.state === 'verifying')
// 这些状态之后不会再有 agent 轮次；进入它们后再拉一轮收尾（结算可能把
// 「已记录待投递」改成「未进入模型」，必须让人看到）就停。
const settled = computed(() => !['queued', 'triaging', 'implementing', 'verifying'].includes(props.state))

const items = ref([]) // 按 id 升序
const rev = ref(0)
const draft = ref('')
const sending = ref(false)
const sendError = ref('')
const loadError = ref('')

const STATUS_META = {
  pending: { label: '投递中', tone: 'run' },
  delivering: { label: '投递中', tone: 'run' },
  delivered: { label: '已送达', tone: 'ok' },
  queued: { label: '已记录待投递', tone: 'warn' },
  failed: { label: '未进入模型', tone: 'bad' },
}
const statusMeta = (s) => STATUS_META[s] || { label: s, tone: 'idle' }

// 按 id 就地覆盖（同一行多次出现是幂等的）；乐观插入的临时行用负 id。
function upsert(row) {
  const i = items.value.findIndex((x) => x.id === row.id)
  if (i >= 0) items.value[i] = { ...items.value[i], ...row }
  else items.value.push(row)
  items.value.sort((a, b) => a.id - b.id)
}

async function pull() {
  for (;;) {
    const res = await api.interjections(props.taskId, rev.value)
    for (const row of res.interjections || []) upsert(row)
    rev.value = res.rev ?? rev.value
    loadError.value = ''
    if (!res.more) return
  }
}

let finalPullDone = false
const poller = createPoller(async () => {
  try {
    await pull()
  } catch (e) {
    loadError.value = e.message || '加载失败'
  }
  if (settled.value) {
    // 终态后的「收尾一拉」本身就触发读时结算，拿到的必定是结算后的终态。
    if (finalPullDone) return false
    finalPullDone = true
  }
  return true
}, 2000)

watch(settled, (now) => {
  if (!now) {
    finalPullDone = false
    poller.start()
  }
})

let tempId = 0
async function send() {
  const message = draft.value.trim()
  if (!message || sending.value) return
  sending.value = true
  sendError.value = ''
  const temp = { id: --tempId, status: 'pending', body: message, createdAt: new Date().toISOString() }
  upsert(temp)
  try {
    const res = await api.interject(props.taskId, message)
    items.value = items.value.filter((x) => x.id !== temp.id)
    upsert({ ...temp, ...res, id: res.id, body: message })
    draft.value = ''
  } catch (e) {
    items.value = items.value.filter((x) => x.id !== temp.id)
    sendError.value = e.message || '发送失败'
    // 409/422/503 也已落库（failed），把那一行拉回来显示原因
  } finally {
    sending.value = false
    pull().catch(() => {})
  }
}

function onKeydown(e) {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) send()
}

onMounted(() => {
  pull()
    .catch((e) => (loadError.value = e.message || '加载失败'))
    .finally(() => poller.start())
})
onUnmounted(() => poller.stop())
</script>

<template>
  <div v-if="canInterject || items.length" class="card interject-card">
    <div class="spread">
      <div class="label">运行中插话</div>
      <span v-if="canInterject && state === 'verifying'" class="faint hint">
        只在 agent 运行时（含验证失败后的修复轮）投递；验证判定不受插话影响
      </span>
    </div>

    <div v-if="canInterject" class="composer">
      <textarea
        v-model="draft"
        rows="2"
        placeholder="给正在跑的 agent 说一句（不会中断它）"
        :disabled="sending"
        @keydown="onKeydown"
      ></textarea>
      <button class="primary" :disabled="sending || !draft.trim()" @click="send">
        {{ sending ? '发送中…' : '发送' }}
      </button>
    </div>
    <div v-if="sendError" class="send-error">{{ sendError }}</div>
    <div v-if="loadError" class="faint">插话列表拉取失败：{{ loadError }}</div>

    <ol v-if="items.length" class="ij-list">
      <li v-for="it in items" :key="it.id" class="ij-item">
        <div class="row ij-head">
          <span class="who">你</span>
          <span class="faint">{{ formatTime(it.createdAt) }}</span>
          <span class="badge" :class="statusMeta(it.status).tone">{{ statusMeta(it.status).label }}</span>
          <span v-if="it.phase" class="faint mono">{{ it.phase }}</span>
        </div>
        <div class="ij-body">{{ it.body }}</div>
        <div v-if="it.error && it.status === 'failed'" class="ij-error">{{ it.error }}</div>
        <div v-else-if="it.error === 'no_replay_ack'" class="faint">已写入 agent 进程，未收到确认回显</div>
      </li>
    </ol>
    <div v-else class="faint">还没有插话。说出的话会先记录，再送进正在运行的 agent。</div>
  </div>
</template>

<style scoped>
.interject-card { display: flex; flex-direction: column; gap: 10px; }
.hint { font-size: 12.5px; }
.composer { display: flex; gap: 8px; align-items: flex-end; }
.composer textarea { flex: 1; min-width: 0; resize: vertical; }
.send-error { color: var(--bad); font-size: 13px; }
.ij-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.ij-item { border-top: 1px solid var(--border); padding-top: 8px; }
.ij-head { flex-wrap: wrap; gap: 8px; }
.who { font-weight: 500; }
.ij-body { white-space: pre-wrap; word-break: break-word; margin-top: 4px; }
.ij-error { color: var(--bad); font-size: 13px; margin-top: 2px; }
@media (max-width: 600px) {
  .composer { flex-direction: column; align-items: stretch; }
}
</style>
