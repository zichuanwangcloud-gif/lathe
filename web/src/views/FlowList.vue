<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError, formatTime } from '../api'

const flows = ref([])
const loading = ref(true)
const error = ref('')
const onUnauthorized = inject('onUnauthorized')

async function load() {
  loading.value = true
  try {
    const r = await api.flows()
    flows.value = r.flows || []
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="spread" style="margin-bottom: 16px">
    <div>
      <h2 style="margin: 0">编排流程图</h2>
      <p class="dim" style="margin: 4px 0 0">
        一批带依赖关系的任务一次性建图、自动按序调度——不用每个后继手动重新入队。
      </p>
    </div>
    <button class="primary" @click="$router.push('/flows/new')">新建编排图</button>
  </div>

  <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

  <div v-if="loading" class="card empty">加载中…</div>

  <div v-else class="card scroll-x" style="padding: 0">
    <table v-if="flows.length">
      <thead>
        <tr>
          <th>图名</th>
          <th>任务数</th>
          <th>创建时间</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="f in flows" :key="f.id">
          <td><RouterLink :to="`/flows/${f.id}`">{{ f.name }}</RouterLink></td>
          <td class="dim">{{ f.taskCount }}</td>
          <td class="dim">{{ formatTime(f.createdAt) }}</td>
        </tr>
      </tbody>
    </table>
    <div v-else class="empty">
      还没有编排图。点右上角「新建编排图」，把一批有依赖关系的任务连成一张图，一次入队。
    </div>
  </div>
</template>
