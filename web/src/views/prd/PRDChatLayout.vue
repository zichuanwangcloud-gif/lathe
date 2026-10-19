<script setup>
import { ref, computed, provide, inject, onMounted, onUnmounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api, UnauthorizedError } from '../../api'
import { prdTitle } from '../../prd'
import { createPoller } from '../../livelog'

// 模糊任务的外壳：左侧会话栏（DeepSeek / 豆包式的历史对话列表）+ 右边
// 子路由（新建页或某段会话）。
//
// 侧栏常驻、只拉一次再定时刷新；子路由按 path 重建（切会话时旧会话的
// 轮询与游标不能混进新会话）。会话页通过 inject('prdSidebar') 在状态
// 变化时主动通知侧栏重拉，不必等下一次定时刷新。

const route = useRoute()
const onUnauthorized = inject('onUnauthorized')

const prds = ref([])
const total = ref(0)
const loaded = ref(false)
const error = ref('')
const query = ref('')
const limit = ref(100)

// 窄屏下侧栏是抽屉；宽屏默认展开
const narrow = () => window.matchMedia('(max-width: 900px)').matches
const open = ref(!narrow())

async function load() {
  try {
    const r = await api.prds({ limit: limit.value })
    prds.value = r.prds || []
    total.value = r.total || 0
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    loaded.value = true
  }
}

function loadMore() {
  // 后端单页上限 200（store.ListPRDs）
  limit.value = Math.min(200, limit.value + 100)
  load()
}

// 列表接口不带 document（省流量），侧栏拿不到 §1 一句话，只能显示原始输入。
// 打开过的会话把它的标题回填进来，至少看过的那些显示得像样。
const titles = ref({})
function setTitle(id, title) {
  if (title && titles.value[id] !== title) titles.value = { ...titles.value, [id]: title }
}
const titleOf = (p) => titles.value[p.id] || prdTitle(p)

provide('prdSidebar', { refresh: load, setTitle })

// 进行中的会话在侧栏上要能看到在动，所以定时刷新；15 秒够了 —— 正在看的
// 那段会话自己会主动通知，这里兜的是「别的会话跑完了」。
let poller = null
onMounted(() => {
  load()
  poller = createPoller(load, 15000)
  poller.start()
})
onUnmounted(() => poller?.stop())

// 窄屏切会话后自动收起抽屉
watch(
  () => route.path,
  () => {
    if (narrow()) open.value = false
  },
)

// ---------------------------------------------------------------- 分组

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return prds.value
  return prds.value.filter(
    (p) => titleOf(p).toLowerCase().includes(q) || String(p.id) === q.replace(/^#/, ''),
  )
})

// 按最后更新时间分组：今天 / 昨天 / 7 天内 / 30 天内 / 更早（与 DeepSeek 一致）
const groups = computed(() => {
  const startOfToday = new Date()
  startOfToday.setHours(0, 0, 0, 0)
  const t0 = startOfToday.getTime()
  const day = 86400000
  const buckets = [
    { label: '今天', min: t0, items: [] },
    { label: '昨天', min: t0 - day, items: [] },
    { label: '7 天内', min: t0 - 6 * day, items: [] },
    { label: '30 天内', min: t0 - 29 * day, items: [] },
    { label: '更早', min: -Infinity, items: [] },
  ]
  const sorted = [...filtered.value].sort(
    (a, b) => new Date(b.updatedAt) - new Date(a.updatedAt),
  )
  for (const p of sorted) {
    const t = new Date(p.updatedAt).getTime()
    buckets.find((b) => t >= b.min).items.push(p)
  }
  return buckets.filter((b) => b.items.length)
})

// 侧栏只用一个小圆点示意状态：要人动手的（待回答 / 待批准 / 已批准待生成）
// 用醒目色，其余灰。细节进会话里看。
function dotTone(p) {
  if (p.processing) return 'run'
  if (['awaiting_answers', 'ready_for_review', 'approved'].includes(p.state)) return 'warn'
  if (p.state === 'converted') return 'ok'
  return 'idle'
}
const DOT_TITLE = {
  drafting: '起草中',
  awaiting_answers: '等你回答',
  ready_for_review: '等你批准',
  approved: '已批准，待生成任务',
  converted: '已生成任务',
  abandoned: '已搁置',
}

const activeId = computed(() => String(route.params.id || ''))
</script>

<template>
  <div class="chat-shell" :class="{ 'side-open': open }">
    <aside class="side" aria-label="模糊任务会话">
      <div class="side-top">
        <RouterLink to="/prds" class="new-btn" :class="{ on: !activeId }">
          <span aria-hidden="true">＋</span> 新建对话
        </RouterLink>
        <button class="icon" aria-label="收起侧栏" title="收起侧栏" @click="open = false">«</button>
      </div>
      <input v-model="query" class="search" type="search" placeholder="搜索会话或 #编号"
        aria-label="搜索会话" />

      <div class="list">
        <div v-if="error" class="side-msg bad">{{ error }}</div>
        <div v-else-if="!loaded" class="side-msg">加载中…</div>
        <div v-else-if="!prds.length" class="side-msg">
          还没有对话。把那句说不清的需求扔进右边的输入框试试。
        </div>
        <div v-else-if="!groups.length" class="side-msg">没有匹配的会话</div>

        <div v-for="g in groups" :key="g.label" class="group">
          <div class="g-label">{{ g.label }}</div>
          <RouterLink
            v-for="p in g.items"
            :key="p.id"
            :to="`/prds/${p.id}`"
            class="item"
            :class="{ on: activeId === String(p.id) }"
            :title="titleOf(p)"
          >
            <span class="dot" :class="dotTone(p)" :title="p.processing ? '智能体进行中' : DOT_TITLE[p.state]"></span>
            <span class="t">{{ titleOf(p) }}</span>
          </RouterLink>
        </div>

        <button v-if="loaded && prds.length < total && limit < 200" class="more" @click="loadMore">
          加载更多（共 {{ total }} 段）
        </button>
      </div>
    </aside>

    <div v-if="open" class="scrim" @click="open = false"></div>

    <div class="main">
      <button v-if="!open" class="icon reopen" aria-label="展开侧栏" title="展开侧栏" @click="open = true">☰</button>
      <RouterView :key="route.path" />
    </div>
  </div>
</template>

<style scoped>
.chat-shell { display: flex; height: 100%; min-height: 0; position: relative; }

.side {
  width: 260px;
  flex: none;
  display: none;
  flex-direction: column;
  min-height: 0;
  background: var(--surface);
  border-right: 1px solid var(--border);
}
.side-open .side { display: flex; }

.side-top { display: flex; align-items: center; gap: 6px; padding: 12px 12px 8px; }
.new-btn {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 8px 12px;
  border-radius: 12px;
  border: 1px solid var(--border);
  color: var(--text);
  background: var(--surface-2);
  font-weight: 500;
}
.new-btn:hover { text-decoration: none; border-color: var(--accent); }
.new-btn.on { border-color: var(--accent); color: var(--accent); }

.icon { border: none; background: none; padding: 4px 8px; color: var(--text-faint); font-size: 16px; }
.icon:hover:not(:disabled) { color: var(--text); }

.search { margin: 0 12px 8px; padding: 6px 10px; font-size: 13px; border-radius: 10px; }

.list { flex: 1; overflow-y: auto; padding: 0 8px 16px; }
.side-msg { padding: 20px 8px; color: var(--text-faint); font-size: 13px; text-align: center; }
.side-msg.bad { color: var(--bad); }

.group { margin-top: 10px; }
.g-label { font-size: 12px; color: var(--text-faint); padding: 4px 8px; }
.item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 8px;
  border-radius: 8px;
  color: var(--text-dim);
  font-size: 13.5px;
}
.item:hover { background: var(--surface-2); text-decoration: none; color: var(--text); }
.item.on { background: var(--run-bg); color: var(--text); }
.t { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
.dot { flex: none; width: 7px; height: 7px; border-radius: 50%; background: var(--idle); }
.dot.run { background: var(--run); animation: pulse 1.2s ease-in-out infinite; }
.dot.warn { background: var(--warn); }
.dot.ok { background: var(--ok); }
@keyframes pulse { 50% { opacity: .3; } }

.more { width: 100%; margin-top: 10px; font-size: 12.5px; background: none; }

.main { flex: 1; min-width: 0; min-height: 0; position: relative; display: flex; flex-direction: column; }
.reopen { position: absolute; top: 10px; left: 10px; z-index: 5; }

.scrim { display: none; }

@media (max-width: 900px) {
  .side { position: absolute; z-index: 20; top: 0; bottom: 0; left: 0; box-shadow: 4px 0 24px rgba(0, 0, 0, .3); }
  .scrim { display: block; position: absolute; inset: 0; z-index: 15; background: rgba(0, 0, 0, .35); }
}
</style>
