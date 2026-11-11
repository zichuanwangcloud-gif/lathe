<script setup>
// 团队列表（f12）：我加入的团队 + 建团队 + 输入一次性邀请码加入。
//
// 团队共享的是任务可见性，不是凭据、执行身份或仓库配置的编辑权。
import { ref, onMounted, inject } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError } from '../api'

const ROLE_LABEL = { owner: 'owner', admin: 'admin', member: '成员' }

const teams = ref([])
const error = ref('')
const notice = ref('')
const newName = ref('')
const code = ref('')
const busy = ref(false)
const onUnauthorized = inject('onUnauthorized')
const router = useRouter()

async function load() {
  try {
    const r = await api.teams()
    teams.value = r.teams || []
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function create() {
  const name = newName.value.trim()
  if (!name) return
  busy.value = true
  error.value = ''
  try {
    const t = await api.createTeam(name)
    newName.value = ''
    router.push(`/teams/${t.id}`)
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function join() {
  const c = code.value.trim()
  if (!c) return
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.acceptInvitation(c)
    code.value = ''
    router.push(`/teams/${r.teamId}`)
  } catch (e) {
    // 码无效/已用/过期/已是成员统一是同一个提示，不区分（防探测）
    error.value = e.message
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <h1>团队</h1>
  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>
  <div v-if="notice" class="card">{{ notice }}</div>

  <div class="grid">
    <div class="card">
      <h2>用邀请码加入</h2>
      <p class="faint">
        邀请码由团队 owner / admin 生成，通过你们自己信任的渠道转交，48 小时内单次有效。
      </p>
      <form class="row" style="display: flex; gap: 10px" @submit.prevent="join">
        <input v-model="code" class="mono" style="flex: 1" placeholder="邀请码" :disabled="busy" />
        <button class="primary" :disabled="busy || !code.trim()">加入</button>
      </form>
    </div>

    <div class="card">
      <h2>新建团队</h2>
      <p class="faint">你会成为它的 owner。团队名 1–64 个字符，不要求全站唯一。</p>
      <form class="row" style="display: flex; gap: 10px" @submit.prevent="create">
        <input v-model="newName" style="flex: 1" maxlength="64" placeholder="团队名" :disabled="busy" />
        <button :disabled="busy || !newName.trim()">新建</button>
      </form>
    </div>
  </div>

  <div v-if="!teams.length" class="card empty">你还没有加入任何团队。</div>
  <div class="teams">
    <RouterLink v-for="t in teams" :key="t.id" :to="`/teams/${t.id}`" class="card team">
      <div class="spread">
        <h2>{{ t.name }}</h2>
        <span class="badge" :class="{ ok: t.role === 'owner' }">{{ ROLE_LABEL[t.role] || t.role }}</span>
      </div>
      <div class="faint">{{ t.memberCount }} 名成员 · {{ t.sharedRepos }} 个共享仓库</div>
    </RouterLink>
  </div>

  <p class="faint" style="margin-top: 12px">
    团队共享的是任务与验证证据的<b>可见性</b>：任务仍由创建者用他自己的凭据执行，仓库配置只有仓库属主能改，
    凭据永不共享。站内管理员不会自动成为任何团队的成员。
  </p>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0 0 16px; }
h2 { font-size: 15px; margin: 0 0 6px; }
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 12px;
  margin-bottom: 16px;
}
.teams {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 12px;
}
.team { color: inherit; text-decoration: none; }
</style>
