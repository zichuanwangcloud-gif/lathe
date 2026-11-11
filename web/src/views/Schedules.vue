<script setup>
import { ref, computed, watch, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import BaseDialog from '../components/BaseDialog.vue'
import { confirmDialog } from '../confirm'

// 定时建单（docs/features/scheduled-issue.md）：到点只在 Linear 上建一张单，
// 之后那张单和人手工建的一样走 webhook → 接单 → 分诊 → 验证 → PR。
// 调度器本身不碰代码，这一页只管规则与运行记录。

const onUnauthorized = inject('onUnauthorized')

const schedules = ref([])
const triggerLabel = ref('')
const loading = ref(false)
const error = ref('')
const notice = ref('')

const WEEKDAYS = [
  { bit: 1, label: '一' }, { bit: 2, label: '二' }, { bit: 3, label: '三' }, { bit: 4, label: '四' },
  { bit: 5, label: '五' }, { bit: 6, label: '六' }, { bit: 0, label: '日' },
]
// 双花括号拼在脚本里：写进模板插值会被 Vue 当成插值的结束符
const PLACEHOLDERS = ['date', 'time', 'weekday', 'datetime', 'week', 'schedule'].map((p) => `{${'{'}${p}}${'}'}`)
const RUN_TONE = { created: 'ok', failed: 'bad', skipped: 'warn', pending: 'run' }
const RUN_LABEL = { created: '已建单', failed: '失败', skipped: '跳过', pending: '执行中' }
const TRIGGER_LABEL = { schedule: '准点', catchup: '补偿', manual: '立即运行' }
const REASON_LABEL = {
  missed_window: '错过补偿窗口',
  owner_disabled: '属主已停用',
  schedule_disabled: '规则已停用',
  interrupted: '结果未知（节点中断）',
}

function fail(e, target = error) {
  if (e instanceof UnauthorizedError) return onUnauthorized()
  target.value = e.message
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await api.schedules()
    schedules.value = r.schedules || []
    triggerLabel.value = r.triggerLabel || ''
  } catch (e) {
    fail(e)
  } finally {
    loading.value = false
  }
}

// ---------- 新建 / 编辑 ----------
const showEdit = ref(false)
const editing = ref(null) // 编辑中的规则 id；null = 新建
const form = ref({})
const saving = ref(false)
const formError = ref('')
const teams = ref([])
const teamsError = ref('')
const preview = ref({ description: '', next: [] })
const previewError = ref('')

const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'

function hhmm(minute) {
  const m = minute ?? 540
  return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`
}
function toMinute(s) {
  const [h, m] = (s || '09:00').split(':').map(Number)
  return h * 60 + m
}

function blankForm() {
  return {
    name: '', enabled: true, kind: 'weekly', time: '09:00', weekdays: [1], intervalHours: 24,
    timezone: browserTZ, linearTeamId: '', intakeMode: 'assign',
    titleTemplate: '每周依赖升级 {{week}}', bodyTemplate: '', priority: 0,
  }
}

function formOf(s) {
  const days = []
  for (let b = 0; b < 7; b++) if ((s.weekdays || 0) & (1 << b)) days.push(b)
  return {
    name: s.name, enabled: s.enabled, kind: s.kind, time: hhmm(s.minuteOfDay),
    weekdays: days.length ? days : [1], intervalHours: (s.intervalMinutes || 1440) / 60,
    timezone: s.timezone, linearTeamId: s.linearTeamId, intakeMode: s.intakeMode,
    titleTemplate: s.titleTemplate, bodyTemplate: s.bodyTemplate, priority: s.priority,
    anchorAt: s.anchorAt,
  }
}

// 时间定义部分（preview 与保存共用）
function specBody(f) {
  const b = { kind: f.kind, timezone: f.timezone }
  if (f.kind === 'interval') {
    b.intervalMinutes = Math.round(Number(f.intervalHours) * 60)
    if (f.anchorAt) b.anchorAt = f.anchorAt
  } else {
    b.minuteOfDay = toMinute(f.time)
    if (f.kind === 'weekly') b.weekdays = f.weekdays.reduce((acc, d) => acc | (1 << d), 0)
  }
  return b
}

function payload(f) {
  return {
    ...specBody(f),
    name: f.name.trim(), enabled: f.enabled, linearTeamId: f.linearTeamId, intakeMode: f.intakeMode,
    titleTemplate: f.titleTemplate, bodyTemplate: f.bodyTemplate, priority: Number(f.priority) || 0,
  }
}

async function openEdit(s) {
  editing.value = s ? s.id : null
  form.value = s ? formOf(s) : blankForm()
  formError.value = ''
  showEdit.value = true
  if (!teams.value.length) {
    try {
      const r = await api.linearTeams()
      teams.value = r.teams || []
      teamsError.value = ''
    } catch (e) {
      if (e instanceof UnauthorizedError) return onUnauthorized()
      teamsError.value = e.message
    }
  }
  if (!form.value.linearTeamId && teams.value.length) form.value.linearTeamId = teams.value[0].id
  refreshPreview()
}

let previewTimer = null
function refreshPreview() {
  clearTimeout(previewTimer)
  previewTimer = setTimeout(async () => {
    try {
      preview.value = await api.previewSchedule(specBody(form.value))
      previewError.value = ''
    } catch (e) {
      if (e instanceof UnauthorizedError) return onUnauthorized()
      preview.value = { description: '', next: [] }
      previewError.value = e.message
    }
  }, 250)
}
watch(
  () => showEdit.value && JSON.stringify(specBody(form.value)),
  (v) => { if (v) refreshPreview() },
)

const canSave = computed(() =>
  form.value.name?.trim() && form.value.titleTemplate?.trim() && form.value.linearTeamId &&
  (form.value.kind !== 'weekly' || form.value.weekdays.length))

async function save() {
  saving.value = true
  formError.value = ''
  try {
    if (editing.value) await api.updateSchedule(editing.value, payload(form.value))
    else await api.createSchedule(payload(form.value))
    showEdit.value = false
    await load()
  } catch (e) {
    fail(e, formError)
  } finally {
    saving.value = false
  }
}

async function toggle(s) {
  const f = formOf(s)
  f.enabled = !s.enabled
  error.value = ''
  try {
    await api.updateSchedule(s.id, payload(f))
    await load()
  } catch (e) {
    fail(e)
  }
}

async function remove(s) {
  const ok = await confirmDialog(
    `删除规则「${s.name}」？运行记录会一起删除；Linear 上已建的单和 Lathe 里的任务都保留。想留记录可以只停用不删除。`)
  if (!ok) return
  try {
    await api.deleteSchedule(s.id)
    if (expanded.value === s.id) expanded.value = null
    await load()
  } catch (e) {
    fail(e)
  }
}

async function runNow(s) {
  error.value = ''
  notice.value = ''
  try {
    const r = await api.runSchedule(s.id)
    const run = r.run || {}
    notice.value = run.status === 'created'
      ? `已在 Linear 建单 ${run.linearIssueIdentifier || ''}，等待 webhook 接单`
      : `立即运行结果：${RUN_LABEL[run.status] || run.status}${run.error ? '（' + run.error + '）' : ''}`
    await load()
    if (expanded.value === s.id) await loadRuns(s.id)
  } catch (e) {
    fail(e)
  }
}

// ---------- 运行记录 ----------
const expanded = ref(null)
const runs = ref([])
const runsError = ref('')

async function toggleRuns(s) {
  if (expanded.value === s.id) {
    expanded.value = null
    return
  }
  expanded.value = s.id
  await loadRuns(s.id)
}

async function loadRuns(id) {
  runsError.value = ''
  try {
    const r = await api.scheduleRuns(id)
    runs.value = r.runs || []
  } catch (e) {
    fail(e, runsError)
  }
}

// created 超过 10 分钟仍无任务：多半是 webhook 没配好
function notPickedUp(r) {
  if (r.status !== 'created' || r.task) return false
  const t = new Date(r.finishedAt || r.createdAt).getTime()
  return Date.now() - t > 10 * 60 * 1000
}

function fmtAbs(iso) {
  if (!iso) return '—'
  return new Date(iso).toLocaleString()
}

onMounted(load)
</script>

<template>
  <div>
    <div class="spread" style="margin-bottom: 16px">
      <div>
        <h2 style="margin: 0">定时建单</h2>
        <p class="dim" style="margin: 4px 0 0">
          到点用你的 Linear 令牌在 Linear 上建一张单，之后和手工建的单一样走接单 → 分诊 → 验证 → PR。
          调度器本身不改代码。
        </p>
      </div>
      <button class="primary" @click="openEdit(null)">新建规则</button>
    </div>

    <div v-if="error" role="alert" class="error-banner">{{ error }}</div>
    <div v-if="notice" class="card" style="margin-bottom: 12px">{{ notice }}</div>

    <div v-if="loading" class="card empty">加载中…</div>
    <div v-else-if="!schedules.length" class="card empty">
      还没有定时规则。适合周期性的活：每周一升级依赖、每天清一遍 lint 告警……
      前提是 Linear 令牌与 webhook 已在「个人设置」配好，并已登记仓库。
    </div>
    <div v-else class="card scroll-x" style="padding: 0">
      <table>
        <thead>
          <tr><th>名称</th><th>规则</th><th>下次触发</th><th>最近结果</th><th>启用</th><th></th></tr>
        </thead>
        <tbody>
          <template v-for="s in schedules" :key="s.id">
            <tr :class="{ paused: s.pausedReason && !s.enabled }">
              <td>
                <a href="#" @click.prevent="toggleRuns(s)">{{ s.name }}</a>
                <div v-if="s.pausedReason && !s.enabled" class="badge warn" style="margin-top: 4px">
                  {{ s.pausedReason }}
                </div>
              </td>
              <td class="dim">{{ s.description }} · {{ s.linearTeamKey || s.linearTeamId }}
                · {{ s.intakeMode === 'label' ? '打接单标签' : '指派给我' }}</td>
              <td class="dim">{{ s.enabled ? fmtAbs(s.nextFireAt) : '—' }}</td>
              <td>
                <span v-if="s.lastRun" class="badge" :class="RUN_TONE[s.lastRun.status]">
                  {{ RUN_LABEL[s.lastRun.status] || s.lastRun.status }}
                </span>
                <span v-else class="dim">—</span>
              </td>
              <td><input type="checkbox" :checked="s.enabled" :aria-label="`启用 ${s.name}`" @change="toggle(s)" /></td>
              <td class="row" style="gap: 6px; justify-content: flex-end">
                <button @click="runNow(s)">立即运行</button>
                <button @click="openEdit(s)">编辑</button>
                <button @click="remove(s)">删除</button>
              </td>
            </tr>
            <tr v-if="expanded === s.id">
              <td colspan="6" class="runs-cell">
                <div v-if="runsError" role="alert" class="error-banner">{{ runsError }}</div>
                <div v-else-if="!runs.length" class="dim">还没有运行记录。</div>
                <table v-else class="runs">
                  <thead>
                    <tr><th>计划时刻</th><th>触发</th><th>状态</th><th>Linear 单</th><th>任务</th><th>说明</th></tr>
                  </thead>
                  <tbody>
                    <tr v-for="r in runs" :key="r.id">
                      <td class="mono">{{ fmtAbs(r.scheduledFor) }}</td>
                      <td>{{ TRIGGER_LABEL[r.trigger] || r.trigger }}</td>
                      <td><span class="badge" :class="RUN_TONE[r.status]">{{ RUN_LABEL[r.status] || r.status }}</span></td>
                      <td>
                        <a v-if="r.linearIssueUrl" :href="r.linearIssueUrl" target="_blank" rel="noopener">
                          {{ r.linearIssueIdentifier }}</a>
                        <span v-else class="dim">—</span>
                      </td>
                      <td>
                        <RouterLink v-if="r.task" :to="`/tasks/${r.task.id}`">#{{ r.task.id }} {{ r.task.state }}</RouterLink>
                        <span v-else class="dim">—</span>
                      </td>
                      <td class="dim">
                        <div v-if="r.reason">{{ REASON_LABEL[r.reason] || r.reason }}</div>
                        <div v-if="r.missedCount > 0">合并了 {{ r.missedCount }} 次错过的时刻</div>
                        <div v-if="r.error" class="err-text">{{ r.error }}</div>
                        <div v-if="notPickedUp(r)" class="warn-text">
                          单已建好但还没被接单：检查 Linear webhook 是否指向你的回调地址并勾选了 Issue 事件
                        </div>
                        <div class="dim">{{ formatTime(r.createdAt) }}</div>
                      </td>
                    </tr>
                  </tbody>
                </table>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <BaseDialog v-if="showEdit" label-id="schedule-edit-title" width="640px" @close="showEdit = false">
      <h3 id="schedule-edit-title" style="margin-top: 0">{{ editing ? '编辑定时规则' : '新建定时规则' }}</h3>
      <div v-if="formError" role="alert" class="error-banner">{{ formError }}</div>

      <label class="field">
        <span>名称</span>
        <input v-model="form.name" maxlength="100" placeholder="例如：每周依赖升级" data-autofocus />
      </label>

      <div class="field">
        <span>类型</span>
        <div class="row" style="gap: 12px">
          <label><input v-model="form.kind" type="radio" value="daily" /> 每日</label>
          <label><input v-model="form.kind" type="radio" value="weekly" /> 每周</label>
          <label><input v-model="form.kind" type="radio" value="interval" /> 每 N 小时</label>
        </div>
      </div>

      <label v-if="form.kind !== 'interval'" class="field">
        <span>时间</span>
        <input v-model="form.time" type="time" style="width: 140px" />
      </label>
      <div v-if="form.kind === 'weekly'" class="field">
        <span>星期</span>
        <div class="row" style="gap: 8px">
          <label v-for="d in WEEKDAYS" :key="d.bit">
            <input v-model="form.weekdays" type="checkbox" :value="d.bit" /> 周{{ d.label }}
          </label>
        </div>
      </div>
      <label v-if="form.kind === 'interval'" class="field">
        <span>间隔小时数（1–168）</span>
        <input v-model.number="form.intervalHours" type="number" min="1" max="168" style="width: 120px" />
      </label>
      <label class="field">
        <span>时区（IANA 名）</span>
        <input v-model="form.timezone" placeholder="Asia/Shanghai" />
      </label>

      <div class="card preview">
        <div v-if="previewError" class="err-text">{{ previewError }}</div>
        <template v-else>
          <div>{{ preview.description }}</div>
          <div class="dim">接下来 5 次：</div>
          <ul>
            <li v-for="t in preview.next" :key="t" class="mono">{{ fmtAbs(t) }}</li>
          </ul>
        </template>
      </div>

      <label class="field">
        <span>Linear 团队</span>
        <select v-model="form.linearTeamId">
          <option v-for="t in teams" :key="t.id" :value="t.id">{{ t.key }} · {{ t.name }}</option>
        </select>
        <span v-if="teamsError" class="err-text">{{ teamsError }}</span>
      </label>

      <div class="field">
        <span>接单方式</span>
        <div class="row" style="gap: 12px">
          <label><input v-model="form.intakeMode" type="radio" value="assign" /> 指派给我</label>
          <label :class="{ dim: !triggerLabel }">
            <input v-model="form.intakeMode" type="radio" value="label" :disabled="!triggerLabel" />
            打接单标签{{ triggerLabel ? `「${triggerLabel}」` : '（管理员未在系统设置里开启标签接单）' }}
          </label>
        </div>
      </div>

      <label class="field">
        <span>标题模板</span>
        <input v-model="form.titleTemplate" maxlength="200" />
      </label>
      <label class="field">
        <span>正文模板（可选）</span>
        <textarea v-model="form.bodyTemplate" rows="5" maxlength="8000"
          placeholder="写清这次要做什么，agent 分诊时会读到。"></textarea>
      </label>
      <p class="dim" style="margin-top: -4px">
        可用占位符：<span v-for="p in PLACEHOLDERS" :key="p" class="mono">{{ p }} </span>
      </p>
      <label class="field">
        <span>优先级（Linear：0 无、1 紧急 … 4 低）</span>
        <input v-model.number="form.priority" type="number" min="0" max="4" style="width: 120px" />
      </label>
      <label class="row" style="gap: 6px; margin-bottom: 12px">
        <input v-model="form.enabled" type="checkbox" /> 启用（保存时校验 Linear 令牌、webhook 与团队）
      </label>

      <div class="row" style="justify-content: flex-end; gap: 8px">
        <button @click="showEdit = false">取消</button>
        <button class="primary" :disabled="saving || !canSave" @click="save">{{ saving ? '保存中…' : '保存' }}</button>
      </div>
    </BaseDialog>
  </div>
</template>

<style scoped>
tr.paused td { background: var(--warn-bg); }
.runs-cell { background: var(--idle-bg); }
.runs { width: 100%; }
.preview { margin: 4px 0 12px; padding: 8px 12px; }
.preview ul { margin: 4px 0 0; padding-left: 18px; }
.err-text { color: var(--bad); white-space: pre-wrap; word-break: break-all; }
.warn-text { color: var(--warn); }
</style>
