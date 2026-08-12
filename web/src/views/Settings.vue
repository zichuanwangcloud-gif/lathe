<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError } from '../api'

const config = ref(null)
const error = ref('')
const onUnauthorized = inject('onUnauthorized')

const ITEMS = [
  { key: 'linear', name: 'Linear', env: 'LATHE_LINEAR_TOKEN', desc: '拉取 issue、回帖' },
  { key: 'linearWebhook', name: 'Linear Webhook', env: 'LATHE_LINEAR_WEBHOOK_SECRET', desc: '校验 webhook 签名' },
  { key: 'linearUser', name: 'Linear 用户 ID', env: 'LATHE_LINEAR_USER_ID', desc: '只接指派给该用户的 issue' },
  { key: 'github', name: 'GitHub', env: 'LATHE_GITHUB_TOKEN', desc: '推分支、开 PR' },
]

async function load() {
  try {
    config.value = await api.config()
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

onMounted(load)
</script>

<template>
  <h1>设置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <div class="card">
    <div class="label">凭据</div>
    <p class="dim note">
      凭据只从环境变量读取，界面不提供填写入口，也不会显示凭据内容本身 ——
      明文密钥入库与 integrations 表的设计约定冲突（token_ref 指向外部
      secret store）。等接了真正的 secret store 再开放在线配置。
    </p>

    <table v-if="config">
      <thead>
        <tr><th>集成</th><th>状态</th><th>环境变量</th><th>用途</th></tr>
      </thead>
      <tbody>
        <tr v-for="i in ITEMS" :key="i.key">
          <td>{{ i.name }}</td>
          <td>
            <span class="badge" :class="config[i.key]?.configured ? 'ok' : 'bad'">
              {{ config[i.key]?.configured ? '已配置' : '未配置' }}
            </span>
          </td>
          <td class="mono dim">{{ i.env }}</td>
          <td class="dim">{{ i.desc }}</td>
        </tr>
      </tbody>
    </table>
  </div>

  <div v-if="config?.runtime" class="card" style="margin-top: 16px">
    <div class="label">运行时</div>
    <dl>
      <dt>节点</dt><dd class="mono">{{ config.runtime.node }}</dd>
      <dt>工作区根目录</dt><dd class="mono break">{{ config.runtime.workspaceRoot }}</dd>
      <dt>依赖 store</dt><dd class="mono break">{{ config.runtime.pnpmStore || '未配置' }}</dd>
      <dt>claude 可执行文件</dt><dd class="mono">{{ config.runtime.claudeBin }}</dd>
      <dt>agent 超时</dt><dd>{{ config.runtime.agentTimeout }}</dd>
      <dt>执行模式</dt><dd>{{ config.runtime.mode }}</dd>
    </dl>
  </div>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0 0 16px; }
.label { font-size: 12.5px; color: var(--text-dim); margin-bottom: 10px; font-weight: 500; }
.note { margin: 0 0 16px; font-size: 13px; max-width: 68ch; }

dl { display: grid; grid-template-columns: 150px 1fr; gap: 8px 12px; margin: 0; }
dt { color: var(--text-dim); font-size: 13px; }
dd { margin: 0; }
.break { word-break: break-all; }
</style>
