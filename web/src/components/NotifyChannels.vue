<script setup>
// 站外通知渠道（Slack incoming webhook / 飞书自定义机器人，f09）。
//
// 三条刻意的取舍（docs/features/notify-channels.md）：
//   - URL 与密钥框是 password、永不回显：服务端只回 https://<host>/…；
//   - 留空即保留原值：只改勾选框时请求里没有凭据，按钮是「保存」、不发测试消息；
//     凭据框有输入时按钮变成「保存并发送测试消息」；
//   - 事件勾选框的文案取服务端的 eventLabels（与邮件主题同一张表），前端不写中文标签。
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { confirmDialog } from '../confirm'

const onUnauthorized = inject('onUnauthorized')

const META = {
  slack: {
    name: 'Slack 通知',
    desc: '把任务失败、等你补需求、等你放行、PR 已开、模糊任务待签字推到 Slack 频道。',
    where: 'Slack → 应用 Incoming Webhooks → Add New Webhook to Workspace，复制 Webhook URL',
    placeholder: 'https://hooks.slack.com/services/…',
  },
  feishu: {
    name: '飞书通知',
    desc: '同样的五类事件推到飞书群（也支持 Lark 国际版域名）。',
    where: '飞书群 → 设置 → 群机器人 → 添加机器人 → 自定义机器人，复制 webhook 地址',
    placeholder: 'https://open.feishu.cn/open-apis/bot/v2/hook/…',
  },
}
const EVENT_KEYS = ['failed', 'blockedSpec', 'awaitingApproval', 'prOpen', 'prdReady']
const EVENT_TYPE_KEY = {
  failed: 'failed',
  blocked_spec: 'blockedSpec',
  awaiting_approval: 'awaitingApproval',
  pr_open: 'prOpen',
  prd_ready_for_review: 'prdReady',
}
const STATUS_LABEL = { pending: '排队中', sending: '发送中', sent: '已送达', skipped: '已跳过', gave_up: '已放弃' }

const channels = ref([])
const labels = ref({})
const forms = ref({})
const results = ref({})
const deliveries = ref([])
const showDeliveries = ref(false)
const busy = ref('')
const error = ref('')

function resetForm(c) {
  forms.value[c.kind] = {
    url: '',
    secret: '',
    clearSecret: false,
    enabled: c.enabled,
    events: { ...c.events },
  }
}

async function load() {
  try {
    const res = await api.notifyChannels()
    channels.value = res.channels || []
    labels.value = res.eventLabels || {}
    for (const c of channels.value) resetForm(c)
    const d = await api.notifyDeliveries(10)
    deliveries.value = d.deliveries || []
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

function credsTouched(kind) {
  const f = forms.value[kind]
  return !!(f && (f.url.trim() || f.secret.trim() || f.clearSecret))
}

function badge(c) {
  if (!c.configured) return { cls: 'idle', text: '未配置' }
  if (c.testOk === false) return { cls: 'bad', text: '测试失败' }
  return { cls: 'ok', text: '已配置' }
}

async function save(kind) {
  const f = forms.value[kind]
  busy.value = kind
  error.value = ''
  try {
    const body = { enabled: f.enabled, events: { ...f.events } }
    if (f.url.trim()) body.url = f.url.trim()
    if (f.secret.trim()) body.secret = f.secret.trim()
    if (f.clearSecret) body.clearSecret = true
    const res = await api.saveNotifyChannel(kind, body)
    results.value[kind] = res.test ? res.test : { ok: true, saved: true }
    await load()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    results.value[kind] = { ok: false, error: e.message }
  } finally {
    busy.value = ''
  }
}

async function resend(kind) {
  busy.value = kind
  try {
    const res = await api.testNotifyChannel(kind)
    results.value[kind] = res.test
    await load()
  } catch (e) {
    results.value[kind] = { ok: false, error: e.message }
  } finally {
    busy.value = ''
  }
}

async function remove(kind) {
  if (!(await confirmDialog(`确认删除${META[kind].name}？投递记录会一起删除。`))) return
  busy.value = kind
  try {
    await api.deleteNotifyChannel(kind)
    results.value[kind] = null
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function retry(id) {
  busy.value = 'retry-' + id
  try {
    await api.retryDelivery(id)
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

function resultText(r) {
  if (r.saved) return '✓ 已保存'
  return r.ok ? '✓ 测试消息已发送' : '✗ 测试消息发送失败：' + (r.error || '未知原因')
}

onMounted(load)
</script>

<template>
  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

  <div v-for="c in channels" :key="c.kind" class="card cred">
    <div class="spread">
      <div>
        <h2>{{ META[c.kind]?.name || c.kind }}</h2>
        <p class="faint desc">{{ META[c.kind]?.desc }}</p>
      </div>
      <span class="badge" :class="badge(c).cls">{{ badge(c).text }}</span>
    </div>

    <div v-if="c.configured" class="current">
      <span class="dim">当前：</span>
      <code class="mono">{{ c.maskedURL }}</code>
      <span v-if="c.kind === 'feishu'" class="dim">· {{ c.hasSecret ? '已设签名' : '未设签名' }}</span>
      <span v-if="c.testAt" class="faint">· 上次测试 {{ formatTime(c.testAt) }}{{ c.testOk ? ' 成功' : ' 失败' }}</span>
    </div>
    <div v-if="c.testOk === false && c.testError" role="alert" class="error-banner small">{{ c.testError }}</div>

    <div v-if="results[c.kind]" class="result" :class="results[c.kind].ok ? 'good' : 'bad'">
      {{ resultText(results[c.kind]) }}
    </div>

    <form v-if="forms[c.kind]" class="form-grid" @submit.prevent="save(c.kind)">
      <input
        v-model="forms[c.kind].url"
        type="password"
        :placeholder="c.configured ? '留空则保持不变' : META[c.kind]?.placeholder"
        autocomplete="off"
      />
      <template v-if="c.kind === 'feishu'">
        <input
          v-model="forms[c.kind].secret"
          type="password"
          :disabled="forms[c.kind].clearSecret"
          :placeholder="c.hasSecret ? '签名密钥：留空则保持不变' : '签名密钥（可选）'"
          autocomplete="off"
        />
        <p class="faint hint">飞书机器人的「签名校验」密钥，没开就不填。</p>
        <label v-if="c.hasSecret" class="check">
          <input v-model="forms[c.kind].clearSecret" type="checkbox" /> 清除签名密钥
        </label>
      </template>

      <label class="check strong">
        <input v-model="forms[c.kind].enabled" type="checkbox" /> 启用此渠道
      </label>
      <div class="events">
        <label v-for="k in EVENT_KEYS" :key="k" class="check">
          <input v-model="forms[c.kind].events[k]" type="checkbox" :disabled="!forms[c.kind].enabled" />
          {{ labels[k] || k }}
        </label>
      </div>

      <div class="row">
        <button class="primary" :disabled="busy === c.kind || (!c.configured && !forms[c.kind].url.trim())">
          {{ busy === c.kind ? '处理中…' : credsTouched(c.kind) || !c.configured ? '保存并发送测试消息' : '保存' }}
        </button>
        <button v-if="c.configured" :disabled="busy === c.kind" @click.prevent="resend(c.kind)">重发测试</button>
        <button v-if="c.configured" class="danger" :disabled="busy === c.kind" @click.prevent="remove(c.kind)">删除</button>
      </div>
    </form>

    <p class="faint hint">获取位置：{{ META[c.kind]?.where }}</p>
  </div>

  <div v-if="deliveries.length" class="card cred">
    <div class="spread">
      <h2>最近投递</h2>
      <button @click="showDeliveries = !showDeliveries">{{ showDeliveries ? '收起' : '展开' }}</button>
    </div>
    <table v-if="showDeliveries" class="deliveries">
      <thead>
        <tr><th>时间</th><th>渠道</th><th>事件</th><th>状态</th><th>尝试</th><th></th></tr>
      </thead>
      <tbody>
        <tr v-for="d in deliveries" :key="d.id">
          <td class="faint">{{ formatTime(d.createdAt) }}</td>
          <td>{{ META[d.kind]?.name || d.kind }}</td>
          <td>{{ d.eventType === 'test' ? '测试消息' : labels[EVENT_TYPE_KEY[d.eventType]] || d.eventType }}</td>
          <td :title="d.lastError || ''">
            <span class="badge" :class="d.status === 'sent' ? 'ok' : d.status === 'gave_up' ? 'bad' : 'idle'">
              {{ STATUS_LABEL[d.status] || d.status }}
            </span>
          </td>
          <td>{{ d.attempts }}</td>
          <td>
            <button
              v-if="d.status === 'gave_up' && d.eventType !== 'test'"
              :disabled="busy === 'retry-' + d.id"
              @click="retry(d.id)"
            >重试</button>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
h2 { font-size: 15px; margin: 0; }
.cred { margin-bottom: 16px; }
.desc { margin: 5px 0 0; font-size: 12.5px; max-width: 68ch; }
.current { margin: 14px 0 0; display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 13px; }
.form-grid { display: flex; flex-direction: column; gap: 8px; margin: 14px 0 10px; max-width: 560px; }
.events { display: flex; flex-wrap: wrap; gap: 6px 16px; padding-left: 4px; }
.check { display: flex; align-items: center; gap: 6px; font-size: 13px; }
.check.strong { font-weight: 500; }
.row { display: flex; gap: 8px; flex-wrap: wrap; }
.result { margin: 12px 0 0; padding: 9px 12px; border-radius: var(--radius); font-size: 13px; }
.result.good { background: var(--ok-bg); color: var(--ok); }
.result.bad { background: var(--bad-bg); color: var(--bad); }
.error-banner.small { margin: 12px 0 0; font-size: 13px; }
.hint { margin: 0; font-size: 12px; }
.deliveries { width: 100%; margin-top: 12px; font-size: 13px; border-collapse: collapse; }
.deliveries th, .deliveries td { text-align: left; padding: 4px 8px; }
</style>
