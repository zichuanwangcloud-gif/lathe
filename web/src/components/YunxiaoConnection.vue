<script setup>
// 云效（Projex）任务来源的个人连接卡片（f08，docs/features/tracker-yunxiao.md §3.3）。
//
// 令牌与 webhook 密钥只进不出：界面只展示掩码，输入框留空保存 = 保留原值。
import { ref, computed, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { confirmDialog } from '../confirm'

const onUnauthorized = inject('onUnauthorized', () => {})

const conn = ref(null)
const form = ref(blankForm())
const token = ref('')
const webhookSecret = ref('')
const busy = ref('')
const error = ref('')
const result = ref(null)
const runID = ref('')
const runResult = ref(null)
const copied = ref(false)

function blankForm() {
  return {
    endpoint: 'https://openapi-rdc.aliyuncs.com',
    organizationId: '',
    triggerLabel: '',
    statusStarted: '',
    statusPrOpen: '',
    statusMerged: '',
    statusCancel: '',
  }
}

const webhookURL = computed(() =>
  conn.value?.webhookPath ? `${location.origin}${conn.value.webhookPath}` : '',
)

function handle(e) {
  if (e instanceof UnauthorizedError) return onUnauthorized()
  error.value = e.message
}

async function load() {
  try {
    const c = await api.yunxiaoConnection()
    conn.value = c
    if (c.configured) {
      form.value = {
        endpoint: c.endpoint || blankForm().endpoint,
        organizationId: c.organizationId || '',
        triggerLabel: c.triggerLabel || '',
        statusStarted: c.statusStarted || '',
        statusPrOpen: c.statusPrOpen || '',
        statusMerged: c.statusMerged || '',
        statusCancel: c.statusCancel || '',
      }
    }
    error.value = ''
  } catch (e) {
    handle(e)
  }
}

async function save() {
  busy.value = 'save'
  error.value = ''
  result.value = null
  try {
    const res = await api.saveYunxiaoConnection({
      ...form.value,
      token: token.value.trim(),
      webhookSecret: webhookSecret.value.trim(),
    })
    result.value = res.verify
    token.value = ''
    webhookSecret.value = ''
    await load()
  } catch (e) {
    handle(e)
  } finally {
    busy.value = ''
  }
}

async function verify() {
  busy.value = 'verify'
  result.value = null
  try {
    const res = await api.verifyYunxiaoConnection()
    result.value = res.verify
    await load()
  } catch (e) {
    result.value = { ok: false, error: e.message }
  } finally {
    busy.value = ''
  }
}

async function remove() {
  if (!(await confirmDialog('确认删除云效连接？已接单的任务不受影响，但之后不会再接云效的单。'))) return
  busy.value = 'delete'
  try {
    await api.deleteYunxiaoConnection()
    form.value = blankForm()
    result.value = null
    await load()
  } catch (e) {
    handle(e)
  } finally {
    busy.value = ''
  }
}

async function run() {
  const id = runID.value.trim()
  if (!id) return
  busy.value = 'run'
  runResult.value = null
  try {
    const res = await api.runYunxiaoWorkitem(id)
    runResult.value = { ok: true, text: `已建任务 #${res.taskId}（${res.externalKey}）` }
    runID.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    runResult.value = { ok: false, text: e.message }
  } finally {
    busy.value = ''
  }
}

async function copy() {
  try {
    await navigator.clipboard.writeText(webhookURL.value)
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    // 非安全上下文：地址就在输入框里，手动复制即可
  }
}

onMounted(load)
</script>

<template>
  <div class="card cred yunxiao">
    <div class="spread">
      <div>
        <h2>云效连接</h2>
        <p class="faint desc">
          把阿里云云效（Projex）工作项作为需求来源：指派给你（或打上触发标签）的工作项会接单，
          分诊提问、PR 链接都回帖在云效评论区。只读写你自己的连接，令牌与 webhook 密钥加密存储、只显示末尾几位。
        </p>
      </div>
      <span class="badge" :class="conn?.configured ? (conn.verifiedAt ? 'ok' : 'warn') : 'idle'">
        {{ conn?.configured ? (conn.verifiedAt ? '已验证' : '已配置') : '未配置' }}
      </span>
    </div>

    <div v-if="error" role="alert" class="error-banner small">{{ error }}</div>

    <div v-if="conn?.configured" class="current">
      <span class="dim">令牌：</span><code class="mono">{{ conn.tokenMasked }}</code>
      <span v-if="conn.accountName" class="dim">· 账号 {{ conn.accountName }}</span>
      <span v-if="conn.verifiedAt" class="faint">· 验证于 {{ formatTime(conn.verifiedAt) }}</span>
    </div>
    <div v-if="conn?.verifyError" role="alert" class="error-banner small">{{ conn.verifyError }}</div>
    <div v-if="result" class="result" :class="result.ok ? 'good' : 'bad'">
      {{ result.ok ? `✓ 已连接到云效账号 ${result.accountName || ''}` : `✗ ${result.error}` }}
    </div>

    <form @submit.prevent="save">
      <div class="fields">
        <label>
          <span>接入点</span>
          <input v-model="form.endpoint" class="mono" placeholder="https://openapi-rdc.aliyuncs.com" />
          <small class="faint">中心版保持默认；Region 版改成实例域名（需管理员先在系统设置里登记）。</small>
        </label>
        <label>
          <span>组织 ID</span>
          <input v-model="form.organizationId" class="mono" placeholder="中心版必填；Region 版留空" />
        </label>
        <label>
          <span>个人访问令牌</span>
          <input
            v-model="token"
            type="password"
            autocomplete="off"
            :placeholder="conn?.configured ? '输入新值以替换（留空保留）' : '粘贴令牌'"
          />
        </label>
        <label>
          <span>Webhook 密钥</span>
          <input
            v-model="webhookSecret"
            type="password"
            autocomplete="off"
            :placeholder="conn?.hasWebhookSecret ? `当前 ${conn.webhookSecretMasked}，输入新值以替换` : '至少 16 位随机串'"
          />
        </label>
        <label>
          <span>触发标签</span>
          <input v-model="form.triggerLabel" placeholder="留空 = 只按指派接单" />
        </label>
        <label>
          <span>取消状态</span>
          <input v-model="form.statusCancel" placeholder="如「已取消」；留空 = 不联动取消" />
        </label>
        <label>
          <span>开跑时推到</span>
          <input v-model="form.statusStarted" placeholder="如「处理中」；留空 = 不同步" />
        </label>
        <label>
          <span>PR 已开时推到</span>
          <input v-model="form.statusPrOpen" placeholder="如「待验证」；留空 = 不同步" />
        </label>
        <label>
          <span>已合并时推到</span>
          <input v-model="form.statusMerged" placeholder="如「已完成」；留空 = 不同步" />
        </label>
      </div>
      <p class="faint hint">状态名填工作项状态的显示名（如「处理中」），大小写与首尾空格不敏感；状态同步失败不影响任务本身。</p>
      <div class="row form">
        <button class="primary" :disabled="!!busy">{{ busy === 'save' ? '处理中…' : '保存并验证' }}</button>
        <button v-if="conn?.configured" :disabled="!!busy" @click.prevent="verify">重新验证</button>
        <button v-if="conn?.configured" class="danger" :disabled="!!busy" @click.prevent="remove">删除</button>
      </div>
    </form>

    <template v-if="webhookURL">
      <h3>回调地址</h3>
      <div class="row form">
        <input readonly :value="webhookURL" class="mono" @focus="$event.target.select()" />
        <button @click.prevent="copy">{{ copied ? '已复制 ✓' : '复制' }}</button>
      </div>
      <p class="faint hint">
        在云效项目 → 设置 → 自动化规则里用 Webhook 模板新建两条规则，触发分别选「字段变更」与「状态变更」，
        地址填上面这个，Body 选【工作项数据】，Secret 填与上面「Webhook 密钥」相同的串。
      </p>
    </template>

    <template v-if="conn?.configured">
      <h3>按工作项 ID 开跑</h3>
      <form class="row form" @submit.prevent="run">
        <input v-model="runID" class="mono" placeholder="工作项 ID（不是编号）" />
        <button class="primary" :disabled="!!busy || !runID.trim()">{{ busy === 'run' ? '处理中…' : '开跑' }}</button>
      </form>
      <div v-if="runResult" class="result" :class="runResult.ok ? 'good' : 'bad'">{{ runResult.text }}</div>
    </template>
  </div>
</template>

<style scoped>
h2 { font-size: 15px; margin: 0; }
h3 { font-size: 13.5px; margin: 18px 0 0; }
.cred { margin-bottom: 16px; }
.desc { margin: 5px 0 0; font-size: 12.5px; max-width: 68ch; }
.current { margin: 14px 0 0; display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 13px; }
.fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 14px;
  margin: 16px 0 8px;
}
label { display: flex; flex-direction: column; gap: 5px; }
label > span { font-size: 13px; color: var(--text-dim); }
label small { font-size: 12px; }
.form { margin: 12px 0 8px; flex-wrap: wrap; }
.form input { flex: 1; min-width: 200px; }
.result { margin: 12px 0 0; padding: 9px 12px; border-radius: var(--radius); font-size: 13px; }
.result.good { background: var(--ok-bg); color: var(--ok); }
.result.bad { background: var(--bad-bg); color: var(--bad); }
.error-banner.small { margin: 12px 0 0; font-size: 13px; }
.hint { margin: 0; font-size: 12px; }
</style>
