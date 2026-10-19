<script setup>
import { ref, computed, onMounted, inject } from 'vue'
import { useRouter } from 'vue-router'
import { api, UnauthorizedError, PRD_TYPE_META } from '../../api'
import ChatComposer from '../../components/prd/ChatComposer.vue'

// 新建对话页（DeepSeek 首页的形态）：居中一个大输入框，选好仓库与类型，
// 把那句说不清的话敲进去回车即开聊。
//
// 「发送即跑第一轮」：原来是建完停在详情页、再手动点「跑下一轮」，理由是
// 起轮要花钱、该由人显式触发。在对话形态下「按发送」本身就是那个显式
// 动作，所以建完直接带 autorun 标记跳进会话，由会话页发起第一轮。

const router = useRouter()
const onUnauthorized = inject('onUnauthorized')
const sidebar = inject('prdSidebar', null)

const repos = ref([])
const reposLoaded = ref(false)
const repoId = ref(null)
const prdType = ref('feature')
const input = ref('')
const refPrdId = ref('')
const showRef = ref(false)
const creating = ref(false)
const error = ref('')

const TYPES = ['feature', 'defect', 'refactor']

const EXAMPLES = [
  { type: 'defect', text: '任务失败之后现场留着没人收，磁盘早晚满 —— 想办法治一下' },
  { type: 'feature', text: '想让管理员能在系统设置里调预览的资源阈值，现在写死了' },
  { type: 'refactor', text: 'runner 里的管线函数太长了，加个阶段都得改一大坨' },
]

onMounted(async () => {
  try {
    const r = await api.repos()
    repos.value = r.repos || []
    if (repos.value.length) repoId.value = repos.value[0].id
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
  } finally {
    reposLoaded.value = true
  }
})

const canSend = computed(() => !!repoId.value && !!input.value.trim())

async function send() {
  if (!canSend.value || creating.value) return
  creating.value = true
  error.value = ''
  try {
    const refId = Number.parseInt(refPrdId.value)
    const r = await api.createPrd({
      repoId: repoId.value,
      prdType: prdType.value,
      originalInput: input.value.trim(),
      refPrdId: Number.isFinite(refId) && refId > 0 ? refId : undefined,
    })
    sidebar?.refresh()
    // autorun 放 history.state 而不是 query：不进地址栏，分享/收藏的链接
    // 不会带着它再触发一轮。会话页读完即清。
    router.push({ path: `/prds/${r.id}`, state: { autorun: true } })
  } catch (e) {
    if (e instanceof UnauthorizedError) return onUnauthorized()
    error.value = e.message
    creating.value = false
  }
}

function useExample(ex) {
  input.value = ex.text
  prdType.value = ex.type
}
</script>

<template>
  <div class="new">
    <div class="hero">
      <h1>有什么说不清的需求？</h1>
      <p class="dim">
        一句话就够。智能体会复述理解、读代码、一次问你不超过三个关键问题，
        把它聊成一份可签字的 PRD —— 你逐节确认之后，再一键生成正式任务。
      </p>

      <div v-if="error" role="alert" class="error-banner">{{ error }}</div>

      <div v-if="reposLoaded && !repos.length" class="card no-repo">
        你名下还没有仓库 —— 先到 <RouterLink to="/repos">仓库配置</RouterLink> 登记一个，
        智能体需要知道读哪个仓库的代码。
      </div>

      <ChatComposer
        v-else
        v-model="input"
        :placeholder="`例：${EXAMPLES[0].text}`"
        :can-send="canSend"
        :busy="creating"
        :disabled="!repos.length"
        send-label="开始对话"
        autofocus
        @send="send"
      >
        <template #tools>
          <label class="pill">
            <span class="sr">仓库</span>
            <span aria-hidden="true">📦</span>
            <select v-model="repoId" aria-label="目标仓库">
              <option v-for="r in repos" :key="r.id" :value="r.id">{{ r.providerRepo }}</option>
            </select>
          </label>
          <div class="seg" role="radiogroup" aria-label="需求类型">
            <button
              v-for="t in TYPES"
              :key="t"
              role="radio"
              :aria-checked="prdType === t"
              :class="{ on: prdType === t }"
              :title="PRD_TYPE_META[t].hint"
              @click="prdType = t"
            >{{ PRD_TYPE_META[t].label }}</button>
          </div>
          <button v-if="!showRef" class="pill ghost" title="接续一份已有 PRD 作为上下文" @click="showRef = true">
            ＋ 接续 PRD
          </button>
          <label v-else class="pill">
            <span>接续 #</span>
            <input v-model="refPrdId" type="number" min="1" placeholder="编号" aria-label="接续的 PRD 编号" />
          </label>
        </template>
      </ChatComposer>

      <p v-if="repos.length" class="type-hint faint">{{ PRD_TYPE_META[prdType].label }}：{{ PRD_TYPE_META[prdType].hint }}</p>

      <div v-if="repos.length" class="examples">
        <button v-for="ex in EXAMPLES" :key="ex.text" class="ex" @click="useExample(ex)">
          <span class="badge idle">{{ PRD_TYPE_META[ex.type].label }}</span>
          <span>{{ ex.text }}</span>
        </button>
      </div>

      <p class="faint foot">需求已经写得清清楚楚？直接去 <RouterLink to="/issues">工单</RouterLink>，不用走规划。</p>
    </div>
  </div>
</template>

<style scoped>
.new { flex: 1; overflow-y: auto; display: flex; justify-content: center; padding: 0 16px; }
.hero { width: min(760px, 100%); padding: 12vh 0 48px; }
h1 { font-size: 28px; margin: 0 0 8px; text-align: center; letter-spacing: -0.01em; }
.hero > p.dim { text-align: center; margin: 0 auto 28px; max-width: 600px; }
.no-repo { text-align: center; }

.pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--border);
  border-radius: 999px;
  padding: 2px 4px 2px 10px;
  font-size: 12.5px;
  color: var(--text-dim);
  background: var(--surface-2);
}
.pill select, .pill input {
  border: none;
  background: transparent;
  padding: 2px 4px;
  font-size: 12.5px;
  max-width: 200px;
}
.pill input { width: 70px; }
.pill.ghost { padding: 3px 10px; }
.sr { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); }

.seg { display: inline-flex; border: 1px solid var(--border); border-radius: 999px; overflow: hidden; }
.seg button {
  border: none;
  border-radius: 0;
  background: var(--surface-2);
  padding: 3px 12px;
  font-size: 12.5px;
  color: var(--text-dim);
}
.seg button + button { border-left: 1px solid var(--border); }
.seg button.on { background: var(--run-bg); color: var(--accent); }

.type-hint { font-size: 12.5px; margin: 8px 4px 0; }

.examples { display: grid; gap: 8px; margin-top: 28px; }
.ex {
  display: flex;
  align-items: center;
  gap: 10px;
  text-align: left;
  border-radius: 12px;
  padding: 10px 14px;
  background: transparent;
  color: var(--text-dim);
}
.ex:hover:not(:disabled) { color: var(--text); background: var(--surface); }

.foot { text-align: center; margin-top: 36px; font-size: 13px; }
</style>
