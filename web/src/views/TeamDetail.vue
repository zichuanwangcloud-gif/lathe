<script setup>
// 团队详情（f12）：成员表 + 角色操作 + 邀请码。
//
// 按钮显隐照 docs/features/team-roles.md §3.6 的逐格判定表：越权按钮不渲染，
// 而不是点了再报 404。权限判定仍在服务端，这里只是体验。
import { ref, computed, onMounted, inject } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, formatTime } from '../api'
import { auth } from '../auth'
import { confirmDialog } from '../confirm'

const props = defineProps({ id: { type: String, required: true } })
const router = useRouter()
const onUnauthorized = inject('onUnauthorized')

const ROLE_LABEL = { owner: 'owner', admin: 'admin', member: '成员' }

const team = ref(null)
const invitations = ref([])
const error = ref('')
const busy = ref(false)
const newCode = ref(null) // 刚生成的明文邀请码（只展示这一次）
const inviteRole = ref('member')
const inviteEmail = ref('')
const renaming = ref('')
const copied = ref(false)

const me = computed(() => auth.user?.id)
const myRole = computed(() => team.value?.myRole)
const isOwner = computed(() => myRole.value === 'owner')
const canInvite = computed(() => myRole.value === 'owner' || myRole.value === 'admin')

// 逐格判定表：owner 可移 admin/member；admin 只可移 member；任何人可退出自己（owner 除外）
function canRemove(m) {
  if (m.userId === me.value) return myRole.value !== 'owner'
  if (myRole.value === 'owner') return m.role !== 'owner'
  if (myRole.value === 'admin') return m.role === 'member'
  return false
}

async function load() {
  try {
    team.value = await api.team(props.id)
    renaming.value = team.value.name
    if (canInvite.value) {
      const r = await api.teamInvitations(props.id)
      invitations.value = r.invitations || []
    }
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function run(fn, confirmText) {
  if (confirmText && !(await confirmDialog(confirmText))) return false
  busy.value = true
  error.value = ''
  try {
    await fn()
    return true
  } catch (e) {
    error.value = e.message
    return false
  } finally {
    busy.value = false
  }
}

async function invite() {
  const ok = await run(async () => {
    const inv = await api.createInvitation(props.id, {
      role: inviteRole.value,
      inviteeEmail: inviteEmail.value.trim() || undefined,
    })
    newCode.value = inv.code
    inviteEmail.value = ''
    copied.value = false
  })
  if (ok) await load()
}

async function copyCode() {
  try {
    await navigator.clipboard.writeText(newCode.value)
    copied.value = true
  } catch {
    copied.value = false
  }
}

async function revoke(inv) {
  if (await run(() => api.revokeInvitation(props.id, inv.id), `撤销末尾为 ${inv.hint} 的邀请码？`)) await load()
}

async function setRole(m, role) {
  if (await run(() => api.setMemberRole(props.id, m.userId, role))) await load()
}

async function remove(m) {
  const self = m.userId === me.value
  const text = self
    ? '确认退出这个团队？你共享到这里的仓库会同时回到私有。'
    : `确认移除 ${m.email}？他共享到这个团队的仓库会同时回到私有。`
  if (!(await run(() => api.removeMember(props.id, m.userId), text))) return
  if (self) router.push('/teams')
  else await load()
}

async function transfer(m) {
  const text = `把 owner 转让给 ${m.email}？转让后你会成为 admin。`
  if (await run(() => api.transferTeam(props.id, m.userId), text)) await load()
}

async function rename() {
  const name = renaming.value.trim()
  if (!name || name === team.value.name) return
  if (await run(() => api.renameTeam(props.id, name))) await load()
}

async function destroy() {
  if (await run(() => api.deleteTeam(props.id), '确认删除这个团队？需要先没有任何共享仓库。')) router.push('/teams')
}

onMounted(load)
</script>

<template>
  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>
  <div v-if="!team && !error" class="empty">加载中…</div>

  <template v-if="team">
    <div class="spread head">
      <div class="row">
        <h1>{{ team.name }}</h1>
        <span class="badge" :class="{ ok: isOwner }">我是 {{ ROLE_LABEL[myRole] || myRole }}</span>
      </div>
      <button @click="router.push('/teams')">返回</button>
    </div>

    <div class="card">
      <h2>成员</h2>
      <p class="faint">成员邮箱只对同团队成员可见。</p>
      <div class="scroll-x">
        <table>
          <thead>
            <tr><th>邮箱</th><th>角色</th><th>加入时间</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="m in team.members" :key="m.userId">
              <td>{{ m.email }}<span v-if="m.userId === me" class="faint">（我）</span></td>
              <td><span class="badge" :class="{ ok: m.role === 'owner' }">{{ ROLE_LABEL[m.role] || m.role }}</span></td>
              <td class="dim">{{ formatTime(m.joinedAt) }}</td>
              <td class="wrap">
                <template v-if="isOwner && m.userId !== me">
                  <button v-if="m.role === 'member'" :disabled="busy" @click="setRole(m, 'admin')">设为 admin</button>
                  <button v-if="m.role === 'admin'" :disabled="busy" @click="setRole(m, 'member')">降为成员</button>
                  <button :disabled="busy" @click="transfer(m)">转让 owner</button>
                </template>
                <button v-if="canRemove(m)" class="danger" :disabled="busy" @click="remove(m)">
                  {{ m.userId === me ? '退出团队' : '移除' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-if="canInvite" class="card">
      <h2>邀请码</h2>
      <p class="faint">
        邀请不按邮箱投递：生成一次性邀请码，通过你信任的渠道交给对方。48 小时内单次有效，可撤销。
        邮箱只是给你看的备注，不参与判定。
      </p>
      <form class="row" style="display: flex; gap: 10px; flex-wrap: wrap" @submit.prevent="invite">
        <select v-model="inviteRole" :disabled="busy">
          <option value="member">成员</option>
          <option value="admin">admin</option>
        </select>
        <input v-model="inviteEmail" style="flex: 1" placeholder="备注（可选，如对方邮箱）" :disabled="busy" />
        <button class="primary" :disabled="busy">生成邀请码</button>
      </form>
      <div v-if="newCode" class="code-box">
        <p>这是明文邀请码，<b>只显示这一次</b>：</p>
        <div class="row" style="display: flex; gap: 8px">
          <code class="mono">{{ newCode }}</code>
          <button type="button" @click="copyCode">{{ copied ? '已复制' : '复制' }}</button>
        </div>
      </div>
      <table v-if="invitations.length" style="margin-top: 12px">
        <thead><tr><th>码尾</th><th>角色</th><th>备注</th><th>过期时间</th><th></th></tr></thead>
        <tbody>
          <tr v-for="inv in invitations" :key="inv.id">
            <td class="mono">…{{ inv.hint }}</td>
            <td>{{ ROLE_LABEL[inv.role] || inv.role }}</td>
            <td class="dim">{{ inv.inviteeEmail || '—' }}</td>
            <td class="dim">{{ formatTime(inv.expiresAt) }}</td>
            <td><button :disabled="busy" @click="revoke(inv)">撤销</button></td>
          </tr>
        </tbody>
      </table>
      <p v-else class="faint" style="margin-top: 10px">没有待接受的邀请。</p>
    </div>

    <div v-if="isOwner" class="card">
      <h2>团队设置</h2>
      <form class="row" style="display: flex; gap: 10px" @submit.prevent="rename">
        <input v-model="renaming" maxlength="64" style="flex: 1" :disabled="busy" />
        <button :disabled="busy || !renaming.trim() || renaming.trim() === team.name">改名</button>
      </form>
      <p class="faint" style="margin-top: 12px">
        删除团队前需先没有共享仓库（移除共享了仓库的成员会同时收回其共享）。
      </p>
      <button class="danger" :disabled="busy" @click="destroy">删除团队</button>
    </div>
  </template>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0; }
h2 { font-size: 15px; margin: 0 0 6px; }
.head { margin-bottom: 16px; }
.card { margin-bottom: 16px; }
.code-box { margin-top: 12px; padding: 10px; border: 1px dashed var(--border, #ccc); border-radius: 6px; }
.code-box code { font-size: 15px; word-break: break-all; }
</style>
