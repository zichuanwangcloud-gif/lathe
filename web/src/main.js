// Lathe 管理界面入口。
import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Board from './views/Board.vue'
import TaskDetail from './views/TaskDetail.vue'
import Repos from './views/Repos.vue'
import Settings from './views/Settings.vue'
import './style.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'board', component: Board },
    { path: '/tasks/:id', name: 'task', component: TaskDetail, props: true },
    { path: '/repos', name: 'repos', component: Repos },
    { path: '/settings', name: 'settings', component: Settings },
  ],
})

createApp(App).use(router).mount('#app')
