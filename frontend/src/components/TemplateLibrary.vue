<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  TEMPLATE_SCOPE_LABELS,
  TEMPLATE_TOKEN_KEYS,
  cloneTemplate,
  templateScopeClass,
  type TemplateScope,
  type VideoTemplate,
  type VideoTemplatePreview,
} from '../types/template'
import { formatClock } from '../types/video'
import TemplateEditor from './TemplateEditor.vue'

const emit = defineEmits<{
  (event: 'use-template', templateId: string): void
}>()

const templates = ref<VideoTemplate[]>([])
const selectedId = ref('')
const loading = ref(false)
const loadError = ref('')
const actionError = ref('')

const keyword = ref('')
const scopeFilter = ref<'all' | TemplateScope>('all')
const activeTag = ref('')

const preview = ref<VideoTemplatePreview | null>(null)
const previewLoading = ref(false)
const topic = ref('')
const vars = ref<Record<string, string>>({})

const editorOpen = ref(false)
const editing = ref<VideoTemplate | null>(null)
const saving = ref(false)
const saveError = ref('')

const selected = computed(() => templates.value.find((item) => item.id === selectedId.value) ?? null)

/** 标签云：内置模板与个人模板共用一套检索维度（§6 模板库可检索） */
const tagOptions = computed(() => {
  const counts = new Map<string, number>()
  for (const template of templates.value) {
    for (const tag of template.tags ?? []) {
      counts.set(tag, (counts.get(tag) ?? 0) + 1)
    }
  }
  return [...counts.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 12)
    .map(([tag, count]) => ({ tag, count }))
})

const filtered = computed(() => {
  const text = keyword.value.trim().toLowerCase()
  return templates.value.filter((template) => {
    if (scopeFilter.value !== 'all' && template.scope !== scopeFilter.value) {
      return false
    }
    if (activeTag.value && !(template.tags ?? []).includes(activeTag.value)) {
      return false
    }
    if (!text) {
      return true
    }
    const haystack = [template.name, template.description, template.version, ...(template.tags ?? [])]
      .join(' ')
      .toLowerCase()
    return haystack.includes(text)
  })
})

const builtinCount = computed(() => templates.value.filter((item) => item.scope === 'builtin').length)
const customCount = computed(() => templates.value.filter((item) => item.scope !== 'builtin').length)

function tokenLabel(key: string): string {
  return TEMPLATE_TOKEN_KEYS.find((item) => item.key === key)?.label ?? key
}

function toggleTag(tag: string) {
  activeTag.value = activeTag.value === tag ? '' : tag
}

async function load(preferId = '') {
  loading.value = true
  try {
    const list = await backend.listVideoTemplates()
    templates.value = list ?? []
    loadError.value = ''

    const target = preferId || selectedId.value
    if (target && templates.value.some((item) => item.id === target)) {
      selectedId.value = target
    } else {
      selectedId.value = templates.value.length > 0 ? templates.value[0].id : ''
    }
    await refreshPreview()
  } catch (error) {
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

/** 模板里的 opening.vars 决定预览表单要收集哪些变量，已填的值保留 */
function syncVars(template: VideoTemplate | null) {
  const next: Record<string, string> = {}
  for (const name of template?.opening?.vars ?? []) {
    next[name] = vars.value[name] ?? ''
  }
  vars.value = next
}

async function refreshPreview() {
  const template = selected.value
  if (!template) {
    preview.value = null
    return
  }
  previewLoading.value = true
  try {
    preview.value = await backend.previewVideoTemplate(template.id, topic.value.trim(), { ...vars.value })
    actionError.value = ''
  } catch (error) {
    preview.value = null
    actionError.value = errorText(error)
  } finally {
    previewLoading.value = false
  }
}

watch(selectedId, () => {
  syncVars(selected.value)
  void refreshPreview()
})

onMounted(() => {
  void load()
})

function useForTask() {
  if (!selected.value) {
    return
  }
  emit('use-template', selected.value.id)
}

function startCreate() {
  saveError.value = ''
  editing.value = null
  editorOpen.value = true
}

function startEdit() {
  if (!selected.value) {
    return
  }
  saveError.value = ''
  editing.value = cloneTemplate(selected.value, { keepId: true })
  editorOpen.value = true
}

function startDuplicate() {
  if (!selected.value) {
    return
  }
  saveError.value = ''
  const copy = cloneTemplate(selected.value, { keepId: false })
  copy.name = `${selected.value.name} 副本`
  editing.value = copy
  editorOpen.value = true
}

function cancelEdit() {
  if (saving.value) {
    return
  }
  editorOpen.value = false
  editing.value = null
  saveError.value = ''
}

async function handleSave(template: VideoTemplate) {
  saving.value = true
  saveError.value = ''
  try {
    const saved = await backend.saveVideoTemplate(template)
    editorOpen.value = false
    editing.value = null
    await load(saved.id)
  } catch (error) {
    saveError.value = errorText(error)
  } finally {
    saving.value = false
  }
}

async function removeSelected() {
  const template = selected.value
  if (!template || template.scope === 'builtin') {
    return
  }
  if (!window.confirm(`删除模板「${template.name}」？已创建的任务仍保留版本快照。`)) {
    return
  }
  actionError.value = ''
  try {
    await backend.deleteVideoTemplate(template.id)
    selectedId.value = ''
    await load()
  } catch (error) {
    actionError.value = errorText(error)
  }
}
</script>

<template>
  <section class="library">
    <header class="library__head">
      <div>
        <h2 class="library__title">视频模板库</h2>
        <p class="library__subtitle">
          模板只提供默认值：风格词表、人物图文、开场白、字幕与发布档、流水线默认值。选中后仍要在新建任务里逐项确认，三道确认门照常生效。
        </p>
      </div>
      <div class="library__stats">
        <span class="ui-chip">共 {{ templates.length }} 个</span>
        <span class="ui-chip ui-chip--ok">内置 {{ builtinCount }}</span>
        <span class="ui-chip ui-chip--warn">自定义 {{ customCount }}</span>
        <button class="ui-btn ui-btn--primary" type="button" @click="startCreate">+ 新建模板</button>
        <button class="ui-btn" type="button" :disabled="loading" @click="load()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>
    <p v-if="actionError" class="ui-notice ui-notice--error">{{ actionError }}</p>

    <div class="library__body">
      <aside class="list">
        <div class="list__filters">
          <input v-model="keyword" class="ui-input" type="text" placeholder="搜索名称 / 描述 / 标签" />
          <select v-model="scopeFilter" class="ui-select">
            <option value="all">全部作用域</option>
            <option value="builtin">内置</option>
            <option value="team">团队</option>
            <option value="personal">个人</option>
          </select>
        </div>

        <div v-if="tagOptions.length > 0" class="tags">
          <button
            v-for="item in tagOptions"
            :key="item.tag"
            class="tag"
            :class="{ 'tag--active': activeTag === item.tag }"
            type="button"
            @click="toggleTag(item.tag)"
          >
            {{ item.tag }}<span class="tag__count">{{ item.count }}</span>
          </button>
        </div>

        <p v-if="filtered.length === 0 && !loading" class="list__empty">
          没有匹配的模板。清空筛选条件，或点右上角「+ 新建模板」自己定义一份。
        </p>

        <button
          v-for="template in filtered"
          :key="template.id"
          type="button"
          class="item"
          :class="{ 'item--active': template.id === selectedId }"
          @click="selectedId = template.id"
        >
          <span class="item__top">
            <span class="item__name">{{ template.name }}</span>
            <span class="ui-chip" :class="templateScopeClass(template.scope)">
              {{ TEMPLATE_SCOPE_LABELS[template.scope] ?? template.scope }}
            </span>
          </span>
          <span class="item__meta">
            <span class="item__version">v{{ template.version }}</span>
            <span>·</span>
            <span>{{ template.defaults?.aspectRatio || '—' }}</span>
            <span>·</span>
            <span>{{ template.defaults?.durationS ? `${template.defaults.durationS}s` : '—' }}</span>
            <span>·</span>
            <span>{{ (template.characters ?? []).length }} 角色</span>
          </span>
          <span v-if="template.description" class="item__desc">{{ template.description }}</span>
          <span class="item__meta">
            <span>{{ formatClock(template.updatedAt) }}</span>
          </span>
        </button>
      </aside>

      <div class="detail">
        <TemplateEditor
          v-if="editorOpen"
          :template="editing"
          :saving="saving"
          :error="saveError"
          @save="handleSave"
          @cancel="cancelEdit"
        />

        <template v-else-if="selected">
          <section class="ui-card detail__head">
            <div class="detail__title-row">
              <div>
                <h3 class="detail__title">
                  {{ selected.name }}
                  <span class="ui-chip" :class="templateScopeClass(selected.scope)">
                    {{ TEMPLATE_SCOPE_LABELS[selected.scope] ?? selected.scope }}
                  </span>
                  <span class="ui-chip">v{{ selected.version }}</span>
                </h3>
                <p v-if="selected.description" class="detail__desc">{{ selected.description }}</p>
              </div>
              <div class="detail__actions">
                <button class="ui-btn ui-btn--primary" type="button" @click="useForTask">用此模板新建任务</button>
                <button v-if="selected.scope !== 'builtin'" class="ui-btn" type="button" @click="startEdit">编辑</button>
                <button class="ui-btn" type="button" @click="startDuplicate">另存为</button>
                <button
                  v-if="selected.scope !== 'builtin'"
                  class="ui-btn ui-btn--danger"
                  type="button"
                  @click="removeSelected"
                >
                  删除
                </button>
              </div>
            </div>
            <p class="hint">
              版本 v{{ selected.version }} 会随任务一起快照保存：之后修改模板不会影响已创建的任务，复现与回滚都以任务内的快照为准。
            </p>
          </section>

          <section class="ui-card block">
            <div class="block__head">
              <h4 class="ui-h3">预览与最小样</h4>
              <span class="ui-chip">开场白按使用次数轮换</span>
            </div>
            <div class="grid grid--2">
              <label class="ui-field">
                <span class="ui-label">预览主题（插值 {topic}）</span>
                <input v-model="topic" class="ui-input" type="text" placeholder="手冲咖啡入门" />
              </label>
              <label v-for="(_, name) in vars" :key="name" class="ui-field">
                <span class="ui-label">变量 {{ '{' + name + '}' }}</span>
                <input v-model="vars[name]" class="ui-input" type="text" :placeholder="name" />
              </label>
            </div>
            <div class="preview__actions">
              <button class="ui-btn" type="button" :disabled="previewLoading" @click="refreshPreview">
                {{ previewLoading ? '生成中…' : '刷新预览' }}
              </button>
            </div>

            <p v-if="preview" class="preview__opening">{{ preview.opening || '（该模板未定义开场白）' }}</p>
            <p v-if="preview && (preview.openingAll ?? []).length > 1" class="hint">
              全部变体：{{ (preview.openingAll ?? []).join(' / ') }}
            </p>
          </section>

          <section v-if="preview" class="ui-card block">
            <h4 class="ui-h3">风格（受控词表）</h4>
            <dl class="summary">
              <div v-for="(value, key) in preview.styleTokens ?? {}" :key="key" class="summary__row">
                <dt>{{ tokenLabel(String(key)) }}</dt>
                <dd>{{ value }}</dd>
              </div>
              <div v-if="preview.styleSummary" class="summary__row">
                <dt>摘要</dt>
                <dd>{{ preview.styleSummary }}</dd>
              </div>
              <div v-if="preview.styleSnippet" class="summary__row">
                <dt>片段（通用）</dt>
                <dd>{{ preview.styleSnippet }}</dd>
              </div>
            </dl>
            <p class="hint">提示词只拼可度量描述；cinematic、8K、高级感这类无增益词会在合规检查里降级为警告。</p>
          </section>

          <section v-if="preview && (preview.characters ?? []).length > 0" class="ui-card block">
            <h4 class="ui-h3">人物插图（图文成对）</h4>
            <ul class="bullets">
              <li v-for="(item, index) in preview.characters ?? []" :key="index">{{ item }}</li>
            </ul>
            <p class="hint">支持主体参考的模型直接喂定妆图；不支持的模型自动降级为外观描述文字。</p>
          </section>

          <section v-if="preview" class="ui-card block">
            <h4 class="ui-h3">字幕 / 发布档 / 默认值</h4>
            <dl class="summary">
              <div class="summary__row">
                <dt>字幕</dt>
                <dd>{{ preview.subtitle || '—' }}</dd>
              </div>
              <div class="summary__row">
                <dt>发布档</dt>
                <dd>{{ preview.publish || '—' }}</dd>
              </div>
              <div class="summary__row">
                <dt>默认值</dt>
                <dd>{{ preview.defaults || '—' }}</dd>
              </div>
              <div class="summary__row">
                <dt>品牌资产</dt>
                <dd>{{ (preview.assets ?? []).length > 0 ? (preview.assets ?? []).join('；') : '未定义' }}</dd>
              </div>
            </dl>
          </section>

          <section
            v-if="preview && ((preview.compliance ?? []).length > 0 || (preview.warnings ?? []).length > 0)"
            class="ui-card block"
          >
            <h4 class="ui-h3">合规检查</h4>
            <p v-for="(item, index) in preview.compliance ?? []" :key="`c${index}`" class="ui-notice ui-notice--error">
              {{ item }}（命中违禁词会阻止任务进入流水线）
            </p>
            <p v-for="(item, index) in preview.warnings ?? []" :key="`w${index}`" class="ui-notice ui-notice--warn">
              {{ item }}
            </p>
          </section>

          <section v-if="selected.tags.length > 0" class="ui-card block">
            <h4 class="ui-h3">标签</h4>
            <div class="tags">
              <span v-for="tag in selected.tags" :key="tag" class="tag tag--static">{{ tag }}</span>
            </div>
          </section>
        </template>

        <div v-else class="detail__empty">
          <p class="empty__title">选择一个模板查看预览</p>
          <p class="empty__desc">
            左侧列表按作用域与标签检索模板；右侧展示开场白、风格词表、人物图文与发布档的最小样。
          </p>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.library {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  min-height: 0;
}

.library__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.library__title {
  font-size: 16px;
  font-weight: 600;
}

.library__subtitle {
  max-width: 680px;
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.library__stats {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.library__body {
  display: grid;
  grid-template-columns: minmax(240px, 320px) minmax(0, 1fr);
  gap: 14px;
  flex: 1;
  min-height: 0;
}

.list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.list__filters {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.list__empty {
  padding: 18px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.tag {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 9px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.03);
  color: var(--color-text-muted);
  font: inherit;
  font-size: 11.5px;
  cursor: pointer;
}

.tag--active {
  border-color: rgba(255, 180, 58, 0.6);
  background: rgba(255, 180, 58, 0.12);
  color: var(--color-accent);
}

.tag--static {
  cursor: default;
}

.tag__count {
  font-family: var(--font-mono);
  opacity: 0.7;
}

.item {
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

.item:hover {
  border-color: rgba(255, 255, 255, 0.2);
  background: rgba(255, 255, 255, 0.05);
}

.item--active {
  border-color: rgba(255, 180, 58, 0.55);
  background: rgba(255, 180, 58, 0.08);
}

.item__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.item__name {
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.item__meta {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--color-text-muted);
}

.item__version {
  font-family: var(--font-mono);
}

.item__desc {
  font-size: 11.5px;
  line-height: 1.6;
  color: var(--color-text-muted);
}

.detail {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.detail__head {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px;
}

.detail__title-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.detail__title {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 15px;
  font-weight: 600;
}

.detail__desc {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.detail__actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
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

.empty__title {
  font-size: 14px;
  font-weight: 600;
}

.empty__desc {
  max-width: 400px;
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.block {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 14px;
}

.block__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.grid {
  display: grid;
  gap: 10px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
}

.preview__actions {
  display: flex;
  justify-content: flex-end;
}

.preview__opening {
  padding: 10px 12px;
  border-left: 3px solid rgba(255, 180, 58, 0.7);
  background: rgba(255, 180, 58, 0.06);
  font-size: 13px;
  line-height: 1.8;
}

.bullets {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
  list-style: disc;
}

.summary {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.summary__row {
  display: grid;
  grid-template-columns: 96px minmax(0, 1fr);
  gap: 10px;
  font-size: 12px;
  line-height: 1.7;
}

.summary__row dt {
  color: var(--color-text-muted);
}

.summary__row dd {
  min-width: 0;
  word-break: break-word;
}

.hint {
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

@media (max-width: 1000px) {
  .library__body {
    grid-template-columns: 1fr;
    grid-template-rows: minmax(0, 220px) minmax(0, 1fr);
  }
}
</style>
