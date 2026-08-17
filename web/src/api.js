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
  const data = text ? JSON.parse(text) : {}

  if (resp.status === 409 && data.mustChangePassword) onPasswordChangeRequired()

  if (!resp.ok) {
    const err = new Error(data.error || `请求失败（HTTP ${resp.status}）`)
    err.status = resp.status
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
  repos: () => request('/api/repos'),
  config: () => request('/api/config'),

  trigger: (issueKey) =>
    request('/api/tasks', { method: 'POST', body: JSON.stringify({ issueKey }) }),
  linearIssues: () => request('/api/linear/issues'),
  linearIssue: (id) => request(`/api/linear/issues/${encodeURIComponent(id)}`),
  startIssue: (issueId, issueKey) =>
    request('/api/tasks', { method: 'POST', body: JSON.stringify({ issueId, issueKey }) }),
  retry: (id) => request(`/api/tasks/${id}/retry`, { method: 'POST' }),
  cancel: (id) => request(`/api/tasks/${id}/cancel`, { method: 'POST' }),
  updateRepo: (id, body) =>
    request(`/api/repos/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  createRepo: (body) =>
    request('/api/repos', { method: 'POST', body: JSON.stringify(body) }),

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
