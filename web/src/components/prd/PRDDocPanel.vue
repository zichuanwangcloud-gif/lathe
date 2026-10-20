<script setup>
import { ref, computed, watch } from 'vue'
import {
  SECTIONS,
  SECTION_STATUS,
  AC_CATEGORY,
  VERIFY_LABEL,
  QUESTION_LABEL,
  TASK_KIND_LABEL,
  FINDING_KIND,
  DISPOSITIONS,
} from '../../prd'
import ThinkingBlock from './ThinkingBlock.vue'

// 右侧 PRD 面板（豆包「文档画布」的位置）：对话在左边推进，文档在这里
// 逐节成形。逐节确认、复核处置这两类「人的签字」都落在这里 —— 它们是
// 对内容的判断，挨着内容做才不会签错节。
//
// 面板只管展示与收集输入，所有写请求都 emit 给会话页去发：状态机开关、
// 错误提示、重读 PRD 都在那边统一处理。

const props = defineProps({
  prd: { type: Object, required: true },
  caps: { type: Object, required: true },
  md: { type: Function, required: true },
  acting: { type: Boolean, default: false },
  // 自检不过时后端回的整份报告（阻塞项可点跳节）
  checkReport: { type: Object, default: null },
  reviewEvents: { type: Array, default: () => [] },
  reviewing: { type: Boolean, default: false },
  tab: { type: String, default: 'doc' },
})
const emit = defineEmits([
  'update:tab',
  'close',
  'confirm-section',
  'save-dispositions',
  'answer-reverse',
  'dismiss-report',
])

const doc = computed(() => props.prd.document || {})
const tasks = computed(() => props.prd.taskBlocks?.tasks || [])
const review = computed(() => props.prd.reviewReport || null)
const findings = computed(() => review.value?.findings || [])

// 挂着反向确认但人还没答复的 AC：定稿自检要求它们全部有答复。
const unansweredReverse = computed(() =>
  (doc.value.criteria || []).filter((c) => c.reverseConfirm && c.reverseAccepted == null),
)
// 「全部接受」一次提交：逐条点 20 次是另一种形式的问题轰炸。
function acceptAllReverse() {
  const answers = {}
  for (const c of unansweredReverse.value) answers[c.id] = true
  emit('answer-reverse', answers)
}

const sectionsView = computed(() =>
  SECTIONS.map((s) => ({ ...s, body: doc.value[s.key] || { status: '', body: '' } })),
)
const counts = computed(() => {
  const c = { confirmed: 0, inferred: 0, pending: 0 }
  for (const s of sectionsView.value) if (c[s.body.status] != null) c[s.body.status]++
  return c
})

// ---------------------------------------------------------------- 跳节

const highlighted = ref('')
let highlightTimer = null
function gotoSection(key) {
  if (!key) return
  emit('update:tab', 'doc')
  // 切页签后 DOM 下一帧才有
  requestAnimationFrame(() => {
    const el = document.getElementById(`sec-${key}`)
    if (!el) return
    el.scrollIntoView({ behavior: 'smooth', block: 'start' })
    highlighted.value = key
    clearTimeout(highlightTimer)
    highlightTimer = setTimeout(() => (highlighted.value = ''), 2400)
  })
}
defineExpose({ gotoSection })

// ---------------------------------------------------------------- 复核处置
// 处置是人的责任，复核 agent 刻意不填（internal/prd/service.go RunReview）。
// 本地编辑态，保存之后才落库；轮询刷新不覆盖没保存的输入。发现没有 id：
// 报告整体覆盖写，下标在一份报告内稳定，下标既是本地 key 也是定位方式。

const dispEdits = ref({})
function seed() {
  const next = {}
  findings.value.forEach((f, i) => {
    next[i] = dispEdits.value[i] || { disposition: f.disposition || '', note: f.dispositionNote || '' }
  })
  dispEdits.value = next
}
watch(findings, seed, { immediate: true })
// 报告被重跑覆盖后，旧下标的编辑态全部作废
watch(
  () => review.value?.rawText,
  () => {
    dispEdits.value = {}
    seed()
  },
)

const unhandledCount = computed(
  () => findings.value.filter((f, i) => !dispEdits.value[i]?.disposition).length,
)
// 「认可风险不改」必须写理由：不写理由的风险接受，等于没人为它负责。
const acceptNeedsNote = computed(() =>
  findings.value.some((f, i) => {
    const d = dispEdits.value[i]
    return d?.disposition === 'accept' && !d?.note?.trim()
  }),
)
const dirty = computed(() =>
  findings.value.some((f, i) => {
    const d = dispEdits.value[i]
    return (d?.disposition || '') !== (f.disposition || '') || (d?.note || '') !== (f.dispositionNote || '')
  }),
)

function saveDispositions() {
  emit(
    'save-dispositions',
    findings.value.map((f, i) => ({
      index: i,
      disposition: dispEdits.value[i]?.disposition || '',
      dispositionNote: dispEdits.value[i]?.note || '',
    })),
  )
}
</script>

<template>
  <aside class="panel" aria-label="PRD 文档">
    <header class="p-head">
      <div class="tabs" role="tablist">
        <button role="tab" :aria-selected="tab === 'doc'" :class="{ on: tab === 'doc' }"
          @click="emit('update:tab', 'doc')">文档</button>
        <button role="tab" :aria-selected="tab === 'tasks'" :class="{ on: tab === 'tasks' }"
          @click="emit('update:tab', 'tasks')">
          任务<span v-if="tasks.length" class="count">{{ tasks.length }}</span>
        </button>
        <button role="tab" :aria-selected="tab === 'review'" :class="{ on: tab === 'review' }"
          @click="emit('update:tab', 'review')">
          复核<span v-if="unhandledCount" class="count warn">{{ unhandledCount }}</span>
        </button>
      </div>
      <button class="icon" aria-label="收起文档面板" title="收起" @click="emit('close')">✕</button>
    </header>

    <div class="p-body">
      <!-- ============================================================ 文档 -->
      <template v-if="tab === 'doc'">
        <div class="legend">
          <span class="badge ok">✅ {{ counts.confirmed }}</span>
          <span class="badge warn">🤖 {{ counts.inferred }}</span>
          <span class="badge bad">❓ {{ counts.pending }}</span>
          <span class="faint">有 ❓ 或 🤖 都进不了评审，需逐节过目确认</span>
        </div>

        <div v-if="checkReport" class="report">
          <div class="spread">
            <strong>{{ checkReport.blocking?.length ? '自检未过 —— 这些拦着不让进评审' : '自检提醒' }}</strong>
            <button class="link" @click="emit('dismiss-report')">收起</button>
          </div>
          <ul>
            <li v-for="(f, i) in checkReport.blocking || []" :key="'b' + i">
              <span class="badge bad">阻塞</span>
              <span>{{ f.message }}</span>
              <button v-if="f.section" class="link" @click="gotoSection(f.section)">跳到该节</button>
            </li>
            <li v-for="(f, i) in checkReport.warnings || []" :key="'w' + i">
              <span class="badge warn">提醒</span>
              <span>{{ f.message }}</span>
              <button v-if="f.section" class="link" @click="gotoSection(f.section)">跳到该节</button>
            </li>
          </ul>
        </div>

        <section
          v-for="s in sectionsView"
          :key="s.key"
          :id="`sec-${s.key}`"
          class="sec"
          :class="{ flash: highlighted === s.key, empty: !s.body.body }"
        >
          <div class="sec-head">
            <span class="sec-mark" :title="SECTION_STATUS[s.body.status]?.label || '未开始'">
              {{ SECTION_STATUS[s.body.status]?.mark || '○' }}
            </span>
            <strong>{{ s.title }}</strong>
            <button
              v-if="['inferred', 'pending'].includes(s.body.status) && caps.confirmSection"
              class="confirm"
              :disabled="acting || s.body.status === 'pending'"
              :title="s.body.status === 'pending'
                ? '这一节还挂着未决问题（❓）：先在对话里把问题答掉，才能确认'
                : '看过这一节的内容，把 🤖 改成 ✅'"
              @click="emit('confirm-section', s.key)"
            >确认本节</button>
          </div>

          <div v-if="s.body.body" class="md" v-html="md(s.body.body)"></div>
          <p v-else class="faint none">还没有内容</p>

          <ul v-if="s.body.questions?.length" class="sec-q">
            <li v-for="(q, i) in s.body.questions" :key="i">❓ {{ q }}</li>
          </ul>

          <!-- §3：目标必须带达成判据，非目标必须带理由 -->
          <template v-if="s.key === 'goals'">
            <div v-for="g in doc.goalList || []" :key="g.id" class="item">
              <span class="mono tag">{{ g.id }}</span>
              <div>{{ g.text }}<div class="faint">判据：{{ g.measure }}</div></div>
            </div>
            <div v-for="(n, i) in doc.nonGoals || []" :key="'n' + i" class="item">
              <span class="badge idle">非目标</span>
              <div>{{ n.text }}<div class="faint">{{ n.reason }}</div></div>
            </div>
          </template>

          <!-- §4：三种变体共用 Given/When/Then 形状 -->
          <template v-if="s.key === 'scenarios'">
            <div v-for="sc in doc.scenarioList || []" :key="sc.id" class="item">
              <span class="mono tag">{{ sc.id }}</span>
              <div>
                <strong>{{ sc.title }}</strong>
                <div class="gwt">
                  <div><em>Given</em> {{ sc.given }}</div>
                  <div><em>When</em> {{ sc.when }}</div>
                  <div><em>Then</em> {{ sc.then }}</div>
                </div>
                <div v-if="sc.existingTest" class="faint mono">现有测试：{{ sc.existingTest }}</div>
              </div>
            </div>
          </template>

          <!-- §7：AC 卡片 + 系统附加的上线条件 -->
          <template v-if="s.key === 'acceptance'">
            <div v-if="caps.reverse && unansweredReverse.length" class="bulk">
              <span class="faint">{{ unansweredReverse.length }} 条反向确认未答复</span>
              <button class="link" :disabled="acting" @click="acceptAllReverse">
                全部接受
              </button>
              <span class="faint tiny">接受 = 认可「这条 AC 放过的那些情况」</span>
            </div>
            <div v-for="c in doc.criteria || []" :key="c.id" class="item ac">
              <span class="mono tag">{{ c.id }}</span>
              <div>
                <div class="ac-meta">
                  <span class="badge idle">{{ AC_CATEGORY[c.category] || c.category }}</span>
                  <span v-if="!c.na" class="faint">{{ VERIFY_LABEL[c.verify] || c.verify || '—' }}</span>
                  <span class="faint mono">
                    {{ [...(c.goalRefs || []), ...(c.scenarioRefs || [])].join(' ') }}
                    <template v-if="c.taskKey">→ {{ c.taskKey }}</template>
                  </span>
                </div>
                <template v-if="c.na">
                  <span class="badge idle">N/A</span> <span class="dim">{{ c.reason }}</span>
                </template>
                <template v-else>
                  <div class="gwt">
                    <div><em>Given</em> {{ c.given }}</div>
                    <div><em>When</em> {{ c.when }}</div>
                    <div><em>Then</em> {{ c.then }}</div>
                  </div>
                  <div v-if="c.evidence" class="faint">判据：{{ c.evidence }}</div>
                  <div v-if="c.manualSteps" class="faint">走查：{{ c.manualSteps }}</div>
                  <div v-if="c.reverseConfirm" class="faint rev">
                    反向确认：{{ c.reverseConfirm }}
                    <span class="badge" :class="c.reverseAccepted == null ? 'bad' : 'ok'">
                      {{ c.reverseAccepted == null ? '未答复' : c.reverseAccepted ? '接受' : '不接受' }}
                    </span>
                    <!-- 人在这里答复，而不是让智能体把每条反向确认都当对话问题问
                         （那是问题轰炸的一大来源）。定稿自检要求每条都有答复。 -->
                    <template v-if="caps.reverse">
                      <button class="link" :disabled="acting"
                        @click="emit('answer-reverse', { [c.id]: true })">接受</button>
                      <button class="link" :disabled="acting"
                        @click="emit('answer-reverse', { [c.id]: false })">不接受</button>
                    </template>
                  </div>
                </template>
              </div>
            </div>
            <div v-if="doc.dod?.length" class="dod">
              <div class="faint">上线条件（系统附加，智能体不编写）</div>
              <ul><li v-for="(d, i) in doc.dod" :key="i">{{ d }}</li></ul>
            </div>
          </template>

          <!-- §9：未决问题 Q-n -->
          <template v-if="s.key === 'risks'">
            <div v-for="q in doc.questions || []" :key="q.id" class="item">
              <span class="mono tag">{{ q.id }}</span>
              <div>
                {{ q.text }}
                <span class="badge" :class="q.status === 'open' ? 'bad' : 'ok'">
                  {{ QUESTION_LABEL[q.status] || q.status }}
                </span>
                <div v-if="q.answer" class="faint">答：{{ q.answer }}</div>
              </div>
            </div>
          </template>

          <!-- §10：决策记录 -->
          <template v-if="s.key === 'decisionLogSect'">
            <div v-for="(d, i) in doc.decisions || []" :key="i" class="item">
              <span class="faint tag">R{{ d.round }}</span>
              <div>
                <div class="dim">{{ d.question }}</div>
                <div>→ {{ d.decision }}</div>
                <div v-if="d.reason" class="faint">{{ d.reason }}</div>
              </div>
            </div>
          </template>
        </section>

        <!-- 附录 B：代码证据索引。「感觉应该做」不是证据 -->
        <section v-if="doc.evidences?.length" class="sec">
          <div class="sec-head"><strong>附录 B · 代码证据</strong></div>
          <ul class="evid">
            <li v-for="(e, i) in doc.evidences" :key="i">
              <code class="mono">{{ e.ref }}</code>
              <span v-if="e.note" class="dim"> —— {{ e.note }}</span>
            </li>
          </ul>
        </section>
      </template>

      <!-- ============================================================ 任务 -->
      <template v-else-if="tab === 'tasks'">
        <p class="faint intro">
          §8 任务块就是「一键生成」的图节点：每个任务单独过分诊、单独出 PR，
          依赖按 depends_on 连成编排图。
        </p>
        <div v-if="!tasks.length" class="blank">还没拆分 —— 对话走到「拆分」阶段智能体才会产出任务表。</div>
        <div v-for="t in tasks" :key="t.key" class="task">
          <div class="task-head">
            <span class="mono tag">{{ t.key }}</span>
            <strong>{{ t.title }}</strong>
          </div>
          <div class="task-meta">
            <span class="badge idle">{{ TASK_KIND_LABEL[t.kind] || t.kind }}</span>
            <span class="faint">{{ t.dependsOn ? `依赖 ${t.dependsOn}` : '根节点' }}</span>
            <span class="faint">≈ {{ t.estimate?.lines ?? '—' }} 行 / {{ t.estimate?.files ?? '—' }} 文件</span>
            <span v-if="t.verifyHint" class="faint">验证 {{ t.verifyHint }}</span>
            <span v-if="t.acceptance?.length" class="faint mono">{{ t.acceptance.join(' ') }}</span>
          </div>
          <details v-if="t.description">
            <summary class="faint">展开描述</summary>
            <div class="md" v-html="md(t.description)"></div>
            <p v-if="t.filesHint?.length" class="faint mono">参考文件：{{ t.filesHint.join('、') }}</p>
          </details>
        </div>
      </template>

      <!-- ============================================================ 复核 -->
      <template v-else>
        <p class="faint intro">
          对抗复核是验收标准自己的「红阶段」：一个只看意图与 AC（看不到方案）的智能体，
          试着构造「过了全部 AC 却违背意图」的实现。不跑就进不了评审。
        </p>

        <ThinkingBlock :events="reviewEvents" :running="reviewing" label="复核" :md="md" />

        <div v-if="!review && !reviewing" class="blank">还没跑过。在对话下方点「跑对抗复核」。</div>

        <template v-if="review">
          <p v-if="review.verdict" class="verdict">{{ review.verdict }}</p>
          <p v-if="!findings.length" class="dim">复核没找出洞 —— 「攻不动」本身是个有价值的结论。</p>

          <div v-for="(f, i) in findings" :key="i" class="finding">
            <div class="row" style="gap: 8px; flex-wrap: wrap">
              <span class="badge" :class="FINDING_KIND[f.kind]?.tone || 'warn'">
                {{ FINDING_KIND[f.kind]?.label || f.kind }}
              </span>
              <strong>{{ f.title }}</strong>
            </div>
            <div v-if="f.detail" class="md" v-html="md(f.detail)"></div>
            <div v-if="f.acRefs?.length || f.scenarioRefs?.length" class="faint mono">
              关联：{{ [...(f.acRefs || []), ...(f.scenarioRefs || [])].join(' ') }}
            </div>
            <div v-if="dispEdits[i]" class="disp" role="radiogroup" :aria-label="`发现 ${i + 1} 的处置`">
              <label v-for="d in DISPOSITIONS" :key="d.value" class="chip"
                :class="{ on: dispEdits[i].disposition === d.value }">
                <input
                  type="radio"
                  :name="`disp-${i}`"
                  :value="d.value"
                  :disabled="caps.frozen || prd.processing"
                  v-model="dispEdits[i].disposition"
                />
                {{ d.label }}
              </label>
            </div>
            <input
              v-if="dispEdits[i]?.disposition"
              v-model="dispEdits[i].note"
              class="disp-note"
              :disabled="caps.frozen || prd.processing"
              :aria-label="`发现 ${i + 1} 的处置理由`"
              :placeholder="dispEdits[i].disposition === 'accept'
                ? '认可风险必须写理由：为什么这个洞可以不补'
                : '理由（可选）'"
            />
          </div>

          <div v-if="findings.length && !caps.frozen" class="save-row">
            <span v-if="acceptNeedsNote" class="faint">「认可风险不改」必须写理由才能保存。</span>
            <span v-else-if="unhandledCount" class="faint">还有 {{ unhandledCount }} 条没标处置。</span>
            <button
              class="primary"
              :disabled="acting || prd.processing || acceptNeedsNote || !dirty"
              @click="saveDispositions"
            >保存处置</button>
          </div>

          <details v-if="review.rawText" class="raw">
            <summary class="faint">复核原文（附录 C 原样留档）</summary>
            <pre>{{ review.rawText }}</pre>
          </details>
        </template>
      </template>
    </div>
  </aside>
</template>

<style scoped>
.panel {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: var(--surface);
  border-left: 1px solid var(--border);
}
.p-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 10px 12px 0 16px;
  border-bottom: 1px solid var(--border);
}
.tabs { display: flex; gap: 2px; }
.tabs button {
  border: none;
  background: none;
  border-radius: 0;
  border-bottom: 2px solid transparent;
  padding: 8px 12px;
  color: var(--text-dim);
}
.tabs button.on { color: var(--text); border-bottom-color: var(--accent); }
.tabs button:hover:not(:disabled) { color: var(--text); border-color: transparent; border-bottom-color: var(--border); }
.tabs button.on:hover:not(:disabled) { border-bottom-color: var(--accent); }
.count {
  margin-left: 5px;
  font-size: 11px;
  padding: 0 6px;
  border-radius: 999px;
  background: var(--idle-bg);
  color: var(--text-dim);
}
.count.warn { background: var(--warn-bg); color: var(--warn); }
.icon { border: none; background: none; padding: 4px 8px; color: var(--text-faint); margin-bottom: 6px; }
.icon:hover:not(:disabled) { color: var(--text); }

.p-body { flex: 1; overflow-y: auto; padding: 14px 16px 40px; }
.intro { margin: 0 0 12px; font-size: 13px; }
.blank { padding: 28px 8px; text-align: center; color: var(--text-faint); }

.legend { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-bottom: 12px; font-size: 12.5px; }

.report {
  border: 1px solid var(--bad);
  background: var(--bad-bg);
  border-radius: var(--radius);
  padding: 10px 12px;
  margin-bottom: 14px;
  font-size: 13px;
}
.report ul { list-style: none; margin: 8px 0 0; padding: 0; }
.report li { display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap; padding: 4px 0; }
.link { border: none; background: none; padding: 0 2px; color: var(--accent); font-size: 13px; }
.link:hover:not(:disabled) { text-decoration: underline; }
.link:disabled { opacity: .45; }
.bulk {
  display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
  margin: 0 0 8px; padding: 6px 10px;
  border: 1px solid var(--border); border-radius: 8px; background: var(--surface-2);
}
.rev { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }

.sec {
  padding: 12px 0 14px;
  border-bottom: 1px solid var(--border);
  scroll-margin-top: 8px;
  transition: background .3s;
}
.sec.flash { background: var(--run-bg); border-radius: 6px; }
.sec.empty .sec-head strong { color: var(--text-faint); font-weight: 500; }
.sec-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
.sec-mark { width: 20px; text-align: center; flex: none; }
.confirm { margin-left: auto; padding: 2px 10px; font-size: 12.5px; }
.none { margin: 2px 0 0 28px; font-size: 12.5px; }
.sec > .md, .sec > .item, .sec > .sec-q, .sec > .dod, .sec > .evid { margin-left: 28px; }

.sec-q { margin-top: 6px; padding-left: 0; list-style: none; color: var(--warn); font-size: 13px; }

.item { display: flex; gap: 10px; padding: 6px 0; font-size: 13px; border-top: 1px dashed var(--border); }
.item:first-of-type { border-top: none; }
.tag { flex: none; min-width: 38px; color: var(--text-dim); }
.gwt { color: var(--text-dim); margin: 2px 0; }
.gwt em { font-style: normal; color: var(--text-faint); font-size: 12px; margin-right: 4px; }
.ac-meta { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; margin-bottom: 2px; }
.dod { margin-top: 8px; font-size: 13px; }
.dod ul, .evid { margin: 4px 0 0; padding-left: 18px; font-size: 13px; }

.task {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  margin-bottom: 10px;
  font-size: 13px;
}
.task-head { display: flex; gap: 8px; align-items: baseline; }
.task-meta { display: flex; gap: 10px; flex-wrap: wrap; align-items: center; margin: 6px 0 2px; }
.task details summary { cursor: pointer; font-size: 12.5px; }

.verdict { padding: 8px 10px; background: var(--surface-2); border-radius: 6px; margin: 8px 0; }
.finding { border-top: 1px solid var(--border); padding: 12px 0; font-size: 13.5px; }
.disp { display: flex; gap: 6px; flex-wrap: wrap; margin-top: 8px; }
.chip {
  display: inline-flex;
  align-items: center;
  border: 1px solid var(--border);
  border-radius: 999px;
  padding: 3px 12px;
  font-size: 12.5px;
  cursor: pointer;
  color: var(--text-dim);
}
.chip input { position: absolute; opacity: 0; pointer-events: none; }
.chip:focus-within { outline: 2px solid var(--accent); outline-offset: 1px; }
.chip.on { border-color: var(--accent); color: var(--accent); background: var(--run-bg); }
.disp-note { width: 100%; margin-top: 8px; }
.save-row { display: flex; align-items: center; justify-content: flex-end; gap: 10px; margin-top: 10px; }
.save-row .faint { margin-right: auto; font-size: 12.5px; }
.raw { margin-top: 14px; }
.raw summary { cursor: pointer; font-size: 12.5px; }
.raw pre { margin-top: 6px; max-height: 320px; overflow-y: auto; }

.md { font-size: 13.5px; }
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) { margin: 10px 0 4px; font-size: 14px; }
.md :deep(p) { margin: 4px 0; }
.md :deep(ul), .md :deep(ol) { margin: 4px 0; padding-left: 20px; }
.md :deep(table) { width: 100%; border-collapse: collapse; margin: 6px 0; font-size: 12.5px; display: block; overflow-x: auto; }
.md :deep(th), .md :deep(td) { border: 1px solid var(--border); padding: 4px 6px; text-align: left; }
.md :deep(code) {
  font-family: var(--mono); font-size: 12px;
  background: var(--bg); border: 1px solid var(--border);
  border-radius: 4px; padding: 0 4px;
}
.md :deep(pre code) { background: none; border: none; padding: 0; }
.md :deep(a) { word-break: break-all; }
</style>
