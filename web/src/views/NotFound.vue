<script setup>
// 兜底路由页：未知地址此前渲染成空壳，看起来像白屏挂了。
</script>

<template>
  <div class="card empty">
    <h1 style="margin: 0 0 8px">页面不存在</h1>
    <p class="dim" style="margin: 0 0 12px">地址可能已失效或输入有误。</p>
    <p style="margin: 0"><RouterLink to="/">回到任务看板</RouterLink></p>
  </div>
</template>
