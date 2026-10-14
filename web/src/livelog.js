// livelog.js —— 详情页「实时日志 + 轮询」的三件共用工具。
//
// 为什么要有它：TaskDetail / PRDDetail 原来的写法是 setInterval 增量拉事件、
// events.push 只加不减、markdown 每次渲染现算。长任务或多轮 PRD 下三者叠加：
// 实测 3 万条事件时 DOM 涨到 8.6 万节点、JS 堆近 300MB，浏览器主线程被占满，
// 鼠标都挪不动。这里把三处各自补上上限：
//
//   1. appendCapped —— 内存里只留最近 MAX_LIVE_EVENTS 条，并报告丢了多少，
//      界面明说「更早的已省略」，不静默丢（与后端 EventSink 溢出计数同一立场）。
//   2. createMarkdown —— 按原文缓存渲染结果，缓存本身有上限，不跨页无限长。
//   3. createPoller —— 上一轮请求没回来就不发下一轮。setInterval 不等 async
//      回调，慢网络下请求会叠加、同一批事件被重复 push，越卡越堆。

import { marked } from 'marked'
import DOMPurify from 'dompurify'

// 实时日志面板的保留条数。给人看进度用，不是归档：完整事件流在库里，
// 需要全量时可以直接查 API。
export const MAX_LIVE_EVENTS = 500

// appendCapped 把 items 追加进 list（就地修改），超出 max 时从头部裁掉。
// 返回本次裁掉的条数，调用方累加后展示给人。
export function appendCapped(list, items, max = MAX_LIVE_EVENTS) {
  if (!items?.length) return 0
  list.push(...items)
  const over = list.length - max
  if (over > 0) {
    list.splice(0, over)
    return over
  }
  return 0
}

// createMarkdown 返回一个带缓存的 md(s)：模型输出不可信，marked 出的 HTML
// 必须过 DOMPurify。缓存按原文为键 —— 文本不变就不重算，所以在 textarea
// 里每敲一个字触发的整页重渲染不再把全部正文重新 parse + sanitize 一遍。
// 超过 max 条时整体清空（简单、够用，避免 LRU 簿记的开销）。
export function createMarkdown(max = 1000) {
  marked.setOptions({ breaks: true })
  const cache = new Map()
  return function md(s) {
    const src = s || ''
    let html = cache.get(src)
    if (html === undefined) {
      html = DOMPurify.sanitize(marked.parse(src))
      if (cache.size >= max) cache.clear()
      cache.set(src, html)
    }
    return html
  }
}

// createPoller 每 interval 毫秒调用一次 fn，但**上一次没结束绝不发下一次**
// （自链式 setTimeout）。fn 返回 false 表示停止轮询。stop() 幂等，
// 卸载后即使在途请求晚到，也不会再排下一轮。
export function createPoller(fn, interval) {
  let timer = null
  let stopped = false
  async function tick() {
    if (stopped) return
    let keep = true
    try {
      keep = (await fn()) !== false
    } catch {
      // fn 自己负责报错展示；这里只保证轮询链不断
    }
    if (!stopped && keep) timer = setTimeout(tick, interval)
  }
  return {
    start() {
      stopped = false
      clearTimeout(timer)
      timer = setTimeout(tick, interval)
    },
    stop() {
      stopped = true
      clearTimeout(timer)
      timer = null
    },
  }
}
