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

// 登记新仓库（P1.5 第二步）：各归各的名下，不再需要管理员手工插库
const newRepo = ref('')
const newBusy = ref(false)

// 基线目录检测/部署状态，按 repo.id 存一份（不进 repos.value，
// 避免跟随 load() 的自动刷新被清空——检测结果是临时的，重新加载
// 仓库列表不代表基线状态变了）。
const baseline = ref({}) // { [repoId]: { loading, error, status } }

async function add() {
  const providerRepo = newRepo.value.trim()
  if (!providerRepo) return
  newBusy.value = true
  error.value = ''
  try {
    await api.createRepo({ providerRepo })
    newRepo.value = ''
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    newBusy.value = false
  }
}

async function load() {
  try {
    const r = await api.repos()
    // 编辑态与服务端数据分开，避免输入过程中被自动刷新覆盖
    repos.value = (r.repos || []).map((x) => ({
      ...x,
      protectedText: (x.protectedBranches || []).join(', '),
      excludeText: (x.excludeDirs || []).join(', '),
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

  const excludeDirs = (repo.excludeText || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)

  try {
    await api.updateRepo(repo.id, {
      defaultBranch: repo.defaultBranch,
      hotfixBase: repo.hotfixBase,
      protectedBranches,
      branchPattern: repo.branchPattern,
      gateMode: repo.gateMode,
      excludeDirs,
      verifyTierOverride: repo.verifyTierOverride || '',
      baselineDir: repo.baselineDir || '',
    })
    saved.value = repo.id
    setTimeout(() => (saved.value = null), 2000)
    await load()
  } catch (e) {
    error.value = e.message
  }
}

// 检测基线目录：扫描 compose 文件、问 docker compose 每个文件当前的
// 服务运行状态。人点的，不随页面加载自动触发——避免每次打开配置页
// 都触发一轮 docker 调用。
async function detectBaseline(repo) {
  baseline.value = { ...baseline.value, [repo.id]: { loading: true, error: '', status: null } }
  try {
    const status = await api.repoBaseline(repo.id)
    baseline.value = { ...baseline.value, [repo.id]: { loading: false, error: '', status } }
  } catch (e) {
    baseline.value = { ...baseline.value, [repo.id]: { loading: false, error: e.message, status: null } }
  }
}

// 部署基线：把人指定的一个 compose 文件跑起来（docker compose up -d）。
// 有数据风险的决定交给人拍板，成功后自动重新检测一次看最新状态。
async function deployBaseline(repo, composeFile) {
  const cur = baseline.value[repo.id] || {}
  baseline.value = { ...baseline.value, [repo.id]: { ...cur, deploying: composeFile } }
  try {
    await api.deployRepoBaseline(repo.id, composeFile)
    await detectBaseline(repo)
  } catch (e) {
    baseline.value = {
      ...baseline.value,
      [repo.id]: { ...cur, deploying: null, error: e.message },
    }
  }
}

onMounted(load)
</script>

<template>
  <h1>仓库配置</h1>
  <div v-if="error" class="error-banner">{{ error }}</div>

  <div class="card">
    <h2>登记仓库</h2>
    <p class="faint" style="margin: 6px 0 12px">
      把你要让 Lathe 干活的 GitHub 仓库登记到自己名下，例如 <code class="mono">Clouditera/CloudRouter</code>。
      登记后把 issue 指派给你的 Linear 账号即可触发任务。
    </p>
    <form class="row" style="display: flex; gap: 10px" @submit.prevent="add">
      <input
        v-model="newRepo"
        class="mono"
        style="flex: 1"
        placeholder="owner/repo"
        :disabled="newBusy"
      />
      <button class="primary" :disabled="newBusy || !newRepo.trim()">
        {{ newBusy ? '登记中…' : '登记' }}
      </button>
    </form>
  </div>

  <div v-if="!repos.length" class="card empty">
    你名下还没有仓库 —— 在上方登记一个，issue 才会被路由到这里。
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
        <span>验证排除目录</span>
        <input v-model="repo.excludeText" class="mono" placeholder="apps/console" />
        <small class="faint">
          逗号分隔，相对仓库根的路径或目录名。停止维护的目录在这里排除，
          其存量问题不再参与构建/lint 扫描
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

      <label class="full">
        <span>基线目录</span>
        <div class="row" style="display: flex; gap: 8px">
          <input
            v-model="repo.baselineDir"
            class="mono"
            style="flex: 1"
            placeholder="/opt/CloudRouter"
          />
          <button
            type="button"
            :disabled="!repo.baselineDir || baseline[repo.id]?.loading"
            @click="detectBaseline(repo)"
          >
            {{ baseline[repo.id]?.loading ? '检测中…' : '检测' }}
          </button>
        </div>
        <small class="faint">
          基线分支已经在本机常驻跑着开发环境时（如 <code class="mono">/opt/CloudRouter</code>
          的 <code class="mono">pnpm up</code>），登记它的目录——之后任务预览/worktree 起服务
          默认直接连它已经在跑的中间件，不必每次重新建一套。留空则维持现有行为。
        </small>
      </label>
    </div>

    <div v-if="baseline[repo.id]" class="baseline-status">
      <p v-if="baseline[repo.id].error" class="error-banner">{{ baseline[repo.id].error }}</p>
      <template v-else-if="baseline[repo.id].status">
        <p class="faint">
          分支：<span class="mono">{{ baseline[repo.id].status.branch || '未知' }}</span>
          <span v-if="baseline[repo.id].status.headMatchesDefault === false" class="badge warn">
            与默认基线分支不一致
          </span>
        </p>
        <p v-if="!baseline[repo.id].status.services?.length" class="faint">
          没有发现任何 compose 服务。
        </p>
        <ul v-else class="baseline-services">
          <li v-for="svc in baseline[repo.id].status.services" :key="svc.composeFile + svc.service">
            <span class="badge" :class="svc.running ? 'ok' : 'warn'">
              {{ svc.running ? '运行中' : '未运行' }}
            </span>
            <span class="mono">{{ svc.containerName || svc.service }}</span>
            <span v-if="svc.dbKind" class="faint">（{{ svc.dbKind }}）</span>
            <span class="faint">{{ svc.composeFile }}</span>
            <button
              v-if="!svc.running"
              type="button"
              :disabled="baseline[repo.id].deploying === svc.composeFile"
              @click="deployBaseline(repo, svc.composeFile)"
            >
              {{ baseline[repo.id].deploying === svc.composeFile ? '部署中…' : '部署' }}
            </button>
          </li>
        </ul>
      </template>
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

.baseline-status { margin: -6px 0 14px; }
.baseline-services { list-style: none; padding: 0; margin: 8px 0 0; display: flex; flex-direction: column; gap: 6px; }
.baseline-services li { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
</style>
