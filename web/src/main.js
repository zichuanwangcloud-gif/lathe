// Lathe 管理界面入口。
import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Board from './views/Board.vue'
import LinearIssues from './views/LinearIssues.vue'
import TaskDetail from './views/TaskDetail.vue'
import FlowList from './views/FlowList.vue'
import FlowCanvas from './views/FlowCanvas.vue'
import PRDList from './views/PRDList.vue'
import PRDDetail from './views/PRDDetail.vue'
import Issues from './views/Issues.vue'
import IssueDetail from './views/IssueDetail.vue'
import Repos from './views/Repos.vue'
import PersonalSettings from './views/PersonalSettings.vue'
import SystemSettings from './views/SystemSettings.vue'
import Users from './views/Users.vue'
import Login from './views/Login.vue'
import Register from './views/Register.vue'
import ForgotPassword from './views/ForgotPassword.vue'
import ResetPassword from './views/ResetPassword.vue'
import ChangePassword from './views/ChangePassword.vue'
import NotFound from './views/NotFound.vue'
import { auth, refresh, isAdmin, mustChangePassword, hasLinearToken } from './auth'
import { setPasswordChangeHandler } from './api'
import './style.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'board', component: Board },
    // 内置工单（docs/09）：不依赖 Linear 的手动需求入口，所有人可用
    { path: '/issues', name: 'issues', component: Issues },
    { path: '/issues/:id', name: 'issue', component: IssueDetail, props: true },
    { path: '/flows', name: 'flows', component: FlowList },
    { path: '/flows/new', name: 'flow-new', component: FlowCanvas },
    { path: '/flows/:id', name: 'flow', component: FlowCanvas, props: true },
    // 模糊任务（docs/10）：说不清的需求先对话成 PRD，签字后才生成正式任务
    { path: '/prds', name: 'prds', component: PRDList },
    { path: '/prds/:id', name: 'prd', component: PRDDetail, props: true },
    // 「Linear 任务」只有绑定了 Linear API 令牌的人可见（菜单与守卫同一依据）
    { path: '/linear', name: 'linear', component: LinearIssues, meta: { linear: true } },
    { path: '/tasks/:id', name: 'task', component: TaskDetail, props: true },
    { path: '/repos', name: 'repos', component: Repos },
    { path: '/settings', name: 'settings', component: PersonalSettings },
    { path: '/admin/settings', name: 'system-settings', component: SystemSettings, meta: { admin: true } },
    { path: '/users', name: 'users', component: Users, meta: { admin: true } },
    { path: '/change-password', name: 'change-password', component: ChangePassword },

    { path: '/login', name: 'login', component: Login, meta: { public: true } },
    { path: '/register', name: 'register', component: Register, meta: { public: true } },
    { path: '/forgot-password', name: 'forgot', component: ForgotPassword, meta: { public: true } },
    { path: '/reset-password', name: 'reset', component: ResetPassword, meta: { public: true } },
    // 兜底：未知地址给明确的 404 页而不是空壳。放在最后，且只在登录后可见
    // （未登录先被守卫送去登录页）。
    { path: '/:pathMatch(.*)*', name: 'not-found', component: NotFound },
  ],
})

router.beforeEach(async (to) => {
  if (!auth.ready) await refresh()

  if (to.meta.public) {
    // 已登录的人点进登录/注册页直接送回看板。重置密码页除外：
    // 用户可能正登录着，却拿着给自己发的重置链接过来。
    return auth.authenticated && to.name !== 'reset' ? { name: 'board' } : true
  }

  if (!auth.authenticated) return { name: 'login', query: { next: to.fullPath } }

  // 初始密码没改之前哪也去不了 —— 与服务端的 409 闸门对齐，
  // 这里只是省掉一次注定失败的请求，不是安全边界。
  if (mustChangePassword() && to.name !== 'change-password') {
    return { name: 'change-password' }
  }
  if (to.meta.admin && !isAdmin()) return { name: 'board' }
  // 未绑令牌的人直达 /linear 时拦回看板。这不是安全边界 ——
  // 服务端接口本身会拒绝，这里只是省一次注定失败的页面加载。
  if (to.meta.linear && !hasLinearToken()) return { name: 'board' }
  return true
})

// 任何接口回 409 mustChangePassword 时，把人带到改密页
setPasswordChangeHandler(() => {
  if (router.currentRoute.value.name !== 'change-password') {
    router.push({ name: 'change-password' })
  }
})

createApp(App).use(router).mount('#app')
