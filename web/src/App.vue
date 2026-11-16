<script setup>
import { onMounted, provide } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from './api'
import { auth, refresh, clear, isAdmin, hasLinearToken } from './auth'
import { computed } from 'vue'
import ConfirmDialog from './components/ConfirmDialog.vue'
import NavIcon from './components/NavIcon.vue'

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

// 侧栏分组：工作流在上、配置居中、管理员项单列一组，与 Multica 的分区一致
const navGroups = computed(() => [
  {
    label: '工作区',
    items: [
      { to: '/', icon: 'board', text: '任务看板', exact: true },
      { to: '/issues', icon: 'issues', text: '工单' },
      { to: '/prds', icon: 'prd', text: '模糊任务' },
      { to: '/flows', icon: 'flow', text: '编排' },
      hasLinearToken() && { to: '/linear', icon: 'linear', text: 'Linear 任务' },
    ].filter(Boolean),
  },
  {
    label: '配置',
    items: [
      { to: '/repos', icon: 'repo', text: '仓库配置' },
      { to: '/settings', icon: 'user', text: '个人设置' },
    ],
  },
  isAdmin() && {
    label: '管理',
    items: [
      { to: '/admin/settings', icon: 'settings', text: '系统设置' },
      { to: '/users', icon: 'users', text: '用户管理' },
    ],
  },
].filter(Boolean))

const initial = computed(() => (auth.user?.email || '?').slice(0, 1).toUpperCase())

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
    <aside class="sidebar">
      <RouterLink to="/" class="brand">
        <span class="logo" aria-hidden="true">L</span>
        <span>Lathe</span>
      </RouterLink>
      <nav aria-label="主导航">
        <div v-for="g in navGroups" :key="g.label" class="nav-group">
          <div class="nav-label">{{ g.label }}</div>
          <!-- 详情类子路径（/prds/3、/issues/7）也点亮所属菜单；看板是 /，只认精确匹配 -->
          <RouterLink
            v-for="it in g.items" :key="it.to" :to="it.to" class="nav-item"
            :exact-active-class="it.exact ? 'on' : ''" :active-class="it.exact ? '' : 'on'"
          >
            <NavIcon :name="it.icon" />
            <span>{{ it.text }}</span>
          </RouterLink>
        </div>
      </nav>
      <div class="account">
        <div class="who">
          <span class="avatar" aria-hidden="true">{{ initial }}</span>
          <div class="who-text">
            <div class="email" :title="auth.user?.email">{{ auth.user?.email }}</div>
            <div v-if="isAdmin()" class="role">管理员</div>
          </div>
        </div>
        <RouterLink to="/change-password" class="nav-item" active-class="on">
          <NavIcon name="key" /><span>修改密码</span>
        </RouterLink>
        <button class="nav-item ghost" @click="logout">
          <NavIcon name="logout" /><span>退出</span>
        </button>
      </div>
    </aside>
    <!-- 按 path 作 key：/tasks/1 → /tasks/2 这类同组件换参数时重建实例，
         否则旧任务的事件、游标、轮询状态会混进新页面。只看 path 不看
         query，看板改筛选/翻页（只动 query）不会被重建 -->
    <main class="page" :class="{ bleed: route.meta.fullBleed }">
      <div class="page-inner"><RouterView :key="route.meta.shellKey || route.path" /></div>
    </main>
    <ConfirmDialog />
  </div>
</template>

<style scoped>
/* 外壳：Multica 的三层表面 —— 侧栏落在 --shell 上，内容放进一张浮起的圆角画布，
   画布自己管滚动，侧栏始终钉住。 */
.shell {
  height: 100vh; height: 100dvh;
  display: grid;
  grid-template-columns: 232px minmax(0, 1fr);
  background: var(--shell);
}

.sidebar {
  display: flex;
  flex-direction: column;
  min-height: 0;
  padding: 10px 8px;
  gap: 8px;
}
.brand {
  display: flex; align-items: center; gap: 8px;
  padding: 6px 8px;
  font-weight: 600; font-size: 15px;
  color: var(--text);
}
.brand:hover { text-decoration: none; }
.logo {
  width: 22px; height: 22px;
  display: grid; place-items: center;
  border-radius: 6px;
  background: var(--primary); color: var(--primary-fg);
  font-size: 12px; font-weight: 700;
}
nav { flex: 1; min-height: 0; overflow-y: auto; display: flex; flex-direction: column; gap: 14px; padding-top: 6px; }
.nav-group { display: flex; flex-direction: column; gap: 1px; }
.nav-label { padding: 2px 8px 4px; font-size: 11px; line-height: 15px; font-weight: 500; color: var(--text-faint); }

.nav-item {
  display: flex; align-items: center; gap: 8px;
  height: 30px; padding: 0 8px;
  border-radius: var(--radius-sm);
  color: var(--text-dim);
  font-size: 13px; font-weight: 500;
  width: 100%; justify-content: flex-start;
  min-height: 0;
}
.nav-item:hover { text-decoration: none; background: color-mix(in oklab, var(--surface-2) 70%, transparent); color: var(--text); }
.nav-item.on { background: var(--surface-selected); color: var(--text); }
.nav-item .icon { flex: none; opacity: .85; }

.account { border-top: 1px solid var(--border); padding-top: 8px; display: flex; flex-direction: column; gap: 1px; }
.who { display: flex; align-items: center; gap: 8px; padding: 4px 8px 8px; min-width: 0; }
.avatar {
  width: 26px; height: 26px; flex: none;
  display: grid; place-items: center;
  border-radius: 50%;
  background: var(--accent-bg); color: var(--accent);
  font-size: 12px; font-weight: 600;
}
.who-text { min-width: 0; }
.email { font-size: 12.5px; color: var(--text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.role { font-size: 11px; color: var(--text-faint); }

.page {
  margin: 8px 8px 8px 0;
  min-width: 0; min-height: 0;
  background: var(--bg);
  border-radius: var(--radius-lg);
  border: 1px solid var(--border);
  box-shadow: var(--surface-shadow);
  overflow: auto;
}
.page-inner { padding: 24px 32px; max-width: 1280px; margin: 0 auto; }

/* 铺满视口的页面（模糊任务的对话界面）：内容区自己管滚动，
   输入框才能钉在底部、对话流在中间滚 —— 整页滚动做不出对话形态。 */
.page.bleed { overflow: hidden; display: flex; flex-direction: column; }
.page.bleed .page-inner { flex: 1; min-height: 0; width: 100%; max-width: none; padding: 0; display: flex; flex-direction: column; }

/* 窄屏：侧栏退成顶部横条，画布贴边 */
@media (max-width: 768px) {
  .shell { display: flex; flex-direction: column; height: auto; min-height: 100dvh; }
  .shell.bleed { height: 100dvh; }
  .sidebar { flex-direction: row; align-items: center; padding: 6px 8px; overflow-x: auto; }
  nav { flex-direction: row; overflow: visible; padding: 0; gap: 4px; }
  .nav-group { flex-direction: row; }
  .nav-label, .who, .account .nav-item span { display: none; }
  .nav-item { width: auto; white-space: nowrap; }
  .account { border-top: 0; padding: 0; flex-direction: row; }
  .page { margin: 0; border-radius: 0; border-width: 1px 0 0; flex: 1; }
  .page-inner { padding: 16px; }
}
</style>
