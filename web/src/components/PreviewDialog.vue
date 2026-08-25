<script setup>
// 任务预览环境：在任务的 worktree 里发现 Dockerfile，人选要起哪几个，
// 构建镜像、起容器、映射随机宿主机端口，手动点完后一键停止并清理。
//
// 静态测试（无论 AI 跑多少）替代不了肉眼确认前端实际效果 —— 这个
// 弹窗就是「跑起来给我看看」的入口。
import { ref, computed, watch, onMounted, onUnmounted, inject, nextTick } from 'vue'
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
// 每个候选的选择态：{ [path]: { checked, ports, env } }，ports 是逗号
// 分隔文本，env 是 compose 变量名 → 值（可选的预填默认值）
const picks = ref({})
// 附加基础设施与额外 env（仅注入 Dockerfile 容器）
const infra = ref([])
const extraEnv = ref('')
// AI 推荐：{ state, error, result }；showAll 控制是否展开全部候选
const rec = ref(null)
const showAll = ref(false)
let recTimer = null

let pollTimer = null

const building = computed(() => op.value?.state === 'building')
const recommending = computed(() => rec.value?.state === 'running')
const selectedCount = computed(() => Object.values(picks.value).filter((p) => p.checked).length)
const hasDockerfilePick = computed(() =>
  candidates.value.some((c) => c.kind !== 'compose' && picks.value[c.path]?.checked),
)
// 有推荐时默认只显示推荐项，其余折叠 —— 21 个候选平铺谁也看不过来
const visibleCandidates = computed(() => {
  if (!rec.value?.result || showAll.value) return candidates.value
  const hit = candidates.value.filter((c) => c.path === rec.value.result.path)
  return hit.length ? hit : candidates.value
})
const recEnvNames = computed(() => Object.keys(rec.value?.result?.env || {}).sort())

// 构建完成（building true→false 且容器出现）给明确成功信号
const justStarted = ref(false)
watch(building, (now, before) => {
  if (before && !now && containers.value.length > 0) justStarted.value = true
})

// 构建日志自动滚到底部（除非人往上翻了）
const logEl = ref(null)
watch(
  () => op.value?.progress,
  async () => {
    await nextTick()
    const el = logEl.value
    if (el && el.scrollTop + el.clientHeight >= el.scrollHeight - 40) {
      el.scrollTop = el.scrollHeight
    }
  },
)

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
    // 默认全不选，端口预填 EXPOSE 解析结果、compose 可选变量预填
    // 默认值 —— 起什么、填什么是人的决定
    const p = {}
    for (const c of candidates.value) {
      const env = {}
      for (const e of c.env || []) env[e.name] = e.default || ''
      p[c.path] = { checked: false, ports: (c.ports || []).join(', '), env }
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
    if (c.kind === 'compose') {
      const env = {}
      for (const e of c.env || []) {
        const v = (p.env[e.name] || '').trim()
        if (e.required && !v) {
          error.value = `${c.path} 的必填变量 ${e.name} 未填写`
          return
        }
        if (v) env[e.name] = v
      }
      selections.push({ path: c.path, kind: 'compose', env })
      continue
    }
    const ports = parsePorts(p.ports)
    if (ports.some((n) => !Number.isInteger(n) || n <= 0 || n > 65535)) {
      error.value = `${c.path} 的端口含非法值（1..65535，逗号分隔）`
      return
    }
    selections.push({ path: c.path, kind: 'dockerfile', ports })
  }
  if (!selections.length) return
  // 额外 env：每行 KEY=VALUE
  const env = {}
  for (const line of extraEnv.value.split('\n')) {
    const t = line.trim()
    if (!t) continue
    const eq = t.indexOf('=')
    if (eq <= 0) {
      error.value = `环境变量行格式不对（应 KEY=VALUE）：${t}`
      return
    }
    env[t.slice(0, eq).trim()] = t.slice(eq + 1)
  }
  busy.value = true
  error.value = ''
  justStarted.value = false
  try {
    await api.previewStart(props.task.id, { selections, infra: infra.value, env })
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
    justStarted.value = false
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

async function recommend() {
  error.value = ''
  try {
    const resp = await api.previewRecommend(props.task.id)
    if (resp.op) {
      rec.value = resp.op // 缓存命中直接出结果
      return
    }
    rec.value = { state: 'running' }
    pollRecommend()
  } catch (e) {
    guard(e)
  }
}

function pollRecommend() {
  clearTimeout(recTimer)
  recTimer = setTimeout(async () => {
    try {
      const { op: o } = await api.previewRecommendStatus(props.task.id)
      rec.value = o
      if (o?.state === 'running') pollRecommend()
    } catch {
      /* 网络抖动下轮再说 */
      pollRecommend()
    }
  }, 2000)
}

// 采用推荐：勾选推荐候选、预填变量与基础设施，展开让人核对。
// compose 的变量进候选自己的 env 表单；dockerfile 的变量进对话框级
// 额外 env 文本框（注入应用容器的通道）。
function adoptRecommendation() {
  const r = rec.value?.result
  if (!r || !picks.value[r.path]) return
  picks.value[r.path].checked = true
  if (r.kind === 'compose') {
    for (const [name, s] of Object.entries(r.env || {})) {
      if (s.value && name in picks.value[r.path].env) {
        picks.value[r.path].env[name] = s.value
      }
    }
  } else {
    const lines = Object.entries(r.env || {})
      .filter(([, s]) => s.value)
      .map(([name, s]) => `${name}=${s.value}`)
    if (lines.length) {
      const existing = extraEnv.value.trim()
      extraEnv.value = existing ? existing + '\n' + lines.join('\n') : lines.join('\n')
    }
    if (r.infra?.length) infra.value = [...r.infra]
  }
  showAll.value = true
}

async function loadRecommend() {
  try {
    const { op: o } = await api.previewRecommendStatus(props.task.id)
    rec.value = o
    if (o?.state === 'running') pollRecommend()
  } catch {
    /* 推荐状态是增强信息，拿不到不挡主流程 */
  }
}

onMounted(() => {
  loadCandidates()
  loadStatus()
  loadRecommend()
})
onUnmounted(() => {
  clearTimeout(pollTimer)
  clearTimeout(recTimer)
})
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

      <!-- 构建成功提示：从「构建中」回到列表的变化太微妙，必须明说 -->
      <div v-if="justStarted" class="success-banner">
        ✓ 构建完成，服务已启动。点下方链接打开验证；用完记得「停止并清理」。
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

      <!-- 构建状态：实时日志尾部 —— 分钟级黑盒里「在编译」与「卡死了」的区分手段 -->
      <div v-if="building" class="section">
        <div class="dim">正在构建镜像并启动容器…… 冷构建可能要几分钟，弹窗每 2 秒自动刷新。</div>
        <pre v-if="op?.progress" ref="logEl" class="buildlog mono">{{ op.progress }}</pre>
        <button class="danger" :disabled="busy" @click="stop">取消构建并清理</button>
      </div>
      <div v-else-if="op?.state === 'failed'" class="error-banner small">
        上次启动失败：{{ op.error }}
      </div>

      <!-- 候选：Dockerfile 单镜像 或 compose 编排（拓扑+依赖的标准声明） -->
      <div class="section">
        <div class="row spread">
          <div class="label">选择要启动的服务（基于 worktree 里的 Dockerfile / compose 编排）</div>
          <button class="link" :disabled="recommending || building" @click="recommend">
            {{ recommending ? 'AI 分析中……' : 'AI 推荐' }}
          </button>
        </div>

        <!-- AI 推荐卡片：建议只是预填，启动前由人核对 -->
        <div v-if="rec?.state === 'done' && rec.result" class="rec-card">
          <div class="rec-head">
            <span class="badge ok">{{ rec.result.kind === 'compose' ? '编排' : '镜像' }}</span>
            <span class="mono">{{ rec.result.path }}</span>
          </div>
          <div class="dim">{{ rec.result.reason }}</div>
          <div v-for="name in recEnvNames" :key="name" class="rec-env mono">
            {{ name }}={{ rec.result.env[name].value || '（需人填）' }}
            <span class="faint">← {{ rec.result.env[name].source || '无来源，请核对' }}</span>
          </div>
          <div v-if="rec.result.infra?.length" class="dim">附加基础设施：{{ rec.result.infra.join(', ') }}</div>
          <div v-if="rec.result.notes" class="rec-notes">⚠ {{ rec.result.notes }}</div>
          <button class="primary" :disabled="building" @click="adoptRecommendation">采用推荐（自动勾选并预填）</button>
        </div>
        <div v-else-if="rec?.state === 'failed'" class="error-banner small">
          AI 推荐失败：{{ rec.error }}（仍可手工选择）
        </div>

        <div v-if="loading" class="dim">扫描中……</div>
        <div v-else-if="!candidates.length" class="dim">
          worktree 里没找到 Dockerfile 或 compose 文件 —— 这个仓库可能不支持容器化运行。
        </div>
        <template v-else>
          <div v-for="c in visibleCandidates" :key="c.path" class="cand-block">
            <div class="row cand" :class="{ recommended: rec?.result?.path === c.path }">
              <input type="checkbox" v-model="picks[c.path].checked" :disabled="building" />
              <span class="badge" :class="c.kind === 'compose' ? 'ok' : 'idle'">
                {{ c.kind === 'compose' ? '编排' : '镜像' }}
              </span>
              <span class="mono name">{{ c.path }}</span>
              <span v-if="rec?.result?.path === c.path" class="badge ok">推荐</span>
              <template v-if="c.kind !== 'compose'">
                <input
                  class="ports-input mono"
                  v-model="picks[c.path].ports"
                  :disabled="building || !picks[c.path].checked"
                  placeholder="容器端口，逗号分隔"
                />
                <span v-if="!c.ports?.length" class="faint">未声明 EXPOSE，请手工填端口</span>
              </template>
              <span v-else class="faint">端口由编排声明，启动时重置为随机宿主端口</span>
            </div>
            <!-- compose 必填变量：连不连共享测试库这类决定由人拍板 -->
            <div v-if="c.kind === 'compose' && picks[c.path].checked && c.env?.length" class="env-grid">
              <div v-for="e in c.env" :key="e.name" class="env-row">
                <label class="mono dim">{{ e.name }}<span v-if="e.required" class="req">*</span></label>
                <input
                  class="mono"
                  v-model="picks[c.path].env[e.name]"
                  :placeholder="e.required ? '必填' : '可选'"
                  :disabled="building"
                />
              </div>
            </div>
          </div>
          <button v-if="rec?.result && candidates.length > 1" class="link faint" @click="showAll = !showAll">
            {{ showAll ? '收起全部候选' : `展开全部候选（${candidates.length}）` }}
          </button>
        </template>

        <!-- 附加基础设施：仅注入 Dockerfile 容器（compose 的依赖自己声明） -->
        <template v-if="hasDockerfilePick">
          <div class="label">附加基础设施（起官方镜像进任务网络，连接串自动注入）</div>
          <div class="row">
            <label v-for="i in ['postgres', 'redis', 'mysql']" :key="i" class="infra-pick">
              <input type="checkbox" :value="i" v-model="infra" :disabled="building" /> {{ i }}
            </label>
          </div>
          <textarea
            class="mono env-text"
            v-model="extraEnv"
            rows="2"
            placeholder="额外环境变量，每行 KEY=VALUE（可选，注入所有选中的 Dockerfile 容器）"
            :disabled="building"
          ></textarea>
        </template>

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
.cand.recommended {
  border-left: 2px solid var(--ok);
  padding-left: 8px;
}
.link {
  border: none;
  background: none;
  color: var(--accent, #7af);
  font-size: 12.5px;
  padding: 0;
  text-decoration: underline;
}
.link:disabled { color: var(--text-dim); text-decoration: none; }
.rec-card {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px;
  border: 1px solid var(--ok);
  border-radius: var(--radius);
  background: var(--ok-bg);
  font-size: 12.5px;
}
.rec-head { display: flex; align-items: center; gap: 8px; font-weight: 500; }
.rec-env { font-size: 12px; word-break: break-all; }
.rec-notes { color: var(--warn); }
.cand-block { display: flex; flex-direction: column; gap: 6px; }
.env-grid {
  margin-left: 26px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px 10px;
  border-left: 2px solid var(--border);
}
.env-row { display: flex; align-items: center; gap: 10px; font-size: 12.5px; }
.env-row label { min-width: 220px; word-break: break-all; }
.env-row input { flex: 1; }
.req { color: var(--bad); }
.infra-pick { display: inline-flex; align-items: center; gap: 6px; margin-right: 16px; font-size: 13px; }
.env-text {
  width: 100%;
  resize: vertical;
  font-size: 12px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--bg);
  color: var(--text);
}
button { align-self: flex-start; }
.error-banner.small { font-size: 13px; }

.success-banner {
  padding: 10px 12px;
  border: 1px solid var(--ok);
  border-radius: var(--radius);
  background: color-mix(in srgb, var(--ok) 10%, transparent);
  color: var(--ok);
  font-size: 13px;
}

.buildlog {
  margin: 0;
  padding: 10px 12px;
  max-height: 220px;
  overflow-y: auto;
  background: var(--bg-soft, #0d1117);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
