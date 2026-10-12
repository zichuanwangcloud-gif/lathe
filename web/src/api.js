// API 客户端。
//
// 401 统一抛出可识别的错误，由路由守卫切到登录页；
// 409 且带 mustChangePassword 表示账号还没改初始密码，交给注入的钩子跳转。

export class UnauthorizedError extends Error {
  constructor() {
    super('未登录')
    this.name = 'UnauthorizedError'
  }
}

// 强制改密的跳转钩子由 main.js 注入 —— api.js 不该认识 router。
let onPasswordChangeRequired = () => {}
export function setPasswordChangeHandler(fn) {
  onPasswordChangeRequired = fn
}

async function request(path, options = {}) {
  const resp = await fetch(path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
  })

  if (resp.status === 401) throw new UnauthorizedError()

  const text = await resp.text()
  // 响应不一定是 JSON（路由未注册时 mux 返回纯文本 "404 page not found"，
  // 反代故障时可能是 HTML 错误页）。盲目 JSON.parse 会把「接口 404」
  // 变成一串看不懂的解析报错 —— 先判形态再解析。
  let data = {}
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      const err = new Error(`请求失败（HTTP ${resp.status}）：${text.slice(0, 120)}`)
      err.status = resp.status
      throw err
    }
  }

  if (resp.status === 409 && data.mustChangePassword) onPasswordChangeRequired()

  if (!resp.ok) {
    const err = new Error(data.error || `请求失败（HTTP ${resp.status}）`)
    err.status = resp.status
    // 失败响应的正文一并带上：有些接口的 4xx 不只是一句话，还捎着结构化
    // 结果（PRD 提交评审的 422 里是整份自检报告，前端要逐条列出来并能
    // 跳到对应节）。只丢一个 message 的话那份报告就没了。
    err.data = data
    throw err
  }
  return data
}

export const api = {
  me: () => request('/api/me'),
  login: (email, password) =>
    request('/api/login', { method: 'POST', body: JSON.stringify({ email, password }) }),
  logout: () => request('/api/logout', { method: 'POST' }),
  register: (email, password) =>
    request('/api/register', { method: 'POST', body: JSON.stringify({ email, password }) }),

  setNotifyEmail: (email) =>
    request('/api/me/notify-email', { method: 'PUT', body: JSON.stringify({ email }) }),
  changePassword: (currentPassword, newPassword) =>
    request('/api/password/change', {
      method: 'POST',
      body: JSON.stringify({ currentPassword, newPassword }),
    }),
  forgotPassword: (email) =>
    request('/api/password/forgot', { method: 'POST', body: JSON.stringify({ email }) }),
  resetPassword: (token, password) =>
    request('/api/password/reset', { method: 'POST', body: JSON.stringify({ token, password }) }),

  tasks: (params = {}) => {
    const q = new URLSearchParams()
    if (params.state) q.set('state', params.state)
    if (params.limit) q.set('limit', params.limit)
    if (params.offset) q.set('offset', params.offset)
    const qs = q.toString()
    return request(`/api/tasks${qs ? '?' + qs : ''}`)
  },
  task: (id) => request(`/api/tasks/${id}`),
  // 执行日志增量轮询（docs/04 §3.3）：after 传上次的 last_id，首轮传 0
  taskEvents: (id, after = 0, limit = 200) =>
    request(`/api/tasks/${id}/events?after=${after}&limit=${limit}`),
  stats: () => request('/api/stats'),
  // 成本聚合独立于 /api/stats：看板每 5 秒轮询那个端点，而成本是决策视图、
  // 不需要 5 秒新鲜度（后端注释里有完整理由）。
  costStats: () => request('/api/stats/cost'),
  repos: () => request('/api/repos'),
  config: () => request('/api/config'),

  // 编排流程图（PRD 07）：批量建图、查图、列出我建过的图
  flows: () => request('/api/flows'),
  flow: (id) => request(`/api/flows/${id}`),
  createFlow: (body) => request('/api/flows', { method: 'POST', body: JSON.stringify(body) }),

  // 内置工单（docs/09）：手动建单、评论区问答、开跑/取消联动
  issues: (params = {}) => {
    const q = new URLSearchParams()
    if (params.state) q.set('state', params.state)
    if (params.limit) q.set('limit', params.limit)
    if (params.offset) q.set('offset', params.offset)
    const qs = q.toString()
    return request(`/api/issues${qs ? '?' + qs : ''}`)
  },
  issue: (id) => request(`/api/issues/${id}`),
  createIssue: (body) => request('/api/issues', { method: 'POST', body: JSON.stringify(body) }),
  updateIssue: (id, body) => request(`/api/issues/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteIssue: (id) => request(`/api/issues/${id}`, { method: 'DELETE' }),
  addIssueComment: (id, body) =>
    request(`/api/issues/${id}/comments`, { method: 'POST', body: JSON.stringify({ body }) }),
  startInternalIssue: (id) => request(`/api/issues/${id}/start`, { method: 'POST' }),

  // 模糊任务规划（docs/10-prd-template.md）：多轮对话式智能体读代码产出
  // PRD，对抗复核 + 自检过闸、人逐节签字后，一键生成正式任务与编排图。
  prds: (params = {}) => {
    const q = new URLSearchParams()
    if (params.state) q.set('state', params.state)
    if (params.limit) q.set('limit', params.limit)
    if (params.offset) q.set('offset', params.offset)
    const qs = q.toString()
    return request(`/api/prds${qs ? '?' + qs : ''}`)
  },
  prd: (id) => request(`/api/prds/${id}`),
  createPrd: (body) => request('/api/prds', { method: 'POST', body: JSON.stringify(body) }),
  prdRounds: (id) => request(`/api/prds/${id}/rounds`),
  // 规划事件增量轮询：after 传上次的 lastId，首轮传 0。
  // 注意游标字段是驼峰 lastId，与既有 taskEvents 的 last_id 不同。
  prdEvents: (id, after = 0, limit = 200) =>
    request(`/api/prds/${id}/events?after=${after}&limit=${limit}`),
  // 跑一轮对话：awaiting_answers 时带上人的回答；stage 可选，不传由后端按
  // 轮次推进阶段。回 {prd, questions, notes} —— 跑完一轮整份 PRD 都可能变，
  // 所以直接给新的一份
  runPrdRound: (id, body = {}) =>
    request(`/api/prds/${id}/rounds`, { method: 'POST', body: JSON.stringify(body) }),
  reviewPrd: (id) => request(`/api/prds/${id}/review`, { method: 'POST' }),
  // 成功回 {state, report}；自检不过抛 422，报告在 err.data.report
  // （见 request 里的 err.data 注释）
  submitPrd: (id) => request(`/api/prds/${id}/submit`, { method: 'POST' }),
  approvePrd: (id) => request(`/api/prds/${id}/approve`, { method: 'POST' }),
  // 打回不带理由：理由的归宿是下一轮对话的回答，不是 append-only 的轮次记录
  rejectPrd: (id) => request(`/api/prds/${id}/reject`, { method: 'POST' }),
  abandonPrd: (id) => request(`/api/prds/${id}/abandon`, { method: 'POST' }),
  convertPrd: (id) => request(`/api/prds/${id}/convert`, { method: 'POST' }),

  // 逐节确认（🤖 → ✅）：自检要求所有推断节被人过目。❓ 的节回 409 ——
  // 挂着未决问题不许跳过去确认。回更新后的完整 PRDRow。
  confirmPrdSection: (id, section) =>
    request(`/api/prds/${id}/sections/${encodeURIComponent(section)}/confirm`, { method: 'POST' }),
  // 复核发现的处置：按下标定位（ReviewFinding 没有 id，报告是整体覆盖写的，
  // 下标在一份报告内稳定）。accept 必须带理由，否则 400 且整批不写。
  // 回更新后的完整 PRDRow。
  savePrdDispositions: (id, findings) =>
    request(`/api/prds/${id}/review/dispositions`, {
      method: 'PUT',
      body: JSON.stringify({ findings }),
    }),

  trigger: (issueKey) =>
    request('/api/tasks', { method: 'POST', body: JSON.stringify({ issueKey }) }),
  linearIssues: () => request('/api/linear/issues'),
  linearIssue: (id) => request(`/api/linear/issues/${encodeURIComponent(id)}`),
  startIssue: (issueId, issueKey) =>
    request('/api/tasks', { method: 'POST', body: JSON.stringify({ issueId, issueKey }) }),
  retry: (id, mode = 'auto') =>
    request(`/api/tasks/${id}/retry`, { method: 'POST', body: JSON.stringify({ mode }) }),
  retryPlan: (id) => request(`/api/tasks/${id}/retry-plan`),
  cancel: (id) => request(`/api/tasks/${id}/cancel`, { method: 'POST' }),
  // 人工闸门放行（gate_mode=manual）：验证已过，人看完点这个才推分支开 PR
  approve: (id) => request(`/api/tasks/${id}/approve`, { method: 'POST' }),

  // 任务预览环境：worktree 里构建镜像、起容器给人手动点
  previewCandidates: (id) => request(`/api/tasks/${id}/preview/candidates`),
  previewStatus: (id) => request(`/api/tasks/${id}/preview/status`),
  previewStart: (id, body) =>
    request(`/api/tasks/${id}/preview/start`, { method: 'POST', body: JSON.stringify(body) }),
  previewStop: (id) => request(`/api/tasks/${id}/preview/stop`, { method: 'POST' }),
  previewRecommend: (id) => request(`/api/tasks/${id}/preview/recommend`, { method: 'POST' }),
  previewRecommendStatus: (id) => request(`/api/tasks/${id}/preview/recommend`),

  adminSettings: () => request('/api/admin/settings'),
  saveAdminSettings: (body) =>
    request('/api/admin/settings', { method: 'PUT', body: JSON.stringify(body) }),
  updateRepo: (id, body) =>
    request(`/api/repos/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  createRepo: (body) =>
    request('/api/repos', { method: 'POST', body: JSON.stringify(body) }),
  repoBaseline: (id) => request(`/api/repos/${id}/baseline`),
  deployRepoBaseline: (id, composeFile) =>
    request(`/api/repos/${id}/baseline/deploy`, {
      method: 'POST',
      body: JSON.stringify({ composeFile }),
    }),

  integrations: () => request('/api/integrations'),
  saveIntegration: (kind, token) =>
    request(`/api/integrations/${kind}`, { method: 'PUT', body: JSON.stringify({ token }) }),
  verifyIntegration: (kind) =>
    request(`/api/integrations/${kind}/verify`, { method: 'POST' }),
  deleteIntegration: (kind) =>
    request(`/api/integrations/${kind}`, { method: 'DELETE' }),

  smtp: () => request('/api/smtp'),
  saveSmtp: (body) => request('/api/smtp', { method: 'PUT', body: JSON.stringify(body) }),
  verifySmtp: (testTo) =>
    request('/api/smtp/verify', { method: 'POST', body: JSON.stringify({ testTo }) }),
  deleteSmtp: () => request('/api/smtp', { method: 'DELETE' }),

  users: () => request('/api/admin/users'),
  enableUser: (id) => request(`/api/admin/users/${id}/enable`, { method: 'POST' }),
  disableUser: (id) => request(`/api/admin/users/${id}/disable`, { method: 'POST' }),
  setUserRole: (id, role) =>
    request(`/api/admin/users/${id}/role`, { method: 'POST', body: JSON.stringify({ role }) }),
  resetUserPassword: (id, password) =>
    request(`/api/admin/users/${id}/password`, {
      method: 'POST',
      body: JSON.stringify({ password: password || '' }),
    }),
  deleteUser: (id) => request(`/api/admin/users/${id}`, { method: 'DELETE' }),
}

// 状态的中文名与配色，全站统一。
export const STATE_META = {
  queued: { label: '排队中', tone: 'idle' },
  triaging: { label: '分诊中', tone: 'run' },
  blocked_spec: { label: '待补充需求', tone: 'warn' },
  blocked_dep: { label: '等待前驱', tone: 'warn' },
  awaiting_approval: { label: '待放行', tone: 'warn' },
  implementing: { label: '实现中', tone: 'run' },
  verifying: { label: '验证中', tone: 'run' },
  pr_open: { label: '已开 PR', tone: 'ok' },
  review_feedback: { label: '待处理评审', tone: 'warn' },
  merged: { label: '已合并', tone: 'ok' },
  failed: { label: '失败', tone: 'bad' },
  cancelled: { label: '已取消', tone: 'idle' },
}

export const stateLabel = (s) => STATE_META[s]?.label || s
export const stateTone = (s) => STATE_META[s]?.tone || 'idle'

// PRD（模糊任务）自己的状态机与 task 没有一条共用的边（internal/prd/state.go
// 的包注释说明了为什么不复用），所以配色表也各管各的。
export const PRD_STATE_META = {
  drafting: { label: '起草中', tone: 'run' },
  awaiting_answers: { label: '待回答', tone: 'warn' },
  ready_for_review: { label: '待批准', tone: 'warn' },
  approved: { label: '已批准', tone: 'ok' },
  converted: { label: '已生成任务', tone: 'ok' },
  abandoned: { label: '已放弃', tone: 'idle' },
}

export const prdStateLabel = (s) => PRD_STATE_META[s]?.label || s
export const prdStateTone = (s) => PRD_STATE_META[s]?.tone || 'idle'

// PRD 类型决定 §4 §7 §8 的变体（docs/10 §3）
export const PRD_TYPE_META = {
  defect: { label: '缺陷', hint: '§4 写复现路径，§2 证据必须含一次复现' },
  feature: { label: '功能', hint: '七类验收标准完整表' },
  refactor: { label: '重构', hint: '§4 写行为保持清单，首个任务必须是补特征测试' },
}

export const prdTypeLabel = (t) => PRD_TYPE_META[t]?.label || t

export function formatTime(iso) {
  if (!iso) return '—'
  const d = new Date(iso)
  const now = new Date()
  const diff = (now - d) / 1000

  if (diff < 60) return '刚刚'
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  return d.toLocaleString('zh-CN', { hour12: false })
}

export function formatDuration(ms) {
  if (ms == null) return '—'
  if (ms < 1000) return `${ms}ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60000)}m${Math.round((ms % 60000) / 1000)}s`
}
