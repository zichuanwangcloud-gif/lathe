<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { refresh } from '../auth'

const router = useRouter()

const email = ref('')
const password = ref('')
const confirm = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  // 两次不一致在前端就拦下：这个错误服务端无从判断（它只收到一个密码）
  if (password.value !== confirm.value) {
    error.value = '两次输入的密码不一致'
    return
  }
  busy.value = true
  try {
    await api.register(email.value, password.value)
    await refresh() // 注册接口直接签发了会话，刷一下就是登录态
    router.push({ name: 'board' })
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <form class="card login-card" @submit.prevent="submit">
      <h1>注册</h1>
      <p class="dim">注册后即可登录，无需邮箱验证</p>

      <input v-model="email" type="email" placeholder="邮箱" autocomplete="username" autofocus />
      <input
        v-model="password"
        type="password"
        placeholder="密码（至少 8 位）"
        autocomplete="new-password"
      />
      <input
        v-model="confirm"
        type="password"
        placeholder="再输一次密码"
        autocomplete="new-password"
      />

      <div v-if="error" class="error-banner">{{ error }}</div>

      <button class="primary" type="submit" :disabled="busy || !email || !password || !confirm">
        {{ busy ? '注册中…' : '注册' }}
      </button>

      <div class="auth-links">
        <RouterLink to="/login">已有账号，去登录</RouterLink>
      </div>
    </form>
  </div>
</template>
