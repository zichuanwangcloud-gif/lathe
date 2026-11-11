<script setup>
// 顶栏收件箱的未读徽标（docs/features/inbox.md §3.5）。
//
// 自己轮询 GET /api/inbox/count：30 秒一次，页面隐藏时暂停（回到前台立刻
// 补一次），路由切换时立刻刷新 —— 人从详情页处理完回来，数字不该等半分钟。
// 拿不到数就不显示，徽标只是提示，不打断页面。
import { ref, watch, onMounted, onUnmounted } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import { inboxSignal } from '../inbox'

const POLL_MS = 30000

const unread = ref(0)
const route = useRoute()
let timer = null

async function load() {
  if (document.visibilityState === 'hidden') return
  try {
    unread.value = (await api.inboxCount()).unread || 0
  } catch {
    /* 401 由页面自己的请求去处理；其余失败保留上次的数 */
  }
}

function onVisibility() {
  if (document.visibilityState === 'visible') load()
}

onMounted(() => {
  load()
  timer = setInterval(load, POLL_MS)
  document.addEventListener('visibilitychange', onVisibility)
})
onUnmounted(() => {
  clearInterval(timer)
  document.removeEventListener('visibilitychange', onVisibility)
})
watch(() => route.fullPath, load)
// 收件箱页里标已读 / 忽略之后立刻刷新
watch(() => inboxSignal.version, load)
</script>

<template>
  <span v-if="unread > 0" class="inbox-badge" :aria-label="`${unread} 条未读`">{{ unread > 99 ? '99+' : unread }}</span>
</template>

<style scoped>
.inbox-badge {
  display: inline-block;
  min-width: 18px;
  padding: 0 5px;
  margin-left: 2px;
  border-radius: 999px;
  background: var(--bad);
  color: #fff;
  font-size: 11px;
  font-weight: 600;
  line-height: 18px;
  text-align: center;
  vertical-align: 1px;
}
</style>
