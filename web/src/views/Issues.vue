<script setup>
import { ref, onMounted, inject } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, formatTime } from '../api'
import BaseDialog from '../components/BaseDialog.vue'

// 工单页（docs/09-internal-issues.md）：手动维护的需求列表。
// 不依赖 Linear —— 在这里建单、写需求，开跑后任务流向「任务看板」。

const router = useRouter()

const issues = ref([])
const total = ref(0)
const loading = ref(false)
const error = ref('')
const stateFilter = ref('')

const onUnauthorized = inject('onUnauthorized')

// ---------- 新建工单 ----------
const showCreate = ref(false)
const repos = ref([])
const createForm = ref({ repoId: null, title: '', description: '', priority: 0 })
const creating = ref(false)
const createError = ref('')

const STATE_LABEL = { open: '待处理', in_progress: '进行中', done: '已完成', cancelled: '已取消' }
const issueStateLabel = (s) => STATE_LABEL[s] || s
const issueStateTone = (s) =>
  ({ open: 'idle', in_progress: 'run', done: 'ok', cancelled: 'idle' })[s] || 'idle'

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await api.issues({ state: stateFilter.value || undefined, limit: 100 })
    issues.value = r.issues || []
    total.value = r.total || 0
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function openCreate() {
  showCreate.value = true
  createError.value = ''
  if (!repos.value.length) {
    try {
      const r = await api.repos()
      repos.value = r.repos || []
      if (repos.value.length && !createForm.value.repoId) {
        createForm.value.repoId = repos.value[0].id
      }
    } catch (e) {
      if (e instanceof UnauthorizedError) return onUnauthorized()
      createError.value = e.message
    }
  }
}

async function submitCreate() {
  if (!createForm.value.repoId || !createForm.value.title.trim()) return
  creating.value = true
  createError.value = ''
  try {
    const r = await api.createIssue({
      repoId: createForm.value.repoId,
      title: createForm.value.title.trim(),
      description: createForm.value.description,
      priority: createForm.value.priority || 0,
    })
    showCreate.value = false
    createForm.value = { repoId: createForm.value.repoId, title: '', description: '', priority: 0 }
    // 建完直达详情页：补描述、开跑都在那儿
    router.push(`/issues/${r.issue.id}`)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    createError.value = e.message
  } finally {
    creating.value = false
  }
}

onMounted(load)
</script>

<template>
  <div>
    <div class="spread" style="margin-bottom: 16px">
      <div>
        <h2 style="margin: 0">工单</h2>
        <p class="dim" style="margin: 4px 0 0">
          手动维护的需求列表 —— 不经过 Linear。建单、写需求、开跑；agent 的提问会落在工单评论区。
        </p>
      </div>
      <button class="primary" @click="openCreate">新建工单</button>
    </div>

    <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

    <div class="card toolbar-row" style="margin-bottom: 12px">
      <label class="row" style="gap: 6px">
        <span class="dim">状态</span>
        <select v-model="stateFilter" @change="load">
          <option value="">全部</option>
          <option value="open">待处理</option>
          <option value="in_progress">进行中</option>
          <option value="done">已完成</option>
          <option value="cancelled">已取消</option>
        </select>
      </label>
      <span class="dim" style="margin-left: auto">{{ total }} 个工单</span>
    </div>

    <div v-if="loading" class="card empty">加载中…</div>
    <div v-else-if="!issues.length" class="card empty">
      还没有工单。点右上角「新建工单」，写清标题和需求描述，就能开跑。
    </div>
    <div v-else class="card scroll-x" style="padding: 0">
      <table>
        <thead>
          <tr><th>编号</th><th>标题</th><th>状态</th><th>优先级</th><th>更新时间</th></tr>
        </thead>
        <tbody>
          <tr v-for="it in issues" :key="it.id">
            <td><RouterLink :to="`/issues/${it.id}`" class="mono">{{ it.key }}</RouterLink></td>
            <td>{{ it.title }}</td>
            <td><span class="badge" :class="issueStateTone(it.state)">{{ issueStateLabel(it.state) }}</span></td>
            <td class="dim">{{ it.priority || '—' }}</td>
            <td class="dim">{{ formatTime(it.updatedAt) }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 新建对话框：基座管 Escape/焦点/aria，这里只留业务字段 -->
    <BaseDialog v-if="showCreate" label-id="create-issue-title" @close="showCreate = false">
      <h3 id="create-issue-title" style="margin-top: 0">新建工单</h3>
      <div v-if="createError" role="alert" class="error-banner">{{ createError }}</div>
      <div v-if="!repos.length" class="empty">
        你名下还没有仓库 —— 先到「仓库配置」页登记一个，工单需要知道任务在哪个仓库上执行。
      </div>
      <template v-else>
        <label class="field">
          <span>仓库</span>
          <select v-model="createForm.repoId">
            <option v-for="r in repos" :key="r.id" :value="r.id">{{ r.providerRepo }}</option>
          </select>
        </label>
        <label class="field">
          <span>标题</span>
          <input v-model="createForm.title" placeholder="一句话说清要干什么" data-autofocus />
        </label>
        <label class="field">
          <span>需求描述（markdown，可后补）</span>
          <textarea v-model="createForm.description" rows="8"
            placeholder="现象 / 期望行为 / 复现步骤。写不清的话 agent 会在评论区提问。"></textarea>
        </label>
        <label class="field">
          <span>优先级（数值大的先被调度，0 为默认）</span>
          <input v-model.number="createForm.priority" type="number" style="width: 120px" />
        </label>
        <div class="row" style="justify-content: flex-end; gap: 8px">
          <button @click="showCreate = false">取消</button>
          <button class="primary" :disabled="creating || !createForm.title.trim()" @click="submitCreate">
            {{ creating ? '创建中…' : '创建' }}
          </button>
        </div>
      </template>
    </BaseDialog>
  </div>
</template>

<style scoped>
.toolbar-row { display: flex; align-items: center; padding: 8px 12px; }
</style>
