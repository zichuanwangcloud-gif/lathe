<script setup>
// 平台自度量面板（docs/05-roadmap.md §3.6 第 15 条）。
//
// 形态选择（先定形态，再定颜色）：
//   - 「参与统计的任务数 / 失败单数 / 最慢环节」= 几个头条数字 → KPI 行，
//     不是图。柱状图画三个孤立的数没有意义
//   - 各环节时长 = 同一个量在四个环节间比较 → 水平条。**用水平不用竖直**：
//     环节名是中文（「验证」「实现」），横排比旋转 45° 好读
//   - 每个环节的 p50 与 p95 = 同一个量的两个统计点 → **meter**：p95 当量程
//     满宽（同色系浅档），p50 是里面那条实心填充。这是「比值对上限」的标准
//     形态，比画两根并排的柱子更能表达「典型值落在尾部之内」
//   - 失败分类 = 类别 + 量值 → 单色水平条 + 直接标签。**刻意不按值上色**：
//     那会把条长重复编码成色相，白占掉唯一的空闲通道；
//     也**刻意不用分类色**：阶段是量值比较的对象，不是需要区分的身份
//
// 颜色：全部落在蓝这一个 hue 上（数据色只有一档实色 + 一档浅底），
// 所以不存在「相邻分类色在色盲下分不开」的问题。
//
// 4px 圆角只给条的数据端（远离基线的那端），基线端切平 —— 圆角是数据端的
// 装饰，不是盒子的装饰。
import { ref, computed, onMounted } from 'vue'
import { api, formatDuration } from '../api.js'

const data = ref(null)
const err = ref('')
const loading = ref(false)
const showTable = ref(false)

// 环节的中文名与展示顺序。键是后端的契约（store.SegmentQueue 等），
// 顺序与后端 segmentOrder 一致 —— 两处都定死，不让图例随数据重排。
const SEGMENTS = [
  { key: 'queue', label: '排队等待' },
  { key: 'triage', label: '分诊' },
  { key: 'implement', label: '实现' },
  { key: 'verify', label: '验证' },
  { key: 'total', label: '端到端' },
]

async function load() {
  loading.value = true
  try {
    data.value = await api.metricsStats()
    err.value = ''
  } catch (e) {
    err.value = e.message || '加载失败'
  } finally {
    loading.value = false
  }
}
onMounted(load)

// segments 按后端给的顺序取，只保留有样本的环节。
// total 单独拿开：它是「全程」，混在环节条里会被误读成第五个环节。
const stageSegments = computed(() => {
  const by = {}
  for (const s of data.value?.segments || []) by[s.key] = s
  return SEGMENTS.filter((s) => s.key !== 'total')
    .map((s) => ({ ...s, stat: by[s.key] }))
    .filter((s) => s.stat)
})

const totalSegment = computed(() => {
  for (const s of data.value?.segments || []) {
    if (s.key === 'total') return s
  }
  return null
})

// 条的比例尺：所有环节共用一条轴（p95 的最大值），否则各环节的条长
// 各自为政，读者没法横向比较「哪一环更慢」—— 那样画出来的图不如看表格。
const scaleMax = computed(() => {
  let max = 0
  for (const s of stageSegments.value) {
    max = Math.max(max, Number(s.stat.p95Ms) || 0)
  }
  return max > 0 ? max : 1 // 防除零；全为 0 时所有条都画成 0 宽
})

function p95Pct(stat) {
  return Math.min(100, ((Number(stat.p95Ms) || 0) / scaleMax.value) * 100)
}
function p50Pct(stat) {
  // p50 恒 <= p95，所以不必再 min，但 p95 为 0 时会算出 NaN —— 分开兜
  const v = Number(stat.p50Ms) || 0
  return Math.min(100, (v / scaleMax.value) * 100)
}

// 最慢环节：只看有样本的四段（不含端到端），用 p95 判定 ——
// 「大多数单子在这一环最多花多久」比 p50 更接近「哪一环在拖」。
const slowest = computed(() => {
  let best = null
  for (const s of stageSegments.value) {
    if (!best || Number(s.stat.p95Ms) > Number(best.stat.p95Ms)) best = s
  }
  return best
})

// 失败分类条的比例尺
const maxFailure = computed(() => {
  let max = 0
  for (const f of data.value?.failureStages || []) max = Math.max(max, Number(f.count) || 0)
  return max > 0 ? max : 1
})

const failureTotal = computed(() =>
  (data.value?.failureStages || []).reduce((s, f) => s + (Number(f.count) || 0), 0)
)

function fmt(ms) {
  // formatDuration 对 null/undefined 返回「—」；0 会渲染成 0ms，这是对的
  return formatDuration(Number(ms) || 0)
}
</script>

<template>
  <div class="card metrics-panel">
    <div class="head">
      <div>
        <div class="label">平台度量</div>
        <div class="faint">
          各环节耗时分位数与失败分类，从状态转移事件流聚合（不是
          tasks.updated_at —— 那一列每次 UPDATE 都被刷新，算不出分段）
        </div>
      </div>
      <div class="wrap">
        <button :disabled="loading" @click="load">{{ loading ? '刷新中…' : '刷新' }}</button>
        <button @click="showTable = !showTable">{{ showTable ? '看图' : '看表格' }}</button>
      </div>
    </div>

    <p v-if="err" class="bad-text">{{ err }}</p>

    <template v-else-if="data">
      <!-- KPI 行：孤立数字不该画成图 -->
      <div class="kpis">
        <div class="kpi">
          <div class="kpi-num">{{ data.tasks }}</div>
          <div class="dim">参与统计的任务</div>
        </div>
        <div class="kpi">
          <div class="kpi-num">{{ failureTotal }}</div>
          <div class="dim">失败任务</div>
        </div>
        <div class="kpi">
          <div class="kpi-num">{{ totalSegment ? fmt(totalSegment.p50Ms) : '—' }}</div>
          <div class="dim">端到端中位数</div>
        </div>
        <div class="kpi">
          <div class="kpi-num">{{ slowest ? slowest.label : '—' }}</div>
          <div class="dim">最慢环节（按 p95）</div>
        </div>
      </div>

      <!-- 表格视图：完整数据（含 max 与累计耗时），也是颜色之外的第二条信息通道 -->
      <div v-if="showTable" class="section">
        <div class="sub">各环节耗时</div>
        <div class="scroll-x">
          <table>
            <thead>
              <tr>
                <th>环节</th>
                <th class="num">样本</th>
                <th class="num">p50</th>
                <th class="num">p95</th>
                <th class="num">最大</th>
                <th class="num">累计</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in data.segments" :key="s.key">
                <td>{{ (SEGMENTS.find((x) => x.key === s.key) || {}).label || s.key }}</td>
                <td class="num mono">{{ s.count }}</td>
                <td class="num mono">{{ fmt(s.p50Ms) }}</td>
                <td class="num mono">{{ fmt(s.p95Ms) }}</td>
                <td class="num mono">{{ fmt(s.maxMs) }}</td>
                <td class="num mono">{{ fmt(s.totalMs) }}</td>
              </tr>
              <tr v-if="!data.segments.length">
                <td colspan="6" class="faint">还没有跑完的任务可以统计</td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="sub" style="margin-top: 16px">失败分类</div>
        <div class="scroll-x">
          <table>
            <thead>
              <tr><th>失败阶段</th><th class="num">次数</th></tr>
            </thead>
            <tbody>
              <tr v-for="f in data.failureStages" :key="f.stage || 'unknown'">
                <td>{{ f.label }}</td>
                <td class="num mono">{{ f.count }}</td>
              </tr>
              <tr v-if="!data.failureStages.length">
                <td colspan="2" class="faint">这个账号下还没有失败过的任务</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <template v-else>
        <!-- 环节耗时：meter（p95 是量程，p50 是填充） -->
        <div class="section">
          <div class="sub">各环节耗时 · p50 与 p95</div>

          <div v-if="stageSegments.length" class="meters">
            <div v-for="s in stageSegments" :key="s.key" class="mrow">
              <div class="mname">{{ s.label }}</div>
              <div
                class="mtrack"
                role="img"
                :aria-label="`${s.label}：p50 ${fmt(s.stat.p50Ms)}，p95 ${fmt(s.stat.p95Ms)}，样本 ${s.stat.count}`"
              >
                <div v-if="p50Pct(s.stat) > 0" class="mfill" :style="{ width: p50Pct(s.stat) + '%' }" />
                <div class="mp95" :style="{ left: p95Pct(s.stat) + '%' }" />
              </div>
              <!-- 直接标签：index 线标出 p95 的位置，数值写在条外。
                   颜色永远不是唯一的信息通道 -->
              <div class="mvals">
                <span class="mv">{{ fmt(s.stat.p50Ms) }}</span>
                <span class="faint">/ {{ fmt(s.stat.p95Ms) }}</span>
                <span class="faint">·{{ s.stat.count }} 单</span>
              </div>
            </div>
          </div>

          <!-- 空态说清「为什么空」：不是没数据，是还没有跑完的单子 -->
          <p v-else class="faint pad">
            还没有走完「分诊 → 实现 → 验证」的任务，暂时算不出环节耗时。<br />
            任务跑通一次后这里就会有数据。
          </p>

          <!-- 图例：两档值必须说明谁是谁，身份不能只靠深浅 -->
          <div v-if="stageSegments.length" class="legend">
            <span class="lg"><i class="sw p50" /> p50（典型）</span>
            <span class="lg"><i class="sw p95mark" /> p95（尾部）</span>
            <span class="faint">最长的一环按 p95 定，共 {{ stageSegments.length }} 个环节可比</span>
          </div>
        </div>

        <!-- 失败分类：单色水平条 + 直接标签 -->
        <div class="section">
          <div class="sub">失败分类</div>
          <div v-if="data.failureStages.length" class="fails">
            <div v-for="f in data.failureStages" :key="f.stage || 'unknown'" class="frow">
              <div class="fname">{{ f.label }}</div>
              <div class="ftrack">
                <div v-if="f.count > 0" class="fstrip" :style="{ width: (Number(f.count) / maxFailure) * 100 + '%' }" />
              </div>
              <div class="fcount">{{ f.count }}</div>
            </div>
          </div>
          <p v-else class="faint pad">这个账号下还没有失败过的任务。</p>
        </div>
      </template>
    </template>

    <p v-else class="faint pad">加载中…</p>
  </div>
</template>

<style scoped>
/* 配色：数据色只用一个 hue 的两档（实色 + 浅底），因此不存在相邻分类色
   在色盲下分不开的问题。两档都按本项目的实际 surface（亮 #ffffff、
   暗 #171a21）取过，暗底 #3987e5 与亮底 #2a78d6 对各自 surface 都 >= 3:1。 */
.metrics-panel {
  --data: #3987e5;
  --data-soft: rgba(57, 135, 229, 0.18);
}
@media (prefers-color-scheme: light) {
  :root:not([data-theme='dark']) .metrics-panel {
    --data: #2a78d6;
    --data-soft: rgba(42, 120, 214, 0.16);
  }
}

.head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; margin-bottom: 14px; }
.head .wrap { display: flex; gap: 8px; flex-shrink: 0; }
.sub { color: var(--text-dim); margin-bottom: 10px; }
.section { margin-top: 18px; }
.bad-text { color: var(--bad); }
.pad { padding: 20px 0; }

/* KPI：数字用比例 figures，不用 tabular-nums（大字号下等宽数字显松散） */
.kpis { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 12px; }
.kpi-num { font-size: 22px; font-weight: 600; }

/* ---- 环节耗时 meter ---- */
.meters { display: flex; flex-direction: column; gap: 10px; }
.mrow {
  display: grid;
  grid-template-columns: 72px 1fr auto;
  align-items: center;
  gap: 10px;
}
.mname { color: var(--text-dim); font-size: 13px; }
/* 量程轨道：p95 的浅底。右端切平（它是数据刻度不是盒子），左端也不圆 ——
   条形图的数据端在右边 */
.mtrack {
  position: relative;
  height: 18px;
  background: var(--data-soft);
  border-radius: 0 4px 4px 0;
  overflow: visible;
}
/* p50 实心填充：唯一的数据端（右端）给 4px 圆角 */
.mfill {
  position: absolute;
  left: 0; top: 0; bottom: 0;
  background: var(--data);
  border-radius: 0 4px 4px 0;
}
/* p95 位标：一条 2px 的 index 线，压在同色浅底上仍然看得见 —— 它标的是
   「尾部到这里」，不是另一条数据条 */
.mp95 {
  position: absolute;
  top: -3px; bottom: -3px;
  width: 2px;
  background: var(--text-dim);
  transform: translateX(-1px);
}
.mvals { font-variant-numeric: tabular-nums; font-size: 13px; white-space: nowrap; }
.mv { font-weight: 600; }

.legend { display: flex; flex-wrap: wrap; gap: 16px; margin-top: 12px; align-items: center; font-size: 13px; }
.lg { display: inline-flex; align-items: center; gap: 6px; }
/* 图例镜像标记形状：填充色块 / index 线 */
.sw { display: inline-block; }
.sw.p50 { width: 14px; height: 10px; border-radius: 0 3px 3px 0; background: var(--data); }
.sw.p95mark { width: 2px; height: 14px; background: var(--text-dim); }

/* ---- 失败分类 ---- */
.fails { display: flex; flex-direction: column; gap: 8px; }
/* 三列网格：名字 / 条 / 次数。**不给条留自适应宽度** —— 早期版本让条与
   标签同行挤，最长的条会把标签顶出卡片右边界（截图里「验证未通过」被裁掉
   过）。网格把三者的位置钉死，条再长也只在中间那一列里伸缩。 */
.frow {
  display: grid;
  grid-template-columns: minmax(120px, max-content) 1fr 48px;
  align-items: center;
  gap: 10px;
}
.fname { color: var(--text); font-size: 13px; }
.ftrack { min-width: 0; }
/* 单色条：按值上色会把条长重复编码成色相（反模式：value-ramp on
   nominal categories），这里量值只由长度承载。数据端（右端）4px 圆角 */
.fstrip {
  height: 14px;
  background: var(--data);
  border-radius: 0 4px 4px 0;
}
/* 次数列右对齐 + tabular-nums：纵向可比 */
.fcount {
  color: var(--text);
  font-size: 13px;
  font-weight: 600;
  text-align: right;
  font-variant-numeric: tabular-nums;
}

table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--border); }
th { color: var(--text-dim); font-weight: 500; }
.num { text-align: right; font-variant-numeric: tabular-nums; }
</style>
