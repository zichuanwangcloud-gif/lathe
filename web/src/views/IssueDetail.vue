<script setup>
import { ref, onMounted, inject, computed } from 'vue'
import { RouterLink } from 'vue-router'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { api, UnauthorizedError, formatTime, stateLabel, stateTone } from '../api'
import { confirmDialog } from '../confirm'

// 工单详情页（docs/09-internal-issues.md）：需求本体 + 评论区问答 +
// 关联任务。agent 的提问（blocked_spec）与人的补充在同一条评论流里 ——
// 提问模式与 Linear 路径完全一致，只是评论区在 Lathe 里。

const props = defineProps({ id: { type: String, required: true } })

const issue = ref(null)
const comments = ref([])
const tasks = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')

const editing = ref(false)
const editForm = ref({ title: '', description: '', priority: 0 })
const acting = ref(false)

const newComment = ref('')
const commenting = ref(false)

const onUnauthorized = inject('onUnauthorized')

// marked 出 HTML 后必须过一层消毒（与 TaskDetail.vue 同一管线）。
marked.setOptions({ breaks: true })
const md = (s) => DOMPurify.sanitize(marked.parse(s || ''))

const STATE_LABEL = { open: '待处理', in_progress: '进行中', done: '已完成', cancelled: '已取消' }
const issueStateLabel = (s) => STATE_LABEL[s] || s
const issueStateTone = (s) =>
  ({ open: 'idle', in_progress: 'run', done: 'ok', cancelled: 'idle' })[s] || 'idle'

const TERMINAL = new Set(['merged', 'failed', 'cancelled'])
const activeTask = computed(() => (tasks.value || []).find((t) => !TERMINAL.has(t.state)) || null)
const canStart = computed(
  () => issue.value && (issue.value.state === 'open' || issue.value.state === 'done') && !activeTask.value,
)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await api.issue(props.id)
    issue.value = r.issue
    comments.value = r.comments || []
    tasks.value = r.tasks || []
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function startEdit() {
  editForm.value = {
    title: issue.value.title,
    description: issue.value.description,
    priority: issue.value.priority,
  }
  editing.value = true
}

async function saveEdit() {
  acting.value = true
  error.value = ''
  try {
    const r = await api.updateIssue(props.id, {
      title: editForm.value.title.trim(),
      description: editForm.value.description,
      priority: editForm.value.priority || 0,
    })
    issue.value = r.issue
    editing.value = false
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    acting.value = false
  }
}

async function setState(state, confirmText) {
  if (confirmText && !(await confirmDialog(confirmText))) return
  acting.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.updateIssue(props.id, { state })
    issue.value = r.issue
    if (r.warning) notice.value = r.warning
    if (state === 'cancelled' && (r.cancelledTasks || []).length) {
      notice.value = `已联动取消 ${r.cancelledTasks.length} 个在途任务：#${r.cancelledTasks.join('、#')}（在途 agent 会跑完当前轮次后停下——与 Linear 取消联动同一语义）`
    }
    await load()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    acting.value = false
  }
}

async function start() {
  acting.value = true
  error.value = ''
  try {
    await api.startInternalIssue(props.id)
    notice.value = '已入队 —— 任务流向「任务看板」，这里的状态会自动跟进。'
    await load()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    acting.value = false
  }
}

async function removeIssue() {
  if (!(await confirmDialog('确认删除这个工单？评论会一并删除。（有关联任务的工单只能取消、不能删除）'))) return
  acting.value = true
  error.value = ''
  try {
    await api.deleteIssue(props.id)
    window.history.length > 1 ? history.back() : (location.href = '/issues')
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
    acting.value = false
  }
}

async function submitComment() {
  const body = newComment.value.trim()
  if (!body) return
  commenting.value = true
  error.value = ''
  try {
    await api.addIssueComment(props.id, body)
    newComment.value = ''
    await load()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    commenting.value = false
  }
}

// agent 评论的作者标记 'task-<id>' → 可点跳任务详情
function commentTaskId(c) {
  if (!c.actor) return null
  const m = /^task-(\d+)$/.exec(c.actor)
  return m ? m[1] : null
}

onMounted(load)
</script>

<template>
  <div v-if="loading" class="card empty">加载中…</div>
  <div v-else-if="error && !issue" class="card empty">{{ error }}</div>
  <div v-else-if="issue">
    <div class="spread" style="margin-bottom: 16px">
      <div>
        <div class="row" style="gap: 10px">
          <h2 class="mono" style="margin: 0">{{ issue.key }}</h2>
          <span class="badge" :class="issueStateTone(issue.state)">{{ issueStateLabel(issue.state) }}</span>
        </div>
        <p class="dim" style="margin: 4px 0 0">
          创建于 {{ formatTime(issue.createdAt) }} · 更新于 {{ formatTime(issue.updatedAt) }}
        </p>
      </div>
      <RouterLink to="/issues" class="dim">← 返回工单列表</RouterLink>
    </div>

    <div v-if="error" role="alert" class="error-banner">{{ error }}</div>
    <div v-if="notice" role="alert" class="error-banner" style="color: var(--ok); border-color: var(--ok)">{{ notice }}</div>

    <!-- 本体 -->
    <div class="card" style="margin-bottom: 12px">
      <template v-if="!editing">
        <div class="spread">
          <h3 style="margin: 0">{{ issue.title }}</h3>
          <button @click="startEdit">编辑</button>
        </div>
        <div v-if="issue.description" class="md" v-html="md(issue.description)" style="margin-top: 8px"></div>
        <p v-else class="dim" style="margin: 8px 0 0">（无描述 —— 描述不清的单子，agent 会在下方评论区提问）</p>
        <p class="dim" style="margin: 8px 0 0; font-size: 12px">优先级：{{ issue.priority || '—' }}</p>
      </template>
      <template v-else>
        <label class="field">
          <span>标题</span>
          <input v-model="editForm.title" />
        </label>
        <label class="field">
          <span>需求描述（markdown）</span>
          <textarea v-model="editForm.description" rows="10"></textarea>
        </label>
        <label class="field">
          <span>优先级</span>
          <input v-model.number="editForm.priority" type="number" style="width: 120px" />
        </label>
        <div class="row" style="gap: 8px">
          <button class="primary" :disabled="acting || !editForm.title.trim()" @click="saveEdit">
            {{ acting ? '保存中…' : '保存' }}
          </button>
          <button @click="editing = false">取消</button>
        </div>
      </template>
    </div>

    <!-- 动作 -->
    <div class="card row" style="margin-bottom: 12px; gap: 8px; flex-wrap: wrap">
      <button
        v-if="canStart"
        class="primary"
        :disabled="acting"
        @click="start"
      >开始执行</button>
      <span v-else-if="activeTask" class="dim">
        进行中：
        <RouterLink :to="`/tasks/${activeTask.id}`" class="mono">任务 #{{ activeTask.id }}</RouterLink>
        <span class="badge" :class="stateTone(activeTask.state)" style="margin-left: 6px">
          {{ stateLabel(activeTask.state) }}
        </span>
      </span>
      <span v-else-if="issue.state === 'cancelled'" class="dim">已取消 —— 重新打开后才能再开跑</span>

      <span style="flex: 1"></span>

      <button v-if="issue.state === 'open'" :disabled="acting" @click="setState('in_progress')">标为进行中</button>
      <button v-if="issue.state === 'in_progress' && !activeTask" :disabled="acting" @click="setState('open')">打回待处理</button>
      <button v-if="issue.state === 'in_progress'" :disabled="acting" @click="setState('done')">标为完成</button>
      <button
        v-if="issue.state === 'done' || issue.state === 'cancelled'"
        :disabled="acting"
        @click="setState('open')"
      >重新打开</button>
      <button
        v-if="issue.state !== 'cancelled'"
        class="danger"
        :disabled="acting"
        @click="setState('cancelled', '确认取消这个工单？在途任务会被联动取消。')"
      >取消工单</button>
      <button v-if="!tasks.length" class="danger" :disabled="acting" @click="removeIssue">删除</button>
    </div>

    <!-- 关联任务 -->
    <div v-if="tasks.length" class="card" style="margin-bottom: 12px">
      <div class="label" style="margin-bottom: 8px">关联任务</div>
      <table>
        <thead><tr><th>任务</th><th>状态</th><th>分支</th><th>PR</th><th>创建时间</th></tr></thead>
        <tbody>
          <tr v-for="t in tasks" :key="t.id">
            <td><RouterLink :to="`/tasks/${t.id}`" class="mono">#{{ t.id }}</RouterLink></td>
            <td><span class="badge" :class="stateTone(t.state)">{{ stateLabel(t.state) }}</span></td>
            <td class="mono dim">{{ t.branchName || '—' }}</td>
            <td>
              <a v-if="t.prUrl" :href="t.prUrl" target="_blank" rel="noopener">查看</a>
              <span v-else class="dim">—</span>
            </td>
            <td class="dim">{{ formatTime(t.createdAt) }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 评论区：人的补充与 agent 的提问在同一条流里 -->
    <div class="card">
      <div class="label" style="margin-bottom: 8px">
        评论（{{ comments.length }}）
        <span class="dim" style="font-weight: normal">
          —— agent 判定需求不清时会在这里提问；你回复后，到看板重试任务即可让它带着新信息继续。
        </span>
      </div>

      <div v-if="!comments.length" class="dim" style="padding: 8px 0">还没有评论。</div>
      <div v-for="c in comments" :key="c.id" class="comment" :class="{ agent: !!c.actor }">
        <div class="comment-head">
          <template v-if="c.actor">
            <RouterLink v-if="commentTaskId(c)" :to="`/tasks/${commentTaskId(c)}`" class="mono">
              lathe · 任务 #{{ commentTaskId(c) }}
            </RouterLink>
            <span v-else class="mono">{{ c.actor }}</span>
            <span class="badge run" style="margin-left: 6px">agent</span>
          </template>
          <span v-else>{{ c.authorName }}</span>
          <span class="dim" style="margin-left: 8px">{{ formatTime(c.createdAt) }}</span>
        </div>
        <div class="md" v-html="md(c.body)"></div>
      </div>

      <div class="comment-box">
        <textarea
          v-model="newComment"
          rows="3"
          placeholder="补充复现步骤、澄清期望行为……（markdown）"
        ></textarea>
        <div class="row" style="justify-content: flex-end; margin-top: 6px">
          <button class="primary" :disabled="commenting || !newComment.trim()" @click="submitComment">
            {{ commenting ? '发送中…' : '评论' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.field { display: block; margin: 10px 0; }
.field > span { display: block; font-size: 12px; color: var(--text-dim); margin-bottom: 4px; }
.label { font-size: 12.5px; color: var(--text-dim); font-weight: 500; }
.comment { border-top: 1px solid var(--border); padding: 10px 0; }
.comment.agent { background: var(--run-bg); border-radius: 6px; padding: 10px; margin: 4px 0; }
.comment-head { font-size: 12px; margin-bottom: 4px; }
.comment-box { border-top: 1px solid var(--border); padding-top: 10px; margin-top: 10px; }
.comment-box textarea { width: 100%; box-sizing: border-box; }
/* markdown 正文排版（与 TaskDetail 同一套，作用域内自给自足） */
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) { margin: 12px 0 6px; font-size: 14.5px; }
.md :deep(p) { margin: 6px 0; }
.md :deep(ul), .md :deep(ol) { margin: 6px 0; padding-left: 22px; }
.md :deep(code) {
  font-family: var(--mono); font-size: 12px;
  background: var(--bg); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 5px;
}
.md :deep(pre) { margin: 8px 0; }
.md :deep(pre code) { background: none; border: none; padding: 0; }
.md :deep(a) { word-break: break-all; }
</style>
