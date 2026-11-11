// 收件箱页与顶栏徽标之间的「刚改过」信号：页里标已读 / 忽略后递增，
// 徽标据此立刻重拉计数，不必等下一个 30 秒轮询。
import { reactive } from 'vue'

export const inboxSignal = reactive({ version: 0 })
export const bumpInbox = () => inboxSignal.version++

// 种类 → 组标题与徽标色，顺序即页面分组顺序（与 internal/inbox 的 Kinds() 一致）。
export const INBOX_KINDS = [
  { kind: 'task_blocked_spec', label: '等你补充需求', tone: 'warn' },
  { kind: 'task_awaiting_approval', label: '等你放行', tone: 'warn' },
  { kind: 'task_review_parked', label: 'review 停车待处理', tone: 'warn' },
  { kind: 'task_blocked_dep', label: '前驱阻塞', tone: 'warn' },
  { kind: 'task_failed', label: '失败待处置', tone: 'bad' },
  { kind: 'prd_run_failed', label: 'PRD 运行失败', tone: 'bad' },
  { kind: 'prd_answers', label: 'PRD 等你回答', tone: 'warn' },
  { kind: 'prd_review', label: 'PRD 等你批准', tone: 'warn' },
  { kind: 'prd_confirm', label: 'PRD 待逐节确认', tone: 'run' },
]
