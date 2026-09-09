<script setup>
// 成本面板（docs/08-debt-cleanup.md T5）。
//
// 形态选择（先定形态，再定颜色）：
//   - 累计花费 = 一个数字 → stat tile，不是「只有一根柱子的柱状图」
//   - 30 天日花费 = 随时间变化的单序列 → 面积/折线；单序列不需要图例，
//     标题已经说明画的是什么
//   - 分诊 / 实现占比 = 部分对整体、类别少 → 水平堆叠条，不是饼图
//   - 花费最高的任务 = 有序量值 → 表格 + 单色横条。**刻意不按值上色** ——
//     那会把条长这一个信息重复编码成色相，白占掉唯一的空闲通道
//
// 配色：分类色取 blue / orange / aqua 三槽，已用调色校验器按本项目实际
// surface（亮 #ffffff、暗 #171a21）跑过两个模式：亮度带、色度下限、
// CVD 相邻分离度（最差 ΔE 9.2/9.4，门限 8）、常视力下限（27.6/26.5，门限 15）
// 全部 PASS。亮色模式下 aqua 对 surface 对比度 2.82:1 低于 3:1，触发
// relief 规则 —— 所以堆叠条**始终带可见直接标签**，且提供表格视图，
// 颜色永远不是唯一的信息通道。
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { api, formatDuration } from '../api.js'

const cost = ref(null)
const err = ref('')
const loading = ref(false)
const showTable = ref(false)

// 图表实际像素宽度：用 ResizeObserver 量出来再按真实尺寸画 SVG，
// 而不是给 viewBox 配 preserveAspectRatio 缩放 —— 那样 2px 描边会跟着
// 被拉伸，「细线」这条规格就失效了。
const plotBox = ref(null)
const plotW = ref(640)
let ro = null

async function load() {
  loading.value = true
  try {
    cost.value = await api.costStats()
    err.value = ''
  } catch (e) {
    err.value = e.message || '加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  if (plotBox.value && typeof ResizeObserver !== 'undefined') {
    ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect?.width
      if (w) plotW.value = Math.max(320, Math.floor(w))
    })
    ro.observe(plotBox.value)
  }
})
onUnmounted(() => ro && ro.disconnect())

// ---------------------------------------------------------------- 格式化

// 金额：小额保留 4 位（agent 单次常在几分钱量级），过千用紧凑写法。
// 大数字用比例数字（默认），不加 tabular-nums —— 等宽数字会让 121
// 这种数在大字号下显得松散；tabular-nums 只留给需要纵向对齐的表格列。
function fmtUSD(v) {
  const n = Number(v || 0)
  if (n >= 1000) return '$' + (n / 1000).toFixed(1) + 'K'
  if (n >= 1) return '$' + n.toFixed(2)
  return '$' + n.toFixed(4)
}

// y 轴刻度专用：必须是「干净数字」而不是 4 位小数。
// fmtUSD 会给出 $0.0000（7 字符 ≈46px @11px tabular），右对齐在
// PAD.left-8 处会向左溢出容器 —— 这是纯算术能查出来的溢出，
// 不该等到看见才发现。
function fmtAxis(v) {
  const n = Number(v || 0)
  if (n === 0) return '$0'
  if (n >= 1000) return '$' + (n / 1000).toFixed(1) + 'K'
  if (n >= 10) return '$' + Math.round(n)
  if (n >= 1) return '$' + n.toFixed(1)
  return '$' + n.toFixed(2)
}

// 阶段桶的中文名与配色槽位。顺序固定，不随数据变 ——
// 颜色跟随实体而非排名，筛掉一个桶不会把其余重新上色。
const PHASES = [
  { key: 'implement', label: '实现', slot: 1 },
  { key: 'triage', label: '分诊', slot: 2 },
  { key: 'other', label: '其他', slot: 3 },
]

const phases = computed(() => {
  const by = cost.value?.byPhase || {}
  const total = PHASES.reduce((s, p) => s + Number(by[p.key]?.usd || 0), 0)
  return PHASES.map((p) => {
    const usd = Number(by[p.key]?.usd || 0)
    return {
      ...p,
      usd,
      runs: Number(by[p.key]?.runs || 0),
      pct: total > 0 ? usd / total : 0,
    }
  }).filter((p) => p.usd > 0)
})

const phaseTotal = computed(() => phases.value.reduce((s, p) => s + p.usd, 0))

const avgPerRun = computed(() => {
  const c = cost.value
  if (!c || !c.runs) return 0
  return c.totalUsd / c.runs
})

// ---------------------------------------------------------------- 折线几何

// 内边距把 x 轴文字带算进容器高度 —— 固定高度却没给轴留位置，
// 会让卡片里长出一条小小的纵向滚动条（反模式清单里有这一条）。
const PAD = { top: 14, right: 14, bottom: 26, left: 52 }
const PLOT_H = 150

const days = computed(() => cost.value?.byDay || [])

const yMax = computed(() => {
  const max = Math.max(0, ...days.value.map((d) => Number(d.usd || 0)))
  if (max <= 0) return 1
  // 向上取整到一个干净刻度：轴要承载没被直接标注的那些值
  const mag = Math.pow(10, Math.floor(Math.log10(max)))
  return Math.ceil(max / mag) * mag
})

const innerW = computed(() => Math.max(60, plotW.value - PAD.left - PAD.right))

function xAt(i) {
  const n = days.value.length
  if (n <= 1) return PAD.left + innerW.value / 2
  return PAD.left + (innerW.value * i) / (n - 1)
}
function yAt(v) {
  return PAD.top + PLOT_H - (PLOT_H * Number(v || 0)) / yMax.value
}

const linePath = computed(() =>
  days.value.map((d, i) => `${i === 0 ? 'M' : 'L'}${xAt(i).toFixed(1)},${yAt(d.usd).toFixed(1)}`).join(' ')
)

// 面积用 10% 透明度的水洗，不是饱和色块
const areaPath = computed(() => {
  if (!days.value.length) return ''
  const base = PAD.top + PLOT_H
  return (
    linePath.value +
    ` L${xAt(days.value.length - 1).toFixed(1)},${base} L${xAt(0).toFixed(1)},${base} Z`
  )
})

// 三条水平网格线（含 0 与顶），实线 hairline、比 surface 深一档
const yTicks = computed(() => {
  const m = yMax.value
  return [0, m / 2, m].map((v) => ({ v, y: yAt(v) }))
})

// x 轴只标首末与中点：不给每个点都写数字（反模式），
// 密集日期标签会互相碰撞
const xTicks = computed(() => {
  const n = days.value.length
  if (!n) return []
  const idx = n === 1 ? [0] : n < 5 ? days.value.map((_, i) => i) : [0, Math.floor((n - 1) / 2), n - 1]
  return idx.map((i) => ({ i, x: xAt(i), label: (days.value[i].day || '').slice(5) }))
})

// ---------------------------------------------------------------- 悬浮层

// 十字准线找 X：读者瞄的是日期，不是一条 2px 的线。
// 命中区是整个绘图区，就近吸附 —— 不要求指针精确落在标记上。
const hoverIdx = ref(-1)
const hover = computed(() => (hoverIdx.value >= 0 ? days.value[hoverIdx.value] : null))

function onMove(e) {
  const n = days.value.length
  if (!n) return
  const rect = e.currentTarget.getBoundingClientRect()
  const x = e.clientX - rect.left
  const t = (x - PAD.left) / Math.max(1, innerW.value)
  hoverIdx.value = Math.max(0, Math.min(n - 1, Math.round(t * (n - 1))))
}
function onLeave() {
  hoverIdx.value = -1
}

const tooltipStyle = computed(() => {
  if (hoverIdx.value < 0) return { display: 'none' }
  const x = xAt(hoverIdx.value)
  // 靠右时向左翻转，避免溢出卡片
  const flip = x > PAD.left + innerW.value * 0.65
  return {
    left: flip ? 'auto' : `${x + 10}px`,
    right: flip ? `${plotW.value - x + 10}px` : 'auto',
    top: `${PAD.top}px`,
  }
})
</script>

<template>
  <div class="card cost-panel">
    <div class="head">
      <div>
        <div class="label">agent 花费</div>
        <div class="faint">
          从执行事件流按阶段聚合（不是任务表那一列 —— 它被修复回路每轮覆盖，只留最后一轮）
        </div>
      </div>
      <div class="wrap">
        <button :disabled="loading" @click="load">{{ loading ? '刷新中…' : '刷新' }}</button>
        <button @click="showTable = !showTable">{{ showTable ? '看图' : '看表格' }}</button>
      </div>
    </div>

    <p v-if="err" class="bad-text">{{ err }}</p>

    <template v-else-if="cost">
      <!-- KPI 行：这些是数字，不该画成图 -->
      <div class="kpis">
        <div class="kpi">
          <div class="kpi-num">{{ fmtUSD(cost.totalUsd) }}</div>
          <div class="dim">累计花费</div>
        </div>
        <div class="kpi">
          <div class="kpi-num">{{ cost.runs }}</div>
          <div class="dim">执行次数</div>
        </div>
        <div class="kpi">
          <div class="kpi-num">{{ fmtUSD(avgPerRun) }}</div>
          <div class="dim">平均每次</div>
        </div>
        <div class="kpi">
          <div class="kpi-num">{{ formatDuration(cost.durationMs) }}</div>
          <div class="dim">累计耗时</div>
        </div>
      </div>

      <!-- 阶段占比：水平堆叠条 + 始终可见的直接标签（亮色模式 aqua 对比度
           不足 3:1，relief 规则要求标签或表格，两者都给） -->
      <div v-if="phases.length" class="section">
        <div class="sub">阶段占比</div>
        <div class="stack" role="img" :aria-label="`阶段花费占比，共 ${fmtUSD(phaseTotal)}`">
          <div
            v-for="(p, i) in phases"
            :key="p.key"
            class="seg"
            :class="[`slot-${p.slot}`, { first: i === 0, last: i === phases.length - 1 }]"
            :style="{ flexGrow: Math.max(p.pct, 0.02) }"
            :title="`${p.label} ${fmtUSD(p.usd)} · ${p.runs} 次`"
          />
        </div>
        <!-- 图例：两个及以上序列必须有图例，身份不能只靠颜色 -->
        <div class="legend">
          <span v-for="p in phases" :key="p.key" class="lg">
            <i :class="`sw slot-${p.slot}`" />
            {{ p.label }}
            <b>{{ fmtUSD(p.usd) }}</b>
            <span class="faint">{{ Math.round(p.pct * 100) }}% · {{ p.runs }} 次</span>
          </span>
        </div>
      </div>

      <!-- 30 天日花费：单序列，无需图例 -->
      <div class="section">
        <div class="sub">最近 30 天（按 UTC 日）</div>

        <div v-if="showTable" class="scroll-x">
          <table class="daily">
            <thead><tr><th>日期</th><th class="num">花费</th><th class="num">执行次数</th></tr></thead>
            <tbody>
              <tr v-for="d in days" :key="d.day">
                <td class="mono">{{ d.day }}</td>
                <td class="num mono">{{ fmtUSD(d.usd) }}</td>
                <td class="num mono">{{ d.runs }}</td>
              </tr>
              <tr v-if="!days.length"><td colspan="3" class="faint">这段时间没有 agent 执行记录</td></tr>
            </tbody>
          </table>
        </div>

        <div v-else ref="plotBox" class="plot-box">
          <svg
            v-if="days.length"
            class="plot"
            :width="plotW"
            :height="PAD.top + PLOT_H + PAD.bottom"
            @pointermove="onMove"
            @pointerleave="onLeave"
          >
            <!-- 网格：实线 hairline，比 surface 深一档，退到背景里 -->
            <g class="grid">
              <line v-for="t in yTicks" :key="'g' + t.v" :x1="PAD.left" :x2="plotW - PAD.right" :y1="t.y" :y2="t.y" />
            </g>
            <g class="axis-text">
              <text v-for="t in yTicks" :key="'y' + t.v" :x="PAD.left - 8" :y="t.y + 4" text-anchor="end">
                {{ fmtAxis(t.v) }}
              </text>
              <text
                v-for="t in xTicks"
                :key="'x' + t.i"
                :x="t.x"
                :y="PAD.top + PLOT_H + 18"
                :text-anchor="t.i === 0 ? 'start' : t.i === days.length - 1 ? 'end' : 'middle'"
              >{{ t.label }}</text>
            </g>

            <path class="area" :d="areaPath" />
            <path class="line" :d="linePath" />

            <!-- 十字准线 + 命中点 -->
            <g v-if="hoverIdx >= 0">
              <line
                class="crosshair"
                :x1="xAt(hoverIdx)" :x2="xAt(hoverIdx)"
                :y1="PAD.top" :y2="PAD.top + PLOT_H"
              />
              <circle class="dot" :cx="xAt(hoverIdx)" :cy="yAt(days[hoverIdx].usd)" r="4" />
            </g>

            <!-- 末点直接标注：选择性标注，不给每个点写数字 -->
            <g v-if="hoverIdx < 0 && days.length">
              <circle class="dot" :cx="xAt(days.length - 1)" :cy="yAt(days[days.length - 1].usd)" r="4" />
              <text
                class="end-label"
                :x="xAt(days.length - 1) - 8"
                :y="Math.max(PAD.top + 10, yAt(days[days.length - 1].usd) - 10)"
                text-anchor="end"
              >{{ fmtUSD(days[days.length - 1].usd) }}</text>
            </g>
          </svg>

          <p v-else class="faint pad">这段时间没有 agent 执行记录。</p>

          <!-- 悬浮读数：值在前、标签在后 —— 读者已经知道是哪条序列，要的是数字 -->
          <div v-if="hover" class="tip" :style="tooltipStyle">
            <div class="tip-v">{{ fmtUSD(hover.usd) }}</div>
            <div class="faint">{{ hover.day }} · {{ hover.runs }} 次执行</div>
          </div>
        </div>
      </div>

      <!-- 花费最高的任务：单色横条，不按值上色 -->
      <div v-if="cost.topTasks.length" class="section">
        <div class="sub">花费最高的任务</div>
        <div class="scroll-x">
          <table class="tops">
            <thead>
              <tr><th>Issue</th><th>状态</th><th>占比</th><th class="num">花费</th><th class="num">次数</th></tr>
            </thead>
            <tbody>
              <tr v-for="t in cost.topTasks" :key="t.taskId">
                <td><router-link :to="`/tasks/${t.taskId}`" class="mono">{{ t.issueKey }}</router-link></td>
                <td class="faint">{{ t.state }}</td>
                <td class="barcell">
                  <span
                    class="tbar"
                    :style="{ width: (cost.topTasks[0].usd > 0 ? (t.usd / cost.topTasks[0].usd) * 100 : 0) + '%' }"
                  />
                </td>
                <td class="num mono">{{ fmtUSD(t.usd) }}</td>
                <td class="num mono">{{ t.runs }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>

    <p v-else class="faint">加载中…</p>
  </div>
</template>

<style scoped>
/* 配色槽位。项目是暗色优先（:root 即暗色，亮色走 prefers-color-scheme），
   这里沿用同一套约定，不另立门户。
   三个槽位已用校验器按本项目实际 surface 跑过两个模式，全部 PASS。 */
.cost-panel {
  --s1: #3987e5; /* blue   —— 实现 / 单序列 */
  --s2: #d95926; /* orange —— 分诊 */
  --s3: #199e70; /* aqua   —— 其他 */
  --grid: #2c2c2a;
  --axis: #383835;
}
@media (prefers-color-scheme: light) {
  :root:not([data-theme='dark']) .cost-panel {
    --s1: #2a78d6;
    --s2: #eb6834;
    --s3: #1baf7a;
    --grid: #e1e0d9;
    --axis: #c3c2b7;
  }
}

.head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; margin-bottom: 14px; }
.head .wrap { display: flex; gap: 8px; flex-shrink: 0; }
.sub { color: var(--text-dim); margin-bottom: 8px; }
.section { margin-top: 18px; }
.bad-text { color: var(--bad); }
.pad { padding: 24px 0; }

/* KPI：数字用比例figures，不用 tabular-nums（大字号下等宽数字显松散） */
.kpis { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 12px; }
.kpi-num { font-size: 24px; font-weight: 600; }

/* 堆叠条：段间 2px surface 间隙做分隔，不给标记描边 */
.stack { display: flex; height: 22px; gap: 2px; }
.seg { min-width: 3px; }
.seg.first { border-radius: 4px 0 0 4px; }
.seg.last { border-radius: 0 4px 4px 0; }
.seg.first.last { border-radius: 4px; }
.slot-1 { background: var(--s1); }
.slot-2 { background: var(--s2); }
.slot-3 { background: var(--s3); }

.legend { display: flex; flex-wrap: wrap; gap: 16px; margin-top: 10px; }
.lg { display: inline-flex; align-items: center; gap: 6px; }
/* 图例镜像标记形状：面积/条用色块 */
.sw { width: 10px; height: 10px; border-radius: 2px; display: inline-block; }
/* 文字一律用文本色，绝不穿数据色 —— 浅色相当文字读不清 */
.lg, .lg b { color: var(--text); }

.plot-box { position: relative; }
.plot { display: block; touch-action: none; }
.grid line { stroke: var(--grid); stroke-width: 1; }
.axis-text text { fill: var(--text-faint); font-size: 11px; font-variant-numeric: tabular-nums; }
.area { fill: var(--s1); fill-opacity: 0.1; stroke: none; }
.line { fill: none; stroke: var(--s1); stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
.crosshair { stroke: var(--axis); stroke-width: 1; }
/* 端点/命中点带 2px surface 环，跨线重叠时仍看得清 */
.dot { fill: var(--s1); stroke: var(--surface); stroke-width: 2; }
.end-label { fill: var(--text); font-size: 11px; font-weight: 600; }

.tip {
  position: absolute; pointer-events: none;
  background: var(--surface-2); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 6px 10px; white-space: nowrap;
}
.tip-v { font-weight: 600; font-variant-numeric: tabular-nums; }

/* 表格列里的数字要纵向对齐，这里才用 tabular-nums */
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--border); }
th { color: var(--text-dim); font-weight: 500; }
.num { text-align: right; font-variant-numeric: tabular-nums; }
.barcell { width: 40%; min-width: 90px; }
/* 排行条：所有条同一个颜色。按值上色会把条长重复编码成色相，
   白占掉唯一的空闲通道（反模式：value-ramp on nominal categories） */
.tbar { display: block; height: 8px; border-radius: 0 4px 4px 0; background: var(--s1); }
</style>
