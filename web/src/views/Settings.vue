<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'

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

onMounted(load)
</script>

<template>
  <h1>设置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <p class="dim note">
    凭据以 AES-256-GCM 加密后存入数据库，主密钥保存在数据库之外（环境变量或独立文件），
    因此拿到数据库转储也无法解出凭据。界面上永远只显示末尾几位。
  </p>

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
      <span class="sep">·</span>
      也可用环境变量 <code class="mono">{{ META[item.kind]?.env }}</code>（优先级低于此处配置）
    </p>
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

dl { display: grid; grid-template-columns: 150px 1fr; gap: 8px 12px; margin: 0; }
dt { color: var(--text-dim); font-size: 13px; }
dd { margin: 0; }
.break { word-break: break-all; }
</style>
