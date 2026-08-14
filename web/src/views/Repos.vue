<script setup>
import { ref, onMounted, inject } from 'vue'
import { api, UnauthorizedError } from '../api'

const repos = ref([])
const error = ref('')
const saved = ref(null)
const onUnauthorized = inject('onUnauthorized')

const GATE_MODES = [
  { value: 'direct', label: 'direct — 直接干', desc: '接单即实现，不分诊不询问。单子质量稳定时用。' },
  { value: 'guarded', label: 'guarded — 兜底询问', desc: '默认直接干，仅在分诊置信度低时回帖提问并暂停。' },
  { value: 'plan-first', label: 'plan-first — 计划先行', desc: '先回帖实施计划，静默观察期无异议后自动执行。' },
  { value: 'manual', label: 'manual — 逐单确认', desc: '每单都要人工点确认才动手。' },
]

const VERIFY_TIERS = [
  { value: '', label: '自动 — 按改动面判定', desc: 'diff 只碰前端展示层/文案 → light；碰到后端、migration、计费或跨前后端 → heavy。' },
  { value: 'light', label: '强制 light', desc: '只做构建 + lint + 类型检查。纯展示层仓库（如官网）适用。' },
  { value: 'heavy', label: '强制 heavy', desc: '必须给出红-绿复现证明：复现测试在改动前失败、改动后通过，回归通过。' },
]

async function load() {
  try {
    const r = await api.repos()
    // 编辑态与服务端数据分开，避免输入过程中被自动刷新覆盖
    repos.value = (r.repos || []).map((x) => ({
      ...x,
      protectedText: (x.protectedBranches || []).join(', '),
    }))
    error.value = ''
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  }
}

async function save(repo) {
  error.value = ''
  saved.value = null

  const protectedBranches = repo.protectedText
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)

  if (!protectedBranches.length) {
    error.value = '受保护分支不能为空 —— 这是禁止直接推送的最后一道闸门'
    return
  }

  try {
    await api.updateRepo(repo.id, {
      defaultBranch: repo.defaultBranch,
      hotfixBase: repo.hotfixBase,
      protectedBranches,
      branchPattern: repo.branchPattern,
      gateMode: repo.gateMode,
      verifyTierOverride: repo.verifyTierOverride || '',
    })
    saved.value = repo.id
    setTimeout(() => (saved.value = null), 2000)
    await load()
  } catch (e) {
    error.value = e.message
  }
}

onMounted(load)
</script>

<template>
  <h1>仓库配置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <div v-if="!repos.length" class="card empty">
    尚未配置仓库。P0 需要先往 repos 表插一条记录：
    <pre style="margin-top: 12px; text-align: left">INSERT INTO repos (user_id, provider_repo)
VALUES (1, 'Clouditera/CloudRouter');</pre>
  </div>

  <div v-for="repo in repos" :key="repo.id" class="card repo">
    <div class="spread">
      <h2 class="mono">{{ repo.providerRepo }}</h2>
      <span v-if="saved === repo.id" class="badge ok">已保存</span>
    </div>

    <div class="fields">
      <label>
        <span>默认基线分支</span>
        <input v-model="repo.defaultBranch" />
        <small class="faint">fix / feature 从这条分支分叉</small>
      </label>

      <label>
        <span>hotfix 基线分支</span>
        <input v-model="repo.hotfixBase" />
        <small class="faint">hotfix 从这条分支分叉</small>
      </label>

      <label>
        <span>分支命名模式</span>
        <input v-model="repo.branchPattern" class="mono" />
        <small class="faint">占位符：{kind} {key} {slug}</small>
      </label>

      <label>
        <span>受保护分支</span>
        <input v-model="repo.protectedText" class="mono" />
        <small class="faint">逗号分隔。Lathe 永不直接推送这些分支</small>
      </label>

      <label class="full">
        <span>准入档位</span>
        <select v-model="repo.gateMode">
          <option v-for="m in GATE_MODES" :key="m.value" :value="m.value">{{ m.label }}</option>
        </select>
        <small class="faint">
          {{ GATE_MODES.find((m) => m.value === repo.gateMode)?.desc }}
        </small>
      </label>

      <label class="full">
        <span>验证档位</span>
        <select v-model="repo.verifyTierOverride">
          <option v-for="t in VERIFY_TIERS" :key="t.value" :value="t.value">{{ t.label }}</option>
        </select>
        <small class="faint">
          {{ VERIFY_TIERS.find((t) => t.value === (repo.verifyTierOverride || ''))?.desc }}
        </small>
      </label>
    </div>

    <button class="primary" @click="save(repo)">保存</button>
  </div>
</template>

<style scoped>
h1 { font-size: 22px; margin: 0 0 16px; }
h2 { font-size: 15px; margin: 0; }

.repo { margin-bottom: 16px; }

.fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 14px;
  margin: 16px 0;
}
label { display: flex; flex-direction: column; gap: 5px; }
label > span { font-size: 13px; color: var(--text-dim); }
label small { font-size: 12px; }
.full { grid-column: 1 / -1; }
</style>
