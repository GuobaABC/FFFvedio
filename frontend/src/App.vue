<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'

import ProviderSettings from './components/ProviderSettings.vue'
import CrawlResults from './components/CrawlResults.vue'
import CrawlTaskCreate from './components/CrawlTaskCreate.vue'
import TemplateLibrary from './components/TemplateLibrary.vue'
import VideoProjects from './components/VideoProjects.vue'
import VideoResults from './components/VideoResults.vue'
import VideoTaskCreate from './components/VideoTaskCreate.vue'
import { focusCrawlTask, handoffCrawlItem } from './composables/crawlNav'
import { useTemplateForTask } from './composables/templateNav'
import { focusVideoTask } from './composables/videoNav'
import type { CrawlItem } from './types/crawl'

interface SubTab {
  id: string
  label: string
  description: string
}

interface MainTab {
  id: string
  label: string
  children: SubTab[]
}

const projectName = 'FFFVedioAgent'
const version = 'v0.0.3'

/** 主选项卡：热点爬取（新建任务 → 执行 → 结果）+ 视频生成 + 系统设置 */
const mainTabs: MainTab[] = [
  {
    id: 'crawl',
    label: '热点爬取',
    children: [
      { id: 'crawl-create', label: '新建任务', description: '' },
      { id: 'crawl-results', label: '爬取结果', description: '' },
    ],
  },
  {
    id: 'video',
    label: '视频生成',
    children: [
      { id: 'video-create', label: '新建任务', description: '' },
      { id: 'video-results', label: '视频生成结果', description: '' },
      { id: 'video-projects', label: '项目视图', description: '' },
      { id: 'video-templates', label: '模板库', description: '' },
    ],
  },
  {
    id: 'settings',
    label: '系统设置',
    children: [{ id: 'providers', label: '提供商', description: '' }],
  },
]

const activeMainTabId = ref(mainTabs[0].id)
const activeSubTabId = ref(mainTabs[0].children[0].id)

/** 主选项卡的展开状态，默认展开首个主选项卡 */
const expandedMainTabIds = ref<string[]>([mainTabs[0].id])

const isExpanded = (id: string) => expandedMainTabIds.value.includes(id)

const activeMainTab = computed(
  () => mainTabs.find((tab) => tab.id === activeMainTabId.value) ?? mainTabs[0],
)
const subTabs = computed(() => activeMainTab.value.children)
const activeSubTab = computed(
  () => subTabs.value.find((sub) => sub.id === activeSubTabId.value) ?? subTabs.value[0],
)

/** 点击主选项卡：已展开则收起，收起则展开并激活 */
function toggleMainTab(tab: MainTab) {
  if (isExpanded(tab.id)) {
    expandedMainTabIds.value = expandedMainTabIds.value.filter((id) => id !== tab.id)
    return
  }

  expandedMainTabIds.value = [...expandedMainTabIds.value, tab.id]
  activeMainTabId.value = tab.id
  if (!tab.children.some((sub) => sub.id === activeSubTabId.value)) {
    activeSubTabId.value = tab.children.length > 0 ? tab.children[0].id : ''
  }
}

/** 点击子选项卡：同时激活所属主选项卡，避免高亮与内容不一致 */
function selectSubTab(mainTab: MainTab, subId: string) {
  activeMainTabId.value = mainTab.id
  activeSubTabId.value = subId
}

/** 新建爬取任务成功后：定位到该任务并切到「爬取结果」任务视图 */
function handleCrawlCreated(taskId: string) {
  focusCrawlTask(taskId)
  activeMainTabId.value = 'crawl'
  activeSubTabId.value = 'crawl-results'
  if (!expandedMainTabIds.value.includes('crawl')) {
    expandedMainTabIds.value = [...expandedMainTabIds.value, 'crawl']
  }
}

/** 爬取条目带入视频生成：写入草稿后切到「视频生成 → 新建任务」 */
function handleCrawlToVideo(item: CrawlItem) {
  handoffCrawlItem(item)
  activeMainTabId.value = 'video'
  activeSubTabId.value = 'video-create'
  if (!expandedMainTabIds.value.includes('video')) {
    expandedMainTabIds.value = [...expandedMainTabIds.value, 'video']
  }
}

/** 新建任务成功后：定位到该任务并切到「视频生成结果」任务视图 */
function handleTaskCreated(taskId: string) {
  focusVideoTask(taskId)
  activeMainTabId.value = 'video'
  activeSubTabId.value = 'video-results'
  if (!expandedMainTabIds.value.includes('video')) {
    expandedMainTabIds.value = [...expandedMainTabIds.value, 'video']
  }
}

/** 项目视图尾帧「用为新建首帧」：交接已在组件内完成，这里只切到「视频生成 → 新建任务」 */
function handleProjectsToCreate() {
  activeMainTabId.value = 'video'
  activeSubTabId.value = 'video-create'
  if (!expandedMainTabIds.value.includes('video')) {
    expandedMainTabIds.value = [...expandedMainTabIds.value, 'video']
  }
}

/** 模板库「用此模板新建任务」：写入待用模板 ID 后切到「视频生成 → 新建任务」 */
function handleUseTemplate(templateId: string) {
  useTemplateForTask(templateId)
  activeMainTabId.value = 'video'
  activeSubTabId.value = 'video-create'
  if (!expandedMainTabIds.value.includes('video')) {
    expandedMainTabIds.value = [...expandedMainTabIds.value, 'video']
  }
}

/* 右上角悬浮时钟：YYYY:MM:DD HH:mm:ss */
const pad = (value: number) => String(value).padStart(2, '0')

function formatTime(date: Date): string {
  const datePart = `${date.getFullYear()}:${pad(date.getMonth() + 1)}:${pad(date.getDate())}`
  const timePart = `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  return `${datePart} ${timePart}`
}

const currentTime = ref(formatTime(new Date()))
let timerId: number | undefined

onMounted(() => {
  timerId = window.setInterval(() => {
    currentTime.value = formatTime(new Date())
  }, 1000)
})

onUnmounted(() => {
  if (timerId !== undefined) {
    window.clearInterval(timerId)
  }
})
</script>

<template>
  <div class="app-shell">
    <aside class="sidebar">
      <div class="brand">
        <div class="brand__mark" aria-hidden="true">FFF</div>
        <div class="brand__meta">
          <p class="brand__name">{{ projectName }}</p>
          <p class="brand__version">{{ version }}</p>
        </div>
      </div>

      <nav class="nav" aria-label="选项卡">
        <div v-for="tab in mainTabs" :key="tab.id" class="nav__group">
          <button
            type="button"
            class="nav__item nav__item--primary"
            :class="{ 'nav__item--active': tab.id === activeMainTabId }"
            :aria-expanded="isExpanded(tab.id)"
            @click="toggleMainTab(tab)"
          >
            <span>{{ tab.label }}</span>
            <span
              class="nav__chevron"
              :class="{ 'nav__chevron--open': isExpanded(tab.id) }"
              aria-hidden="true"
            ></span>
          </button>

          <div v-if="isExpanded(tab.id)" class="nav__children">
            <button
              v-for="sub in tab.children"
              :key="sub.id"
              type="button"
              class="nav__item nav__item--sub"
              :class="{ 'nav__item--active': sub.id === activeSubTabId }"
              :aria-current="sub.id === activeSubTabId ? 'page' : undefined"
              @click="selectSubTab(tab, sub.id)"
            >
              {{ sub.label }}
            </button>
          </div>
        </div>
      </nav>

      <div class="sidebar__spacer"></div>

      <p class="sidebar__foot">Fast · Fast · Fast</p>
    </aside>

    <main class="workspace">
      <div class="clock">{{ currentTime }}</div>

      <section class="panel" role="tabpanel">
        <CrawlTaskCreate v-if="activeSubTab.id === 'crawl-create'" @created="handleCrawlCreated" />
        <CrawlResults v-else-if="activeSubTab.id === 'crawl-results'" @to-video="handleCrawlToVideo" />
        <VideoTaskCreate v-else-if="activeSubTab.id === 'video-create'" @created="handleTaskCreated" />
        <VideoResults v-else-if="activeSubTab.id === 'video-results'" />
        <VideoProjects v-else-if="activeSubTab.id === 'video-projects'" @to-create="handleProjectsToCreate" />
        <TemplateLibrary v-else-if="activeSubTab.id === 'video-templates'" @use-template="handleUseTemplate" />
        <ProviderSettings v-else-if="activeSubTab.id === 'providers'" />
        <div v-else class="panel__empty">
          <p class="panel__title">{{ activeSubTab.label }}</p>
          <p class="panel__desc">{{ activeSubTab.description }}</p>
        </div>
      </section>
    </main>
  </div>
</template>

<style scoped>
.app-shell {
  display: grid;
  grid-template-columns: 3fr 7fr;
  height: 100%;
  background: var(--color-bg);
}

/* 左侧：logo + 选项卡导航 */
.sidebar {
  display: flex;
  flex-direction: column;
  min-width: 0;
  padding: 18px 16px;
  border-right: 1px solid var(--color-border);
  background: #0c0c10;
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
}

.brand__mark {
  display: grid;
  place-items: center;
  width: 36px;
  height: 36px;
  border-radius: 11px;
  background: linear-gradient(140deg, #ffc85c 0%, var(--color-accent-strong) 100%);
  color: #1a1207;
  font-family: var(--font-mono);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.04em;
  box-shadow: 0 6px 16px rgba(255, 154, 61, 0.22);
}

.brand__meta {
  min-width: 0;
}

.brand__name {
  font-size: 14px;
  font-weight: 600;
  line-height: 1.3;
}

.brand__version {
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.4;
  color: var(--color-text-muted);
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin-top: 22px;
}

.nav__group {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.nav__item {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 7px 10px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--color-text-muted);
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: color 160ms ease, background-color 160ms ease;
}

.nav__item:hover {
  background: rgba(255, 255, 255, 0.05);
  color: var(--color-text);
}

.nav__item--primary {
  justify-content: space-between;
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
}

.nav__chevron {
  flex: none;
  width: 6px;
  height: 6px;
  margin-right: 4px;
  border-right: 1.5px solid currentColor;
  border-bottom: 1.5px solid currentColor;
  transform: rotate(-45deg);
  transition: transform 160ms ease;
}

.nav__chevron--open {
  transform: rotate(45deg) translate(-1px, -1px);
}

.nav__children {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.nav__item--primary.nav__item--active {
  background: rgba(255, 180, 58, 0.1);
  color: var(--color-accent);
}

.nav__item--sub {
  padding-left: 22px;
  font-size: 12.5px;
}

.nav__item--sub.nav__item--active {
  background: rgba(255, 255, 255, 0.05);
  color: var(--color-accent);
}

.nav__item:focus-visible {
  outline: 2px solid var(--color-accent);
  outline-offset: 2px;
}

.sidebar__spacer {
  flex: 1;
}

.sidebar__foot {
  font-size: 11px;
  letter-spacing: 0.16em;
  text-transform: uppercase;
  color: var(--color-text-muted);
  opacity: 0.7;
}

/* 右侧：内容区 + 右上角悬浮时钟 */
.workspace {
  position: relative;
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
  background: radial-gradient(120% 90% at 50% 0%, #15151c 0%, var(--color-bg) 62%);
}

.clock {
  position: absolute;
  top: 16px;
  right: 20px;
  z-index: 2;
  padding: 6px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(22, 22, 28, 0.78);
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
  color: var(--color-text);
  font-family: var(--font-mono);
  font-size: 12px;
  letter-spacing: 0.06em;
  font-variant-numeric: tabular-nums;
  user-select: none;
}

.panel {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
  padding: 60px 24px 24px;
  overflow: auto;
}

.panel__empty {
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
  background: rgba(255, 255, 255, 0.015);
  text-align: center;
}

.panel__title {
  font-size: 15px;
  font-weight: 600;
}

.panel__desc {
  max-width: 360px;
  font-size: 12px;
  color: var(--color-text-muted);
}
</style>
