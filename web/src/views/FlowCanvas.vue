<script setup>
import { ref, reactive, computed, onMounted, onUnmounted, inject, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, stateLabel, stateTone } from '../api'
import { hasLinearToken } from '../auth'

// 编排画布：无 :id 时是"建图"模式（从 Linear 选单、拖拽连出依赖、一键
// 入队）；有 :id 时是"查看"模式（轮询真实状态、只做视觉重排）。两态
// 共用同一套拖拽/平移/缩放/连线交互，差异只在数据来源与是否允许连线
// 建依赖——已提交的图不支持在这里改依赖关系，见 04-25 编排落地方案里
// "不做"第 1 条。

const props = defineProps({ id: { type: String, default: null } })
const isNew = computed(() => !props.id)
const router = useRouter()
const onUnauthorized = inject('onUnauthorized')

const NODE_W = 220
const NODE_H = 100

// ---------- 画布视图态（平移/缩放），两态共用 ----------
const view = reactive({ panX: 40, panY: 20, zoom: 1 })
const canvasWrap = ref(null)

function zoomBy(delta) {
  view.zoom = Math.min(1.6, Math.max(0.5, view.zoom + delta))
}
function resetView() {
  view.panX = 40
  view.panY = 20
  view.zoom = 1
}
function onWheel(ev) {
  ev.preventDefault()
  zoomBy(ev.deltaY < 0 ? 0.06 : -0.06)
}

// ---------- 节点数据：建图模式是纯前端内存态，查看模式来自轮询 ----------
const nodes = ref([]) // { id, issueKey, title, priority, dependsOn, dependsOnAt, x, y, state?, profile? }
const selectedId = ref(null)
const selectedNode = computed(() => nodes.value.find((n) => n.id === selectedId.value) || null)

function nodeById(id) {
  return nodes.value.find((n) => n.id === id) || null
}
function anchorRight(n) {
  return { x: n.x + NODE_W, y: n.y + NODE_H / 2 }
}
function anchorLeft(n) {
  return { x: n.x, y: n.y + NODE_H / 2 }
}
function edgePath(from, to) {
  const dx = Math.max(40, Math.abs(to.x - from.x) / 2)
  return `M ${from.x} ${from.y} C ${from.x + dx} ${from.y}, ${to.x - dx} ${to.y}, ${to.x} ${to.y}`
}

const edges = computed(() =>
  nodes.value
    .filter((n) => n.dependsOn != null)
    .map((n) => {
      const from = nodeById(n.dependsOn)
      if (!from) return null
      const satisfied = isNew.value
        ? null // 建图阶段还没有任何真实状态，不判定满足/等待
        : n.dependsOnAt === 'merged'
          ? from.state === 'merged'
          : ['pr_open', 'review_feedback', 'merged'].includes(from.state)
      return { key: `${from.id}-${n.id}`, from, to: n, dependsOnAt: n.dependsOnAt, satisfied }
    })
    .filter(Boolean),
)

// ---------- 拖拽节点 ----------
const drag = reactive({ active: false, id: null, moved: false, locked: false, startX: 0, startY: 0, origX: 0, origY: 0 })

function readonlyDrag() {
  return !isNew.value && viewReadonly.value
}

function startDrag(n, ev) {
  ev.stopPropagation()
  // 只读态也要走完整套 down/move/up：否则"点节点开检视面板"这个
  // 点击判定（在 onPointerUp 里，靠 drag.active 触发）在只读默认态下
  // 永远不会跑——只读只该锁住"改位置"这一步，不能连点击选中一起锁掉。
  drag.active = true
  drag.locked = readonlyDrag()
  drag.id = n.id
  drag.moved = false
  drag.startX = ev.clientX
  drag.startY = ev.clientY
  drag.origX = n.x
  drag.origY = n.y
}

function onPointerMove(ev) {
  if (drag.active) {
    const n = nodeById(drag.id)
    if (!n) return
    const dx = (ev.clientX - drag.startX) / view.zoom
    const dy = (ev.clientY - drag.startY) / view.zoom
    if (Math.abs(dx) > 3 || Math.abs(dy) > 3) drag.moved = true
    if (drag.locked) return
    n.x = drag.origX + dx
    n.y = drag.origY + dy
    return
  }
  if (pan.active) {
    view.panX = pan.origX + (ev.clientX - pan.startX)
    view.panY = pan.origY + (ev.clientY - pan.startY)
    return
  }
  if (connecting.fromId != null) updateConnectPos(ev)
}

function onPointerUp(ev) {
  if (drag.active) {
    if (!drag.moved) {
      selectedId.value = selectedId.value === drag.id ? null : drag.id
      if (!isNew.value && selectedId.value) loadInspector(selectedId.value)
    } else if (!isNew.value && !drag.locked) {
      saveLayout()
    }
    drag.active = false
    drag.id = null
    return
  }
  if (pan.active) {
    pan.active = false
    return
  }
  if (connecting.fromId != null) finishConnect(ev)
}

// ---------- 平移画布 ----------
const pan = reactive({ active: false, startX: 0, startY: 0, origX: 0, origY: 0 })
function startPan(ev) {
  // 节点/连线圆点自己的 pointerdown 已经 stopPropagation，能冒泡到这里
  // 的只剩画布背景（world/svg 本身）——不用再按 target 精确匹配
  // canvasWrap，否则 world 铺满整个可视区域时几乎永远匹配不上。
  pan.active = true
  pan.startX = ev.clientX
  pan.startY = ev.clientY
  pan.origX = view.panX
  pan.origY = view.panY
}
function onCanvasClick(ev) {
  if (!ev.target.closest('.fc-node')) selectedId.value = null
}

// ---------- 建图模式：从右侧圆点拖出连线建依赖 ----------
const connecting = reactive({ fromId: null, x: 0, y: 0, targetId: null })

function worldPoint(ev) {
  const rect = canvasWrap.value.getBoundingClientRect()
  return {
    x: (ev.clientX - rect.left - view.panX) / view.zoom,
    y: (ev.clientY - rect.top - view.panY) / view.zoom,
  }
}
function startConnect(n, ev) {
  ev.stopPropagation()
  connecting.fromId = n.id
  updateConnectPos(ev)
}
function updateConnectPos(ev) {
  const p = worldPoint(ev)
  connecting.x = p.x
  connecting.y = p.y
  const el = document.elementFromPoint(ev.clientX, ev.clientY)
  const nodeEl = el && el.closest ? el.closest('[data-node-id]') : null
  // 建图模式下节点 id 是 "n0" 这种字符串（查看模式才是数字任务 id，
  // 但查看模式根本不渲染连线圆点），不能 Number() 强转，否则永远
  // 匹配不上——找不到目标节点，连线永远建不成。
  connecting.targetId = nodeEl ? nodeEl.dataset.nodeId : null
}
const connectPath = computed(() => {
  if (connecting.fromId == null) return ''
  const from = anchorRight(nodeById(connecting.fromId))
  return edgePath(from, { x: connecting.x, y: connecting.y })
})
function dependsOnChainIncludes(startId, targetId) {
  let cur = nodeById(startId)
  let guard = 0
  while (cur && cur.dependsOn != null && guard++ < nodes.value.length) {
    if (cur.dependsOn === targetId) return true
    cur = nodeById(cur.dependsOn)
  }
  return false
}
function finishConnect() {
  const fromId = connecting.fromId
  const targetId = connecting.targetId
  connecting.fromId = null
  connecting.targetId = null
  if (targetId == null || targetId === fromId) return
  // 目标已经是 from 的（间接）前驱：接上会成环，拒绝。
  if (dependsOnChainIncludes(fromId, targetId)) return
  const target = nodeById(targetId)
  target.dependsOn = fromId
  if (!target.dependsOnAt) target.dependsOnAt = 'pr_open'
}
function clearDepends(n) {
  n.dependsOn = null
}
function removeNode(id) {
  nodes.value = nodes.value.filter((n) => n.id !== id)
  nodes.value.forEach((n) => {
    if (n.dependsOn === id) n.dependsOn = null
  })
  if (selectedId.value === id) selectedId.value = null
}

onMounted(() => {
  window.addEventListener('pointermove', onPointerMove)
  window.addEventListener('pointerup', onPointerUp)
})
onUnmounted(() => {
  window.removeEventListener('pointermove', onPointerMove)
  window.removeEventListener('pointerup', onPointerUp)
  clearInterval(pollTimer)
})

// ================= 建图模式 =================
const repos = ref([])
const repoId = ref(null)
const flowName = ref('')
const submitting = ref(false)
const submitError = ref('')
const warnings = ref([])

const pickerOpen = ref(false)
const pickerIssues = ref([])
const pickerLoading = ref(false)
let uidSeq = 0
const addedKeys = computed(() => new Set(nodes.value.map((n) => n.issueKey)))

async function loadRepos() {
  try {
    const r = await api.repos()
    repos.value = r.repos || []
    if (repos.value.length && !repoId.value) repoId.value = repos.value[0].id
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    submitError.value = e.message
  }
}

async function openPicker() {
  pickerOpen.value = !pickerOpen.value
  if (!pickerOpen.value || pickerIssues.value.length) return
  pickerLoading.value = true
  try {
    const r = await api.linearIssues()
    pickerIssues.value = r.issues || []
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    submitError.value = e.message
  } finally {
    pickerLoading.value = false
  }
}

function addIssue(issue) {
  if (addedKeys.value.has(issue.identifier)) return
  const idx = nodes.value.length
  nodes.value.push({
    id: `n${uidSeq++}`,
    issueKey: issue.identifier,
    issueId: issue.id,
    title: issue.title,
    priority: 0,
    dependsOn: null,
    dependsOnAt: '',
    x: 60 + (idx % 3) * 260,
    y: 60 + Math.floor(idx / 3) * 150,
  })
}

// 按拓扑序（每个节点的 dependsOn 必须指向本批次里更早的节点）排出
// 提交顺序——用户拖连线的先后与建图请求要求的下标顺序无关，提交前
// 必须重排。图在连线阶段已经保证是森林（拒绝成环、入度 ≤ 1），
// 这里只是简单的多轮扫描：每轮把"父节点已在结果里或没有父节点"的
// 节点加入结果，直到全部处理完。
function topoOrder() {
  const done = new Set()
  const order = []
  const remaining = [...nodes.value]
  while (remaining.length) {
    const before = remaining.length
    for (let i = remaining.length - 1; i >= 0; i--) {
      const n = remaining[i]
      if (n.dependsOn == null || done.has(n.dependsOn)) {
        order.push(n)
        done.add(n.id)
        remaining.splice(i, 1)
      }
    }
    if (remaining.length === before) break // 不该发生（图已保证是森林），防御性退出
  }
  return order
}

async function submit() {
  if (!repoId.value) {
    submitError.value = '请先选择仓库'
    return
  }
  if (!nodes.value.length) {
    submitError.value = '请先从 Linear 选单里选至少一个 issue'
    return
  }
  submitting.value = true
  submitError.value = ''
  try {
    const order = topoOrder()
    const indexOf = new Map(order.map((n, i) => [n.id, i]))
    const payload = {
      name: flowName.value.trim() || `编排图 ${new Date().toLocaleString('zh-CN')}`,
      repoId: repoId.value,
      nodes: order.map((n) => ({
        issueKey: n.issueKey,
        issueId: n.issueId,
        title: n.title,
        priority: n.priority || 0,
        dependsOnIndex: n.dependsOn == null ? null : indexOf.get(n.dependsOn),
        dependsOnAt: n.dependsOnAt || undefined,
      })),
    }
    const res = await api.createFlow(payload)
    router.push({ name: 'flow', params: { id: String(res.flowId) } })
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    submitError.value = e.message
  } finally {
    submitting.value = false
  }
}

// ================= 查看模式 =================
const flowMeta = ref(null)
const loadError = ref('')
const viewReadonly = ref(true)
let pollTimer = null

function layoutKey(id) {
  return `flow:${id}:layout`
}
function loadSavedLayout(id) {
  try {
    return JSON.parse(localStorage.getItem(layoutKey(id)) || '{}')
  } catch {
    return {}
  }
}
function saveLayout() {
  if (isNew.value) return
  const saved = {}
  nodes.value.forEach((n) => {
    saved[n.id] = { x: n.x, y: n.y }
  })
  try {
    localStorage.setItem(layoutKey(props.id), JSON.stringify(saved))
  } catch {
    // localStorage 不可用（隐私模式/存储已满）：布局这次就不记了，
    // 不是关键功能，静默放弃即可。
  }
}

// 自动布局：按 dependsOn 链深度分层（同一深度从上往下排），只在
// "这个任务此前没有已知位置"时使用——localStorage 里存过的位置优先，
// 避免每次轮询都把手动拖过的位置弹回去。
function autoLayout(list) {
  const depth = new Map()
  const rowInDepth = new Map()
  const depthOf = (t) => {
    if (depth.has(t.id)) return depth.get(t.id)
    if (t.dependsOn == null) return 0
    const parent = list.find((x) => x.id === t.dependsOn)
    return parent ? depthOf(parent) + 1 : 0
  }
  list.forEach((t) => depth.set(t.id, depthOf(t)))
  return list.map((t) => {
    const d = depth.get(t.id)
    const row = rowInDepth.get(d) || 0
    rowInDepth.set(d, row + 1)
    return { x: 60 + d * 260, y: 60 + row * 150 }
  })
}

async function loadFlow() {
  try {
    const r = await api.flow(props.id)
    flowMeta.value = { id: r.id, repoId: r.repoId, name: r.name }
    const saved = loadSavedLayout(props.id)
    const positions = autoLayout(r.tasks)
    const existing = new Map(nodes.value.map((n) => [n.id, n]))
    nodes.value = r.tasks.map((t, i) => {
      const prev = existing.get(t.id)
      const pos = saved[t.id] || (prev ? { x: prev.x, y: prev.y } : positions[i])
      return {
        id: t.id,
        issueKey: t.issueKey,
        priority: t.priority,
        dependsOn: t.dependsOn,
        dependsOnAt: t.dependsOnAt,
        profile: t.profile,
        state: t.state,
        x: pos.x,
        y: pos.y,
      }
    })
    loadError.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    loadError.value = e.message
  }
}

// 检视面板：懒加载现成的任务详情接口（分支/PR/失败原因等），比
// GetFlow 的精简视图更丰富，不需要再扩后端。
const inspector = ref(null)
const inspectorLoading = ref(false)
async function loadInspector(id) {
  inspectorLoading.value = true
  inspector.value = null
  try {
    inspector.value = await api.task(id)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
  } finally {
    inspectorLoading.value = false
  }
}

// FlowCanvas 服务 /flows/new 与 /flows/:id 两条路由，Vue Router 对同一
// 组件的路由切换默认复用实例（不销毁重建）——建图成功后 router.push 到
// /flows/:id，或者反过来从一张已有图点"新建"，都是同一个组件实例在
// 换 props.id，必须靠这个 watch 把两种模式互相切换时的状态清干净
// （轮询定时器、节点列表、检视面板），不能假设组件会重新 mount。
watch(
  () => props.id,
  () => {
    clearInterval(pollTimer)
    pollTimer = null
    selectedId.value = null
    inspector.value = null
    nodes.value = []
    if (isNew.value) {
      submitError.value = ''
      warnings.value = []
      loadRepos()
    } else {
      loadFlow()
      pollTimer = setInterval(loadFlow, 5000)
    }
  },
  { immediate: true },
)

</script>

<template>
  <div class="spread" style="margin-bottom: 12px">
    <div>
      <h2 style="margin: 0">{{ isNew ? '新建编排图' : flowMeta?.name || '编排图' }}</h2>
      <p class="dim" style="margin: 4px 0 0">
        {{
          isNew
            ? '从 Linear 选单，拖出连线定依赖，一键批量入队。'
            : '任务依赖关系与实时状态；拖拽仅调整本地展示位置，不改变依赖。'
        }}
      </p>
    </div>
    <RouterLink to="/flows" class="dim">← 返回编排图列表</RouterLink>
  </div>

  <div v-if="submitError || loadError" class="error-banner">{{ submitError || loadError }}</div>
  <div v-if="warnings.length" class="error-banner" style="color: var(--warn); border-color: var(--warn)">
    <div v-for="(w, i) in warnings" :key="i">{{ w }}</div>
  </div>

  <div class="card toolbar-row">
    <template v-if="isNew">
      <select v-model="repoId">
        <option v-for="r in repos" :key="r.id" :value="r.id">{{ r.providerRepo }}</option>
      </select>
      <input v-model="flowName" placeholder="图名（可选）" style="width: 220px" />
      <div class="popover-anchor" style="position: relative">
        <button v-if="hasLinearToken()" @click="openPicker">从 Linear 选单</button>
        <div v-if="pickerOpen" class="card picker-popover">
          <div v-if="pickerLoading" class="dim">加载中…</div>
          <label v-for="i in pickerIssues" :key="i.id" class="row" style="padding: 4px 0">
            <input
              type="checkbox"
              :checked="addedKeys.has(i.identifier)"
              :disabled="addedKeys.has(i.identifier)"
              @change="addIssue(i)"
            />
            <span class="mono faint" style="font-size: 12px">{{ i.identifier }}</span>
            <span style="font-size: 13px">{{ i.title }}</span>
          </label>
          <div v-if="!pickerLoading && !pickerIssues.length" class="dim">没有可选的 issue</div>
        </div>
      </div>
      <span v-if="!hasLinearToken()" class="faint">先在「个人设置」绑定 Linear 令牌才能选单</span>
      <div style="flex: 1"></div>
      <button class="primary" :disabled="submitting" @click="submit">
        {{ submitting ? '入队中…' : '一键入队' }}
      </button>
    </template>
    <template v-else>
      <span class="dim">{{ nodes.length }} 个任务</span>
      <div style="flex: 1"></div>
      <label class="row" style="gap: 6px">
        <input type="checkbox" v-model="viewReadonly" />
        只读（关闭后可拖动重排）
      </label>
    </template>
    <div class="zoom-group">
      <button @click="zoomBy(-0.1)">－</button>
      <span class="dim" style="width: 42px; text-align: center">{{ Math.round(view.zoom * 100) }}%</span>
      <button @click="zoomBy(0.1)">＋</button>
      <button @click="resetView">适应画布</button>
    </div>
  </div>

  <div
    ref="canvasWrap"
    class="card canvas-wrap"
    @pointerdown="startPan"
    @click="onCanvasClick"
    @wheel="onWheel"
  >
    <div class="world" :style="{ transform: `translate(${view.panX}px,${view.panY}px) scale(${view.zoom})` }">
      <svg class="edges-svg">
        <defs>
          <marker id="fc-arrow-ok" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto">
            <path d="M0,0 L6,3 L0,6" fill="none" stroke="var(--text-faint)" stroke-width="1.4" />
          </marker>
          <marker id="fc-arrow-wait" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto">
            <path d="M0,0 L6,3 L0,6" fill="none" stroke="var(--warn)" stroke-width="1.4" />
          </marker>
          <marker id="fc-arrow-accent" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto">
            <path d="M0,0 L6,3 L0,6" fill="none" stroke="var(--accent)" stroke-width="1.4" />
          </marker>
        </defs>
        <path
          v-for="e in edges"
          :key="e.key"
          :d="edgePath(anchorRight(e.from), anchorLeft(e.to))"
          fill="none"
          :stroke="e.satisfied === false ? 'var(--warn)' : 'var(--text-faint)'"
          stroke-width="1.6"
          :stroke-dasharray="e.satisfied === false ? '4 4' : null"
          :marker-end="e.satisfied === false ? 'url(#fc-arrow-wait)' : 'url(#fc-arrow-ok)'"
        />
        <path
          v-if="connecting.fromId != null"
          :d="connectPath"
          fill="none"
          stroke="var(--accent)"
          stroke-width="1.6"
          stroke-dasharray="4 4"
          marker-end="url(#fc-arrow-accent)"
        />
      </svg>

      <div
        v-for="e in edges"
        :key="'lbl-' + e.key"
        class="edge-label"
        :class="{ wait: e.satisfied === false }"
        :style="{
          left: (anchorRight(e.from).x + anchorLeft(e.to).x) / 2 + 'px',
          top: (anchorRight(e.from).y + anchorLeft(e.to).y) / 2 - 16 + 'px',
        }"
      >
        {{ e.dependsOnAt === 'merged' ? '合并后' : 'PR 开启后' }}
        <span v-if="e.satisfied === false">· 等待</span>
        <span v-else-if="e.satisfied === true">· ✓</span>
      </div>

      <div
        v-for="n in nodes"
        :key="n.id"
        :data-node-id="n.id"
        class="fc-node"
        :class="{ selected: n.id === selectedId }"
        :style="{ transform: `translate(${n.x}px,${n.y}px)` }"
        @pointerdown="startDrag(n, $event)"
      >
        <div class="row" style="justify-content: space-between">
          <span class="mono" style="color: var(--accent); font-weight: 600; font-size: 12px">{{ n.issueKey }}</span>
          <button v-if="isNew" class="icon-btn" title="移除" @pointerdown.stop @click.stop="removeNode(n.id)">✕</button>
          <span v-else class="badge" :class="stateTone(n.state)">{{ stateLabel(n.state) }}</span>
        </div>
        <div v-if="isNew" style="font-size: 13px; margin: 4px 0">{{ n.title }}</div>

        <template v-if="isNew">
          <div class="row" style="gap: 6px; margin-top: 4px">
            <span class="faint" style="font-size: 11px">优先级</span>
            <input type="number" v-model.number="n.priority" style="width: 56px; padding: 2px 6px" @pointerdown.stop />
          </div>
          <div v-if="n.dependsOn != null" class="row" style="gap: 6px; margin-top: 4px">
            <select v-model="n.dependsOnAt" style="padding: 2px 6px" @pointerdown.stop>
              <option value="pr_open">前驱 PR 开启后</option>
              <option value="merged">前驱合并后</option>
            </select>
            <button class="icon-btn" title="取消依赖" @click.stop="clearDepends(n)">✕</button>
          </div>
          <span
            v-if="isNew"
            class="handle out"
            title="拖出以连接依赖"
            @pointerdown="startConnect(n, $event)"
          ></span>
        </template>
      </div>
    </div>
  </div>

  <div v-if="!isNew" class="inspector-panel" :class="{ open: selectedId != null }">
    <button class="icon-btn" style="position: absolute; top: 10px; right: 10px" @click="selectedId = null">✕</button>
    <template v-if="inspectorLoading">加载中…</template>
    <template v-else-if="inspector">
      <div class="mono" style="color: var(--accent); font-weight: 600">{{ inspector.externalKey }}</div>
      <div style="margin: 6px 0">
        <span class="badge" :class="stateTone(inspector.state)">{{ stateLabel(inspector.state) }}</span>
      </div>
      <div class="kv-row"><span class="dim">分支</span><span class="mono">{{ inspector.branchName || '—' }}</span></div>
      <div class="kv-row">
        <span class="dim">PR</span>
        <a v-if="inspector.prUrl" :href="inspector.prUrl" target="_blank" rel="noopener">查看</a>
        <span v-else class="faint">—</span>
      </div>
      <div v-if="inspector.failureReason" class="kv-row"><span class="dim">失败原因</span><span>{{ inspector.failureReason }}</span></div>
      <div v-if="selectedNode?.dependsOnAt" class="kv-row"><span class="dim">依赖条件</span><span>{{ selectedNode.dependsOnAt === 'merged' ? '前驱合并后' : '前驱 PR 开启后' }}</span></div>
      <RouterLink :to="`/tasks/${inspector.id}`" class="row" style="margin-top: 12px">查看任务详情 →</RouterLink>
    </template>
  </div>
</template>

<style scoped>
.toolbar-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.zoom-group { display: flex; align-items: center; gap: 6px; margin-left: auto; }

.picker-popover {
  position: absolute;
  top: calc(100% + 6px);
  left: 0;
  width: 300px;
  max-height: 320px;
  overflow-y: auto;
  z-index: 30;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.28);
}

.canvas-wrap {
  position: relative;
  height: 60vh;
  overflow: hidden;
  padding: 0;
  cursor: grab;
  background-image: radial-gradient(var(--border) 1px, transparent 1px);
  background-size: 24px 24px;
}

.world { position: absolute; left: 0; top: 0; width: 1600px; height: 1000px; transform-origin: 0 0; }
.edges-svg { position: absolute; left: 0; top: 0; width: 1600px; height: 1000px; overflow: visible; pointer-events: none; }

.edge-label {
  position: absolute;
  transform: translate(-50%, -50%);
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 999px;
  white-space: nowrap;
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--text-faint);
}
.edge-label.wait { color: var(--warn); background: var(--warn-bg); border-color: transparent; }

.fc-node {
  position: absolute;
  width: 220px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  cursor: grab;
  touch-action: none;
  user-select: none;
}
.fc-node.selected { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-bg); }

.icon-btn { border: none; background: none; color: var(--text-faint); cursor: pointer; padding: 0 2px; }
.icon-btn:hover { color: var(--bad); }

.handle.out {
  position: absolute;
  right: -7px;
  top: 50%;
  margin-top: -6px;
  width: 12px;
  height: 12px;
  border-radius: 50%;
  background: var(--surface);
  border: 2px solid var(--accent);
  cursor: crosshair;
}
.handle.out:hover { background: var(--accent); }

.inspector-panel {
  position: fixed;
  top: 0;
  right: 0;
  bottom: 0;
  width: 300px;
  background: var(--surface);
  border-left: 1px solid var(--border);
  padding: 16px;
  transform: translateX(100%);
  transition: transform 0.18s ease;
  overflow-y: auto;
  z-index: 20;
}
.inspector-panel.open { transform: translateX(0); }
.kv-row { display: flex; justify-content: space-between; padding: 4px 0; font-size: 13px; }
</style>
