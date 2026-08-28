<script setup>
// 单条 agent 事件的渲染。
//
// 抽成组件是为了给 subagent 子块复用同一套渲染（0014）：子 agent 的内部
// 步骤与主 agent 的形状完全一致（同样是 tool_use/tool_result/text/thinking），
// 没有理由维护两份模板。
//
// 入参 ev 是 TaskDetail 里 pairTools 处理过的形状：tool_use 上挂了
// result 与 durationMs。
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { formatDuration } from '../api'

defineProps({ ev: { type: Object, required: true } })

// 模型输出不可信：issue 内容可以借间接提示注入让模型吐出带 onerror 的
// 标记，marked 出 HTML 后必须过一层消毒。
marked.setOptions({ breaks: true })
const md = (s) => DOMPurify.sanitize(marked.parse(s || ''))

// 事件 append-only，按 id 缓存渲染结果，避免每轮轮询把整屏 markdown 重算
// 一遍。放模块作用域：缓存按事件 id 唯一，跨实例共享才有意义。
const htmlCache = new Map()
function mdOf(e) {
  let h = htmlCache.get(e.id)
  if (h === undefined) {
    h = md(e.body)
    htmlCache.set(e.id, h)
  }
  return h
}

// 工具行的状态标记。返回 null 表示「无从判断」：老数据没有 toolUseId，
// 此时不能显示 ⋯ —— 那会把历史任务里早已结束的调用说成还在跑。
function toolMark(e) {
  if (!e.payload?.toolUseId) return null
  if (!e.result) return { glyph: '⋯', tone: 'run', title: '进行中或结果未落库' }
  return e.result.payload?.isError
    ? { glyph: '✗', tone: 'bad', title: '报错' }
    : { glyph: '✓', tone: 'ok', title: '完成' }
}

// result 事件的第一行是徽章行（由 payload 重新渲染），正文从空行后开始
function resultText(e) {
  const i = e.body.indexOf('\n\n')
  return i >= 0 ? e.body.slice(i + 2) : ''
}

// verify_step 同理：首行是状态行，失败时后面附截断输出
function verifyLine(e) {
  const i = e.body.indexOf('\n\n')
  return i >= 0 ? e.body.slice(0, i) : e.body
}
function verifyOutput(e) {
  const i = e.body.indexOf('\n\n')
  return i >= 0 ? e.body.slice(i + 2) : ''
}
</script>

<template>
  <!-- init / raw：一行原文 -->
  <div v-if="ev.kind === 'init' || ev.kind === 'raw'" class="faint mono ev-line">
    {{ ev.body }}
  </div>

  <!-- text：模型正文，markdown 渲染 -->
  <div v-else-if="ev.kind === 'text'" class="md" v-html="mdOf(ev)"></div>

  <!-- thinking：灰显折叠 -->
  <details v-else-if="ev.kind === 'thinking'" class="ev-fold">
    <summary class="faint">思考过程</summary>
    <pre>{{ ev.body }}</pre>
  </details>

  <!-- tool_use：工具名 + 参数摘要，结果缝在同一条上（✓/✗ + 耗时） -->
  <div v-else-if="ev.kind === 'tool_use'" class="tool-item">
    <div class="mono tool-line">
      <span
        v-if="toolMark(ev)"
        class="tool-mark"
        :class="'tm-' + toolMark(ev).tone"
        :title="toolMark(ev).title"
      >{{ toolMark(ev).glyph }}</span>
      <span>{{ ev.body }}</span>
      <span v-if="ev.durationMs != null" class="tool-dur">
        {{ formatDuration(ev.durationMs) }}
      </span>
    </div>
    <details v-if="ev.result" class="ev-fold tool-out">
      <summary :class="ev.result.payload?.isError ? 'bad-text' : 'faint'">
        {{ ev.result.payload?.isError ? '报错输出' : '输出' }}
      </summary>
      <pre>{{ ev.result.body }}</pre>
    </details>
  </div>

  <!-- tool_result：配不上发起方时才单独成行（老数据无 toolUseId） -->
  <details v-else-if="ev.kind === 'tool_result'" class="ev-fold">
    <summary :class="ev.payload?.isError ? 'bad-text' : 'faint'">
      工具结果{{ ev.payload?.isError ? '（报错）' : '' }}
    </summary>
    <pre>{{ ev.body }}</pre>
  </details>

  <!-- result：徽章行（耗时/成本/轮数）+ 终局正文 -->
  <div v-else-if="ev.kind === 'result'" class="result-box">
    <div class="row badges">
      <span class="badge" :class="ev.payload?.isError ? 'bad' : 'ok'">
        {{ ev.payload?.isError ? '失败' : '完成' }}
      </span>
      <span class="faint">{{ ev.payload?.numTurns }} 轮</span>
      <span class="faint">{{ formatDuration(ev.payload?.durationMs) }}</span>
      <span class="faint">${{ Number(ev.payload?.costUsd || 0).toFixed(4) }}</span>
      <span v-if="ev.payload?.permissionDenials" class="badge warn">
        权限拦截 ×{{ ev.payload.permissionDenials }}
      </span>
    </div>
    <div v-if="resultText(ev)" class="md" v-html="md(resultText(ev))"></div>
  </div>

  <!-- verify_step：红绿状态色，失败附截断输出 -->
  <div v-else-if="ev.kind === 'verify_step'">
    <div class="mono verify-line" :class="'st-' + (ev.payload?.status || '')">
      {{ verifyLine(ev) }}
    </div>
    <pre v-if="verifyOutput(ev)" class="ev-output">{{ verifyOutput(ev) }}</pre>
  </div>

  <div v-else class="faint mono ev-line">{{ ev.body }}</div>
</template>

<style scoped>
.ev-line { font-size: 12.5px; overflow-wrap: anywhere; }

.ev-fold summary {
  cursor: pointer;
  font-size: 12.5px;
  user-select: none;
}
.ev-fold pre { margin-top: 6px; max-height: 320px; overflow-y: auto; }

.tool-line {
  font-size: 12.5px;
  color: var(--text-dim);
  overflow-wrap: anywhere;
  display: flex;
  align-items: baseline;
  gap: 7px;
}

/* 状态标记定宽，让多行工具调用的文字左边对齐成一列，便于纵向扫读 */
.tool-mark {
  flex: none;
  width: 11px;
  text-align: center;
  font-weight: 600;
}
.tm-ok { color: var(--ok); }
.tm-bad { color: var(--bad); }
.tm-run { color: var(--run); }

.tool-dur {
  flex: none;
  margin-left: auto;
  color: var(--text-faint);
  font-size: 11.5px;
  white-space: nowrap;
}
.tool-out { margin: 4px 0 0 18px; }

.result-box .badges { margin-bottom: 6px; }
.bad-text { color: var(--bad); }

.verify-line { font-size: 12.5px; }
.verify-line.st-passed { color: var(--ok); }
.verify-line.st-failed { color: var(--bad); }
.verify-line.st-error { color: var(--warn); }
.verify-line.st-skipped { color: var(--idle); }
.ev-output { margin-top: 6px; max-height: 320px; overflow-y: auto; }

/* markdown 正文的基本排版（全局没有 md 样式，作用域内自给自足） */
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) {
  margin: 12px 0 6px;
  font-size: 14.5px;
}
.md :deep(p) { margin: 6px 0; }
.md :deep(ul), .md :deep(ol) { margin: 6px 0; padding-left: 22px; }
.md :deep(code) {
  font-family: var(--mono);
  font-size: 12px;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 4px;
  padding: 1px 5px;
}
.md :deep(pre) { margin: 8px 0; }
.md :deep(pre code) { background: none; border: none; padding: 0; }
.md :deep(a) { word-break: break-all; }
</style>
