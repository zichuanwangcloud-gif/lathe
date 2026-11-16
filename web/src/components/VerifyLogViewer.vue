<script setup>
// 完整验证日志查看器（docs/features/run-replay.md §3.3.4）。
//
// 内容来自 GET /api/tasks/{id}/verifications/{vid}/log：服务端按安全切点分块、
// 每块经过脱敏。这里只做两件事：按 nextOffset 追加下一块、多段（复现阶段）
// 用 part 切换。日志正文一律 {{ }} 插值进 <pre>，不经过任何 HTML 渲染。
import { ref, watch } from 'vue'
import { api, UnauthorizedError } from '../api'

const props = defineProps({
  taskId: { type: [String, Number], required: true },
  row: { type: Object, required: true }, // 一条 verification（含 logRef）
})
const emit = defineEmits(['close', 'unauthorized'])

const parts = ref(1)
const part = ref(0)
const chunks = ref([])
const next = ref(0)
const eof = ref(false)
const size = ref(0)
const withheld = ref(null)
const loading = ref(false)
const error = ref('')

function reset() {
  chunks.value = []
  next.value = 0
  eof.value = false
  withheld.value = null
  error.value = ''
}

async function more() {
  if (loading.value || eof.value || withheld.value || next.value == null) return
  loading.value = true
  try {
    const r = await api.verificationLog(props.taskId, props.row.id, { part: part.value, offset: next.value })
    parts.value = r.parts
    size.value = r.size
    if (r.withheld) {
      withheld.value = r.withheldBytes
      return
    }
    chunks.value.push(r.text)
    next.value = r.nextOffset
    eof.value = r.eof
  } catch (e) {
    if (e instanceof UnauthorizedError) return emit('unauthorized')
    error.value = e.data?.code === 'log_overwritten' ? '该运行的日志已被后一次运行覆盖' : e.message
  } finally {
    loading.value = false
  }
}

watch(part, () => {
  reset()
  more()
})
watch(
  () => props.row.id,
  () => {
    part.value = 0
    reset()
    more()
  },
  { immediate: true },
)
</script>

<template>
  <div class="viewer">
    <div class="spread bar">
      <div class="row">
        <span class="mono faint">{{ row.step }} · #{{ row.id }}</span>
        <template v-if="parts > 1">
          <button v-for="i in parts" :key="i" :class="{ primary: part === i - 1 }" @click="part = i - 1">第 {{ i }} 段</button>
        </template>
      </div>
      <div class="row">
        <span class="faint small">已脱敏 · 共 {{ size }} 字节</span>
        <button @click="emit('close')">关闭</button>
      </div>
    </div>
    <div v-if="error" class="faint bad-text">{{ error }}</div>
    <pre v-for="(c, i) in chunks" :key="i" class="chunk">{{ c }}</pre>
    <div v-if="withheld != null" class="faint warn-text">
      此处有一段超过 1MiB、无法安全切分的内容（常见于压缩输出），为避免泄露凭据已停止显示（余下 {{ withheld }} 字节）；
      完整日志在服务器 <code class="mono">&lt;DataDir&gt;/{{ row.logRef.split(' ')[part] }}</code>（0600）。
    </div>
    <div class="row">
      <button v-if="!eof && withheld == null && !error" :disabled="loading" @click="more">{{ loading ? '加载中…' : '加载更多' }}</button>
      <span v-if="eof" class="faint small">— 日志结束 —</span>
    </div>
  </div>
</template>

<style scoped>
.viewer { margin-top: 8px; padding: 10px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg); }
.bar { margin-bottom: 8px; flex-wrap: wrap; gap: 8px; }
.row { gap: 8px; flex-wrap: wrap; align-items: center; }
.chunk { margin: 0; white-space: pre-wrap; word-break: break-all; font-size: 12px; max-height: none; }
.small { font-size: 12px; }
.bad-text { color: var(--bad); }
.warn-text { color: var(--warn); margin: 8px 0; font-size: 12.5px; }
</style>
