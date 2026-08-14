<script setup>
import { ref, computed, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { auth, isAdmin } from '../auth'

// 每个用户专属的 Linear webhook 回调地址（P1.5 第二步）：
// 事件按它路由到本人 —— 用谁的凭据验签、任务归谁的名下。
const webhookURL = computed(() => {
  const slug = auth.user?.webhookSlug
  if (!slug) return ''
  return `${location.origin}/webhooks/linear/${slug}`
})
const copied = ref(false)
async function copyWebhook() {
  try {
    await navigator.clipboard.writeText(webhookURL.value)
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    // 剪贴板不可用（非安全上下文）时退化为选中展示，地址本身就在页面上
  }
}

const items = ref([])
const runtime = ref(null)
const error = ref('')
const busy = ref('')
const drafts = ref({})
const results = ref({})
const onUnauthorized = inject('onUnauthorized')

const META = {
  linear: {
    name: 'Linear API 令牌',
    desc: '用于拉取 issue、回帖。验证时会自动获取你的 Linear 账号 ID，接单判定直接使用，无需另行填写。',
    where: 'Linear → Settings → Security & access → Personal API keys',
    env: 'LATHE_LINEAR_TOKEN',
  },
  linear_webhook: {
    name: 'Linear Webhook 密钥',
    desc: '校验 webhook 请求签名。无法主动验证，需等 Linear 实际投递一次事件才能确认。',
    where: 'Linear → Settings → API → Webhooks，创建时生成',
    env: 'LATHE_LINEAR_WEBHOOK_SECRET',
  },
  github: {
    name: 'GitHub 令牌',
    desc: '用于推分支、开 PR。验证时会检查是否具备 repo 权限。',
    where: 'GitHub → Settings → Developer settings → Personal access tokens',
    env: 'LATHE_GITHUB_TOKEN',
  },
}

async function load() {
  try {
    const [ints, cfg] = await Promise.all([api.integrations(), api.config()])
    items.value = ints.integrations || []
    runtime.value = cfg.runtime
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function save(kind) {
  const token = (drafts.value[kind] || '').trim()
  if (!token) return

  busy.value = kind
  error.value = ''
  try {
    const res = await api.saveIntegration(kind, token)
    results.value[kind] = res.verify
    drafts.value[kind] = ''
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function verify(kind) {
  busy.value = kind
  error.value = ''
  try {
    const res = await api.verifyIntegration(kind)
    results.value[kind] = res.verify
    await load()
  } catch (e) {
    results.value[kind] = { ok: false, error: e.message }
  } finally {
    busy.value = ''
  }
}

async function remove(kind) {
  if (!confirm(`确认删除 ${META[kind].name}？`)) return
  busy.value = kind
  try {
    await api.deleteIntegration(kind)
    results.value[kind] = null
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

// ---------------------------------------------------------------- SMTP
//
// SMTP 是多字段配置，塞不进上面那个「一张卡一个输入框」的循环，
// 所以单独成一块，走自己的 /api/smtp 接口。

const TLS_MODES = [
  { value: 'starttls', label: 'STARTTLS（587 端口，最常见）' },
  { value: 'tls', label: 'TLS（465 端口）' },
  { value: 'none', label: '不加密（仅限内网自建中继）' },
]

const smtp = ref(null)
const smtpPassword = ref('')
const smtpTestTo = ref('')
const smtpResult = ref(null)

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
})
</script>

<template>
  <h1>设置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <p class="dim note">
    凭据以 AES-256-GCM 加密后存入数据库，主密钥保存在数据库之外（环境变量或独立文件），
    因此拿到数据库转储也无法解出凭据。界面上永远只显示末尾几位。
  </p>

  <div v-if="webhookURL" class="card cred">
    <div class="spread">
      <div>
        <h2>你的 Linear Webhook 地址</h2>
        <p class="faint desc">
          在 Linear → Settings → API → Webhooks 里新建 webhook，指向这个地址，并勾选 Issue 事件。
          之后把 issue 指派给你自己，Lathe 就会接单。每个人的地址不同，事件按地址归到本人名下。
        </p>
      </div>
    </div>
    <div class="row form">
      <input readonly :value="webhookURL" class="mono" @focus="$event.target.select()" />
      <button @click.prevent="copyWebhook">{{ copied ? '已复制 ✓' : '复制' }}</button>
    </div>
    <p class="faint hint">
      地址里的随机段用于把事件路由给你；真正的防伪靠下面的 Webhook 密钥按人各自验签。
    </p>
  </div>

  <div v-for="item in items" :key="item.kind" class="card cred">
    <div class="spread">
      <div>
        <h2>{{ META[item.kind]?.name || item.kind }}</h2>
        <p class="faint desc">{{ META[item.kind]?.desc }}</p>
      </div>
      <span class="badge" :class="item.configured ? (item.verifiedAt ? 'ok' : 'warn') : 'idle'">
        {{ item.configured ? (item.verifiedAt ? '已验证' : '已配置') : '未配置' }}
      </span>
    </div>

    <div v-if="item.configured" class="current">
      <span class="dim">当前：</span>
      <code class="mono">{{ item.masked }}</code>
      <span v-if="item.source === 'env'" class="badge idle">来自环境变量</span>
      <span v-if="item.accountName" class="dim">· 账号 {{ item.accountName }}</span>
      <span v-if="item.verifiedAt" class="faint">· 验证于 {{ formatTime(item.verifiedAt) }}</span>
    </div>

    <div v-if="item.verifyError" class="error-banner small">{{ item.verifyError }}</div>

    <div
      v-if="results[item.kind]"
      class="result"
      :class="results[item.kind].ok ? 'good' : 'bad'"
    >
      {{ results[item.kind].ok ? '✓ ' : '✗ ' }}
      {{ results[item.kind].detail || results[item.kind].error }}
    </div>

    <form class="row form" @submit.prevent="save(item.kind)">
      <input
        v-model="drafts[item.kind]"
        type="password"
        :placeholder="item.configured ? '输入新令牌以替换' : '粘贴令牌'"
        autocomplete="off"
      />
      <button class="primary" :disabled="busy === item.kind || !drafts[item.kind]">
        {{ busy === item.kind ? '处理中…' : '保存并验证' }}
      </button>
      <button v-if="item.configured && item.source !== 'env'" :disabled="busy === item.kind" @click.prevent="verify(item.kind)">
        重新验证
      </button>
      <button
        v-if="item.configured && item.source !== 'env'"
        class="danger"
        :disabled="busy === item.kind"
        @click.prevent="remove(item.kind)"
      >删除</button>
    </form>

    <p class="faint hint">
      获取位置：{{ META[item.kind]?.where }}
      <template v-if="isAdmin()">
        <span class="sep">·</span>
        也可用环境变量 <code class="mono">{{ META[item.kind]?.env }}</code>（仅对管理员兜底，优先级低于此处配置）
      </template>
    </p>
  </div>

  <div v-if="smtp" class="card cred">
    <div class="spread">
      <div>
        <h2>邮件发送（SMTP）</h2>
        <p class="faint desc">
          用于「忘记密码」的重置邮件。未配置时用户无法自助找回密码，
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
          <small class="faint">多数服务器要求与认证账号一致</small>
        </label>
        <label>
          <span>发件人显示名</span>
          <input v-model="smtp.fromName" placeholder="Lathe" />
        </label>
        <label>
          <span>测试邮件收件地址</span>
          <input v-model="smtpTestTo" :placeholder="auth.user?.email || '留空则发给自己'" />
          <small class="faint">保存与验证时都会真的投一封测试邮件过去</small>
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
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 13px;
}

.form { margin: 14px 0 10px; flex-wrap: wrap; }
.form input { flex: 1; min-width: 220px; }

.result {
  margin: 12px 0 0;
  padding: 9px 12px;
  border-radius: var(--radius);
  font-size: 13px;
}
.result.good { background: var(--ok-bg); color: var(--ok); }
.result.bad { background: var(--bad-bg); color: var(--bad); }

.error-banner.small { margin: 12px 0 0; font-size: 13px; }

.hint { margin: 0; font-size: 12px; }
.sep { margin: 0 6px; }

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
