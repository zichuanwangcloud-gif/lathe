<script setup>
// 个人 API 令牌（PAT）卡片。
//
// 自带全部状态与三个 API 调用；PersonalSettings.vue 只引用它（与其它卡片互不影响）。
// 一次性明文只放在本组件的 ref 里：不写 localStorage/sessionStorage、不进 URL、
// 不写 history.state；区块关闭即置空，刷新即消失 —— 这是语义要求，不是缺陷。
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { confirmDialog } from '../confirm'

const onUnauthorized = inject('onUnauthorized', () => {})

const tokens = ref([])
const error = ref('')
const busy = ref(false)
const loaded = ref(false)

const name = ref('')
const ttl = ref('90')
const password = ref('')
const TTL_OPTIONS = [
  { value: '7', label: '7 天' },
  { value: '30', label: '30 天' },
  { value: '90', label: '90 天' },
  { value: '365', label: '365 天' },
  { value: '0', label: '永不过期' },
]

// 刚签发的那一把：{ name, token }。只在这里出现一次。
const fresh = ref(null)
const copied = ref('')

function fail(e) {
  if (e instanceof UnauthorizedError) return onUnauthorized()
  if (e.status === 403) error.value = '当前密码不正确'
  else if (e.status === 429) error.value = '尝试过于频繁，请稍后再试'
  else error.value = e.message
}

async function load() {
  try {
    const res = await api.pats()
    tokens.value = res.tokens || []
    loaded.value = true
  } catch (e) {
    fail(e)
  }
}

async function create() {
  if (!name.value.trim() || !password.value) return
  busy.value = true
  error.value = ''
  const currentPassword = password.value
  // 无论成败立即清空口令，不在组件里多留一刻
  password.value = ''
  try {
    const res = await api.createPAT({
      name: name.value,
      expiresInDays: Number(ttl.value),
      currentPassword,
    })
    fresh.value = { name: res.name, token: res.token }
    name.value = ''
    await load()
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

function closeFresh() {
  fresh.value = null
  copied.value = ''
}

async function copy(which, text) {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = which
    setTimeout(() => (copied.value = ''), 2000)
  } catch {
    // 剪贴板不可用（非安全上下文）时，明文/前缀本身就在页面上，可手动选中
  }
}

async function revoke(t) {
  const ok = await confirmDialog(
    `确认吊销令牌「${t.name}」？用这把令牌的脚本会立即失效，此操作不可恢复。`,
  )
  if (!ok) return
  busy.value = true
  error.value = ''
  try {
    await api.revokePAT(t.id)
    await load()
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="card cred">
    <div class="spread">
      <div>
        <h2 class="pat-title">个人 API 令牌</h2>
        <p class="faint pat-desc">
          脚本与 CI 用 <code class="mono">Authorization: Bearer lathe_pat_…</code> 访问接口，
          权限与你本人完全相同。明文只在创建时显示一次。
          <strong>修改或重置密码会吊销你的全部令牌。</strong>
        </p>
      </div>
      <span class="badge" :class="tokens.length ? 'ok' : 'idle'">{{ tokens.length }} 把</span>
    </div>

    <div v-if="error" role="alert" class="error-banner pat-small">{{ error }}</div>

    <div v-if="fresh" class="pat-fresh" role="status">
      <div class="spread">
        <strong>新令牌「{{ fresh.name }}」</strong>
        <button @click.prevent="closeFresh">关闭</button>
      </div>
      <div class="row pat-form">
        <input readonly :value="fresh.token" class="mono" @focus="$event.target.select()" />
        <button class="primary" @click.prevent="copy('fresh', fresh.token)">
          {{ copied === 'fresh' ? '已复制 ✓' : '复制' }}
        </button>
      </div>
      <p class="pat-hint">关闭或刷新后无法再查看；丢失请吊销后重新签发。</p>
    </div>

    <p v-if="loaded && !tokens.length" class="dim pat-empty">
      还没有个人 API 令牌。脚本与 CI 用 Bearer 令牌访问接口时，权限与你本人完全相同。
    </p>

    <table v-if="tokens.length" class="pat-table">
      <thead>
        <tr>
          <th>名字</th>
          <th>前缀</th>
          <th>创建</th>
          <th>最后使用</th>
          <th>过期</th>
          <th>次数</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="t in tokens" :key="t.id" :class="{ 'pat-expired': t.expired }">
          <td>
            {{ t.name }}
            <span v-if="t.expired" class="badge warn">已过期</span>
          </td>
          <td>
            <code class="mono">lathe_pat_{{ t.prefix }}…</code>
            <button class="pat-link" @click.prevent="copy('p' + t.id, 'lathe_pat_' + t.prefix)">
              {{ copied === 'p' + t.id ? '✓' : '复制' }}
            </button>
          </td>
          <td class="faint">{{ formatTime(t.createdAt) }}</td>
          <td :class="t.lastUsedAt ? '' : 'faint'">{{ t.lastUsedAt ? formatTime(t.lastUsedAt) : '从未使用' }}</td>
          <td class="faint">{{ t.expiresAt ? new Date(t.expiresAt).toLocaleDateString('zh-CN') : '永不过期' }}</td>
          <td class="faint">{{ t.useCount }}</td>
          <td><button class="danger" :disabled="busy" @click.prevent="revoke(t)">吊销</button></td>
        </tr>
      </tbody>
    </table>

    <form class="row pat-form" @submit.prevent="create">
      <input v-model="name" placeholder="令牌名字，如 ci-deploy" maxlength="64" autocomplete="off" />
      <select v-model="ttl" aria-label="有效期">
        <option v-for="o in TTL_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</option>
      </select>
      <input
        v-model="password"
        type="password"
        placeholder="当前密码"
        autocomplete="current-password"
        aria-label="当前密码"
      />
      <button class="primary" :disabled="busy || !name.trim() || !password">
        {{ busy ? '处理中…' : '签发令牌' }}
      </button>
    </form>
    <p class="faint pat-hint">
      签发需要重新输入当前密码；每人最多 20 把有效令牌，有效期最长 365 天。
      「最后使用」与次数是 60 秒粒度的近似值。
    </p>
  </div>
</template>

<style scoped>
.pat-title { font-size: 15px; margin: 0; }
.pat-desc { margin: 5px 0 0; font-size: 12.5px; max-width: 68ch; }
.pat-small { margin: 12px 0 0; font-size: 13px; }
.pat-empty { margin: 14px 0 0; font-size: 13px; }
.pat-form { margin: 14px 0 10px; flex-wrap: wrap; }
.pat-form input { flex: 1; min-width: 180px; }
.pat-hint { margin: 0; font-size: 12px; }
.pat-fresh {
  margin: 14px 0 0;
  padding: 10px 12px;
  border-radius: var(--radius);
  background: var(--ok-bg);
}
.pat-fresh .pat-hint { color: var(--ok); }
.pat-table { width: 100%; margin: 14px 0 0; border-collapse: collapse; font-size: 13px; }
.pat-table th { text-align: left; font-weight: 500; color: var(--text-dim); padding: 4px 6px; }
.pat-table td { padding: 6px; border-top: 1px solid var(--border); vertical-align: middle; }
.pat-expired td { opacity: 0.6; }
.pat-link { padding: 0 6px; margin-left: 4px; font-size: 12px; }
</style>
