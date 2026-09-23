// 全局确认对话框服务：取代原生 confirm()。
//
// 原生 confirm 样式与站点脱节、无焦点管理、不能表达后果层级，更做不到
// 「输入名字才放行」这种不可恢复操作的确认强度。这里基于 BaseDialog 实现，
// 返回 Promise<boolean>，调用点 await 即可：
//
//   if (!(await confirmDialog('确认删除？'))) return
//   if (!(await confirmDialog('不可恢复！', { requireText: user.email }))) return
//
// 对话框本体是 components/ConfirmDialog.vue，在 App.vue 挂载一次。
import { reactive } from 'vue'

export const confirmState = reactive({
  open: false,
  text: '',
  // 非空时要求逐字输入该串才能点「确认」——不可恢复操作的最后一道人工闸
  requireText: '',
  resolve: null,
})

export function confirmDialog(text, opts = {}) {
  return new Promise((resolve) => {
    Object.assign(confirmState, {
      open: true,
      text,
      requireText: opts.requireText || '',
      resolve,
    })
  })
}

export function settleConfirm(ok) {
  confirmState.resolve?.(ok)
  confirmState.open = false
  confirmState.resolve = null
}
