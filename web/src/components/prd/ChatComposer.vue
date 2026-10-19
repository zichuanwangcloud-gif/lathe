<script setup>
import { ref, watch, nextTick, onMounted } from 'vue'

// 对话输入框：豆包 / DeepSeek 式的圆角大框，Enter 发送、Shift+Enter 换行、
// 高度随内容自增长。新建页与会话页共用。
//
// 输入法组字期间（中文拼音还在候选态）按 Enter 是选词不是发送 ——
// 不判 isComposing 的话，中文用户每敲一个词就误发一次。

const props = defineProps({
  modelValue: { type: String, default: '' },
  placeholder: { type: String, default: '' },
  disabled: { type: Boolean, default: false },
  // 发送按钮可用与否由父组件判（比如 awaiting_answers 必须有内容）
  canSend: { type: Boolean, default: true },
  busy: { type: Boolean, default: false },
  sendLabel: { type: String, default: '发送' },
  autofocus: { type: Boolean, default: false },
  maxRows: { type: Number, default: 10 },
})
const emit = defineEmits(['update:modelValue', 'send'])

const ta = ref(null)

function autosize() {
  const el = ta.value
  if (!el) return
  el.style.height = 'auto'
  const line = parseFloat(getComputedStyle(el).lineHeight) || 22
  el.style.height = Math.min(el.scrollHeight, line * props.maxRows + 4) + 'px'
}

watch(() => props.modelValue, () => nextTick(autosize))
onMounted(() => {
  autosize()
  if (props.autofocus) ta.value?.focus()
})

function onKeydown(e) {
  if (e.key !== 'Enter' || e.shiftKey || e.isComposing || e.keyCode === 229) return
  e.preventDefault()
  send()
}

function send() {
  if (props.disabled || props.busy || !props.canSend) return
  emit('send')
}

// 父组件「引用问题」后要把焦点放回输入框并移到末尾
function focusEnd() {
  const el = ta.value
  if (!el) return
  el.focus()
  el.setSelectionRange(el.value.length, el.value.length)
}
defineExpose({ focusEnd })
</script>

<template>
  <div class="composer" :class="{ disabled }">
    <slot name="top" />
    <textarea
      ref="ta"
      rows="1"
      :value="modelValue"
      :placeholder="placeholder"
      :disabled="disabled"
      aria-label="输入消息"
      @input="emit('update:modelValue', $event.target.value)"
      @keydown="onKeydown"
    ></textarea>
    <div class="bar">
      <div class="tools"><slot name="tools" /></div>
      <span class="hint faint">Enter 发送 · Shift+Enter 换行</span>
      <button
        class="send"
        :class="{ ready: canSend && !disabled && !busy }"
        :disabled="disabled || busy || !canSend"
        :aria-label="sendLabel"
        :title="sendLabel"
        @click="send"
      >
        <span v-if="busy" class="spin" aria-hidden="true"></span>
        <svg v-else viewBox="0 0 24 24" width="18" height="18" aria-hidden="true">
          <path d="M12 19V5M5 12l7-7 7 7" fill="none" stroke="currentColor"
            stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </button>
    </div>
  </div>
</template>

<style scoped>
.composer {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 18px;
  padding: 12px 12px 8px 16px;
  box-shadow: 0 4px 24px rgba(0, 0, 0, 0.12);
  transition: border-color .15s;
}
.composer:focus-within { border-color: var(--accent); }
.composer.disabled { opacity: .7; }

textarea {
  display: block;
  width: 100%;
  border: none;
  background: transparent;
  resize: none;
  padding: 2px 0;
  line-height: 1.6;
  font-size: 15px;
  max-height: 260px;
  overflow-y: auto;
}
textarea:focus { outline: none; }
textarea:focus-visible { outline: none; }

.bar { display: flex; align-items: center; gap: 8px; margin-top: 6px; }
.tools { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; flex: 1; min-width: 0; }
.hint { font-size: 12px; white-space: nowrap; }

.send {
  flex: none;
  width: 34px;
  height: 34px;
  padding: 0;
  border-radius: 50%;
  display: grid;
  place-items: center;
  border: none;
  background: var(--surface-2);
  color: var(--text-faint);
}
.send.ready { background: var(--accent-strong); color: #fff; }
.send.ready:hover:not(:disabled) { background: var(--accent-strong-hover); }

.spin {
  width: 14px;
  height: 14px;
  border: 2px solid currentColor;
  border-right-color: transparent;
  border-radius: 50%;
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

@media (max-width: 640px) {
  .hint { display: none; }
}
</style>
