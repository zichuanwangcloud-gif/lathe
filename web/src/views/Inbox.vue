<script setup>
// 收件箱（docs/features/inbox.md）：流水线上刻意停下来等你的事，集中在一页。
//
// 这一页只读状态、只领路：放行、重试、回答、确认节、批准都在原来的详情页做，
// 那里有验证报告、重试预览、复核发现这些决策上下文。这里不给任何绕过它们的
// 快捷按钮，「去处理」= 标已读 + 跳转。
import { ref, computed, onMounted, onUnmounted, inject } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, formatTime } from '../api'
import { confirmDialog } from '../confirm'
import { INBOX_KINDS, bumpInbox } from '../inbox'

const POLL_MS = 30000

const onUnauthorized = inject('onUnauthorized')
const router = useRouter()

const tab = ref('unread') // unread | all
const items = ref([])
const unread = ref(0)
const total = ref(0)
const loading = ref(true)
const error = ref('')
const notice = ref('')
const busy = ref('')

const groups = computed(() =>
  INBOX_KINDS.map((k) => ({ ...k, items: items.value.filter((it) => it.kind === k.kind) })).filter(
    (g) => g.items.length,
  ),
)

function handle(e) {
  if (e instanceof UnauthorizedError) {
    onUnauthorized()
    return true
  }
  return false
}

async function load() {
  try {
    const r = await api.inbox({ filter: tab.value })
    items.value = r.items || []
    unread.value = r.unread
    total.value = r.total
    error.value = ''
  } catch (e) {
    if (!handle(e)) error.value = e.message
  } finally {
    loading.value = false
  }
}

function setTab(v) {
  tab.value = v
  load()
}

function flash(msg) {
  notice.value = msg
  setTimeout(() => {
    if (notice.value === msg) notice.value = ''
  }, 3000)
}

// 409：这条事项的停留已经结束（状态变了），用服务端给的当前事项就地替换
function replaceStale(key, current) {
  const i = items.value.findIndex((it) => it.key === key)
  if (i < 0) return
  if (current) items.value.splice(i, 1, current)
  else items.value.splice(i, 1)
  flash('已更新：这件事的状态刚刚变了')
}

async function mark(it, op) {
  busy.value = it.key
  try {
    const fn = { read: api.inboxRead, unread: api.inboxUnread, dismiss: api.inboxDismiss }[op]
    const r = await fn(it.key)
    if (op === 'dismiss') {
      items.value = items.value.filter((x) => x.key !== it.key)
    } else if (r.item) {
      const i = items.value.findIndex((x) => x.key === it.key)
      if (i >= 0) items.value.splice(i, 1, r.item)
    }
    bumpInbox()
    await load()
  } catch (e) {
    if (handle(e)) return
    if (e.status === 409 && e.data && 'current' in e.data) replaceStale(it.key, e.data.current)
    else error.value = e.message
  } finally {
    busy.value = ''
  }
}

// 去处理：先标已读，不论成功失败都跳转 —— 标记失败不能挡着人去处理。
async function go(it) {
  try {
    await api.inboxRead(it.key)
    bumpInbox()
  } catch {
    /* 标记失败不影响跳转 */
  }
  router.push(it.link)
}

async function dismiss(it) {
  const ok = await confirmDialog(
    `忽略「${it.title}」？它会从收件箱消失；同一任务以后再失败会重新出现。看板的「失败」筛选里仍能找到它。`,
  )
  if (ok) mark(it, 'dismiss')
}

async function readAll() {
  if (!(await confirmDialog(`把当前 ${unread.value} 条未读全部标为已读？`))) return
  try {
    const r = await api.inboxReadAll()
    flash(`已标记 ${r.marked} 条`)
    bumpInbox()
    await load()
  } catch (e) {
    if (!handle(e)) error.value = e.message
  }
}

const absTime = (iso) => (iso ? new Date(iso).toLocaleString('zh-CN', { hour12: false }) : '')

let timer = null
onMounted(() => {
  load()
  timer = setInterval(() => {
    if (document.visibilityState !== 'hidden') load()
  }, POLL_MS)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

  <div class="card toolbar">
    <div class="wrap">
      <button :class="{ primary: tab === 'unread' }" :aria-pressed="tab === 'unread'" @click="setTab('unread')">
        未读 <span class="tab-count">{{ unread }}</span>
      </button>
      <button :class="{ primary: tab === 'all' }" :aria-pressed="tab === 'all'" @click="setTab('all')">
        全部 <span class="tab-count">{{ total }}</span>
      </button>
    </div>
    <div class="row">
      <span v-if="notice" class="dim" role="status">{{ notice }}</span>
      <button :disabled="!unread" @click="readAll">全部标为已读</button>
    </div>
  </div>

  <div v-if="loading" class="empty">加载中…</div>
  <div v-else-if="!items.length" class="card empty">
    <template v-if="tab === 'unread' && total">没有未读的事，切到「全部」看看已读的。</template>
    <template v-else>没有在等你处理的事</template>
  </div>

  <section v-for="g in groups" :key="g.kind" class="card group">
    <h3 class="group-title">
      <span class="badge" :class="g.tone">{{ g.label }}</span>
      <span class="faint">{{ g.items.length }}</span>
    </h3>
    <ul class="items">
      <li v-for="it in g.items" :key="it.key" class="item" :class="{ unread: !it.read }">
        <span class="dot" :aria-label="it.read ? '已读' : '未读'"></span>
        <div class="main">
          <div class="spread">
            <span class="row title-row">
              <span class="badge" :class="g.tone">{{ g.label }}</span>
              <span class="title">{{ it.title }}</span>
            </span>
            <span class="faint when" :title="absTime(it.since)">{{ formatTime(it.since) }}</span>
          </div>
          <div class="faint mono">{{ it.repo }}</div>
          <div v-if="it.reason" class="reason dim" :title="it.reason">{{ it.reason }}</div>
          <div class="wrap actions">
            <button class="primary" @click="go(it)">去处理</button>
            <button :disabled="busy === it.key" @click="mark(it, it.read ? 'unread' : 'read')">
              {{ it.read ? '标为未读' : '标为已读' }}
            </button>
            <button v-if="it.dismissible" :disabled="busy === it.key" @click="dismiss(it)">忽略</button>
            <RouterLink v-for="l in it.extra || []" :key="l.href" :to="l.href" class="extra">{{ l.label }}</RouterLink>
          </div>
        </div>
      </li>
    </ul>
  </section>

  <p class="faint" style="margin-top: 12px">每 30 秒自动刷新 · 处理动作都在原来的详情页里完成</p>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}
.tab-count { opacity: .75; margin-left: 2px; }
.group { margin-bottom: 14px; padding: 12px 16px; }
.group-title { display: flex; align-items: center; gap: 8px; margin: 0 0 6px; font-size: 14px; font-weight: 500; }
.items { list-style: none; margin: 0; padding: 0; }
.item { display: flex; gap: 10px; padding: 10px 0; border-top: 1px solid var(--border); }
.item:first-child { border-top: none; }
.dot { flex: none; width: 8px; height: 8px; margin-top: 7px; border-radius: 50%; background: transparent; }
.item.unread .dot { background: var(--accent); }
.item.unread .title { font-weight: 600; }
.main { flex: 1; min-width: 0; }
.title-row { min-width: 0; gap: 8px; }
.title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.when { flex: none; font-size: 12px; }
.reason { margin-top: 4px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.actions { margin-top: 8px; align-items: center; }
.extra { font-size: 13px; }
</style>
