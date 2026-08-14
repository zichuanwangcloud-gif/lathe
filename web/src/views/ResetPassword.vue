<script setup>
import { ref, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'

const route = useRoute()
const router = useRouter()

// 令牌从邮件链接的 query 里来
const token = computed(() => route.query.token || '')

const password = ref('')
const confirm = ref('')
const error = ref('')
const done = ref(false)
const busy = ref(false)

async function submit() {
  error.value = ''
  if (password.value !== confirm.value) {
    error.value = '两次输入的密码不一致'
    return
  }
  busy.value = true
  try {
    await api.resetPassword(token.value, password.value)
    done.value = true
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <div v-if="!token" class="card login-card">
      <h1>链接不完整</h1>
      <p class="dim">这个地址里没有重置令牌，可能是邮件客户端把链接截断了。</p>
      <RouterLink to="/forgot-password">重新发起找回密码</RouterLink>
    </div>

    <div v-else-if="done" class="card login-card">
      <h1>密码已重置</h1>
      <p class="dim">出于安全考虑，该账号此前的所有登录会话都已失效。</p>
      <button class="primary" @click="router.push({ name: 'login' })">去登录</button>
    </div>

    <form v-else class="card login-card" @submit.prevent="submit">
      <h1>设置新密码</h1>

      <input
        v-model="password"
        type="password"
        placeholder="新密码（至少 8 位）"
        autocomplete="new-password"
        autofocus
      />
      <input
        v-model="confirm"
        type="password"
        placeholder="再输一次新密码"
        autocomplete="new-password"
      />

      <div v-if="error" class="error-banner">{{ error }}</div>

      <button class="primary" type="submit" :disabled="busy || !password || !confirm">
        {{ busy ? '提交中…' : '设置新密码' }}
      </button>
    </form>
  </div>
</template>
