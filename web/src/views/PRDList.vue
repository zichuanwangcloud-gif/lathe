<script setup>
import { ref, computed, onMounted, inject } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  api,
  UnauthorizedError,
  formatTime,
  prdStateLabel,
  prdStateTone,
  prdTypeLabel,
  PRD_TYPE_META,
} from '../api'
import BaseDialog from '../components/BaseDialog.vue'

// 模糊任务列表（docs/10-prd-template.md）：一句话说不清的需求先在这里
// 和智能体对话成一份 PRD，签字后才一键生成正式任务与编排图。

const route = useRoute()
const router = useRouter()
const onUnauthorized = inject('onUnauthorized')

const prds = ref([])
const total = ref(0)
const loading = ref(true)
const error = ref('')

// 筛选与页码走 URL query：刷新/后退/分享链接都不丢视图状态（与看板同一做法）
const PAGE_SIZE = 50
const filter = ref(typeof route.query.state === 'string' ? route.query.state : '')
const page = ref(Math.max(1, Number.parseInt(route.query.page) || 1))
const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_SIZE)))

async function load() {
  loading.value = true
  try {
    const r = await api.prds({
      state: filter.value || undefined,
      limit: PAGE_SIZE,
      offset: (page.value - 1) * PAGE_SIZE,
    })
    prds.value = r.prds || []
    total.value = r.total || 0
    error.value = ''
    // 翻到的页可能已经空了（PRD 流出了这个筛选）：退到最后一页而不是晾着空表
    if (!prds.value.length && page.value > 1) {
      page.value = pages.value
      syncQuery()
      return load()
    }
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function syncQuery() {
  router.replace({
    query: {
      ...route.query,
      state: filter.value || undefined,
      page: page.value > 1 ? String(page.value) : undefined,
    },
  })
}

function setFilter(v) {
  filter.value = v
  page.value = 1
  syncQuery()
  load()
}

function goPage(p) {
  page.value = p
  syncQuery()
  load()
}

// 列表里显示 §1 一句话；还没跑过任何一轮就退回原始输入
function headline(p) {
  const one = p.document?.oneLiner?.body
  return (one && one.trim()) || p.originalInput || '（无）'
}

// ---------- 新建模糊任务 ----------
const showCreate = ref(false)
const repos = ref([])
const createForm = ref({ repoId: null, prdType: 'feature', originalInput: '', refPrdId: '' })
const creating = ref(false)
const createError = ref('')

async function openCreate() {
  showCreate.value = true
  createError.value = ''
  if (!repos.value.length) {
    try {
      const r = await api.repos()
      repos.value = r.repos || []
      if (repos.value.length && !createForm.value.repoId) {
        createForm.value.repoId = repos.value[0].id
      }
    } catch (e) {
      if (e instanceof UnauthorizedError) return onUnauthorized()
      createError.value = e.message
    }
  }
}

async function submitCreate() {
  if (!createForm.value.repoId || !createForm.value.originalInput.trim()) return
  creating.value = true
  createError.value = ''
  try {
    const refId = Number.parseInt(createForm.value.refPrdId)
    const r = await api.createPrd({
      repoId: createForm.value.repoId,
      prdType: createForm.value.prdType,
      originalInput: createForm.value.originalInput.trim(),
      refPrdId: Number.isFinite(refId) && refId > 0 ? refId : undefined,
    })
    showCreate.value = false
    createForm.value.originalInput = ''
    createForm.value.refPrdId = ''
    // 建完直达详情页：对话、文档、签字都在那儿
    router.push(`/prds/${r.id}`)
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    createError.value = e.message
  } finally {
    creating.value = false
  }
}

onMounted(load)
</script>

<template>
  <div>
    <div class="spread" style="margin-bottom: 16px">
      <div>
        <h2 style="margin: 0">模糊任务</h2>
        <p class="dim" style="margin: 4px 0 0">
          一句话说不清的需求先在这里对话成一份 PRD —— 智能体读代码、提问、拆任务，
          对抗复核攻过验收标准、你逐节签字之后，才一键生成正式任务与编排图。
        </p>
      </div>
      <button class="primary" @click="openCreate">新建模糊任务</button>
    </div>

    <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

    <div class="card toolbar-row" style="margin-bottom: 12px">
      <label class="row" style="gap: 6px">
        <span class="dim">状态</span>
        <select :value="filter" @change="setFilter($event.target.value)">
          <option value="">全部</option>
          <option value="drafting">起草中</option>
          <option value="awaiting_answers">待回答</option>
          <option value="ready_for_review">待批准</option>
          <option value="approved">已批准</option>
          <option value="converted">已生成任务</option>
          <option value="abandoned">已放弃</option>
        </select>
      </label>
      <span class="dim" style="margin-left: auto">{{ total }} 份 PRD</span>
    </div>

    <div v-if="loading" class="card empty">加载中…</div>
    <div v-else-if="!prds.length" class="card empty">
      还没有模糊任务。点右上角「新建模糊任务」，把那句说不清的需求先扔进来 ——
      需求清楚的单子直接去「工单」，不用走规划。
    </div>
    <div v-else class="card scroll-x" style="padding: 0">
      <table>
        <thead>
          <tr>
            <th>编号</th>
            <th>一句话</th>
            <th>类型</th>
            <th>状态</th>
            <th>轮次</th>
            <th>更新时间</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in prds" :key="p.id">
            <td><RouterLink :to="`/prds/${p.id}`" class="mono">#{{ p.id }}</RouterLink></td>
            <td>
              <div class="headline">{{ headline(p) }}</div>
              <div v-if="p.refPrdId" class="faint">
                接续 <RouterLink :to="`/prds/${p.refPrdId}`" class="mono">#{{ p.refPrdId }}</RouterLink>
              </div>
            </td>
            <td class="dim">{{ prdTypeLabel(p.prdType) }}</td>
            <td>
              <span class="badge" :class="prdStateTone(p.state)">{{ prdStateLabel(p.state) }}</span>
              <span v-if="p.processing" class="badge run" style="margin-left: 6px">进行中</span>
            </td>
            <td class="dim">第 {{ p.round }} 轮</td>
            <td class="dim">{{ formatTime(p.updatedAt) }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 超过一页才显示翻页；筛选/页码都在 URL 里，链接可分享 -->
    <div v-if="pages > 1" class="row" style="justify-content: flex-end; margin-top: 8px">
      <button :disabled="page <= 1" @click="goPage(page - 1)">上一页</button>
      <span class="dim">第 {{ page }} / {{ pages }} 页</span>
      <button :disabled="page >= pages" @click="goPage(page + 1)">下一页</button>
    </div>

    <!-- 新建对话框：基座管 Escape/焦点/aria，这里只留业务字段 -->
    <BaseDialog v-if="showCreate" label-id="create-prd-title" @close="showCreate = false">
      <h3 id="create-prd-title" style="margin-top: 0">新建模糊任务</h3>
      <div v-if="createError" role="alert" class="error-banner">{{ createError }}</div>
      <div v-if="!repos.length" class="empty">
        你名下还没有仓库 —— 先到「仓库配置」页登记一个，规划智能体需要知道读哪个仓库的代码。
      </div>
      <template v-else>
        <label class="field">
          <span>仓库</span>
          <select v-model="createForm.repoId">
            <option v-for="r in repos" :key="r.id" :value="r.id">{{ r.providerRepo }}</option>
          </select>
        </label>
        <label class="field">
          <span>类型</span>
          <select v-model="createForm.prdType">
            <option value="feature">功能</option>
            <option value="defect">缺陷</option>
            <option value="refactor">重构</option>
          </select>
        </label>
        <p class="faint" style="margin: -4px 0 10px">
          {{ PRD_TYPE_META[createForm.prdType]?.hint }}
        </p>
        <label class="field">
          <span>原始输入（就写你现在能说清的那一句，剩下的靠对话问出来）</span>
          <textarea
            v-model="createForm.originalInput"
            rows="5"
            data-autofocus
            placeholder="例：任务失败之后现场留着没人收，磁盘早晚满 —— 想办法治一下"
          ></textarea>
        </label>
        <label class="field">
          <span>接续哪份 PRD（可选，填编号）</span>
          <input v-model="createForm.refPrdId" type="number" style="width: 140px"
            placeholder="如 12" />
        </label>
        <div class="row" style="justify-content: flex-end; gap: 8px">
          <button @click="showCreate = false">取消</button>
          <button
            class="primary"
            :disabled="creating || !createForm.originalInput.trim()"
            @click="submitCreate"
          >{{ creating ? '创建中…' : '创建' }}</button>
        </div>
      </template>
    </BaseDialog>
  </div>
</template>

<style scoped>
.toolbar-row { display: flex; align-items: center; padding: 8px 12px; }
.headline {
  max-width: 560px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
