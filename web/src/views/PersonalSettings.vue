<script setup>
import { ref, computed, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { auth, refresh } from '../auth'

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
const error = ref('')
const busy = ref('')
const drafts = ref({})
const results = ref({})
const onUnauthorized = inject('onUnauthorized')

const META = {
  linear: {
    name: 'Linear API 令牌',
    desc: '用于拉取 issue、回帖。验证时会自动获取你的 Linear 账号 ID，接单判定直接使用，无需另行填写。绑定后顶栏会出现「Linear 任务」菜单。',
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
    const ints = await api.integrations()
    items.value = ints.integrations || []
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

// 凭据变更后刷新全局登录态：hasLinearToken 变了，「Linear 任务」
// 菜单要立刻跟上，不该要求用户手动刷新页面。
async function afterCredentialChange() {
  await load()
  await refresh()
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
    await afterCredentialChange()
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
    await afterCredentialChange()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

// ---------------------------------------------------------------- 通知邮箱
//
// 通知类邮件的收件人；留空回退登录邮箱。密码重置邮件始终发往登录邮箱，
// 不受这里影响 —— 那是账号所有权的证明，不能改道。

const notifyDraft = ref('')
const notifySaved = ref(false)

async function saveNotifyEmail(clear = false) {
  busy.value = 'notify-email'
  error.value = ''
  notifySaved.value = false
  try {
    const res = await api.setNotifyEmail(clear ? '' : notifyDraft.value.trim())
    auth.user = res.user
    notifyDraft.value = ''
    notifySaved.value = true
    setTimeout(() => (notifySaved.value = false), 2000)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

onMounted(load)
</script>

<template>
  <h1>个人设置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <p class="dim note">
    这里的一切都属于你自己的账号：凭据以 AES-256-GCM 加密后存入数据库，
    主密钥保存在数据库之外，界面上永远只显示末尾几位。其他用户（含管理员）
    看不到也用不了你的凭据。
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
      <span v-if="auth.user?.role === 'admin'" class="sep">
        · 也可用环境变量 <code class="mono">{{ META[item.kind]?.env }}</code>（仅对管理员兜底，优先级低于此处配置）
      </span>
    </p>
  </div>

  <div class="card cred">
    <div class="spread">
      <div>
        <h2>通知邮箱</h2>
        <p class="faint desc">
          平台通知类邮件（如任务状态通知）的收件人。留空则使用登录邮箱
          <span class="mono">{{ auth.user?.email }}</span>。
        </p>
      </div>
      <span class="badge" :class="auth.user?.notifyEmail ? 'ok' : 'idle'">
        {{ auth.user?.notifyEmail ? '已设置' : '用登录邮箱' }}
      </span>
    </div>

    <div v-if="auth.user?.notifyEmail" class="current">
      <span class="dim">当前：</span>
      <code class="mono">{{ auth.user.notifyEmail }}</code>
    </div>

    <p class="faint hint" style="margin-top: 10px">
      密码重置邮件始终发往登录邮箱，不受此设置影响 —— 那是账号所有权的证明。
    </p>

    <form class="row form" @submit.prevent="saveNotifyEmail(false)">
      <input
        v-model="notifyDraft"
        type="email"
        placeholder="留空保存即回退登录邮箱"
        autocomplete="off"
      />
      <button class="primary" :disabled="busy === 'notify-email'">
        {{ busy === 'notify-email' ? '处理中…' : notifySaved ? '已保存 ✓' : '保存' }}
      </button>
      <button
        v-if="auth.user?.notifyEmail"
        :disabled="busy === 'notify-email'"
        @click.prevent="saveNotifyEmail(true)"
      >清除</button>
    </form>
  </div>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0 0 16px; }
h2 { font-size: 15px; margin: 0; }

.note { font-size: 13px; max-width: 72ch; margin: 0 0 16px; }

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
.sep { margin: 0 0 0 6px; }
</style>
