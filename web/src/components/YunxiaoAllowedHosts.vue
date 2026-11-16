<script setup>
// 云效 Region 版实例域名允许列表（管理员，f08 §3.7）。
//
// 成员填写的云效接入点只能是中心版 openapi-rdc.aliyuncs.com 或这里登记的
// 域名 —— 防止服务端被当成访问内网的跳板（SSRF）。精确匹配，不支持通配。
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError } from '../api'

const onUnauthorized = inject('onUnauthorized', () => {})
const text = ref('')
const busy = ref(false)
const error = ref('')
const saved = ref(false)

async function load() {
  try {
    const res = await api.yunxiaoAllowedHosts()
    text.value = (res.hosts || []).join('\n')
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function save() {
  busy.value = true
  error.value = ''
  saved.value = false
  try {
    const hosts = text.value.split('\n').map((s) => s.trim()).filter(Boolean)
    const res = await api.saveYunxiaoAllowedHosts(hosts)
    text.value = (res.hosts || []).join('\n')
    saved.value = true
    setTimeout(() => (saved.value = false), 2000)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="card cred">
    <h2>云效 Region 版实例域名</h2>
    <p class="faint desc">
      成员的云效连接默认只能指向中心版 <code class="mono">openapi-rdc.aliyuncs.com</code>。
      使用 Region 版（专属实例）的团队，在这里每行登记一个实例域名后，成员才能保存。
      只接受完整域名（不带协议、端口与通配符），不接受 IP 与 localhost；撤掉某个域名后，已保存的连接下一次调用即失效。
    </p>
    <div v-if="error" role="alert" class="error-banner small">{{ error }}</div>
    <textarea v-model="text" class="mono" rows="4" placeholder="devops.example-region.aliyuncs.com"></textarea>
    <div class="row">
      <button class="primary" :disabled="busy" @click="save">{{ busy ? '保存中……' : '保存' }}</button>
      <span v-if="saved" class="faint">已保存，即刻生效</span>
    </div>
  </div>
</template>

<style scoped>
h2 { font-size: 15px; margin: 0; }
.cred { margin-bottom: 16px; }
.desc { margin: 5px 0 12px; font-size: 12.5px; max-width: 72ch; }
textarea { width: 100%; box-sizing: border-box; margin: 0 0 10px; }
.error-banner.small { margin: 0 0 10px; font-size: 13px; }
.row { display: flex; align-items: center; gap: 10px; }
</style>
