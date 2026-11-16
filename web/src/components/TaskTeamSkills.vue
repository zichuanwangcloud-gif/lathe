<script setup>
// 任务详情里的一行「本次任务钉住的团队技能」（f11）。独立组件，TaskDetail
// 只挂一行：与 f04（任务详情页重构）冲突时整块搬走即可（PRD R9）。
import { ref, onMounted } from 'vue'
import { api } from '../api'

const props = defineProps({ taskId: { type: String, required: true } })
const data = ref(null)

onMounted(async () => {
  try {
    data.value = await api.taskTeamSkills(props.taskId)
  } catch {
    data.value = null // 读不到不影响详情页其余部分
  }
})
</script>

<template>
  <template v-if="data">
    <dt>团队技能</dt>
    <dd>
      <span v-if="!data.pinned" class="faint">尚未钉住（任务还没进入实现阶段）</span>
      <span v-else-if="!data.skills.length" class="faint">无（当时仓库没有启用的团队技能）</span>
      <span v-else class="mono">
        <span v-for="s in data.skills" :key="s.slug" class="pin">{{ s.slug }}@v{{ s.version }}</span>
      </span>
    </dd>
  </template>
</template>

<style scoped>
/* 本组件是多根片段（dt + dd），拿不到 TaskDetail 的 scoped 样式：不抄一份的话
   标签会变黑变大、值带浏览器默认的 40px 左缩进（AC-15 手测截图发现）。
   与 TaskDetail.vue 的 dt/dd 规则保持一致；不去改 TaskDetail（PRD R9：只挂一行）。 */
dt { color: var(--text-dim); font-size: 13px; }
dd { margin: 0; overflow-wrap: anywhere; }
.pin { margin-right: 8px; }
</style>
