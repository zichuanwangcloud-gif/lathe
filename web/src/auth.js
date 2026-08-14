// 全局登录态。
//
// 不引 Pinia：这里只需要一个跨组件共享的响应式对象，Vue 自带的 reactive()
// 就够。用模块级单例而非 provide/inject，是因为路由守卫拿不到组件树。
import { reactive } from 'vue'
import { api } from './api'

export const auth = reactive({
  // ready 表示已经问过服务端一次。守卫据此决定要不要先等一下，
  // 否则刷新页面时会在拿到真实状态前误判成未登录、闪一下登录页。
  ready: false,
  authenticated: false,
  authEnabled: true,
  user: null,
})

export async function refresh() {
  try {
    const me = await api.me()
    auth.authenticated = me.authenticated
    auth.authEnabled = me.authEnabled
    auth.user = me.user
  } catch {
    auth.authenticated = false
    auth.user = null
  } finally {
    auth.ready = true
  }
}

export function clear() {
  auth.authenticated = false
  auth.user = null
}

export const isAdmin = () => auth.user?.role === 'admin'
export const mustChangePassword = () => auth.user?.mustChangePassword === true
