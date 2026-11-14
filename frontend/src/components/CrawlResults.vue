<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { crawlNav } from '../composables/crawlNav'
import { backend, errorText } from '../services/backend'
import {
  CRAWL_STAGE_LABELS,
  CRAWL_STATUS_LABELS,
  crawlStatusClass,
  formatCrawlClock,
  type CrawlItem,
  type CrawlTask,
  type SiteProfile,
} from '../types/crawl'
import CrawlTaskDetail from './CrawlTaskDetail.vue'

const emit = defineEmits<{ (event: 'to-video', item: CrawlItem): void }>()

const tasks = ref<CrawlTask[]>([])
const profiles = ref<SiteProfile[]>([])
const selectedId = ref('')
const loading = ref(false)
const loadError = ref('')
const profileError = ref('')

const selected = computed(() => tasks.value.find((item) => item.id === selectedId.value) ?? null)
const runningCount = computed(() => tasks.value.filter((item) => item.status === 'running').length)
const doneCount = computed(() => tasks.value.filter((item) => item.status === 'done').length)
const failedCount = computed(() => tasks.value.filter((item) => item.status === 'failed').length)
const storedCount = computed(() =>
  tasks.value.reduce((total, task) => total + (task.run?.stats?.stored ?? 0), 0),
)

/** 有任务在跑时按 1.5s 轮询：任务阶段、每条 URL 结果与日志都会自动刷新 */
let pollTimer: number | undefined
const kickTimers: number[] = []

/** 操作返回后后端 goroutine 可能还没落下 running 状态，追加几次短延时刷新补上 */
function scheduleRefresh() {
  void load(true)
  for (const delay of [500, 1500, 3000]) {
    kickTimers.push(window.setTimeout(() => void load(true), delay))
  }
}

async function load(silent = false) {
  if (!silent) {
    loading.value = true
  }
  try {
    const list = await backend.listCrawlTasks()
    tasks.value = list ?? []
    loadError.value = ''

    const preferred = crawlNav.focusTaskId || selectedId.value
    if (preferred && tasks.value.some((item) => item.id === preferred)) {
      selectedId.value = preferred
    } else if (tasks.value.length > 0) {
      selectedId.value = tasks.value[0].id
    } else {
      selectedId.value = ''
    }
    crawlNav.focusTaskId = ''
  } catch (error) {
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

async function loadProfiles() {
  try {
    profiles.value = (await backend.listSiteProfiles()) ?? []
    profileError.value = ''
  } catch (error) {
    profileError.value = errorText(error)
  }
}

async function removeProfile(profile: SiteProfile) {
  if (!window.confirm(`删除站点规则「${profile.siteKey}」？下次该站点会重新走 L3 抽取。`)) {
    return
  }
  try {
    await backend.deleteSiteProfile(profile.siteKey)
    await loadProfiles()
  } catch (error) {
    profileError.value = errorText(error)
  }
}

onMounted(() => {
  void load()
  void loadProfiles()
  pollTimer = window.setInterval(() => {
    if (runningCount.value > 0) {
      void load(true)
    }
  }, 1500)
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
  () => crawlNav.focusTaskId,
  (taskId) => {
    if (!taskId) {
      return
    }
    if (tasks.value.some((item) => item.id === taskId)) {
      selectedId.value = taskId
      crawlNav.focusTaskId = ''
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

async function refreshAll() {
  await load()
  await loadProfiles()
  scheduleRefresh()
}
</script>

<template>
  <section class="results">
    <header class="results__head">
      <div>
        <h2 class="results__title">爬取结果</h2>
        <p class="results__subtitle">
          以任务为视图：每条 URL 的状态机、审计步骤、抽取层级与入库条目都挂在任务下；
          执行期间自动轮询，每次爬取都会刷新这里
        </p>
      </div>
      <div class="results__stats">
        <span class="ui-chip">共 {{ tasks.length }} 个任务</span>
        <span class="ui-chip ui-chip--running">执行中 {{ runningCount }}</span>
        <span class="ui-chip ui-chip--ok">已完成 {{ doneCount }}</span>
        <span class="ui-chip ui-chip--error">失败 {{ failedCount }}</span>
        <span class="ui-chip">累计入库 {{ storedCount }} 条</span>
        <button class="ui-btn" type="button" :disabled="loading" @click="refreshAll">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>

    <div class="results__body">
      <aside class="tasks">
        <p v-if="tasks.length === 0 && !loading" class="tasks__empty">
          还没有爬取任务。到左侧「热点爬取 → 新建任务」粘贴链接或分享文案，创建后回到这里执行。
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
            <span class="ui-chip" :class="crawlStatusClass(task.status)">
              {{ CRAWL_STATUS_LABELS[task.status] ?? task.status }}
            </span>
          </span>
          <span class="task__meta">
            <span>{{ CRAWL_STAGE_LABELS[task.stage] ?? task.stage }}</span>
            <span>·</span>
            <span>{{ task.run?.results?.length ?? 0 }} 条链接</span>
            <span>·</span>
            <span>入库 {{ task.run?.stats?.stored ?? 0 }}</span>
            <span v-if="task.run?.stats?.cached" class="ui-chip ui-chip--ok">
              缓存 {{ task.run.stats.cached }}
            </span>
          </span>
          <span class="task__meta">
            <span class="task__id">{{ task.id }}</span>
            <span>{{ formatCrawlClock(task.updatedAt) }}</span>
          </span>
        </button>

        <details class="profiles">
          <summary>站点规则（L4 沉淀）· {{ profiles.length }}</summary>
          <p v-if="profileError" class="ui-notice ui-notice--error">{{ profileError }}</p>
          <p v-if="profiles.length === 0" class="profiles__empty">
            还没有沉淀规则：新域名首次抽取成功后，会把选择器与清洗规则写进站点画像，下次走快速通道。
          </p>
          <div v-for="profile in profiles" :key="profile.siteKey" class="profile">
            <div class="profile__head">
              <span class="profile__key">{{ profile.siteKey }}</span>
              <span class="ui-chip" :class="profile.enabled ? 'ui-chip--ok' : 'ui-chip--warn'">
                {{ profile.enabled ? '启用' : '已停用' }}
              </span>
              <span class="ui-chip">命中 {{ profile.hitSuccess }} / 失手 {{ profile.hitFail }}</span>
              <span v-if="profile.requiresRender" class="ui-chip ui-chip--warn">需渲染</span>
            </div>
            <p class="profile__host">{{ profile.hostPattern }}</p>
            <ul class="profile__rules">
              <li v-for="(selectors, field) in profile.selectors" :key="field">
                <span class="profile__field">{{ field }}</span>
                <span class="profile__selector">{{ selectors.join(' | ') }}</span>
              </li>
            </ul>
            <div class="profile__foot">
              <span class="profile__time">
                {{ profile.autoGenerated ? '自动沉淀' : '人工维护' }} ·
                {{ formatCrawlClock(profile.lastVerifiedAt || profile.updatedAt) }}
              </span>
              <button class="ui-btn ui-btn--ghost" type="button" @click="removeProfile(profile)">
                删除规则
              </button>
            </div>
          </div>
        </details>
      </aside>

      <div class="detail">
        <CrawlTaskDetail
          v-if="selected"
          :key="selected.id"
          :task="selected"
          @changed="scheduleRefresh"
          @deleted="handleDeleted"
          @to-video="emit('to-video', $event)"
        />
        <div v-else class="detail__empty">
          <p class="detail__title">选择一个任务查看爬取结果</p>
          <p class="detail__desc">
            任务视图包含：五段式阶段时间线、每条 URL 的状态机与审计步骤、抽取层级命中率，以及可直接带入视频生成的条目库。
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
  max-width: 660px;
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.7;
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
  grid-template-columns: minmax(250px, 320px) minmax(0, 1fr);
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
  gap: 6px;
  flex-wrap: wrap;
  font-size: 11px;
  color: var(--color-text-muted);
}

.task__id {
  font-family: var(--font-mono);
  letter-spacing: 0.02em;
}

.profiles {
  margin-top: 4px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(0, 0, 0, 0.18);
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.profiles summary {
  cursor: pointer;
  font-size: 12px;
  color: var(--color-text);
}

.profiles__empty {
  margin-top: 8px;
  line-height: 1.7;
}

.profile {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-top: 8px;
  padding: 8px;
  border: 1px dashed rgba(255, 255, 255, 0.1);
  border-radius: var(--radius-sm);
}

.profile__head {
  display: flex;
  align-items: center;
  gap: 5px;
  flex-wrap: wrap;
}

.profile__key {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text);
}

.profile__host {
  font-family: var(--font-mono);
  font-size: 11px;
  word-break: break-all;
}

.profile__rules {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.profile__rules li {
  display: flex;
  gap: 6px;
  word-break: break-all;
}

.profile__field {
  flex: none;
  min-width: 62px;
  color: var(--color-text);
  font-family: var(--font-mono);
}

.profile__selector {
  font-family: var(--font-mono);
  font-size: 10.5px;
}

.profile__foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  flex-wrap: wrap;
}

.profile__time {
  font-size: 10.5px;
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
  max-width: 420px;
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
