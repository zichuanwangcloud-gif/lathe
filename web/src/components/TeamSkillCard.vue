<script setup>
// 团队自定义技能（f11，docs/features/team-skills.md）：仓库卡片里的一个分区。
//
// 版本不可变：保存 = 写一个新版本（内容没变则不产生新版本）；恢复旧版 = 把它
// 作为新版本再提交一次。停用不是归档：停用后列表仍可见、可重新打开；归档后从
// 列表消失，名字可复用。已经钉住某个版本的任务不受停用/归档/新版本影响。
//
// 编辑器刻意只是纯文本域 + 逐文件标签（PRD §4：不做富文本/高亮/diff）。
import { ref, onMounted } from 'vue'
import { api, formatTime } from '../api'
import { confirmDialog } from '../confirm'

const props = defineProps({ repoId: { type: Number, required: true } })

const skills = ref([])
const error = ref('')
const notice = ref('')
const busy = ref(false)

// 编辑态：新建（sid=null）或某个技能写新版本 / 查看历史版本
const editing = ref(null) // { sid, slug, title, files: [{path, content}], note, active, versions, viewing }

const SKILL_TEMPLATE = `---
name: my-skill
description: 一句话说明这个技能什么时候用
---

# 团队规范

写下 agent 在这个仓库里必须遵守的约定。
`

async function load() {
  try {
    const r = await api.teamSkills(props.repoId)
    skills.value = r.skills || []
    error.value = ''
  } catch (e) {
    error.value = e.message
  }
}

function flash(msg) {
  notice.value = msg
  setTimeout(() => (notice.value = ''), 2500)
}

function startCreate() {
  editing.value = {
    sid: null, slug: '', title: '', note: '', active: 0, versions: [], viewing: null,
    files: [{ path: 'SKILL.md', content: SKILL_TEMPLATE }],
  }
}

async function startEdit(sk) {
  error.value = ''
  try {
    const [detail, latest] = await Promise.all([
      api.teamSkill(props.repoId, sk.id),
      api.teamSkillVersion(props.repoId, sk.id, sk.latestVersion),
    ])
    editing.value = {
      sid: sk.id, slug: sk.slug, title: sk.title, note: '', active: 0,
      versions: detail.versions || [], viewing: null,
      files: (latest.files || []).map((f) => ({ ...f })),
    }
  } catch (e) {
    error.value = e.message
  }
}

async function viewVersion(v) {
  try {
    const r = await api.teamSkillVersion(props.repoId, editing.value.sid, v)
    editing.value.viewing = r
  } catch (e) {
    error.value = e.message
  }
}

function addFile() {
  editing.value.files.push({ path: 'references/notes.md', content: '' })
  editing.value.active = editing.value.files.length - 1
}

function removeFile(i) {
  editing.value.files.splice(i, 1)
  editing.value.active = 0
}

async function save() {
  const ed = editing.value
  busy.value = true
  error.value = ''
  try {
    if (ed.sid == null) {
      await api.createTeamSkill(props.repoId, {
        slug: ed.slug, title: ed.title, note: ed.note, files: ed.files,
      })
      flash('已新建 v1')
    } else {
      const r = await api.appendTeamSkillVersion(props.repoId, ed.sid, { note: ed.note, files: ed.files })
      flash(r.created ? `已保存为 v${r.version}` : `内容没有变化，仍是 v${r.version}`)
      if ((ed.title || '') !== (skills.value.find((s) => s.id === ed.sid)?.title || '')) {
        await api.updateTeamSkill(props.repoId, ed.sid, { title: ed.title })
      }
    }
    editing.value = null
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function restore(v) {
  busy.value = true
  try {
    const r = await api.restoreTeamSkillVersion(props.repoId, editing.value.sid, v)
    flash(r.created ? `已把 v${v} 恢复为 v${r.version}` : `内容与当前版本相同，仍是 v${r.version}`)
    editing.value = null
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function toggle(sk) {
  try {
    await api.updateTeamSkill(props.repoId, sk.id, { enabled: !sk.enabled })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function archive(sk) {
  if (!(await confirmDialog(`归档技能「${sk.slug}」？归档后新任务不再加载它；已经钉住它的任务不受影响。`))) return
  try {
    await api.archiveTeamSkill(props.repoId, sk.id)
    await load()
  } catch (e) {
    error.value = e.message
  }
}

onMounted(load)
</script>

<template>
  <section class="team-skills">
    <div class="spread">
      <h3>团队技能</h3>
      <span v-if="notice" class="badge ok">{{ notice }}</span>
    </div>
    <p class="faint hint">
      为这个仓库登记团队自己的技能（<code class="mono">SKILL.md</code> + 少量附属文件）。任务进入实现阶段时，
      启用的技能会物化到工作区的 <code class="mono">.claude/skills/team-&lt;名字&gt;/</code>，
      与 Lathe 内置技能分开、不进 PR；物化时把 <code class="mono">name:</code> 改写为
      <code class="mono">team-&lt;名字&gt;</code>。每个任务第一次物化时钉住版本，重跑用同一版。
      不要在技能里放凭据，保存时会被拦下。
    </p>
    <p v-if="error" role="alert" class="error-banner">{{ error }}</p>

    <ul v-if="skills.length" class="skill-list">
      <li v-for="sk in skills" :key="sk.id">
        <span class="mono">team-{{ sk.slug }}</span>
        <span v-if="sk.title" class="dim">{{ sk.title }}</span>
        <span class="badge">v{{ sk.latestVersion }}</span>
        <span class="badge" :class="sk.enabled ? 'ok' : 'warn'">{{ sk.enabled ? '启用' : '已停用' }}</span>
        <span class="faint" title="近 30 天本仓库钉住它的任务数">近 30 天 {{ sk.pinnedTaskCount }} 个任务</span>
        <span class="actions">
          <button type="button" @click="startEdit(sk)">编辑 / 历史</button>
          <button type="button" @click="toggle(sk)">{{ sk.enabled ? '停用' : '启用' }}</button>
          <button type="button" @click="archive(sk)">归档</button>
        </span>
      </li>
    </ul>
    <p v-else class="faint">还没有团队技能。</p>

    <button v-if="!editing" type="button" @click="startCreate">新建团队技能</button>

    <div v-if="editing" class="editor">
      <div class="fields">
        <label v-if="editing.sid == null">
          <span>名字</span>
          <input v-model="editing.slug" class="mono" placeholder="acme-testing" />
          <small class="faint">小写字母、数字、单个连字符；不能以 team- 开头，也不能与内置技能同名</small>
        </label>
        <label v-else>
          <span>名字</span>
          <input :value="editing.slug" class="mono" disabled />
          <small class="faint">名字不能改；要换名字请新建一个</small>
        </label>
        <label>
          <span>展示名（可选）</span>
          <input v-model="editing.title" maxlength="100" />
        </label>
        <label class="full">
          <span>本次改动说明（可选）</span>
          <input v-model="editing.note" maxlength="200" />
        </label>
      </div>

      <div class="tabs">
        <button
          v-for="(f, i) in editing.files"
          :key="i"
          type="button"
          :class="{ on: editing.active === i }"
          @click="editing.active = i"
        >{{ f.path || '（未命名）' }}</button>
        <button type="button" @click="addFile">+ 文件</button>
      </div>
      <div v-if="editing.files[editing.active]" class="file">
        <div class="row">
          <input v-model="editing.files[editing.active].path" class="mono" style="flex: 1" />
          <button
            v-if="editing.files[editing.active].path !== 'SKILL.md'"
            type="button"
            @click="removeFile(editing.active)"
          >删除此文件</button>
        </div>
        <textarea v-model="editing.files[editing.active].content" class="mono" rows="16"></textarea>
      </div>

      <div class="row">
        <button class="primary" type="button" :disabled="busy" @click="save">
          {{ busy ? '保存中…' : editing.sid == null ? '新建（v1）' : '保存为新版本' }}
        </button>
        <button type="button" @click="editing = null">取消</button>
      </div>

      <div v-if="editing.versions.length" class="versions">
        <h4>历史版本</h4>
        <ul>
          <li v-for="v in editing.versions" :key="v.version">
            <span class="badge">v{{ v.version }}</span>
            <span class="faint">{{ formatTime(v.createdAt) }}</span>
            <span class="faint">{{ v.totalBytes }} 字节</span>
            <span v-if="v.note" class="dim">{{ v.note }}</span>
            <button type="button" @click="viewVersion(v.version)">查看</button>
            <button type="button" :disabled="busy" @click="restore(v.version)">恢复为新版本</button>
          </li>
        </ul>
        <div v-if="editing.viewing" class="viewing">
          <div class="faint">v{{ editing.viewing.version }} 的内容（原样，未改写 name）</div>
          <div v-for="f in editing.viewing.files" :key="f.path">
            <div class="mono dim">{{ f.path }}</div>
            <pre class="mono">{{ f.content }}</pre>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.team-skills { border-top: 1px solid var(--border); margin: 6px 0 16px; padding-top: 14px; }
h3 { font-size: 14px; margin: 0; }
h4 { font-size: 13px; margin: 12px 0 6px; }
.hint { font-size: 12px; margin: 6px 0 10px; }
.skill-list { list-style: none; padding: 0; margin: 0 0 10px; display: flex; flex-direction: column; gap: 6px; }
.skill-list li { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.actions { margin-left: auto; display: flex; gap: 6px; flex-wrap: wrap; }
.editor { margin-top: 10px; display: flex; flex-direction: column; gap: 10px; }
.fields { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; }
label { display: flex; flex-direction: column; gap: 5px; }
label > span { font-size: 13px; color: var(--text-dim); }
label small { font-size: 12px; }
.full { grid-column: 1 / -1; }
.tabs { display: flex; gap: 6px; flex-wrap: wrap; }
.tabs button.on { border-color: var(--accent-strong); }
.file { display: flex; flex-direction: column; gap: 6px; }
.row { display: flex; gap: 8px; flex-wrap: wrap; }
textarea { width: 100%; box-sizing: border-box; min-height: 240px; }
.versions ul { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 6px; }
.versions li { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.viewing pre { white-space: pre-wrap; word-break: break-word; max-height: 320px; overflow: auto; margin: 4px 0 10px; }
</style>
