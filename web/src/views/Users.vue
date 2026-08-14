<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'
import { auth } from '../auth'

const users = ref([])
const error = ref('')
const busy = ref('')
// 代重置出来的明文密码只在这一次响应里有，刷新就没了，所以单独存住展示
const issued = ref(null)
const onUnauthorized = inject('onUnauthorized')

async function load() {
  try {
    const r = await api.users()
    users.value = r.users || []
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

// 所有操作共用一条路径：标记忙碌 → 调接口 → 重载 → 统一报错
async function act(key, fn) {
  error.value = ''
  busy.value = key
  try {
    const res = await fn()
    await load()
    return res
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

const isSelf = (u) => u.id === auth.user?.id

function toggleDisabled(u) {
  if (!u.disabled && !confirm(`确认停用 ${u.email}？该用户的登录会话会立即失效。`)) return
  act(`toggle-${u.id}`, () => (u.disabled ? api.enableUser(u.id) : api.disableUser(u.id)))
}

function toggleRole(u) {
  const next = u.role === 'admin' ? 'member' : 'admin'
  const what = next === 'admin' ? '提升为管理员' : '降级为普通用户'
  if (!confirm(`确认把 ${u.email} ${what}？`)) return
  act(`role-${u.id}`, () => api.setUserRole(u.id, next))
}

async function resetPassword(u) {
  if (!confirm(`确认重置 ${u.email} 的密码？该用户的所有登录会话会立即失效。`)) return
  const res = await act(`pw-${u.id}`, () => api.resetUserPassword(u.id, ''))
  if (res?.password) issued.value = { email: u.email, password: res.password }
}

function remove(u) {
  if (!confirm(`确认删除用户 ${u.email}？\n\n其名下的任务、仓库配置与凭据都会一并删除，不可恢复。`)) return
  act(`del-${u.id}`, () => api.deleteUser(u.id))
}

async function copyPassword() {
  try {
    await navigator.clipboard.writeText(issued.value.password)
  } catch {
    error.value = '浏览器不允许自动复制，请手动选中上面的密码'
  }
}

onMounted(load)
</script>

<template>
  <h1>用户管理</h1>

  <div v-if="error" class="error-banner">{{ error }}</div>

  <!-- 第一步的语义必须讲清楚，否则管理员会以为删掉某人只影响他自己的任务 -->
  <p class="dim note">当前所有用户共享同一份任务与仓库数据。按用户隔离数据是下一步的工作。</p>

  <!-- 明文密码只出现这一次，给个显眼的位置和复制按钮 -->
  <div v-if="issued" class="card issued">
    <div class="spread">
      <h2>{{ issued.email }} 的新密码</h2>
      <button @click="issued = null">知道了</button>
    </div>
    <p class="dim">只显示这一次，请立即转交本人。对方首次登录时会被要求修改。</p>
    <div class="row">
      <code class="mono pw">{{ issued.password }}</code>
      <button @click="copyPassword">复制</button>
    </div>
  </div>

  <div class="card scroll-x">
    <table>
      <thead>
        <tr>
          <th>邮箱</th>
          <th>角色</th>
          <th>状态</th>
          <th>任务</th>
          <th>最后登录</th>
          <th>注册时间</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="u in users" :key="u.id">
          <td>
            {{ u.email }}
            <span v-if="isSelf(u)" class="faint">（你）</span>
            <div v-if="!u.hasPassword" class="faint">未设置密码，无法登录</div>
          </td>
          <td>
            <span class="badge" :class="u.role === 'admin' ? 'ok' : 'idle'">
              {{ u.role === 'admin' ? '管理员' : '普通用户' }}
            </span>
          </td>
          <td>
            <span class="badge" :class="u.disabled ? 'bad' : 'ok'">
              {{ u.disabled ? '已停用' : '正常' }}
            </span>
            <div v-if="u.mustChangePassword" class="faint">待改初始密码</div>
          </td>
          <td>
            {{ u.taskTotal }}
            <span v-if="u.taskTotal" class="faint">
              （成功 {{ u.taskOk }} · 失败 {{ u.taskFailed }}）
            </span>
          </td>
          <td class="dim">{{ formatTime(u.lastLoginAt) }}</td>
          <td class="dim">{{ formatTime(u.createdAt) }}</td>
          <td>
            <div class="row wrap">
              <button :disabled="busy === `pw-${u.id}`" @click="resetPassword(u)">重置密码</button>
              <template v-if="!isSelf(u)">
                <button :disabled="busy === `role-${u.id}`" @click="toggleRole(u)">
                  {{ u.role === 'admin' ? '降为普通' : '设为管理员' }}
                </button>
                <button :disabled="busy === `toggle-${u.id}`" @click="toggleDisabled(u)">
                  {{ u.disabled ? '启用' : '停用' }}
                </button>
                <button class="danger" :disabled="busy === `del-${u.id}`" @click="remove(u)">
                  删除
                </button>
              </template>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
    <div v-if="!users.length" class="empty">还没有用户</div>
  </div>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0 0 16px; }
h2 { font-size: 15px; margin: 0; }

.note { margin: 0 0 16px; font-size: 13px; }

.issued { margin-bottom: 16px; border-color: var(--ok); }
.issued .spread { margin-bottom: 8px; }
.issued p { margin: 0 0 10px; }
.pw { font-size: 15px; padding: 6px 10px; background: var(--surface-2); border-radius: var(--radius); }
</style>
