<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { auth } from '../auth'

// 系统设置（仅管理员）：SMTP 发信通道是全站唯一的发件人，
// 所有系统邮件（密码重置、通知类）都经它发出；收件人由每个人
// 在「个人设置」里各自决定。

const runtime = ref(null)
const error = ref('')
const busy = ref('')
const onUnauthorized = inject('onUnauthorized')

const TLS_MODES = [
  { value: 'starttls', label: 'STARTTLS（587 端口，最常见）' },
  { value: 'tls', label: 'TLS（465 端口）' },
  { value: 'none', label: '不加密（仅限内网自建中继）' },
]

const smtp = ref(null)
const smtpPassword = ref('')
const smtpTestTo = ref('')
const smtpResult = ref(null)

// 预览环境资源阈值：内存/磁盘占用率超过该百分比时禁止一键起服务，
// 防止预览构建把任务执行挤爆。100 = 不启用该闸门。
const thresholds = ref(null)
const thresholdSaved = ref(false)

async function loadThresholds() {
  try {
    thresholds.value = await api.adminSettings()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function saveThresholds() {
  busy.value = 'thresholds'
  error.value = ''
  thresholdSaved.value = false
  try {
    await api.saveAdminSettings({
      previewMemThreshold: Number(thresholds.value.previewMemThreshold),
      previewDiskThreshold: Number(thresholds.value.previewDiskThreshold),
    })
    thresholdSaved.value = true
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function load() {
  try {
    const cfg = await api.config()
    runtime.value = cfg.runtime
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function loadSmtp() {
  try {
    smtp.value = await api.smtp()
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

function smtpBody() {
  return {
    host: smtp.value.host,
    port: Number(smtp.value.port),
    username: smtp.value.username,
    // 留空表示不改密码，服务端会保留原值
    password: smtpPassword.value,
    fromAddr: smtp.value.fromAddr,
    fromName: smtp.value.fromName,
    tlsMode: smtp.value.tlsMode,
    testTo: smtpTestTo.value,
  }
}

async function saveSmtp() {
  busy.value = 'smtp'
  error.value = ''
  try {
    const res = await api.saveSmtp(smtpBody())
    smtpResult.value = res.verify
    smtpPassword.value = ''
    await loadSmtp()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function verifySmtp() {
  busy.value = 'smtp'
  error.value = ''
  try {
    const res = await api.verifySmtp(smtpTestTo.value)
    smtpResult.value = res.verify
    await loadSmtp()
  } catch (e) {
    smtpResult.value = { ok: false, error: e.message }
  } finally {
    busy.value = ''
  }
}

async function removeSmtp() {
  if (!confirm('确认删除发信配置？删除后「忘记密码」功能将不可用。')) return
  busy.value = 'smtp'
  try {
    await api.deleteSmtp()
    smtpResult.value = null
    smtpPassword.value = ''
    await loadSmtp()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

onMounted(() => {
  load()
  loadSmtp()
  loadThresholds()
})
</script>

<template>
  <h1>系统设置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <p class="dim note">
    这里的配置对全站生效，仅管理员可改。SMTP 是全站唯一的发件通道 ——
    所有系统邮件都从「发件地址」发出；收件人由每个人在「个人设置」里各自决定。
  </p>

  <div v-if="smtp" class="card cred">
    <div class="spread">
      <div>
        <h2>邮件发送（SMTP）</h2>
        <p class="faint desc">
          用于「忘记密码」的重置邮件与后续的通知类邮件。未配置时用户无法自助找回密码，
          只能由管理员在用户管理页代为重置。
        </p>
      </div>
      <span class="badge" :class="smtp.configured ? (smtp.verifiedAt ? 'ok' : 'warn') : 'idle'">
        {{ smtp.configured ? (smtp.verifiedAt ? '已验证' : '已配置') : '未配置' }}
      </span>
    </div>

    <div v-if="smtp.verifiedAt" class="current">
      <span class="faint">上次验证于 {{ formatTime(smtp.verifiedAt) }}</span>
    </div>

    <div v-if="smtp.verifyError" class="error-banner small">{{ smtp.verifyError }}</div>

    <div v-if="smtpResult" class="result" :class="smtpResult.ok ? 'good' : 'bad'">
      {{ smtpResult.ok ? '✓ ' : '✗ ' }}{{ smtpResult.detail || smtpResult.error }}
    </div>

    <form @submit.prevent="saveSmtp">
      <div class="fields">
        <label>
          <span>SMTP 主机</span>
          <input v-model="smtp.host" placeholder="smtp.example.com" />
        </label>
        <label>
          <span>端口</span>
          <input v-model="smtp.port" type="number" min="1" max="65535" />
        </label>
        <label>
          <span>加密方式</span>
          <select v-model="smtp.tlsMode">
            <option v-for="m in TLS_MODES" :key="m.value" :value="m.value">{{ m.label }}</option>
          </select>
        </label>
        <label>
          <span>用户名</span>
          <input v-model="smtp.username" autocomplete="off" placeholder="留空则不认证（匿名中继）" />
        </label>
        <label>
          <span>密码</span>
          <input
            v-model="smtpPassword"
            type="password"
            autocomplete="new-password"
            :placeholder="smtp.passwordSet ? '留空表示不修改' : '邮箱的应用专用密码'"
          />
          <small class="faint" v-if="smtp.passwordSet">当前：{{ smtp.passwordMasked }}</small>
          <small class="faint" v-else>Gmail / QQ / 163 等需要「应用专用密码」，不是登录密码</small>
        </label>
        <label>
          <span>发件地址</span>
          <input v-model="smtp.fromAddr" placeholder="lathe@example.com" />
          <small class="faint">全站所有系统邮件都从这个地址发出</small>
        </label>
        <label>
          <span>发件人显示名</span>
          <input v-model="smtp.fromName" placeholder="Lathe" />
        </label>
        <label>
          <span>测试邮件收件地址</span>
          <input v-model="smtpTestTo" :placeholder="auth.user?.email || '留空则发给自己'" />
          <small class="faint">仅用于验证通道是否可用；保存与验证时都会真的投一封测试邮件过去</small>
        </label>
      </div>

      <div class="row wrap">
        <button class="primary" :disabled="busy === 'smtp'">
          {{ busy === 'smtp' ? '处理中…' : '保存并发送测试邮件' }}
        </button>
        <button v-if="smtp.configured" :disabled="busy === 'smtp'" @click.prevent="verifySmtp">
          重新验证
        </button>
        <button
          v-if="smtp.configured"
          class="danger"
          :disabled="busy === 'smtp'"
          @click.prevent="removeSmtp"
        >删除</button>
      </div>
    </form>
  </div>

  <div v-if="thresholds" class="card cred">
    <div class="spread">
      <div>
        <h2>预览环境资源阈值</h2>
        <p class="faint desc">
          看板「预览」一键起服务前的资源闸门：内存或磁盘占用率达到阈值即禁止启动，
          防止预览构建把任务执行挤爆。填 100 表示不启用该闸门。
        </p>
      </div>
    </div>
    <form class="fields" @submit.prevent="saveThresholds">
      <label>
        <span>内存占用阈值（%）</span>
        <input type="number" min="1" max="100" v-model.number="thresholds.previewMemThreshold" required />
      </label>
      <label>
        <span>磁盘占用阈值（%）</span>
        <input type="number" min="1" max="100" v-model.number="thresholds.previewDiskThreshold" required />
      </label>
    </form>
    <button class="primary" :disabled="busy === 'thresholds'" @click="saveThresholds">
      {{ busy === 'thresholds' ? '保存中……' : '保存阈值' }}
    </button>
    <span v-if="thresholdSaved" class="faint" style="margin-left: 10px">已保存，即刻生效</span>
  </div>

  <div v-if="runtime" class="card">
    <div class="label">运行时</div>
    <dl>
      <dt>节点</dt><dd class="mono">{{ runtime.node }}</dd>
      <dt>工作区根目录</dt><dd class="mono break">{{ runtime.workspaceRoot }}</dd>
      <dt>依赖 store</dt><dd class="mono break">{{ runtime.pnpmStore || '未配置' }}</dd>
      <dt>claude 可执行文件</dt><dd class="mono">{{ runtime.claudeBin }}</dd>
      <dt>agent 超时</dt><dd>{{ runtime.agentTimeout }}</dd>
      <dt>执行模式</dt><dd>{{ runtime.mode }}</dd>
    </dl>
  </div>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0 0 16px; }
h2 { font-size: 15px; margin: 0; }

.note { font-size: 13px; max-width: 72ch; margin: 0 0 16px; }
.label { font-size: 12.5px; color: var(--text-dim); margin-bottom: 10px; font-weight: 500; }

.cred { margin-bottom: 16px; }
.desc { margin: 5px 0 0; font-size: 12.5px; max-width: 68ch; }

.current {
  margin: 14px 0 0;
  font-size: 13px;
}

.result {
  margin: 12px 0 0;
  padding: 9px 12px;
  border-radius: var(--radius);
  font-size: 13px;
}
.result.good { background: var(--ok-bg); color: var(--ok); }
.result.bad { background: var(--bad-bg); color: var(--bad); }

.error-banner.small { margin: 12px 0 0; font-size: 13px; }

.fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 14px;
  margin: 16px 0;
}
label { display: flex; flex-direction: column; gap: 5px; }
label > span { font-size: 13px; color: var(--text-dim); }
label small { font-size: 12px; }

dl { display: grid; grid-template-columns: 150px 1fr; gap: 8px 12px; margin: 0; }
dt { color: var(--text-dim); font-size: 13px; }
dd { margin: 0; }
.break { word-break: break-all; }
</style>
