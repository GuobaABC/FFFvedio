<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { videoNav } from '../composables/videoNav'
import { backend, errorText } from '../services/backend'
import {
  STAGE_LABELS,
  STATUS_LABELS,
  formatClock,
  formatSeconds,
  type VideoTask,
} from '../types/video'
import VideoTaskDetail from './VideoTaskDetail.vue'

const tasks = ref<VideoTask[]>([])
const selectedId = ref('')
const loading = ref(false)
const loadError = ref('')

const selected = computed(() => tasks.value.find((item) => item.id === selectedId.value) ?? null)
const runningCount = computed(() => tasks.value.filter((item) => item.status === 'running').length)
const waitingCount = computed(() => tasks.value.filter((item) => item.status === 'waiting').length)
const doneCount = computed(() => tasks.value.filter((item) => item.status === 'done').length)

/** 有任务在执行时按 2s 轮询刷新，让阶段与日志自动前进 */
let pollTimer: number | undefined
const kickTimers: number[] = []

/** 操作刚返回时后端 goroutine 可能还没落下 running 状态，追加几次短延时刷新补上 */
function scheduleRefresh() {
  void load(true)
  for (const delay of [500, 1500, 3000]) {
    kickTimers.push(window.setTimeout(() => void load(true), delay))
  }
}

function statusClass(status: string): string {
  switch (status) {
    case 'running':
      return 'ui-chip--running'
    case 'waiting':
      return 'ui-chip--warn'
    case 'done':
      return 'ui-chip--ok'
    case 'failed':
      return 'ui-chip--error'
    default:
      return ''
  }
}

async function load(silent = false) {
  if (!silent) {
    loading.value = true
  }
  try {
    const list = await backend.listVideoTasks()
    tasks.value = list ?? []
    loadError.value = ''

    const preferred = videoNav.focusTaskId || selectedId.value
    if (preferred && tasks.value.some((item) => item.id === preferred)) {
      selectedId.value = preferred
    } else if (tasks.value.length > 0) {
      selectedId.value = tasks.value[0].id
    } else {
      selectedId.value = ''
    }
    videoNav.focusTaskId = ''
  } catch (error) {
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
  pollTimer = window.setInterval(() => {
    if (runningCount.value > 0) {
      void load(true)
    }
  }, 2000)
})

onUnmounted(() => {
  if (pollTimer !== undefined) {
    window.clearInterval(pollTimer)
  }
  for (const timer of kickTimers) {
    window.clearTimeout(timer)
  }
})

watch(
  () => videoNav.focusTaskId,
  (taskId) => {
    if (!taskId) {
      return
    }
    if (tasks.value.some((item) => item.id === taskId)) {
      selectedId.value = taskId
      videoNav.focusTaskId = ''
      return
    }
    void load()
  },
)

function handleDeleted(taskId: string) {
  tasks.value = tasks.value.filter((item) => item.id !== taskId)
  if (selectedId.value === taskId) {
    selectedId.value = tasks.value.length > 0 ? tasks.value[0].id : ''
  }
  void load(true)
}
</script>

<template>
  <section class="results">
    <header class="results__head">
      <div>
        <h2 class="results__title">视频生成结果</h2>
        <p class="results__subtitle">
          每个任务是一份可追溯的 RunContext：分镜、提示词、字幕、分段渲染与成片都挂在任务下
        </p>
      </div>
      <div class="results__stats">
        <span class="ui-chip">共 {{ tasks.length }} 个任务</span>
        <span class="ui-chip ui-chip--running">执行中 {{ runningCount }}</span>
        <span class="ui-chip ui-chip--warn">待确认 {{ waitingCount }}</span>
        <span class="ui-chip ui-chip--ok">已完成 {{ doneCount }}</span>
        <button class="ui-btn" type="button" :disabled="loading" @click="load()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>

    <div class="results__body">
      <aside class="tasks">
        <p v-if="tasks.length === 0 && !loading" class="tasks__empty">
          还没有任务。到左侧「视频生成 → 新建任务」填写主题与首帧，创建后回到这里执行。
        </p>
        <button
          v-for="task in tasks"
          :key="task.id"
          type="button"
          class="task"
          :class="{ 'task--active': task.id === selectedId }"
          @click="selectedId = task.id"
        >
          <span class="task__top">
            <span class="task__title" :title="task.title">{{ task.title }}</span>
            <span class="ui-chip" :class="statusClass(task.status)">{{ STATUS_LABELS[task.status] }}</span>
          </span>
          <span class="task__meta">
            <span>{{ STAGE_LABELS[task.stage] ?? task.stage }}</span>
            <span>·</span>
            <span>{{ formatSeconds(task.config.durationS) }} · {{ task.config.aspectRatio }}</span>
            <span>·</span>
            <span>{{ task.run.plan?.clipCount ?? task.run.script?.shots?.length ?? 0 }} 段</span>
          </span>
          <span class="task__meta">
            <span class="task__id">{{ task.id }}</span>
            <span>{{ formatClock(task.updatedAt) }}</span>
            <span v-if="task.run.output?.finalPath" class="ui-chip ui-chip--ok">已出成片</span>
            <span v-else-if="task.error" class="ui-chip ui-chip--error">失败</span>
          </span>
        </button>
      </aside>

      <div class="detail">
        <VideoTaskDetail
          v-if="selected"
          :key="selected.id"
          :task="selected"
          @changed="scheduleRefresh"
          @deleted="handleDeleted"
        />
        <div v-else class="detail__empty">
          <p class="detail__title">选择一个任务查看结果</p>
          <p class="detail__desc">
            任务视图包含：阶段时间线、两道人工确认关卡、分段渲染进度、成片与字幕时间轴。
          </p>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.results {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  min-height: 0;
}

.results__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.results__title {
  font-size: 16px;
  font-weight: 600;
}

.results__subtitle {
  max-width: 620px;
  margin-top: 4px;
  font-size: 12px;
  color: var(--color-text-muted);
}

.results__stats {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.results__body {
  display: grid;
  grid-template-columns: minmax(240px, 300px) minmax(0, 1fr);
  gap: 14px;
  flex: 1;
  min-height: 0;
}

.tasks {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.tasks__empty {
  padding: 18px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.task {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
  color: var(--color-text);
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: border-color 160ms ease, background-color 160ms ease;
}

.task:hover {
  border-color: rgba(255, 255, 255, 0.2);
  background: rgba(255, 255, 255, 0.05);
}

.task--active {
  border-color: rgba(255, 180, 58, 0.55);
  background: rgba(255, 180, 58, 0.08);
}

.task__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.task__title {
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.task__meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  min-width: 0;
  font-size: 11px;
  color: var(--color-text-muted);
}

.task__id {
  font-family: var(--font-mono);
  letter-spacing: 0.02em;
  overflow-wrap: anywhere;
}

.detail {
  min-width: 0;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.detail__empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  height: 100%;
  min-height: 260px;
  padding: 32px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  text-align: center;
}

.detail__title {
  font-size: 14px;
  font-weight: 600;
}

.detail__desc {
  max-width: 400px;
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

@media (max-width: 1000px) {
  .results__body {
    grid-template-columns: 1fr;
    grid-template-rows: minmax(0, 220px) minmax(0, 1fr);
  }
}
</style>
