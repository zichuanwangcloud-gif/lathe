<script setup>
import { ref, onMounted, onUnmounted, computed, inject } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime, formatDuration } from '../api'

const props = defineProps({ id: { type: String, required: true } })
const router = useRouter()
const onUnauthorized = inject('onUnauthorized')

const detail = ref(null)
const error = ref('')
const acting = ref(false)
let timer = null

const task = computed(() => detail.value?.task)
const isTerminal = computed(() =>
  ['merged', 'failed', 'cancelled'].includes(task.value?.state)
)

async function load() {
  try {
    detail.value = await api.task(props.id)
    error.value = ''
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
  timer = setInterval(() => { if (!isTerminal.value) load() }, 5000)
})
onUnmounted(() => clearInterval(timer))
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
        <button
          v-if="task.state === 'failed'"
          class="primary"
          :disabled="acting"
          @click="act(api.retry)"
        >重试</button>
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
</style>
