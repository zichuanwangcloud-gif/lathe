<script setup>
import { onMounted, provide } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from './api'
import { auth, refresh, clear, isAdmin, hasLinearToken } from './auth'
import ConfirmDialog from './components/ConfirmDialog.vue'

const route = useRoute()
const router = useRouter()

async function logout() {
  await api.logout()
  clear()
  router.push({ name: 'login' })
}

// 任意子组件遇到 401 时调用，统一切回登录页。
// 保留这个 provide 是为了让四个既有视图（看板、详情、仓库、设置）一行不用改。
provide('onUnauthorized', () => {
  clear()
  router.push({ name: 'login' })
})

onMounted(() => {
  if (!auth.ready) refresh()
})
</script>

<template>
  <div v-if="!auth.ready" class="empty">加载中…</div>

  <!-- 鉴权不可用（多半是没跑迁移）：说清楚怎么修，而不是只说不可用 -->
  <div v-else-if="!auth.authEnabled" class="login-wrap">
    <div class="card login-card">
      <h1>Lathe</h1>
      <p class="dim">账号体系尚未就绪。</p>
      <p class="dim">先应用数据库迁移，再重启控制面：</p>
      <pre>./bin/lathe migrate up</pre>
    </div>
  </div>

  <!-- 公开页自带居中布局，不套导航外壳 -->
  <RouterView v-else-if="route.meta.public" />

  <div v-else class="shell" :class="{ bleed: route.meta.fullBleed }">
    <header class="topbar">
      <div class="row">
        <RouterLink to="/" class="brand">Lathe</RouterLink>
        <nav class="row">
          <RouterLink to="/">任务看板</RouterLink>
          <RouterLink to="/issues">工单</RouterLink>
          <RouterLink to="/prds">模糊任务</RouterLink>
          <RouterLink to="/flows">编排</RouterLink>
          <RouterLink v-if="hasLinearToken()" to="/linear">Linear 任务</RouterLink>
          <RouterLink to="/repos">仓库配置</RouterLink>
          <RouterLink to="/settings">个人设置</RouterLink>
          <RouterLink v-if="isAdmin()" to="/admin/settings">系统设置</RouterLink>
          <RouterLink v-if="isAdmin()" to="/users">用户管理</RouterLink>
        </nav>
      </div>
      <div class="row">
        <span class="dim">{{ auth.user?.email }}</span>
        <span v-if="isAdmin()" class="badge ok">管理员</span>
        <RouterLink to="/change-password" class="dim">修改密码</RouterLink>
        <button @click="logout">退出</button>
      </div>
    </header>
    <!-- 按 path 作 key：/tasks/1 → /tasks/2 这类同组件换参数时重建实例，
         否则旧任务的事件、游标、轮询状态会混进新页面。只看 path 不看
         query，看板改筛选/翻页（只动 query）不会被重建 -->
    <main :class="{ bleed: route.meta.fullBleed }"><RouterView :key="route.meta.shellKey || route.path" /></main>
    <ConfirmDialog />
  </div>
</template>

<style scoped>
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
nav a.router-link-exact-active,
/* 详情类子路径（/prds/3、/issues/7）也点亮所属菜单；看板是 /，用 exact 才不会处处点亮 */
nav a.router-link-active:not([href="/"]) { color: var(--accent); }

main { padding: 24px; max-width: 1280px; margin: 0 auto; }

/* 铺满视口的页面（模糊任务的对话界面）：外壳定高、内容区自己管滚动，
   输入框才能钉在底部、对话流在中间滚 —— 整页滚动做不出对话形态。 */
.shell.bleed { height: 100vh; height: 100dvh; display: flex; flex-direction: column; }
.shell.bleed .topbar { position: static; flex: none; }
main.bleed { flex: 1; min-height: 0; width: 100%; max-width: none; padding: 0; display: flex; flex-direction: column; }
</style>
