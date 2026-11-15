<script setup lang="ts">
import { ref, watch } from 'vue'

import { backend, errorText } from '../services/backend'
import type { ProjectMemory } from '../types/video'

const props = defineProps<{ projectId: string }>()
const emit = defineEmits<{ (event: 'changed'): void }>()

const memories = ref<ProjectMemory[]>([])
const query = ref('')
const loading = ref(false)
const busy = ref('')
const error = ref('')
const info = ref('')
const showForm = ref(false)
const draft = ref<ProjectMemory>(emptyDraft())

function emptyDraft(): ProjectMemory {
  return {
    id: '',
    projectId: props.projectId,
    episode: 0,
    kind: 'summary',
    title: '',
    content: '',
    payload: '{}',
    sourceTask: '',
    createdAt: '',
    updatedAt: '',
  }
}

async function load() {
  if (!props.projectId) {
    memories.value = []
    return
  }
  loading.value = true
  error.value = ''
  try {
    memories.value = query.value.trim()
      ? await backend.searchProjectMemory(props.projectId, query.value.trim(), 50)
      : await backend.listProjectMemory(props.projectId, 200)
  } catch (err) {
    error.value = errorText(err)
  } finally {
    loading.value = false
  }
}

watch(() => props.projectId, () => {
  query.value = ''
  showForm.value = false
  void load()
}, { immediate: true })

async function run(label: string, task: () => Promise<string>) {
  busy.value = label
  error.value = ''
  info.value = ''
  try {
    info.value = await task()
    await load()
    emit('changed')
  } catch (err) {
    error.value = errorText(err)
  } finally {
    busy.value = ''
  }
}

function rebuild() {
  void run('rebuild', async () => {
    const count = await backend.rebuildProjectMemory(props.projectId)
    return `项目记忆已重建：${count} 条`
  })
}

function embed() {
  void run('embed', async () => {
    const count = await backend.embedProjectMemory(props.projectId)
    return `已补 ${count} 条语义向量（失败时自动只用关键词检索）`
  })
}

function startCreate() {
  draft.value = emptyDraft()
  draft.value.episode = 1
  showForm.value = true
}

function startEdit(memory: ProjectMemory) {
  draft.value = { ...memory }
  showForm.value = true
}

function save() {
  void run('save', async () => {
    if (!draft.value.content.trim()) {
      throw new Error('记忆内容不能为空')
    }
    await backend.upsertProjectMemory({ ...draft.value, projectId: props.projectId })
    showForm.value = false
    return '项目记忆已保存'
  })
}

function remove(memory: ProjectMemory) {
  void run('delete', async () => {
    await backend.deleteProjectMemory(memory.id)
    return '已删除一条项目记忆'
  })
}
</script>

<template>
  <section class="memory">
    <header class="memory__head">
      <div>
        <h3 class="ui-h3">项目记忆</h3>
        <p class="hint">沉淀同项目历史角色/剧情/世界设定；新建任务时自动做关键词 + 语义检索。</p>
      </div>
      <div class="memory__actions">
        <input v-model="query" class="ui-input memory__search" type="text" placeholder="搜索角色 / 剧情 / 设定" @keyup.enter="load" />
        <button class="ui-btn" type="button" @click="load">搜索</button>
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="rebuild">重建记忆</button>
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="embed">补语义向量</button>
        <button class="ui-btn ui-btn--primary" type="button" @click="startCreate">手动新增</button>
      </div>
    </header>

    <p v-if="error" class="ui-notice ui-notice--error">{{ error }}</p>
    <p v-if="info" class="ui-notice ui-notice--ok">{{ info }}</p>
    <p v-if="loading" class="hint">读取中…</p>
    <p v-else-if="memories.length === 0" class="hint">
      还没有项目记忆。任务完成时会自动写入；也可以点「重建记忆」从历史任务提取。
    </p>

    <div v-if="showForm" class="memory-form ui-card">
      <div class="grid grid--2">
        <label class="ui-field">
          <span class="ui-label">类型</span>
          <select v-model="draft.kind" class="ui-select">
            <option value="summary">摘要</option>
            <option value="character">角色</option>
            <option value="plot">剧情</option>
            <option value="world">世界设定</option>
            <option value="event">事件</option>
            <option value="relationship">关系</option>
          </select>
        </label>
        <label class="ui-field">
          <span class="ui-label">集数</span>
          <input v-model.number="draft.episode" class="ui-input" type="number" min="0" step="1" />
        </label>
      </div>
      <label class="ui-field">
        <span class="ui-label">标题</span>
        <input v-model="draft.title" class="ui-input" type="text" placeholder="例：第1集 角色：猪老板" />
      </label>
      <label class="ui-field">
        <span class="ui-label">内容</span>
        <textarea v-model="draft.content" class="ui-textarea" rows="4"></textarea>
      </label>
      <div class="actions">
        <button class="ui-btn ui-btn--primary" type="button" :disabled="busy !== ''" @click="save">保存</button>
        <button class="ui-btn" type="button" @click="showForm = false">取消</button>
      </div>
    </div>

    <div class="memory-list">
      <article v-for="memory in memories" :key="memory.id" class="memory-item">
        <header class="memory-item__head">
          <span class="ui-chip">{{ memory.kind }}</span>
          <span class="ui-chip">第 {{ memory.episode }} 集</span>
          <span class="memory-item__title">{{ memory.title || '（无标题）' }}</span>
          <button class="ui-btn ui-btn--ghost" type="button" @click="startEdit(memory)">编辑</button>
          <button class="ui-btn ui-btn--danger" type="button" :disabled="busy !== ''" @click="remove(memory)">删除</button>
        </header>
        <p class="memory-item__content">{{ memory.content }}</p>
      </article>
    </div>
  </section>
</template>

<style scoped>
.memory {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 0;
  overflow: auto;
  padding-right: 4px;
}

.memory__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.memory__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.memory__search {
  width: 240px;
}

.memory-form {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
}

.grid {
  display: grid;
  gap: 10px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
}

.actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.memory-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.memory-item {
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.memory-item__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.memory-item__title {
  flex: 1;
  min-width: 160px;
  font-size: 12.5px;
  font-weight: 600;
}

.memory-item__content {
  margin: 6px 0 0;
  font-size: 12px;
  line-height: 1.7;
  white-space: pre-wrap;
  color: var(--color-text-muted);
}
</style>
