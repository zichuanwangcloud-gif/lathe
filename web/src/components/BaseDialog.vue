<script setup>
// 全局弹窗基座。
//
// 收编此前 Issues.vue 与 PreviewDialog.vue 各自重复实现的 overlay/dialog ——
// 那两个弹窗都没有 Escape 关闭、没有焦点管理，屏幕阅读器也不知道弹出了
// 对话框（WCAG 2.1.2 / 2.4.3 / 4.1.2）。在这里一次性解决：
//   - Escape 关闭；点击遮罩（非内容区）关闭
//   - 打开时焦点进弹窗（优先 [data-autofocus] 控件），关闭后归还给触发元素
//   - Tab / Shift+Tab 焦点锁在弹窗内循环（focus trap）
//   - 打开期间锁住背景滚动
import { onMounted, onBeforeUnmount, ref } from 'vue'

defineProps({
  // 标题元素的 id，充当弹窗的 accessible name（aria-labelledby）
  labelId: { type: String, required: true },
  width: { type: String, default: '560px' },
})
const emit = defineEmits(['close'])

const dialogEl = ref(null)
let prevFocus = null

const FOCUSABLE =
  'a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])'

function onKeydown(e) {
  if (e.key === 'Escape') {
    e.stopPropagation()
    emit('close')
    return
  }
  if (e.key !== 'Tab') return
  const items = dialogEl.value?.querySelectorAll(FOCUSABLE)
  if (!items?.length) return
  const first = items[0]
  const last = items[items.length - 1]
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault()
    last.focus()
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault()
    first.focus()
  }
}

onMounted(() => {
  prevFocus = document.activeElement
  const dlg = dialogEl.value
  const target = dlg.querySelector('[data-autofocus]') || dlg.querySelector(FOCUSABLE) || dlg
  target.focus()
  document.addEventListener('keydown', onKeydown)
  document.body.style.overflow = 'hidden'
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  document.body.style.overflow = ''
  // 焦点归还：弹窗随 v-if 销毁，不归还会掉到 body 上，键盘用户丢失位置
  prevFocus?.focus?.()
})
</script>

<template>
  <div class="overlay" @click.self="emit('close')">
    <div
      ref="dialogEl"
      class="dialog card"
      role="dialog"
      aria-modal="true"
      :aria-labelledby="labelId"
      :style="{ width }"
      tabindex="-1"
    >
      <slot />
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: 6vh 16px;
  z-index: 100;
}
.dialog {
  max-width: 100%;
  max-height: 84vh;
  overflow-y: auto;
}
/* 弹窗本体只是焦点停泊点（内部没有可交互元素的场景），容器不画焦点环 */
.dialog:focus,
.dialog:focus-visible {
  outline: none;
}
</style>
