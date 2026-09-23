<script setup>
import { ref, computed, onMounted, onUnmounted, inject } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, UnauthorizedError, stateLabel, stateTone, formatTime } from '../api'
import { hasLinearToken } from '../auth'
import PreviewDialog from '../components/PreviewDialog.vue'
import CostPanel from '../components/CostPanel.vue'

const tasks = ref([])
const stats = ref(null)
const total = ref(0)
const error = ref('')
const previewTask = ref(null) // 打开预览弹窗的任务
const onUnauthorized = inject('onUnauthorized')
const route = useRoute()
const router = useRouter()

// 筛选与页码走 URL query：刷新/后退/分享链接都不丢视图状态（Deep Linking）
const PAGE_SIZE = 100
const filter = ref(typeof route.query.state === 'string' ? route.query.state : '')
const page = ref(Math.max(1, Number.parseInt(route.query.page) || 1))
const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_SIZE)))

// 活跃状态：看板默认最关心这些
const ACTIVE = 'queued,triaging,implementing,verifying,pr_open,review_feedback'

let timer = null

async function load() {
  try {
    const [t, s] = await Promise.all([
      api.tasks({ state: filter.value || undefined, limit: PAGE_SIZE, offset: (page.value - 1) * PAGE_SIZE }),
      api.stats(),
    ])
    tasks.value = t.tasks || []
    total.value = t.total
    stats.value = s
    error.value = ''
    // 自动刷新可能让当前页变空（任务流出了这个筛选）：退到最后一页而不是晾着空表
    if (!tasks.value.length && page.value > 1) {
      page.value = pages.value
      syncQuery()
      return load()
    }
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

function syncQuery() {
  router.replace({
    query: {
      ...route.query,
      state: filter.value || undefined,
      page: page.value > 1 ? String(page.value) : undefined,
    },
  })
}

function setFilter(v) {
  filter.value = v
  page.value = 1
  syncQuery()
  load()
}

function goPage(p) {
  page.value = p
  syncQuery()
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
  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

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
      <button :class="{ primary: filter === '' }" :aria-pressed="filter === ''" @click="setFilter('')">全部</button>
      <button :class="{ primary: filter === ACTIVE }" :aria-pressed="filter === ACTIVE" @click="setFilter(ACTIVE)">进行中</button>
      <button :class="{ primary: filter === 'failed' }" :aria-pressed="filter === 'failed'" @click="setFilter('failed')">失败</button>
      <button :class="{ primary: filter === 'blocked_spec' }" :aria-pressed="filter === 'blocked_spec'" @click="setFilter('blocked_spec')">
        待补充
      </button>
      <button :class="{ primary: filter === 'merged' }" :aria-pressed="filter === 'merged'" @click="setFilter('merged')">已合并</button>
    </div>
  </div>

  <CostPanel />

  <div class="card scroll-x" style="padding: 0">
    <table v-if="tasks.length">
      <thead>
        <tr>
          <th>Issue</th>
          <th>状态</th>
          <th>类型</th>
          <th>分支</th>
          <th>PR</th>
          <th>服务</th>
          <th>更新时间</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="t in tasks" :key="t.id">
          <td>
            <RouterLink :to="`/tasks/${t.id}`" class="mono">{{ t.externalKey }}</RouterLink>
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
          <td>
            <!-- 有 worktree 才能起预览；queued/triaging 还没建，merged 已回收 -->
            <button v-if="t.worktreePath" @click="previewTask = t">预览</button>
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

  <!-- 超过一页才显示翻页；筛选/页码都在 URL 里，链接可分享 -->
  <div v-if="pages > 1" class="row" style="justify-content: flex-end; margin-top: 4px">
    <button :disabled="page <= 1" @click="goPage(page - 1)">上一页</button>
    <span class="dim">第 {{ page }} / {{ pages }} 页</span>
    <button :disabled="page >= pages" @click="goPage(page + 1)">下一页</button>
  </div>

  <PreviewDialog v-if="previewTask" :task="previewTask" @close="previewTask = null" />
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
