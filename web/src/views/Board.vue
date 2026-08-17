<script setup>
import { ref, onMounted, onUnmounted, inject } from 'vue'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime } from '../api'
import { hasLinearToken } from '../auth'

const tasks = ref([])
const stats = ref(null)
const total = ref(0)
const filter = ref('')
const error = ref('')
const onUnauthorized = inject('onUnauthorized')

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

function setFilter(v) {
  filter.value = v
  load()
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
      <template v-if="hasLinearToken()">
        暂无任务。到「Linear 任务」挑一个指派给你的 issue 开始执行，或在 Linear 里把 issue
        指派给你自己（webhook 自动接单）。
      </template>
      <template v-else>
        暂无任务。先在「个人设置」绑定 Linear API 令牌，即可浏览并执行指派给你的 issue。
      </template>
    </div>
  </div>

  <p class="faint" style="margin-top: 12px">共 {{ total }} 条 · 每 5 秒自动刷新</p>
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
</style>
