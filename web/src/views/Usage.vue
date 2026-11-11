<script setup>
// 用量统计页（f03，docs/features/usage-stats.md §3.4）。
//
// 数据源是 agent_runs 完整账本（含失败/被杀的运行、PRD 规划、预览推荐），
// 与看板上的 CostPanel（只统计 agent_events 里的任务运行）口径不同，页脚写明。
//
// 形态：数字卡 + 表格为主；趋势是单序列面积图（配色照抄 CostPanel 已校验的
// blue 槽），表格视图始终可用 —— 颜色从不是唯一通道。
// 「未知」与「$0.0000」绝不共用一个渲染：costUsd 为 null 时显示「未知」。
import { ref, computed, watch, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, formatDuration } from '../api'
import { isAdmin } from '../auth'

const route = useRoute()
const router = useRouter()

const BUCKETS = [
  { key: 'triage', label: '分诊' },
  { key: 'implement', label: '实现' },
  { key: 'review', label: 'Review 修改' },
  { key: 'verify', label: '验证' },
  { key: 'push', label: '推送' },
  { key: 'prd_plan', label: 'PRD 规划' },
  { key: 'prd_review', label: 'PRD 复核' },
  { key: 'preview', label: '预览推荐' },
  { key: 'other', label: '其他' },
]
const GROUPS = [
  { key: 'phase', label: '阶段' },
  { key: 'repo', label: '仓库' },
  { key: 'tier', label: '档位' },
  { key: 'day', label: '天' },
  { key: 'task', label: '任务' },
  { key: 'model', label: '模型' },
]
const RANGES = [
  { key: 'today', label: '今日' },
  { key: '7d', label: '7 天' },
  { key: '30d', label: '30 天' },
  { key: 'custom', label: '自定义' },
]
const TIER_LABEL = { light: 'light', heavy: 'heavy', none: '未定档' }
const bucketLabel = (k) => BUCKETS.find((b) => b.key === k)?.label || k

// 筛选状态全部来自 query string：可分享、刷新不复位。
const f = computed(() => ({
  range: route.query.range || '30d',
  from: route.query.from || '',
  to: route.query.to || '',
  repo: route.query.repo || '',
  tier: route.query.tier || '',
  bucket: route.query.bucket || '',
  group: route.query.group || 'phase',
  scope: isAdmin() && route.query.scope === 'all' ? 'all' : 'mine',
}))

function setQuery(patch) {
  const q = { ...route.query, ...patch }
  for (const k of Object.keys(q)) if (q[k] === '' || q[k] == null) delete q[k]
  router.replace({ query: q })
}

const data = ref(null)
const err = ref('')
const loading = ref(false)
const repos = ref([])
const showTable = ref(false)

function requestParams() {
  const v = f.value
  const p = { repo: v.repo, tier: v.tier, bucket: v.bucket, group: v.group, limit: v.group === 'task' ? 50 : '' }
  const now = new Date()
  if (v.range === 'today') p.from = now.toISOString().slice(0, 10)
  else if (v.range === '7d') p.from = new Date(now.getTime() - 7 * 86400e3).toISOString()
  else if (v.range === 'custom') {
    p.from = v.from
    p.to = v.to
  }
  return p
}

async function load() {
  loading.value = true
  try {
    const p = requestParams()
    data.value = f.value.scope === 'all' ? await api.adminUsage(p) : await api.usage(p)
    err.value = ''
  } catch (e) {
    err.value = e.message || '加载失败'
    data.value = null
  } finally {
    loading.value = false
  }
}

watch(() => route.query, load)
onMounted(async () => {
  load()
  try {
    repos.value = (await api.repos()).repos || []
  } catch {
    repos.value = []
  }
})

// ---------------------------------------------------------------- 格式化

function fmtUSD(v) {
  if (v === null || v === undefined) return '未知'
  const n = Number(v)
  if (n >= 1000) return '$' + (n / 1000).toFixed(1) + 'K'
  if (n >= 1) return '$' + n.toFixed(2)
  return '$' + n.toFixed(4)
}
function fmtNum(n) {
  n = Number(n || 0)
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M'
  if (n >= 1e4) return (n / 1e3).toFixed(1) + 'K'
  return String(n)
}
const tokSum = (t) => (t ? t.input + t.output + t.cacheRead + t.cacheCreation : 0)

const totals = computed(() => data.value?.totals || null)
const empty = computed(() => !totals.value || totals.value.runs === 0)
const totalTokens = computed(() => {
  const t = totals.value
  return t ? t.inputTokens + t.outputTokens + t.cacheReadTokens + t.cacheCreationTokens : 0
})

function groupLabel(g) {
  switch (f.value.group) {
    case 'phase':
      return `${g.key}（${bucketLabel(g.bucket)}）`
    case 'repo':
      return g.providerRepo || `仓库 #${g.repoId}`
    case 'tier':
      return TIER_LABEL[g.key] || g.key
    case 'task':
      if (g.kind === 'prd') return g.oneLiner || `PRD #${g.prdId}`
      if (g.externalKey) return g.issueTitle ? `${g.externalKey} · ${g.issueTitle}` : g.externalKey
      return `任务 #${g.taskId}`
    default:
      return g.key
  }
}
function groupLink(g) {
  if (f.value.group !== 'task') return null
  return g.kind === 'prd' ? `/prds/${g.prdId}` : `/tasks/${g.taskId}`
}
// 分组项的 token：model 维度是平铺字段，其余是嵌套 tokens
function groupTokens(g) {
  if (g.tokens) return tokSum(g.tokens)
  return (g.inputTokens || 0) + (g.outputTokens || 0) + (g.cacheReadTokens || 0) + (g.cacheCreationTokens || 0)
}

// ---------------------------------------------------------------- 趋势（单序列面积图）

const PAD = { top: 14, right: 14, bottom: 26, left: 52 }
const PLOT_H = 140
const PLOT_W = 640
const days = computed(() => (data.value?.byDay || []).map((d) => ({ ...d, usd: Number(d.costUsd || 0) })))
const yMax = computed(() => {
  const max = Math.max(0, ...days.value.map((d) => d.usd))
  if (max <= 0) return 1
  const mag = Math.pow(10, Math.floor(Math.log10(max)))
  return Math.ceil(max / mag) * mag
})
const innerW = PLOT_W - PAD.left - PAD.right
const xAt = (i) => (days.value.length <= 1 ? PAD.left + innerW / 2 : PAD.left + (innerW * i) / (days.value.length - 1))
const yAt = (v) => PAD.top + PLOT_H - (PLOT_H * v) / yMax.value
const linePath = computed(() => days.value.map((d, i) => `${i ? 'L' : 'M'}${xAt(i).toFixed(1)},${yAt(d.usd).toFixed(1)}`).join(' '))
const areaPath = computed(() => {
  if (!days.value.length) return ''
  const base = PAD.top + PLOT_H
  return linePath.value + ` L${xAt(days.value.length - 1).toFixed(1)},${base} L${xAt(0).toFixed(1)},${base} Z`
})
function fmtAxis(v) {
  if (v === 0) return '$0'
  if (v >= 10) return '$' + Math.round(v)
  if (v >= 1) return '$' + v.toFixed(1)
  return '$' + v.toFixed(2)
}
</script>

<template>
  <div class="usage-page">
    <div class="spread">
      <h1>用量{{ f.scope === 'all' ? '（全局）' : '' }}</h1>
      <div class="row">
        <button v-if="isAdmin()" @click="setQuery({ scope: f.scope === 'all' ? '' : 'all', repo: '' })">
          {{ f.scope === 'all' ? '只看我的' : '全局用量' }}
        </button>
        <button :disabled="loading" @click="load">{{ loading ? '刷新中…' : '刷新' }}</button>
      </div>
    </div>

    <!-- 四个筛选 + 分组切换，全部写进 query string -->
    <div class="card filters">
      <div class="row wrap">
        <span class="dim">时间</span>
        <button v-for="r in RANGES" :key="r.key" :class="{ active: f.range === r.key }" @click="setQuery({ range: r.key })">
          {{ r.label }}
        </button>
        <template v-if="f.range === 'custom'">
          <input type="date" :value="f.from" aria-label="开始日期" @change="setQuery({ from: $event.target.value })" />
          <span class="dim">至</span>
          <input type="date" :value="f.to" aria-label="结束日期" @change="setQuery({ to: $event.target.value })" />
        </template>
      </div>
      <div class="row wrap">
        <label class="dim">仓库
          <select :value="f.repo" @change="setQuery({ repo: $event.target.value })">
            <option value="">全部</option>
            <option v-for="r in repos" :key="r.id" :value="String(r.id)">{{ r.providerRepo }}</option>
          </select>
        </label>
        <label class="dim">档位
          <select :value="f.tier" @change="setQuery({ tier: $event.target.value })">
            <option value="">全部</option>
            <option value="light">light</option>
            <option value="heavy">heavy</option>
            <option value="none">未定档</option>
          </select>
        </label>
        <label class="dim">阶段
          <select :value="f.bucket" @change="setQuery({ bucket: $event.target.value })">
            <option value="">全部</option>
            <option v-for="b in BUCKETS" :key="b.key" :value="b.key">{{ b.label }}</option>
          </select>
        </label>
      </div>
      <div class="row wrap">
        <span class="dim">分组</span>
        <button v-for="g in GROUPS" :key="g.key" :class="{ active: f.group === g.key }" @click="setQuery({ group: g.key })">
          {{ g.label }}
        </button>
      </div>
    </div>

    <p v-if="err" class="bad-text" role="alert">{{ err }}</p>

    <template v-else-if="data">
      <div v-if="empty" class="card empty-state">
        <div class="kpi-num">—</div>
        <p class="dim">该范围内没有运行记录</p>
      </div>

      <template v-else>
        <!-- KPI：这些是数字，不画成图 -->
        <div class="kpis">
          <div class="card kpi">
            <div class="kpi-num">{{ fmtUSD(totals.costUsd) }}</div>
            <div class="dim">花费</div>
            <div v-if="totals.costKnown < totals.runs" class="faint note">另有 {{ totals.runs - totals.costKnown }} 次费用未知</div>
          </div>
          <div class="card kpi">
            <div class="kpi-num">{{ totals.runs }}</div>
            <div class="dim">运行次数</div>
          </div>
          <div class="card kpi">
            <div class="kpi-num">{{ fmtNum(totalTokens) }}</div>
            <div class="dim">token（输入 {{ fmtNum(totals.inputTokens) }} · 输出 {{ fmtNum(totals.outputTokens) }} · 缓存读 {{ fmtNum(totals.cacheReadTokens) }} · 缓存写 {{ fmtNum(totals.cacheCreationTokens) }}）</div>
            <div v-if="totals.tokensKnown < totals.runs" class="faint note">{{ totals.runs - totals.tokensKnown }} 次运行无 token 数据</div>
          </div>
          <div class="card kpi">
            <div class="kpi-num">{{ formatDuration(totals.durationMs) }}</div>
            <div class="dim">累计耗时 · {{ totals.turns }} 轮</div>
          </div>
        </div>

        <!-- 趋势：单序列面积图；表格视图始终可用 -->
        <div class="card">
          <div class="spread">
            <div class="label">按天花费（UTC）</div>
            <button @click="showTable = !showTable">{{ showTable ? '看图' : '看表格' }}</button>
          </div>
          <div v-if="showTable" class="scroll-x">
            <table>
              <thead><tr><th>日期</th><th class="num">花费</th><th class="num">运行</th></tr></thead>
              <tbody>
                <tr v-for="d in data.byDay" :key="d.key">
                  <td class="mono">{{ d.key }}</td><td class="num mono">{{ fmtUSD(d.costUsd) }}</td><td class="num mono">{{ d.runs }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div v-else class="scroll-x">
            <svg class="plot" :viewBox="`0 0 ${PLOT_W} ${PAD.top + PLOT_H + PAD.bottom}`" role="img" aria-label="按天花费趋势">
              <g class="grid">
                <line v-for="v in [0, yMax / 2, yMax]" :key="'g' + v" :x1="PAD.left" :x2="PLOT_W - PAD.right" :y1="yAt(v)" :y2="yAt(v)" />
              </g>
              <g class="axis-text">
                <text v-for="v in [0, yMax / 2, yMax]" :key="'y' + v" :x="PAD.left - 8" :y="yAt(v) + 4" text-anchor="end">{{ fmtAxis(v) }}</text>
                <text v-if="days.length" :x="xAt(0)" :y="PAD.top + PLOT_H + 18" text-anchor="start">{{ days[0].key.slice(5) }}</text>
                <text v-if="days.length > 1" :x="xAt(days.length - 1)" :y="PAD.top + PLOT_H + 18" text-anchor="end">{{ days[days.length - 1].key.slice(5) }}</text>
              </g>
              <path class="area" :d="areaPath" />
              <path class="line" :d="linePath" />
              <circle v-for="(d, i) in days" :key="d.key" class="dot" :cx="xAt(i)" :cy="yAt(d.usd)" r="3">
                <title>{{ d.key }} · {{ fmtUSD(d.costUsd) }} · {{ d.runs }} 次</title>
              </circle>
            </svg>
          </div>
        </div>

        <!-- 分组明细 -->
        <div class="card">
          <div class="label">按{{ GROUPS.find((g) => g.key === f.group)?.label }}</div>
          <div class="scroll-x">
            <table>
              <thead>
                <tr><th>{{ GROUPS.find((g) => g.key === f.group)?.label }}</th><th class="num">花费</th><th class="num">运行</th><th class="num">token</th><th class="num">耗时</th></tr>
              </thead>
              <tbody>
                <tr v-for="g in data.byGroup" :key="g.key">
                  <td>
                    <router-link v-if="groupLink(g) && f.scope !== 'all'" :to="groupLink(g)">{{ groupLabel(g) }}</router-link>
                    <span v-else>{{ groupLabel(g) }}</span>
                  </td>
                  <td class="num mono">
                    {{ fmtUSD(g.costUsd) }}
                    <span v-if="g.costKnown < g.runs" class="faint" :title="`${g.runs - g.costKnown} 次费用未知`">*</span>
                  </td>
                  <td class="num mono">{{ g.runs }}</td>
                  <td class="num mono">
                    <template v-if="g.tokensKnown > 0">{{ fmtNum(groupTokens(g)) }}</template><template v-else>—</template>
                    <span v-if="g.tokensKnown > 0 && g.tokensKnown < g.runs" class="faint" :title="`${g.runs - g.tokensKnown} 次运行无 token 数据`">*</span>
                  </td>
                  <td class="num mono">{{ g.durationMs != null ? formatDuration(g.durationMs) : '—' }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-if="data.groupTotal > data.byGroup.length" class="faint">另有 {{ data.groupTotal - data.byGroup.length }} 个未列出</p>
          <p class="faint">* 该项有部分运行费用未知或无 token 数据，合计只含已知部分。</p>
        </div>

        <div class="grid2">
          <div class="card">
            <div class="label">阶段桶</div>
            <table>
              <tbody>
                <tr v-for="b in data.byBucket" :key="b.key">
                  <td>{{ bucketLabel(b.key) }}</td><td class="num mono">{{ fmtUSD(b.costUsd) }}</td><td class="num mono">{{ b.runs }} 次</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div class="card">
            <div class="label">模型</div>
            <table>
              <tbody>
                <tr v-for="m in data.byModel" :key="m.key">
                  <td class="mono">{{ m.key === '(unknown)' ? '未知（无明细）' : m.key }}</td>
                  <td class="num mono">{{ fmtUSD(m.costUsd) }}</td>
                  <td class="num mono">{{ fmtNum(m.inputTokens + m.outputTokens + m.cacheReadTokens + m.cacheCreationTokens) }} tok</td>
                </tr>
              </tbody>
            </table>
            <p class="faint">一次运行用到多个模型时各计一次，此表不做合计。</p>
          </div>
        </div>

        <div v-if="f.scope === 'all' && data.byUser" class="card">
          <div class="label">按用户</div>
          <table>
            <thead><tr><th>用户 ID</th><th class="num">花费</th><th class="num">运行</th></tr></thead>
            <tbody>
              <tr v-for="u in data.byUser" :key="u.userId">
                <td class="mono">#{{ u.userId }}</td><td class="num mono">{{ fmtUSD(u.costUsd) }}</td><td class="num mono">{{ u.runs }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>

      <p class="faint footnote">
        口径：本页读 agent 运行账本（每次运行一行，含失败/被杀的运行、PRD 规划与预览推荐）；看板上的「agent 花费」面板只统计任务执行事件流，两者数字可能不同。
        档位按运行所属尝试的档位快照计。上线前的历史数据由事件回填：档位按回填时的任务档位计、不区分 review 轮、没有 token 数据。
        费用取自 CLI 自报，codex 等不报费用的运行显示为「未知」。
      </p>
    </template>

    <p v-else class="faint">加载中…</p>
  </div>
</template>

<style scoped>
.usage-page {
  --s1: #3987e5;
  --grid: #2c2c2a;
}
@media (prefers-color-scheme: light) {
  :root:not([data-theme='dark']) .usage-page {
    --s1: #2a78d6;
    --grid: #e1e0d9;
  }
}
.filters { display: flex; flex-direction: column; gap: 10px; margin-bottom: 14px; }
.wrap { flex-wrap: wrap; gap: 8px; }
button.active { border-color: var(--accent); color: var(--accent); }
.kpis { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; margin-bottom: 14px; }
.kpi-num { font-size: 24px; font-weight: 600; }
.note { margin-top: 4px; }
.empty-state { text-align: center; padding: 32px 16px; }
.card { margin-bottom: 14px; }
.grid2 { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 14px; }
.scroll-x { overflow-x: auto; }
.plot { width: 100%; max-width: 100%; height: auto; display: block; }
.grid line { stroke: var(--grid); stroke-width: 1; }
.axis-text text { fill: var(--text-faint); font-size: 11px; font-variant-numeric: tabular-nums; }
.area { fill: var(--s1); fill-opacity: 0.1; stroke: none; }
.line { fill: none; stroke: var(--s1); stroke-width: 2; stroke-linejoin: round; }
.dot { fill: var(--s1); stroke: var(--surface); stroke-width: 2; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--border); }
th { color: var(--text-dim); font-weight: 500; }
.num { text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
.bad-text { color: var(--bad); }
.footnote { margin-top: 8px; line-height: 1.6; }
</style>
