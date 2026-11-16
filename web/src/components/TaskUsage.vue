<script setup>
// 任务详情页的「用量」卡（f03）：逐次列出这张单子的每次 agent 运行。
//
// 数据是该任务在 agent_runs 里的全部行（不受时间范围限制）。档位逐行显示：
// Fresh 重跑改过档的任务，两次尝试各按各自的档位记，一眼可见。
import { ref, onMounted } from 'vue'
import { api, formatDuration, formatTime } from '../api'

const props = defineProps({ taskId: { type: [String, Number], required: true } })

const data = ref(null)
const err = ref('')

const STATUS = { success: '成功', error: '失败', no_result: '无结果', killed: '被杀' }
const BUCKET = {
  triage: '分诊', implement: '实现', review: 'Review 修改', verify: '验证', push: '推送',
  prd_plan: 'PRD 规划', prd_review: 'PRD 复核', preview: '预览推荐', other: '其他',
}

function fmtUSD(v) {
  if (v === null || v === undefined) return '未知'
  return '$' + Number(v).toFixed(4)
}
const tokens = (r) => r.tokens.input + r.tokens.output + r.tokens.cacheRead + r.tokens.cacheCreation

onMounted(async () => {
  try {
    data.value = await api.taskUsage(props.taskId)
  } catch (e) {
    err.value = e.message || '加载失败'
  }
})
</script>

<template>
  <div class="card task-usage">
    <div class="spread">
      <div class="label">用量</div>
      <div v-if="data && data.summary.runs" class="row badges">
        <span class="badge idle">{{ data.summary.runs }} 次运行</span>
        <span class="badge idle">{{ fmtUSD(data.summary.costUsd) }}</span>
        <span v-if="data.summary.costKnown < data.summary.runs" class="faint">另有 {{ data.summary.runs - data.summary.costKnown }} 次费用未知</span>
      </div>
    </div>
    <p v-if="err" class="faint">{{ err }}</p>
    <p v-else-if="!data" class="faint">加载中…</p>
    <p v-else-if="!data.runs.length" class="faint">— 还没有运行记录</p>
    <div v-else class="scroll-x">
      <table>
        <thead>
          <tr>
            <th>开始</th><th>阶段</th><th>档位</th><th>状态</th><th>模型</th>
            <th class="num">花费</th><th class="num">token</th><th class="num">耗时</th><th class="num">轮数</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(r, i) in data.runs" :key="i">
            <td class="mono faint">{{ formatTime(r.startedAt) }}</td>
            <td>
              {{ r.phase }} <span class="faint">· {{ BUCKET[r.bucket] || r.bucket }}</span>
              <span v-if="r.resume" class="faint">· 续跑</span>
              <span v-if="r.backfill" class="faint" title="上线前的历史数据，由事件回填">· 回填</span>
            </td>
            <td>{{ r.verifyTier || '—' }}</td>
            <td>{{ STATUS[r.status] || r.status }}</td>
            <td class="mono">{{ r.model || '—' }}</td>
            <td class="num mono">{{ fmtUSD(r.costUsd) }}</td>
            <td class="num mono">{{ r.tokensKnown ? tokens(r) : '—' }}</td>
            <td class="num mono">{{ formatDuration(r.durationMs) }}</td>
            <td class="num mono">{{ r.numTurns }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.scroll-x { overflow-x: auto; }
table { width: 100%; border-collapse: collapse; margin-top: 8px; }
th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--border); white-space: nowrap; }
th { color: var(--text-dim); font-weight: 500; }
.num { text-align: right; font-variant-numeric: tabular-nums; }
</style>
