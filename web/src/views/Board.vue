<script setup>
import { ref, onMounted, onUnmounted, inject } from 'vue'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime } from '../api'

const tasks = ref([])
const stats = ref(null)
const total = ref(0)
const filter = ref('')
const error = ref('')
const triggering = ref(false)
const issueKey = ref('')
const onUnauthorized = inject('onUnauthorized')

// ---- 同步 Linear ----
const syncOpen = ref(false)
const syncLoading = ref(false)
const syncError = ref('')
const issues = ref([])
const detail = ref(null)
const detailLoading = ref(false)
const starting = ref(false)
const startError = ref('')
// 已有进行中任务的 issue key —— 同一 issue 同时只能有一个活任务，
// 在界面上提前拦住，比让人点了再撞唯一索引的报错好。
const activeKeys = ref(new Set())

// Linear 优先级数值 → 中文
const PRIORITY = { 1: '紧急', 2: '高', 3: '中', 4: '低' }
const priorityLabel = (p) => PRIORITY[p] || '—'

// 活跃状态：看板默认最关心这些
const ACTIVE = 'queued,triaging,implementing,verifying,pr_open,review_feedback'

let timer = null

async function load() {
  try {
    const [t, s] = await Promise.all([
      api.tasks({ state: filter.value || undefined, limit: 100 }),
      api.stats(),
    ])
    tasks.value = t.tasks || []
    total.value = t.total
    stats.value = s
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function trigger() {
  if (!issueKey.value.trim()) return
  triggering.value = true
  try {
    await api.trigger(issueKey.value.trim())
    issueKey.value = ''
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    triggering.value = false
  }
}

function setFilter(v) {
  filter.value = v
  load()
}

async function openSync() {
  syncOpen.value = true
  detail.value = null
  syncError.value = ''
  syncLoading.value = true
  try {
    const [res, active] = await Promise.all([
      api.linearIssues(),
      api.tasks({ state: ACTIVE, limit: 100 }),
    ])
    issues.value = res.issues || []
    activeKeys.value = new Set((active.tasks || []).map((t) => t.linearIssueKey))
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    syncError.value = e.message
  } finally {
    syncLoading.value = false
  }
}

async function openDetail(issue) {
  detail.value = null
  detailLoading.value = true
  startError.value = ''
  try {
    detail.value = await api.linearIssue(issue.id)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    syncError.value = e.message
  } finally {
    detailLoading.value = false
  }
}

async function start() {
  if (!detail.value) return
  starting.value = true
  startError.value = ''
  try {
    await api.startIssue(detail.value.id, detail.value.identifier)
    closeSync()
    await load()
  } catch (e) {
    startError.value = e.message
  } finally {
    starting.value = false
  }
}

function closeSync() {
  syncOpen.value = false
  detail.value = null
}

onMounted(() => {
  load()
  // 自动刷新：任务在后台流转，页面要能跟着动
  timer = setInterval(load, 5000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <div v-if="stats" class="stats">
    <div class="card stat">
      <div class="stat-num">{{ stats.active }}</div>
      <div class="dim">进行中</div>
    </div>
    <div class="card stat">
      <div class="stat-num">{{ stats.byState?.failed || 0 }}</div>
      <div class="dim">失败</div>
    </div>
    <div class="card stat">
      <div class="stat-num">{{ stats.byState?.blocked_spec || 0 }}</div>
      <div class="dim">待补充需求</div>
    </div>
    <div class="card stat">
      <div class="stat-num">
        {{ stats.successRate < 0 ? '—' : Math.round(stats.successRate * 100) + '%' }}
      </div>
      <div class="dim">成功率</div>
    </div>
  </div>

  <div class="card toolbar">
    <div class="wrap">
      <button :class="{ primary: filter === '' }" @click="setFilter('')">全部</button>
      <button :class="{ primary: filter === ACTIVE }" @click="setFilter(ACTIVE)">进行中</button>
      <button :class="{ primary: filter === 'failed' }" @click="setFilter('failed')">失败</button>
      <button :class="{ primary: filter === 'blocked_spec' }" @click="setFilter('blocked_spec')">
        待补充
      </button>
      <button :class="{ primary: filter === 'merged' }" @click="setFilter('merged')">已合并</button>
    </div>
    <form class="row" @submit.prevent="trigger">
      <button type="button" @click="openSync">同步 Linear</button>
      <input v-model="issueKey" placeholder="手动触发：CR-1326" />
      <button class="primary" :disabled="triggering || !issueKey.trim()">
        {{ triggering ? '排队中…' : '触发' }}
      </button>
    </form>
  </div>

  <div class="card scroll-x" style="padding: 0">
    <table v-if="tasks.length">
      <thead>
        <tr>
          <th>Issue</th>
          <th>状态</th>
          <th>类型</th>
          <th>分支</th>
          <th>PR</th>
          <th>更新时间</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="t in tasks" :key="t.id">
          <td>
            <RouterLink :to="`/tasks/${t.id}`" class="mono">{{ t.linearIssueKey }}</RouterLink>
            <div class="faint mono">{{ t.providerRepo }}</div>
          </td>
          <td>
            <span class="badge" :class="stateTone(t.state)">{{ stateLabel(t.state) }}</span>
            <div v-if="t.failureReason" class="faint failure">{{ t.failureReason }}</div>
          </td>
          <td class="dim">{{ t.taskKind || '—' }}</td>
          <td class="mono dim">{{ t.branchName || '—' }}</td>
          <td>
            <a v-if="t.prUrl" :href="t.prUrl" target="_blank" rel="noopener">查看</a>
            <span v-else class="faint">—</span>
          </td>
          <td class="dim">{{ formatTime(t.updatedAt) }}</td>
        </tr>
      </tbody>
    </table>
    <div v-else class="empty">
      暂无任务。点「同步 Linear」挑一个指派给你的 issue，或手动输入 issue 编号触发。
    </div>
  </div>

  <p class="faint" style="margin-top: 12px">共 {{ total }} 条 · 每 5 秒自动刷新</p>

  <!-- 同步 Linear 弹窗：列表 → 详情 → 开始执行 -->
  <div v-if="syncOpen" class="overlay" @click.self="closeSync">
    <div class="card modal">
      <div class="modal-head">
        <div class="row" style="gap: 8px">
          <button v-if="detail || detailLoading" @click="detail = null">← 返回</button>
          <h2>{{ detail || detailLoading ? 'Issue 详情' : '同步 Linear' }}</h2>
        </div>
        <button @click="closeSync">✕</button>
      </div>

      <div v-if="syncError" class="error-banner small">{{ syncError }}</div>

      <!-- 详情视图 -->
      <div v-if="detailLoading" class="faint" style="padding: 24px 0; text-align: center">
        加载中…
      </div>
      <div v-else-if="detail" class="detail">
        <div class="row wrap" style="gap: 8px; align-items: center">
          <span class="mono">{{ detail.identifier }}</span>
          <span class="badge idle">{{ detail.state }}</span>
          <span v-for="l in detail.labels" :key="l" class="badge run">{{ l }}</span>
          <span class="faint">优先级：{{ priorityLabel(detail.priority) }}</span>
          <a :href="detail.url" target="_blank" rel="noopener">在 Linear 打开 ↗</a>
        </div>
        <h3 class="detail-title">{{ detail.title }}</h3>
        <div class="desc">{{ detail.description || '（无描述）' }}</div>
        <template v-if="detail.comments?.length">
          <div class="label">评论（{{ detail.comments.length }}）</div>
          <div v-for="c in detail.comments" :key="c.id" class="comment">
            <span class="faint">{{ c.userName || '（未知）' }}：</span>{{ c.body }}
          </div>
        </template>

        <div v-if="startError" class="error-banner small">{{ startError }}</div>
        <div class="row" style="margin-top: 14px">
          <button
            class="primary"
            :disabled="starting || activeKeys.has(detail.identifier)"
            @click="start"
          >
            {{
              starting
                ? '排队中…'
                : activeKeys.has(detail.identifier)
                  ? '已有进行中的任务'
                  : '开始执行'
            }}
          </button>
        </div>
      </div>

      <!-- 列表视图 -->
      <template v-else>
        <div v-if="syncLoading" class="faint" style="padding: 24px 0; text-align: center">
          同步中…
        </div>
        <table v-else-if="issues.length">
          <thead>
            <tr>
              <th>Issue</th>
              <th>标题</th>
              <th>状态</th>
              <th>优先级</th>
              <th>更新于</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="i in issues" :key="i.id" class="clickable" @click="openDetail(i)">
              <td class="mono">
                {{ i.identifier }}
                <span v-if="activeKeys.has(i.identifier)" class="badge run">进行中</span>
              </td>
              <td>{{ i.title }}</td>
              <td><span class="badge idle">{{ i.state }}</span></td>
              <td class="dim">{{ priorityLabel(i.priority) }}</td>
              <td class="dim">{{ formatTime(i.updatedAt) }}</td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">没有指派给你、尚未完结的 issue。</div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 12px;
  margin-bottom: 16px;
}
.stat { text-align: center; }
.stat-num { font-size: 28px; font-weight: 600; line-height: 1.2; }

.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}

.failure {
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  margin-top: 3px;
  font-size: 12px;
}

.overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: 8vh 16px 16px;
  z-index: 20;
}
.modal {
  width: 720px;
  max-width: 100%;
  max-height: 80vh;
  overflow-y: auto;
}
.modal-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
}
.modal-head h2 {
  margin: 0;
  font-size: 16px;
}

.clickable {
  cursor: pointer;
}
.clickable:hover td {
  background: var(--surface-2);
}

.detail-title {
  margin: 12px 0 0;
  font-size: 15px;
}
.desc {
  white-space: pre-wrap;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  margin: 10px 0;
  font-size: 13px;
  max-height: 240px;
  overflow-y: auto;
}
.comment {
  border-top: 1px solid var(--border);
  padding: 8px 0;
  font-size: 13px;
  white-space: pre-wrap;
}
</style>
