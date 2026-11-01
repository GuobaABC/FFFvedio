<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'

import { backend, errorText } from '../services/backend'
import { handoffTailFrame } from '../composables/videoNav'
import {
  STAGE_LABELS,
  STATUS_LABELS,
  formatClock,
  formatSeconds,
  type TailFrameAsset,
  type VideoProject,
  type VideoTask,
} from '../types/video'
import VideoTaskDetail from './VideoTaskDetail.vue'

/** App.vue 用来切到「视频生成 → 新建任务」（尾帧「用为新建首帧」交接后） */
const emit = defineEmits<{ (event: 'to-create'): void }>()

/** 左列的虚拟分组：全部 / 未归类 */
const ALL_PROJECTS = '__all__'
const UNASSIGNED = ''

const projects = ref<VideoProject[]>([])
const tasks = ref<VideoTask[]>([])
const assets = ref<TailFrameAsset[]>([])

const selectedProjectId = ref<string>(ALL_PROJECTS)
const activeTab = ref<'tasks' | 'tails'>('tasks')
const selectedTaskId = ref('')

const loading = ref(false)
const loadError = ref('')
const actionError = ref('')

/* 项目行内编辑状态 */
const creating = ref(false)
const newName = ref('')
const editingId = ref('')
const editName = ref('')

const runningCount = computed(() => tasks.value.filter((item) => item.status === 'running').length)

const selectedProject = computed(() =>
  projects.value.find((item) => item.id === selectedProjectId.value) ?? null,
)

function projectMatch(projectId: string): boolean {
  if (selectedProjectId.value === ALL_PROJECTS) {
    return true
  }
  if (selectedProjectId.value === UNASSIGNED) {
    return projectId === UNASSIGNED
  }
  return projectId === selectedProjectId.value
}

const projectTasks = computed(() => tasks.value.filter((item) => projectMatch(item.projectId ?? '')))
const projectAssets = computed(() => assets.value.filter((item) => projectMatch(item.projectId ?? '')))
const selectedTask = computed(
  () => projectTasks.value.find((item) => item.id === selectedTaskId.value) ?? null,
)
const taskById = computed(() => {
  const map = new Map<string, VideoTask>()
  for (const task of tasks.value) {
    map.set(task.id, task)
  }
  return map
})

const projectName = computed(() => {
  if (selectedProjectId.value === ALL_PROJECTS) {
    return '全部任务'
  }
  if (selectedProjectId.value === UNASSIGNED) {
    return '未归类'
  }
  return selectedProject.value?.name ?? '项目已删除'
})

/** 有任务在执行时按 2s 轮询刷新，让阶段与日志自动前进（与结果页一致） */
let pollTimer: number | undefined
const kickTimers: number[] = []

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
    const [taskList, assetList, projectList] = await Promise.all([
      backend.listVideoTasks(),
      backend.listTailFrameAssets(),
      backend.listVideoProjects(),
    ])
    tasks.value = taskList ?? []
    assets.value = assetList ?? []
    projects.value = projectList ?? []
    // 项目被别的窗口删掉了也不让视图悬空
    if (
      selectedProjectId.value !== ALL_PROJECTS &&
      selectedProjectId.value !== UNASSIGNED &&
      !projects.value.some((item) => item.id === selectedProjectId.value)
    ) {
      selectedProjectId.value = ALL_PROJECTS
    }
    if (selectedTaskId.value && !projectTasks.value.some((item) => item.id === selectedTaskId.value)) {
      selectedTaskId.value = projectTasks.value.length > 0 ? projectTasks.value[0].id : ''
    } else if (!selectedTaskId.value && projectTasks.value.length > 0) {
      selectedTaskId.value = projectTasks.value[0].id
    }
    loadError.value = ''
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

function selectProject(id: string) {
  selectedProjectId.value = id
  selectedTaskId.value = ''
}

/* ---------------- 项目增删改 ---------------- */

async function submitNewProject() {
  const name = newName.value.trim()
  if (!name) {
    creating.value = false
    return
  }
  try {
    const project = await backend.createVideoProject(name)
    projects.value = [...projects.value, project]
    newName.value = ''
    creating.value = false
    selectedProjectId.value = project.id
    actionError.value = ''
  } catch (error) {
    actionError.value = errorText(error)
  }
}

function startRename(project: VideoProject) {
  editingId.value = project.id
  editName.value = project.name
}

async function submitRename() {
  const id = editingId.value
  const name = editName.value.trim()
  editingId.value = ''
  if (!id || !name) {
    return
  }
  const current = projects.value.find((item) => item.id === id)
  if (!current || current.name === name) {
    return
  }
  try {
    await backend.renameVideoProject(id, name)
    await load(true)
    actionError.value = ''
  } catch (error) {
    actionError.value = errorText(error)
  }
}

async function removeProject(project: VideoProject) {
  const ok = window.confirm(
    `删除项目「${project.name}」不会删除其 ${project.taskCount} 个任务与尾帧资产，它们将回到「未归类」。确认删除？`,
  )
  if (!ok) {
    return
  }
  try {
    await backend.deleteVideoProject(project.id)
    if (selectedProjectId.value === project.id) {
      selectedProjectId.value = UNASSIGNED
    }
    await load(true)
    actionError.value = ''
  } catch (error) {
    actionError.value = errorText(error)
  }
}

/* ---------------- 任务归属 ---------------- */

async function moveTask(taskId: string, projectId: string) {
  try {
    await backend.moveVideoTaskToProject(taskId, projectId)
    await load(true)
    actionError.value = ''
  } catch (error) {
    actionError.value = errorText(error)
    await load(true)
  }
}

function handleDeleted() {
  selectedTaskId.value = ''
  void load(true)
}

/* ---------------- 尾帧视图 ---------------- */

function viewAssetTask(asset: TailFrameAsset) {
  if (!taskById.value.has(asset.taskId)) {
    return
  }
  activeTab.value = 'tasks'
  selectedTaskId.value = asset.taskId
}

function useTailAsFirstFrame(asset: TailFrameAsset) {
  handoffTailFrame(asset.imagePath)
  emit('to-create')
}

async function removeAsset(asset: TailFrameAsset) {
  const ok = window.confirm(
    `从资产库移除「${asset.taskTitle || asset.taskId}」的尾帧记录？磁盘上的图片文件会保留。`,
  )
  if (!ok) {
    return
  }
  try {
    await backend.deleteTailFrameAsset(asset.id)
    await load(true)
    actionError.value = ''
  } catch (error) {
    actionError.value = errorText(error)
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
</script>

<template>
  <section class="projects">
    <header class="projects__head">
      <div>
        <h2 class="projects__title">项目视图</h2>
        <p class="projects__subtitle">
          按项目查看任务与执行进度；开启「保留尾帧」的任务，其尾帧会出现在这里并可关联回任务
        </p>
      </div>
      <div class="projects__stats">
        <span class="ui-chip">{{ projects.length }} 个项目</span>
        <span class="ui-chip">共 {{ tasks.length }} 个任务</span>
        <span class="ui-chip ui-chip--ok">{{ assets.length }} 条尾帧资产</span>
        <button class="ui-btn" type="button" :disabled="loading" @click="load()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>
    <p v-if="actionError" class="ui-notice ui-notice--error">{{ actionError }}</p>

    <div class="projects__body">
      <!-- 左列：项目列表 -->
      <aside class="plist">
        <button
          type="button"
          class="plist__item"
          :class="{ 'plist__item--active': selectedProjectId === ALL_PROJECTS }"
          @click="selectProject(ALL_PROJECTS)"
        >
          <span class="plist__name">全部任务</span>
          <span class="plist__count">{{ tasks.length }}</span>
        </button>
        <button
          type="button"
          class="plist__item"
          :class="{ 'plist__item--active': selectedProjectId === UNASSIGNED }"
          @click="selectProject(UNASSIGNED)"
        >
          <span class="plist__name">未归类</span>
          <span class="plist__count">
            {{ tasks.filter((t) => !t.projectId).length }} / {{ assets.filter((a) => !a.projectId).length }}
          </span>
        </button>

        <div
          v-for="project in projects"
          :key="project.id"
          class="plist__item plist__item--row"
          :class="{ 'plist__item--active': selectedProjectId === project.id }"
        >
          <template v-if="editingId === project.id">
            <input
              v-model="editName"
              class="ui-input plist__input"
              type="text"
              maxlength="40"
              @keyup.enter="submitRename"
              @blur="submitRename"
            />
          </template>
          <template v-else>
            <button type="button" class="plist__main" @click="selectProject(project.id)">
              <span class="plist__name">{{ project.name }}</span>
            </button>
            <span class="plist__count">{{ project.taskCount }} / {{ project.tailCount }}</span>
            <span class="plist__ops">
              <button type="button" class="plist__op" title="重命名" @click="startRename(project)">改名</button>
              <button type="button" class="plist__op plist__op--danger" title="删除项目（任务回到未归类）" @click="removeProject(project)">删除</button>
            </span>
          </template>
        </div>

        <button v-if="!creating" type="button" class="plist__add" @click="creating = true">
          ＋ 新建项目
        </button>
        <div v-else class="plist__create">
          <input
            v-model="newName"
            class="ui-input"
            type="text"
            maxlength="40"
            placeholder="项目名称（最多 40 字）"
            @keyup.enter="submitNewProject"
          />
          <div class="plist__create-ops">
            <button type="button" class="ui-btn ui-btn--primary" @click="submitNewProject">创建</button>
            <button type="button" class="ui-btn" @click="creating = false; newName = ''">取消</button>
          </div>
        </div>
      </aside>

      <!-- 右侧：当前项目 -->
      <div class="pmain">
        <div class="pmain__bar">
          <h3 class="pmain__name">{{ projectName }}</h3>
          <div class="ptabs">
            <button
              type="button"
              class="ptabs__tab"
              :class="{ 'ptabs__tab--active': activeTab === 'tasks' }"
              @click="activeTab = 'tasks'"
            >
              任务（{{ projectTasks.length }}）
            </button>
            <button
              type="button"
              class="ptabs__tab"
              :class="{ 'ptabs__tab--active': activeTab === 'tails' }"
              @click="activeTab = 'tails'"
            >
              尾帧（{{ projectAssets.length }}）
            </button>
          </div>
        </div>

        <!-- 任务页签：结构对齐结果页，卡片可移动项目 -->
        <div v-if="activeTab === 'tasks'" class="pmain__tasks">
          <aside class="tlist">
            <p v-if="projectTasks.length === 0 && !loading" class="tlist__empty">
              该项目下还没有任务。新建任务时选择此项目，或在下方的任务卡片上移动归属。
            </p>
            <div
              v-for="task in projectTasks"
              :key="task.id"
              class="tcard"
              :class="{ 'tcard--active': task.id === selectedTaskId }"
            >
              <button type="button" class="tcard__main" @click="selectedTaskId = task.id">
                <span class="tcard__top">
                  <span class="tcard__title">{{ task.title }}</span>
                  <span class="ui-chip" :class="statusClass(task.status)">{{ STATUS_LABELS[task.status] }}</span>
                </span>
                <span class="tcard__meta">
                  <span>{{ STAGE_LABELS[task.stage] ?? task.stage }}</span>
                  <span>·</span>
                  <span>{{ formatSeconds(task.config.durationS) }} · {{ task.config.aspectRatio }}</span>
                  <span>·</span>
                  <span>{{ task.run.plan?.clipCount ?? task.run.script?.shots?.length ?? 0 }} 段</span>
                </span>
                <span class="tcard__meta">
                  <span>{{ formatClock(task.updatedAt) }}</span>
                  <span v-if="task.run.output?.finalPath" class="ui-chip ui-chip--ok">已出成片</span>
                  <span v-else-if="task.error" class="ui-chip ui-chip--error">失败</span>
                </span>
              </button>
              <label class="tcard__move">
                <span>移动到</span>
                <select
                  class="ui-select ui-select--mini"
                  :value="task.projectId || ''"
                  @change="moveTask(task.id, ($event.target as HTMLSelectElement).value)"
                >
                  <option value="">未归类</option>
                  <option v-for="project in projects" :key="project.id" :value="project.id">
                    {{ project.name }}
                  </option>
                </select>
              </label>
            </div>
          </aside>
          <div class="pdetail">
            <VideoTaskDetail
              v-if="selectedTask"
              :key="selectedTask.id"
              :task="selectedTask"
              @changed="scheduleRefresh"
              @deleted="handleDeleted"
            />
            <div v-else class="pdetail__empty">
              <p class="pdetail__title">选择一个任务查看详情</p>
              <p class="pdetail__desc">任务详情包含分镜、确认关卡、分段渲染与成片的全部状态。</p>
            </div>
          </div>
        </div>

        <!-- 尾帧页签 -->
        <div v-else class="pmain__tails">
          <p v-if="projectAssets.length === 0 && !loading" class="tails__empty">
            这里还没有尾帧。任务开启「保留最后一段尾帧到资产库」并生成完成后，
            尾帧会自动收录；点卡片上的「查看任务」可以回到产生它的任务。
          </p>
          <div class="tail-grid">
            <div v-for="asset in projectAssets" :key="asset.id" class="tail-card">
              <div class="tail-card__frame">
                <img v-if="asset.mediaUrl" :src="asset.mediaUrl" :alt="asset.taskTitle" loading="lazy" />
                <div v-else class="tail-card__lost">图片文件已丢失</div>
              </div>
              <div class="tail-card__body">
                <p class="tail-card__title" :title="asset.taskTitle">{{ asset.taskTitle || asset.taskId }}</p>
                <p class="tail-card__meta">
                  <span>段 {{ asset.segmentId || '—' }}</span>
                  <span>·</span>
                  <span>{{ formatClock(asset.createdAt) }}</span>
                </p>
                <p class="tail-card__task">{{ asset.taskId }}</p>
                <div class="tail-card__ops">
                  <button
                    type="button"
                    class="ui-btn ui-btn--mini"
                    :disabled="!taskById.has(asset.taskId)"
                    :title="taskById.has(asset.taskId) ? '在任务页签打开该任务' : '来源任务已删除'"
                    @click="viewAssetTask(asset)"
                  >
                    查看任务
                  </button>
                  <button type="button" class="ui-btn ui-btn--mini ui-btn--primary" @click="useTailAsFirstFrame(asset)">
                    用为新建首帧
                  </button>
                  <button type="button" class="ui-btn ui-btn--mini ui-btn--danger" @click="removeAsset(asset)">
                    移除
                  </button>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.projects {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  min-height: 0;
}

.projects__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.projects__title {
  font-size: 16px;
  font-weight: 600;
}

.projects__subtitle {
  max-width: 640px;
  margin-top: 4px;
  font-size: 12px;
  color: var(--color-text-muted);
}

.projects__stats {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.projects__body {
  display: grid;
  grid-template-columns: minmax(220px, 260px) minmax(0, 1fr);
  gap: 14px;
  flex: 1;
  min-height: 0;
}

/* 左列项目 */
.plist {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.plist__item {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 7px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
  color: var(--color-text);
  font: inherit;
  font-size: 12.5px;
  cursor: pointer;
  transition: border-color 160ms ease, background-color 160ms ease;
}

.plist__item:hover {
  border-color: rgba(255, 255, 255, 0.2);
  background: rgba(255, 255, 255, 0.05);
}

.plist__item--active {
  border-color: rgba(255, 180, 58, 0.55);
  background: rgba(255, 180, 58, 0.08);
}

.plist__item--row {
  cursor: default;
}

.plist__main {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 8px;
  border: 0;
  padding: 0;
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.plist__name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: 600;
}

.plist__count {
  flex: none;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--color-text-muted);
}

.plist__ops {
  display: none;
  flex: none;
  gap: 4px;
}

.plist__item--row:hover .plist__ops {
  display: inline-flex;
}

.plist__op {
  padding: 2px 6px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--color-text-muted);
  font-size: 11px;
  cursor: pointer;
}

.plist__op:hover {
  color: var(--color-text);
  border-color: rgba(255, 255, 255, 0.3);
}

.plist__op--danger:hover {
  color: #ff7b6b;
  border-color: rgba(255, 123, 107, 0.5);
}

.plist__input {
  flex: 1;
  min-width: 0;
  font-size: 12px;
}

.plist__add {
  margin-top: 4px;
  padding: 7px 10px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text-muted);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}

.plist__add:hover {
  color: var(--color-accent);
  border-color: rgba(255, 180, 58, 0.4);
}

.plist__create {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px 10px;
  border: 1px solid rgba(255, 180, 58, 0.35);
  border-radius: var(--radius-md);
}

.plist__create-ops {
  display: flex;
  gap: 6px;
}

/* 右侧主区 */
.pmain {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
  min-height: 0;
  padding-right: 4px;
}

.pmain__bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.pmain__name {
  font-size: 14px;
  font-weight: 600;
}

.ptabs {
  display: inline-flex;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  overflow: hidden;
}

.ptabs__tab {
  padding: 5px 14px;
  border: 0;
  background: transparent;
  color: var(--color-text-muted);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}

.ptabs__tab--active {
  background: rgba(255, 180, 58, 0.12);
  color: var(--color-accent);
}

.pmain__tasks {
  display: grid;
  grid-template-columns: minmax(240px, 320px) minmax(0, 1fr);
  gap: 12px;
  flex: 1;
  min-height: 0;
}

.tlist {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-height: 0;
  overflow: auto;
  padding-right: 4px;
}

.tlist__empty {
  padding: 16px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.tcard {
  display: flex;
  flex-direction: column;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
  transition: border-color 160ms ease, background-color 160ms ease;
}

.tcard:hover {
  border-color: rgba(255, 255, 255, 0.2);
  background: rgba(255, 255, 255, 0.05);
}

.tcard--active {
  border-color: rgba(255, 180, 58, 0.55);
  background: rgba(255, 180, 58, 0.08);
}

.tcard__main {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
  padding: 10px 12px;
  border: 0;
  background: transparent;
  color: var(--color-text);
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.tcard__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.tcard__title {
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tcard__meta {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--color-text-muted);
}

.tcard__move {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 12px;
  border-top: 1px dashed var(--color-border);
  font-size: 11px;
  color: var(--color-text-muted);
}

.tcard__move select {
  flex: 1;
  min-width: 0;
}

.pdetail {
  min-width: 0;
  min-height: 0;
  overflow: auto;
}

.pdetail__empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  height: 100%;
  min-height: 240px;
  padding: 32px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  text-align: center;
}

.pdetail__title {
  font-size: 14px;
  font-weight: 600;
}

.pdetail__desc {
  max-width: 420px;
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

/* 尾帧页签 */
.pmain__tails {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding-right: 4px;
}

.tails__empty {
  padding: 16px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.tail-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(230px, 1fr));
  gap: 12px;
  margin-top: 10px;
}

.tail-card {
  display: flex;
  flex-direction: column;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
  overflow: hidden;
}

.tail-card__frame {
  aspect-ratio: 16 / 9;
  background: #0a0a0e;
  display: grid;
  place-items: center;
  overflow: hidden;
}

.tail-card__frame img {
  width: 100%;
  height: 100%;
  object-fit: contain;
}

.tail-card__lost {
  font-size: 11px;
  color: var(--color-text-muted);
}

.tail-card__body {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 10px 10px;
}

.tail-card__title {
  overflow: hidden;
  font-size: 12.5px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tail-card__meta {
  display: flex;
  gap: 6px;
  font-size: 11px;
  color: var(--color-text-muted);
}

.tail-card__task {
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--color-text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tail-card__ops {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 6px;
}

/* 全局样式没有的紧凑修饰，只在本视图用 */
.ui-btn--mini {
  padding: 3px 9px;
  font-size: 11px;
}

.ui-select--mini {
  padding: 2px 6px;
  font-size: 11px;
}

@media (max-width: 1000px) {
  .projects__body {
    grid-template-columns: 1fr;
  }

  .pmain__tasks {
    grid-template-columns: 1fr;
    grid-template-rows: minmax(0, 240px) minmax(0, 1fr);
  }
}
</style>
