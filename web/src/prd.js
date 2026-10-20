// prd.js —— 模糊任务对话界面共用的常量表与小工具。
//
// 从原 PRDDetail.vue 拆出来：对话主区、右侧 PRD 面板、会话侧栏都要用同一套
// 节顺序、状态标记与「状态机开关」，各抄一份迟早漂移。

// 节的顺序与标题跟 internal/prd/document.go 的 Document.Sections() 对齐，
// 两边任何一侧改了都要一起改 —— 自检报告按 Section key 跳节，对不上就跳不动。
export const SECTIONS = [
  { key: 'oneLiner', title: '§1 一句话' },
  { key: 'problem', title: '§2 问题陈述' },
  { key: 'goals', title: '§3 目标与非目标' },
  { key: 'scenarios', title: '§4 用户场景 / 复现路径 / 行为清单' },
  { key: 'codeAnalysis', title: '§5 现状分析' },
  { key: 'solution', title: '§6 方案' },
  { key: 'acceptance', title: '§7 验收标准' },
  { key: 'taskSplit', title: '§8 任务拆分与依赖' },
  { key: 'risks', title: '§9 风险与未决' },
  { key: 'decisionLogSect', title: '§10 决策记录' },
]

export const SECTION_TITLE = Object.fromEntries(SECTIONS.map((s) => [s.key, s.title]))

export const SECTION_STATUS = {
  confirmed: { mark: '✅', label: '已确认', tone: 'ok' },
  inferred: { mark: '🤖', label: '推断待确认', tone: 'warn' },
  pending: { mark: '❓', label: '待答', tone: 'bad' },
}

export const STAGE_LABEL = {
  understand: '理解',
  explore: '探索',
  converge: '收敛',
  split: '拆分',
  finalize: '定稿',
}

export const AC_CATEGORY = {
  happy_path: '正常路径',
  failure_path: '失败路径',
  boundary: '边界',
  invariant: '不变量',
  non_functional: '非功能',
  observability: '可观测',
  compatibility: '兼容性',
  behavior_preserved: '行为保持',
  structural_metric: '结构度量',
}
export const VERIFY_LABEL = { auto_test: '自动化测试', script: '脚本', manual: '人工走查' }
export const QUESTION_LABEL = { open: '未答', answered: '已答', accepted_unanswered: '接受不答' }
export const TASK_KIND_LABEL = { fix: '缺陷修复', feature: '功能', hotfix: '热修' }

// 对抗复核发现的两类（internal/prd/review.go）与三种处置。
export const FINDING_KIND = {
  malicious_compliance: { label: '恶意合规', tone: 'bad' },
  uncovered_branch: { label: '分支未覆盖', tone: 'warn' },
}
export const DISPOSITIONS = [
  { value: 'fix', label: '接受 · 补 AC' },
  { value: 'reject', label: '驳回' },
  { value: 'accept', label: '认可风险不改' },
]

// 对抗复核的事件 phase（与 store.PRDPhaseReview 一致）。规划与复核刻意分开
// 显示：前者是作者说的，后者是攻击者说的，混在一条流里人就分不清。
export const PLAN_PHASE = 'plan'
export const REVIEW_PHASE = 'plan-review'

// prdCaps 把 internal/prd/state.go 的转移表翻成界面开关。processing 时一切
// 写操作禁用 —— 有一轮 agent 正在跑，这时候点任何东西都会撞 store 的 CAS。
// 不是为了省一次注定失败的请求（服务端本来就会拒），而是让人一眼看出这份
// PRD 现在卡在哪一步。
export function prdCaps(prd) {
  const s = prd?.state || ''
  const busy = !!prd?.processing
  const is = (...xs) => xs.includes(s) && !busy
  return {
    round: is('drafting', 'awaiting_answers'),
    // 复核在 drafting 也开着：自检清单要求「复核报告已就位且每条都标了处置」
    // 才放行提交，只在 ready_for_review 才能跑的话这道门永远推不开。
    review: is('drafting', 'ready_for_review'),
    submit: is('drafting'),
    approve: is('ready_for_review'),
    reject: is('ready_for_review'),
    abandon: is('drafting', 'awaiting_answers', 'ready_for_review'),
    resume: s === 'abandoned' && !busy,
    convert: is('approved'),
    // 逐节确认只在起草期有意义：冻结后改不了，待回答时该先答问题
    confirmSection: is('drafting'),
    // 反向确认答复是编辑文档内容，待回答时也能做（挂着的 ❓ 不影响它）
    reverse: is('drafting', 'awaiting_answers'),
    // approved 之后 PRD 冻结（D10-4）：内容一律不可编辑，只能生成任务
    frozen: ['approved', 'converted'].includes(s),
  }
}

// 会话标题：§1 一句话优先，还没跑出来就退回原始输入。
export function prdTitle(p) {
  const one = p?.document?.oneLiner?.body
  return ((one && one.trim()) || p?.originalInput || '（无）').split('\n')[0]
}

// 两份文档快照之间改动过的节（正文或状态变了）。首轮 prev 为空时，凡有
// 内容的节都算「新写出来的」。给对话里的「本轮更新了哪些节」用。
export function changedSections(prev, next) {
  if (!next) return []
  return SECTIONS.filter(({ key }) => {
    const a = prev?.[key] || {}
    const b = next[key] || {}
    if (!b.body && !b.status) return false
    return a.body !== b.body || a.status !== b.status
  }).map((s) => s.key)
}
