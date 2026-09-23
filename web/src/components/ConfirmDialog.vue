<script setup>
// 确认对话框本体（confirm.js 的渲染端）。App.vue 里挂载一次，全站共用。
// 与原生 confirm 的差异：Escape/遮罩点击 = 取消、焦点受控、破坏性操作是
// 实心红按钮，不可恢复操作可要求逐字输入确认串（type-to-confirm）。
import { ref, computed, watch } from 'vue'
import BaseDialog from './BaseDialog.vue'
import { confirmState, settleConfirm } from '../confirm'

const typed = ref('')
// 每次打开清空上一次的输入，避免残留串直接放行下一次删除
watch(
  () => confirmState.open,
  (v) => { if (v) typed.value = '' }
)

const needType = computed(() => !!confirmState.requireText)
const canConfirm = computed(() => !needType.value || typed.value === confirmState.requireText)
</script>

<template>
  <BaseDialog v-if="confirmState.open" label-id="confirm-dialog-title" width="420px" @close="settleConfirm(false)">
    <form @submit.prevent="canConfirm && settleConfirm(true)">
      <h3 id="confirm-dialog-title" style="margin-top: 0">确认操作</h3>
      <p style="white-space: pre-line; margin: 8px 0 14px">{{ confirmState.text }}</p>
      <label v-if="needType" class="field">
        <span>
          此操作不可恢复。输入 <b class="mono">{{ confirmState.requireText }}</b> 以确认：
        </span>
        <input v-model="typed" class="mono" autocomplete="off" data-autofocus />
      </label>
      <div class="row" style="justify-content: flex-end; gap: 8px">
        <button type="button" @click="settleConfirm(false)">取消</button>
        <button
          type="submit"
          class="danger-solid"
          :disabled="!canConfirm"
          :data-autofocus="needType ? undefined : ''"
        >确认</button>
      </div>
    </form>
  </BaseDialog>
</template>
