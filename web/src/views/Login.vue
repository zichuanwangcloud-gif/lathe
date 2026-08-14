<script setup>
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { refresh, auth, mustChangePassword } from '../auth'

const route = useRoute()
const router = useRouter()

const email = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await api.login(email.value, password.value)
    await refresh()
    if (mustChangePassword()) return router.push({ name: 'change-password' })
    // 守卫把原本要去的地址塞在 next 里，登录后送回去
    router.push(route.query.next || { name: 'board' })
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
      <h1>Lathe</h1>
      <p class="dim">登录以继续</p>

      <input v-model="email" type="email" placeholder="邮箱" autocomplete="username" autofocus />
      <input
        v-model="password"
        type="password"
        placeholder="密码"
        autocomplete="current-password"
      />

      <div v-if="error" class="error-banner">{{ error }}</div>

      <button class="primary" type="submit" :disabled="busy || !email || !password">
        {{ busy ? '登录中…' : '登录' }}
      </button>

      <div class="auth-links">
        <RouterLink to="/register">注册新账号</RouterLink>
        <RouterLink to="/forgot-password">忘记密码？</RouterLink>
      </div>

      <p v-if="!auth.authEnabled" class="faint">账号体系不可用，请联系管理员。</p>
    </form>
  </div>
</template>
