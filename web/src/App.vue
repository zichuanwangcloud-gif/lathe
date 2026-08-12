<script setup>
import { ref, onMounted, provide } from 'vue'
import { api, UnauthorizedError } from './api'

const authed = ref(false)
const authEnabled = ref(true)
const loading = ref(true)
const token = ref('')
const loginError = ref('')

async function checkAuth() {
  try {
    const me = await api.me()
    authed.value = me.authenticated
    authEnabled.value = me.authEnabled
  } catch {
    authed.value = false
  } finally {
    loading.value = false
  }
}

async function login() {
  loginError.value = ''
  try {
    await api.login(token.value)
    token.value = ''
    await checkAuth()
  } catch (e) {
    loginError.value = e instanceof UnauthorizedError ? '令牌不正确' : e.message
  }
}

async function logout() {
  await api.logout()
  authed.value = false
}

// 任意子组件遇到 401 时调用，统一切回登录页
provide('onUnauthorized', () => { authed.value = false })

onMounted(checkAuth)
</script>

<template>
  <div v-if="loading" class="empty">加载中…</div>

  <!-- 未配置管理令牌：说清楚怎么开，而不是只说不可用 -->
  <div v-else-if="!authEnabled" class="login-wrap">
    <div class="card login-card">
      <h1>Lathe</h1>
      <p class="dim">管理界面未启用。</p>
      <p class="dim">设置环境变量后重启控制面：</p>
      <pre>LATHE_ADMIN_TOKEN=&lt;自定义一个强口令&gt;</pre>
    </div>
  </div>

  <div v-else-if="!authed" class="login-wrap">
    <form class="card login-card" @submit.prevent="login">
      <h1>Lathe</h1>
      <p class="dim">输入管理令牌（LATHE_ADMIN_TOKEN）</p>
      <input v-model="token" type="password" placeholder="管理令牌" autofocus />
      <div v-if="loginError" class="error-banner">{{ loginError }}</div>
      <button class="primary" type="submit" :disabled="!token">登录</button>
    </form>
  </div>

  <div v-else class="shell">
    <header class="topbar">
      <div class="row">
        <RouterLink to="/" class="brand">Lathe</RouterLink>
        <nav class="row">
          <RouterLink to="/">任务看板</RouterLink>
          <RouterLink to="/repos">仓库配置</RouterLink>
          <RouterLink to="/settings">设置</RouterLink>
        </nav>
      </div>
      <button @click="logout">退出</button>
    </header>
    <main><RouterView /></main>
  </div>
</template>

<style scoped>
.login-wrap {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: 20px;
}
.login-card {
  width: min(420px, 100%);
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.login-card h1 { margin: 0; font-size: 26px; letter-spacing: -0.02em; }
.login-card p { margin: 0; }

.shell { min-height: 100vh; }

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 12px 24px;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
  position: sticky;
  top: 0;
  z-index: 10;
  flex-wrap: wrap;
}
.brand {
  font-weight: 600;
  font-size: 17px;
  color: var(--text);
  margin-right: 12px;
}
.brand:hover { text-decoration: none; }
nav a { color: var(--text-dim); padding: 4px 2px; }
nav a:hover { text-decoration: none; color: var(--text); }
nav a.router-link-exact-active { color: var(--accent); }

main { padding: 24px; max-width: 1280px; margin: 0 auto; }
</style>
