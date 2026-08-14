<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { auth, refresh, mustChangePassword } from '../auth'

const router = useRouter()

const current = ref('')
const next = ref('')
const confirm = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  if (next.value !== confirm.value) {
    error.value = '两次输入的新密码不一致'
    return
  }
  busy.value = true
  try {
    await api.changePassword(current.value, next.value)
    await refresh()
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
      <h1>修改密码</h1>

      <!-- 强制改密时说清楚为什么走不掉，否则界面看起来像卡住了 -->
      <p v-if="mustChangePassword()" class="dim">
        这个账号还在用初始密码，改掉之后才能使用其它功能。
      </p>
      <p v-else class="dim">{{ auth.user?.email }}</p>

      <input
        v-model="current"
        type="password"
        placeholder="当前密码"
        autocomplete="current-password"
        autofocus
      />
      <input
        v-model="next"
        type="password"
        placeholder="新密码（至少 8 位）"
        autocomplete="new-password"
      />
      <input
        v-model="confirm"
        type="password"
        placeholder="再输一次新密码"
        autocomplete="new-password"
      />

      <div v-if="error" class="error-banner">{{ error }}</div>

      <button class="primary" type="submit" :disabled="busy || !current || !next || !confirm">
        {{ busy ? '提交中…' : '修改密码' }}
      </button>

      <p class="faint">改完之后，你在其它设备上的登录会被踢掉。</p>
    </form>
  </div>
</template>
