<script setup>
// 任务预览环境：在任务的 worktree 里发现 Dockerfile，人选要起哪几个，
// 构建镜像、起容器、映射随机宿主机端口，手动点完后一键停止并清理。
//
// 静态测试（无论 AI 跑多少）替代不了肉眼确认前端实际效果 —— 这个
// 弹窗就是「跑起来给我看看」的入口。
import { ref, computed, onMounted, onUnmounted, inject } from 'vue'
import { api, UnauthorizedError } from '../api'

const props = defineProps({
  task: { type: Object, required: true }, // { id, linearIssueKey }
})
const emit = defineEmits(['close'])
const onUnauthorized = inject('onUnauthorized')

const loading = ref(true)
const error = ref('')
const busy = ref(false)
const candidates = ref([])
const resources = ref(null)
const containers = ref([])
const op = ref(null)
// 每个候选的选择态：{ [path]: { checked, ports } }，ports 是逗号分隔文本
const picks = ref({})

let pollTimer = null

const building = computed(() => op.value?.state === 'building')
const selectedCount = computed(() => Object.values(picks.value).filter((p) => p.checked).length)

function guard(e) {
  if (e instanceof UnauthorizedError) return onUnauthorized(), true
  error.value = e.message
  return true
}

async function loadStatus() {
  try {
    const st = await api.previewStatus(props.task.id)
    op.value = st.op
    containers.value = st.containers || []
    if (st.resources) resources.value = st.resources
    if (building.value) schedulePoll()
  } catch (e) {
    guard(e)
  }
}

async function loadCandidates() {
  loading.value = true
  error.value = ''
  try {
    const data = await api.previewCandidates(props.task.id)
    candidates.value = data.candidates || []
    resources.value = data.resources
    // 默认全不选，端口预填 EXPOSE 解析结果 —— 起什么是人的决定
    const p = {}
    for (const c of candidates.value) {
      p[c.path] = { checked: false, ports: (c.ports || []).join(', ') }
    }
    picks.value = p
  } catch (e) {
    guard(e)
  } finally {
    loading.value = false
  }
}

function schedulePoll() {
  clearTimeout(pollTimer)
  pollTimer = setTimeout(async () => {
    await loadStatus()
    if (building.value) schedulePoll()
  }, 2000)
}

function parsePorts(text) {
  return text
    .split(/[,\s]+/)
    .filter(Boolean)
    .map((s) => Number(s))
}

async function start() {
  const selections = []
  for (const c of candidates.value) {
    const p = picks.value[c.path]
    if (!p?.checked) continue
    const ports = parsePorts(p.ports)
    if (ports.some((n) => !Number.isInteger(n) || n <= 0 || n > 65535)) {
      error.value = `${c.path} 的端口含非法值（1..65535，逗号分隔）`
      return
    }
    selections.push({ path: c.path, ports })
  }
  if (!selections.length) return
  busy.value = true
  error.value = ''
  try {
    await api.previewStart(props.task.id, selections)
    await loadStatus()
    schedulePoll()
  } catch (e) {
    guard(e)
  } finally {
    busy.value = false
  }
}

async function stop() {
  if (!confirm('停止并清理该任务的全部预览容器与镜像？')) return
  busy.value = true
  error.value = ''
  try {
    await api.previewStop(props.task.id)
    op.value = null
    await loadStatus()
  } catch (e) {
    guard(e)
  } finally {
    busy.value = false
  }
}

function portURL(host) {
  return `http://${location.hostname}:${host}`
}

function pctTone(used, threshold) {
  return used >= threshold ? 'bad' : used >= threshold - 10 ? 'warn' : 'ok'
}

onMounted(() => {
  loadCandidates()
  loadStatus()
})
onUnmounted(() => clearTimeout(pollTimer))
</script>

<template>
  <div class="overlay" @click.self="emit('close')">
    <div class="dialog card">
      <div class="spread">
        <h2>预览环境 · <span class="mono">{{ task.linearIssueKey }}</span></h2>
        <button class="close" @click="emit('close')">✕</button>
      </div>

      <div v-if="error" class="error-banner small">{{ error }}</div>

      <!-- 资源水位：超阈值时禁启动（系统设置可配阈值） -->
      <div v-if="resources" class="meters">
        <div class="meter">
          <span class="dim">内存</span>
          <div class="bar">
            <div class="fill" :class="pctTone(resources.memUsedPct, resources.memThreshold)"
                 :style="{ width: Math.min(resources.memUsedPct, 100) + '%' }"></div>
            <div class="mark" :style="{ left: resources.memThreshold + '%' }"></div>
          </div>
          <span class="mono">{{ resources.memUsedPct }}%</span>
          <span class="faint">阈值 {{ resources.memThreshold }}%</span>
        </div>
        <div class="meter">
          <span class="dim">磁盘</span>
          <div class="bar">
            <div class="fill" :class="pctTone(resources.diskUsedPct, resources.diskThreshold)"
                 :style="{ width: Math.min(resources.diskUsedPct, 100) + '%' }"></div>
            <div class="mark" :style="{ left: resources.diskThreshold + '%' }"></div>
          </div>
          <span class="mono">{{ resources.diskUsedPct }}%</span>
          <span class="faint">阈值 {{ resources.diskThreshold }}%</span>
        </div>
        <div v-if="!resources.allowed" class="error-banner small" style="grid-column: 1 / -1">
          {{ resources.reason }} —— 已禁止启动新预览（阈值见「系统设置」）
        </div>
      </div>

      <!-- 运行中的容器 -->
      <div v-if="containers.length" class="section">
        <div class="label">运行中的服务</div>
        <div v-for="c in containers" :key="c.name" class="row">
          <span class="badge" :class="c.state === 'running' ? 'ok' : 'idle'">{{ c.state }}</span>
          <span class="mono name">{{ c.name }}</span>
          <span class="ports">
            <a v-for="p in c.ports" :key="p.container" :href="portURL(p.host)" target="_blank" rel="noopener">
              :{{ p.host }} → {{ p.container }}
            </a>
            <span v-if="!c.ports.length" class="faint">无端口映射</span>
          </span>
        </div>
        <button class="danger" :disabled="busy" @click="stop">停止并清理（删容器与镜像）</button>
      </div>

      <!-- 构建状态 -->
      <div v-if="building" class="section dim">
        正在构建镜像并启动容器…… 冷构建可能要几分钟，弹窗每 2 秒自动刷新。
      </div>
      <div v-else-if="op?.state === 'failed'" class="error-banner small">
        上次启动失败：{{ op.error }}
      </div>

      <!-- 候选镜像 -->
      <div class="section">
        <div class="label">选择要启动的镜像（基于 worktree 里的 Dockerfile）</div>
        <div v-if="loading" class="dim">扫描中……</div>
        <div v-else-if="!candidates.length" class="dim">
          worktree 里没找到 Dockerfile —— 这个仓库可能不支持容器化运行。
        </div>
        <div v-for="c in candidates" :key="c.path" class="row cand">
          <input type="checkbox" v-model="picks[c.path].checked" :disabled="building" />
          <span class="mono name">{{ c.path }}</span>
          <input
            class="ports-input mono"
            v-model="picks[c.path].ports"
            :disabled="building || !picks[c.path].checked"
            placeholder="容器端口，逗号分隔"
          />
          <span v-if="!c.ports?.length" class="faint">未声明 EXPOSE，请手工填端口</span>
        </div>
        <button
          class="primary"
          :disabled="busy || building || !selectedCount || (resources && !resources.allowed)"
          @click="start"
        >
          {{ building ? '构建中……' : `启动选中的 ${selectedCount} 个服务` }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: 6vh 16px;
  z-index: 100;
}
.dialog {
  width: 640px;
  max-width: 100%;
  max-height: 84vh;
  overflow-y: auto;
}
h2 { font-size: 16px; margin: 0; }
.close { border: none; background: none; font-size: 15px; padding: 2px 8px; }

.section { margin-top: 16px; display: flex; flex-direction: column; gap: 10px; }
.label { font-size: 12.5px; color: var(--text-dim); font-weight: 500; }

.meters {
  margin-top: 14px;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px 18px;
}
.meter { display: flex; align-items: center; gap: 8px; font-size: 12.5px; }
.bar {
  position: relative;
  flex: 1;
  height: 8px;
  background: var(--border);
  border-radius: 4px;
  overflow: hidden;
}
.fill { height: 100%; border-radius: 4px; }
.fill.ok { background: var(--ok); }
.fill.warn { background: var(--warn, #d29922); }
.fill.bad { background: var(--bad); }
.mark {
  position: absolute;
  top: -2px;
  bottom: -2px;
  width: 2px;
  background: var(--bad);
}

.row { display: flex; align-items: center; gap: 10px; font-size: 13px; flex-wrap: wrap; }
.name { flex: 0 1 auto; word-break: break-all; }
.ports { display: flex; gap: 10px; flex-wrap: wrap; }
.ports-input {
  flex: 0 0 160px;
  font-size: 12.5px;
  padding: 4px 8px;
}
.cand input[type='checkbox'] { margin: 0; }
button { align-self: flex-start; }
.error-banner.small { font-size: 13px; }
</style>
