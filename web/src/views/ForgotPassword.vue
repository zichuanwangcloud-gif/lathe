<script setup>
import { ref } from 'vue'
import { api } from '../api'

const email = ref('')
const error = ref('')
const sent = ref(false)
const busy = ref(false)

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await api.forgotPassword(email.value)
    sent.value = true
  } catch (e) {
    // 只有限流会走到这里 —— 服务端对「邮箱存不存在」一律回同一个 200
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <!-- 提示文案必须中立：说「如果该邮箱已注册」而不是「已发送」，
         否则这句话本身就泄露了账号是否存在，服务端那边的严谨就白做了 -->
    <div v-if="sent" class="card login-card">
      <h1>请查收邮件</h1>
      <p class="dim">如果该邮箱已注册，重置链接已经发出，请查收（记得看看垃圾邮件）。</p>
      <p class="faint">链接 30 分钟内有效，且只能使用一次。</p>
      <div class="auth-links">
        <RouterLink to="/login">返回登录</RouterLink>
      </div>
    </div>

    <form v-else class="card login-card" @submit.prevent="submit">
      <h1>找回密码</h1>
      <p class="dim">输入注册邮箱，我们会发一封重置链接给你</p>

      <input v-model="email" type="email" placeholder="邮箱" autocomplete="username" autofocus />

      <div v-if="error" class="error-banner">{{ error }}</div>

      <button class="primary" type="submit" :disabled="busy || !email">
        {{ busy ? '发送中…' : '发送重置链接' }}
      </button>

      <div class="auth-links">
        <RouterLink to="/login">返回登录</RouterLink>
      </div>
    </form>
  </div>
</template>
