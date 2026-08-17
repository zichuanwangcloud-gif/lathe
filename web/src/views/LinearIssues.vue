<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime } from '../api'

// Linear 任务页：浏览「指派给我、尚未完结」的 issue，看完详情再决定执行。
// 这里只做接单入口；任务一旦入队，后续流转回「任务看板」看。

const issues = ref([])
const loading = ref(false)
const error = ref('')
const errorIsCreds = ref(false)

const detail = ref(null)
const detailLoading = ref(false)

const starting = ref(false)
const startError = ref('')
const startedKey = ref('')

const triggering = ref(false)
const issueKey = ref('')

// 平台侧任务按 issue key 建的索引：接口按更新时间倒序返回，
// 每个 key 第一次出现即最新任务 —— 让 issue 行能直接看到
// 「这个单子在平台上做到哪了」。
const taskByKey = ref(new Map())

// 活跃状态与数据库部分唯一索引一致：除终结态外都算活跃，
// 此时同一 issue 不允许再入队，界面上提前拦住。
const TERMINAL = new Set(['merged', 'failed', 'cancelled'])
const isActive = (t) => t && !TERMINAL.has(t.state)

const onUnauthorized = inject('onUnauthorized')

// Linear 优先级数值 → 中文
const PRIORITY = { 1: '紧急', 2: '高', 3: '中', 4: '低' }
const priorityLabel = (p) => PRIORITY[p] || '—'

async function load() {
  loading.value = true
  error.value = ''
  errorIsCreds.value = false
  try {
    const [res, mine] = await Promise.all([
      api.linearIssues(),
      api.tasks({ limit: 100 }),
    ])
    issues.value = res.issues || []
    const map = new Map()
    for (const t of mine.tasks || []) {
      if (!map.has(t.linearIssueKey)) map.set(t.linearIssueKey, t)
    }
    taskByKey.value = map
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
    // 凭据类错误（未配置/解密失败）由服务端 400 给出指引，配上直达入口
    errorIsCreds.value = e.status === 400
  } finally {
    loading.value = false
  }
}

async function openDetail(issue) {
  detail.value = null
  detailLoading.value = true
  startError.value = ''
  startedKey.value = ''
  try {
    detail.value = await api.linearIssue(issue.id)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
    detailLoading.value = false
    return
  }
  detailLoading.value = false
}

function backToList() {
  detail.value = null
  startError.value = ''
  startedKey.value = ''
}

async function start() {
  if (!detail.value) return
  starting.value = true
  startError.value = ''
  try {
    await api.startIssue(detail.value.id, detail.value.identifier)
    startedKey.value = detail.value.identifier
    await load()
  } catch (e) {
    startError.value = e.message
  } finally {
    starting.value = false
  }
}

async function trigger() {
  const key = issueKey.value.trim()
  if (!key) return
  triggering.value = true
  error.value = ''
  try {
    await api.trigger(key)
    issueKey.value = ''
    startedKey.value = key
    await load()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    triggering.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="head">
    <h1>Linear 任务</h1>
    <form class="row" @submit.prevent="trigger">
      <button type="button" :disabled="loading" @click="load">
        {{ loading ? '刷新中…' : '刷新' }}
      </button>
      <input v-model="issueKey" placeholder="手动触发：CR-1326" />
      <button class="primary" :disabled="triggering || !issueKey.trim()">
        {{ triggering ? '排队中…' : '触发' }}
      </button>
    </form>
  </div>

  <p class="dim note">
    这里列出「指派给你、尚未完结」的 Linear issue。点开看详情，确认后再开始执行；
    执行中的流转与历史记录都在「任务看板」。
  </p>

  <div v-if="error" class="error-banner">
    {{ error }}
    <RouterLink v-if="errorIsCreds" to="/settings">前往个人设置配置 →</RouterLink>
  </div>
  <div v-if="startedKey" class="ok-banner">
    {{ startedKey }} 已入队。<RouterLink to="/">前往任务看板查看 →</RouterLink>
  </div>

  <!-- 详情视图 -->
  <div v-if="detailLoading" class="card faint center">加载中…</div>
  <div v-else-if="detail" class="card">
    <div class="row" style="margin-bottom: 12px">
      <button @click="backToList">← 返回列表</button>
    </div>

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
    <div class="row" style="margin-top: 14px; gap: 12px; align-items: center">
      <button
        class="primary"
        :disabled="starting || isActive(taskByKey.get(detail.identifier))"
        @click="start"
      >
        {{
          starting
            ? '排队中…'
            : isActive(taskByKey.get(detail.identifier))
              ? '已有进行中的任务'
              : '开始执行'
        }}
      </button>
      <RouterLink
        v-if="taskByKey.get(detail.identifier)"
        :to="`/tasks/${taskByKey.get(detail.identifier).id}`"
      >
        查看平台任务（{{ stateLabel(taskByKey.get(detail.identifier).state) }}）→
      </RouterLink>
    </div>
  </div>

  <!-- 列表视图 -->
  <div v-else class="card scroll-x" style="padding: 0">
    <div v-if="loading" class="faint center">同步中…</div>
    <table v-else-if="issues.length">
      <thead>
        <tr>
          <th>Issue</th>
          <th>标题</th>
          <th>状态</th>
          <th>优先级</th>
          <th>平台任务</th>
          <th>更新于</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="i in issues" :key="i.id" class="clickable" @click="openDetail(i)">
          <td class="mono">
            {{ i.identifier }}
            <span v-if="isActive(taskByKey.get(i.identifier))" class="badge run">进行中</span>
          </td>
          <td>{{ i.title }}</td>
          <td><span class="badge idle">{{ i.state }}</span></td>
          <td class="dim">{{ priorityLabel(i.priority) }}</td>
          <td @click.stop>
            <template v-if="taskByKey.get(i.identifier)">
              <span class="badge" :class="stateTone(taskByKey.get(i.identifier).state)">
                {{ stateLabel(taskByKey.get(i.identifier).state) }}
              </span>
              <RouterLink :to="`/tasks/${taskByKey.get(i.identifier).id}`" class="task-link">
                查看
              </RouterLink>
            </template>
            <span v-else class="faint">—</span>
          </td>
          <td class="dim">{{ formatTime(i.updatedAt) }}</td>
        </tr>
      </tbody>
    </table>
    <div v-else-if="!error" class="empty">没有指派给你、尚未完结的 issue。</div>
    <div v-else class="empty">凭据配好后点「刷新」重试。</div>
  </div>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0; }
.head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.note { font-size: 13px; max-width: 72ch; margin: 0 0 16px; }

.center { padding: 24px 0; text-align: center; }

.ok-banner {
  background: var(--ok-bg);
  color: var(--ok);
  padding: 10px 14px;
  border-radius: var(--radius);
  margin-bottom: 16px;
  font-size: 13.5px;
}

.clickable { cursor: pointer; }
.clickable:hover td { background: var(--surface-2); }

.task-link { margin-left: 6px; }

.detail-title { margin: 12px 0 0; font-size: 15px; }
.desc {
  white-space: pre-wrap;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  margin: 10px 0;
  font-size: 13px;
  max-height: 320px;
  overflow-y: auto;
}
.label { font-size: 12.5px; color: var(--text-dim); margin-bottom: 10px; font-weight: 500; }
.comment {
  border-top: 1px solid var(--border);
  padding: 8px 0;
  font-size: 13px;
  white-space: pre-wrap;
}
.error-banner.small { margin: 12px 0 0; font-size: 13px; }
</style>
