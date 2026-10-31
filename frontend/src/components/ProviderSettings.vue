<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  PROVIDER_CATEGORIES,
  createEmptyDraft,
  type Provider,
  type ProviderCategory,
  type ProviderDraft,
} from '../types/provider'

const activeCategory = ref<ProviderCategory>('text')
const providers = ref<Provider[]>([])
const loading = ref(false)
const loadError = ref('')
const feedback = ref<{ tone: 'ok' | 'error'; text: string } | null>(null)

const formOpen = ref(false)
const saving = ref(false)
const fetching = ref(false)
const formError = ref('')
const formNotice = ref('')
const modelOptions = ref<string[]>([])
const form = reactive<ProviderDraft>(createEmptyDraft('text'))

const pendingDeleteId = ref('')

const activeCategoryMeta = computed(() =>
  PROVIDER_CATEGORIES.find((category) => category.id === activeCategory.value),
)
const visibleProviders = computed(() =>
  providers.value.filter((provider) => provider.category === activeCategory.value),
)

function countByCategory(category: ProviderCategory): number {
  return providers.value.filter((provider) => provider.category === category).length
}

async function loadProviders() {
  loading.value = true
  loadError.value = ''
  try {
    providers.value = await backend.listProviders()
  } catch (error) {
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

onMounted(loadProviders)

function resetForm() {
  Object.assign(form, createEmptyDraft(activeCategory.value))
  modelOptions.value = []
  formError.value = ''
  formNotice.value = ''
}

function openCreate() {
  resetForm()
  feedback.value = null
  pendingDeleteId.value = ''
  formOpen.value = true
}

function openEdit(provider: Provider) {
  Object.assign(form, {
    id: provider.id,
    category: provider.category,
    name: provider.name,
    baseUrl: provider.baseUrl,
    apiKey: provider.apiKey,
    model: provider.model,
  })
  modelOptions.value = provider.model ? [provider.model] : []
  formError.value = ''
  formNotice.value = ''
  feedback.value = null
  pendingDeleteId.value = ''
  formOpen.value = true
}

function closeForm() {
  formOpen.value = false
  modelOptions.value = []
  formError.value = ''
  formNotice.value = ''
}

async function handleFetchModels() {
  formError.value = ''
  formNotice.value = ''
  fetching.value = true
  try {
    const models = await backend.fetchModels(form.baseUrl, form.apiKey)
    modelOptions.value = models
    formNotice.value = `已获取 ${models.length} 个模型，点击下方模型名即填入`
  } catch (error) {
    formError.value = errorText(error)
  } finally {
    fetching.value = false
  }
}

async function handleSubmit() {
  formError.value = ''
  formNotice.value = ''
  form.category = activeCategory.value
  saving.value = true
  try {
    const saved = await backend.saveProvider({ ...form })
    activeCategory.value = saved.category
    await loadProviders()
    feedback.value = { tone: 'ok', text: `已保存「${saved.name}」` }
    closeForm()
  } catch (error) {
    formError.value = errorText(error)
  } finally {
    saving.value = false
  }
}

async function handleDelete(provider: Provider) {
  try {
    await backend.deleteProvider(provider.id)
    await loadProviders()
    feedback.value = { tone: 'ok', text: `已删除「${provider.name}」` }
  } catch (error) {
    feedback.value = { tone: 'error', text: errorText(error) }
  } finally {
    pendingDeleteId.value = ''
  }
}

/** API Key 只做展示用途的掩码，避免整屏明文 */
function maskKey(apiKey: string): string {
  if (apiKey.length <= 8) {
    return '••••••••'
  }
  return `${apiKey.slice(0, 4)}••••${apiKey.slice(-4)}`
}
</script>

<template>
  <section class="providers">
    <header class="providers__head">
      <div>
        <h2 class="providers__title">提供商</h2>
        <p class="providers__subtitle">
          共 {{ providers.length }} 条 · API Key 与配置保存在本地 SQLite
        </p>
      </div>
      <button class="btn btn--primary" type="button" @click="openCreate">+ 新增提供商</button>
    </header>

    <div class="cats" role="tablist" aria-label="提供商分类">
      <button
        v-for="category in PROVIDER_CATEGORIES"
        :key="category.id"
        type="button"
        role="tab"
        class="cats__item"
        :class="{ 'cats__item--active': category.id === activeCategory }"
        :aria-selected="category.id === activeCategory"
        @click="activeCategory = category.id"
      >
        {{ category.label }}
        <span class="cats__count">{{ countByCategory(category.id) }}</span>
      </button>
    </div>
    <p class="cats__hint">{{ activeCategoryMeta?.hint }}</p>

    <p v-if="feedback" class="notice" :class="`notice--${feedback.tone}`">{{ feedback.text }}</p>
    <p v-if="loadError" class="notice notice--error">{{ loadError }}</p>

    <form v-if="formOpen" class="form" @submit.prevent="handleSubmit">
      <div class="form__grid">
        <label class="field">
          <span class="field__label">显示名称</span>
          <input v-model="form.name" class="field__input" type="text" placeholder="留空则使用域名" />
        </label>

        <label class="field">
          <span class="field__label">Base URL *</span>
          <input
            v-model="form.baseUrl"
            class="field__input"
            type="text"
            placeholder="https://api.deepseek.com"
            spellcheck="false"
            required
          />
          <span class="field__hint">
            填到版本段即可（如 https://ark.cn-beijing.volces.com/api/v3）；粘贴成完整接口地址（…/chat/completions、…/responses）保存时会自动收敛
          </span>
        </label>

        <label class="field">
          <span class="field__label">API Key *</span>
          <input
            v-model="form.apiKey"
            class="field__input"
            type="password"
            placeholder="sk-..."
            autocomplete="off"
            required
          />
        </label>

        <label class="field">
          <span class="field__label">模型名称 *</span>
          <span class="field__row">
            <input
              v-model="form.model"
              class="field__input"
              type="text"
              placeholder="先获取模型，或手动输入"
              spellcheck="false"
              required
            />
            <button class="btn" type="button" :disabled="fetching" @click="handleFetchModels">
              {{ fetching ? '获取中…' : '获取模型' }}
            </button>
          </span>
        </label>
      </div>

      <!-- 模型候选：用主题化面板代替原生 datalist，避免弹出层白底与深色页面割裂 -->
      <div v-if="modelOptions.length > 0" class="models">
        <span class="models__hint">共 {{ modelOptions.length }} 个可选模型，点击即填入</span>
        <div class="models__list">
          <button
            v-for="model in modelOptions"
            :key="model"
            class="models__item"
            :class="{ 'models__item--active': model === form.model }"
            type="button"
            @click="form.model = model"
          >
            {{ model }}
          </button>
        </div>
      </div>

      <p v-if="formError" class="notice notice--error">{{ formError }}</p>
      <p v-if="formNotice" class="notice notice--ok">{{ formNotice }}</p>

      <div class="form__actions">
        <button class="btn btn--primary" type="submit" :disabled="saving">
          {{ saving ? '保存中…' : form.id ? '保存修改' : '保存' }}
        </button>
        <button class="btn" type="button" @click="closeForm">取消</button>
        <span class="form__tip">按当前分类「{{ activeCategoryMeta?.label }}」保存</span>
      </div>
    </form>

    <div v-if="loading" class="empty">正在读取本地配置…</div>
    <div v-else-if="visibleProviders.length === 0" class="empty">
      <p class="empty__title">「{{ activeCategoryMeta?.label }}」还没有提供商</p>
      <p class="empty__hint">
        点击右上角「新增提供商」，填写 Base URL 与 API Key 后可用「获取模型」拉取模型列表。
      </p>
    </div>

    <ul v-else class="list">
      <li v-for="provider in visibleProviders" :key="provider.id" class="card">
        <div class="card__main">
          <p class="card__name">{{ provider.name }}</p>
          <p class="card__url">{{ provider.baseUrl }}</p>
          <div class="card__meta">
            <span class="tag">{{ provider.model }}</span>
            <span class="card__key">{{ maskKey(provider.apiKey) }}</span>
          </div>
        </div>

        <div class="card__actions">
          <template v-if="pendingDeleteId === provider.id">
            <button class="btn btn--danger" type="button" @click="handleDelete(provider)">
              确认删除
            </button>
            <button class="btn" type="button" @click="pendingDeleteId = ''">取消</button>
          </template>
          <template v-else>
            <button class="btn" type="button" @click="openEdit(provider)">编辑</button>
            <button class="btn" type="button" @click="pendingDeleteId = provider.id">删除</button>
          </template>
        </div>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.providers {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding-right: 4px;
}

.providers__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.providers__title {
  font-size: 16px;
  font-weight: 600;
}

.providers__subtitle {
  margin-top: 4px;
  font-size: 12px;
  color: var(--color-text-muted);
}

.cats {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.cats__item {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 6px 14px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: transparent;
  color: var(--color-text-muted);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  transition: color 160ms ease, border-color 160ms ease, background-color 160ms ease;
}

.cats__item:hover {
  color: var(--color-text);
  border-color: rgba(255, 255, 255, 0.18);
}

.cats__item--active {
  border-color: rgba(255, 180, 58, 0.45);
  background: rgba(255, 180, 58, 0.1);
  color: var(--color-accent);
}

.cats__count {
  padding: 0 6px;
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.08);
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 16px;
}

.cats__hint {
  font-size: 12px;
  color: var(--color-text-muted);
}

.notice {
  padding: 8px 12px;
  border-radius: var(--radius-sm);
  font-size: 12px;
  line-height: 1.6;
}

.notice--ok {
  border: 1px solid rgba(88, 214, 141, 0.3);
  background: rgba(88, 214, 141, 0.08);
  color: #7ee2a8;
}

.notice--error {
  border: 1px solid rgba(255, 107, 107, 0.3);
  background: rgba(255, 107, 107, 0.08);
  color: #ff9b9b;
}

.form {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
}

.form__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: 12px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}

.field__label {
  font-size: 12px;
  color: var(--color-text-muted);
}

.field__row {
  display: flex;
  gap: 8px;
  min-width: 0;
}

.field__hint {
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-muted);
}

/* 模型候选面板：深色卡片 + 强调色悬停，替代原生 datalist 白底弹层 */
.models {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.28);
}

.models__hint {
  font-size: 12px;
  color: var(--color-text-muted);
}

.models__list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  max-height: 148px;
  overflow-y: auto;
}

.models__item {
  padding: 4px 10px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.03);
  color: var(--color-text-muted);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  transition: border-color 160ms ease, background-color 160ms ease, color 160ms ease;
}

.models__item:hover {
  border-color: rgba(255, 180, 58, 0.5);
  color: var(--color-accent);
}

.models__item--active {
  border-color: rgba(255, 180, 58, 0.7);
  background: rgba(255, 180, 58, 0.14);
  color: var(--color-accent);
}

.field__input {
  flex: 1;
  min-width: 0;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.28);
  color: var(--color-text);
  font: inherit;
  font-size: 13px;
  transition: border-color 160ms ease;
}

.field__input::placeholder {
  color: rgba(255, 255, 255, 0.28);
}

.field__input:focus {
  border-color: rgba(255, 180, 58, 0.5);
  outline: none;
}

.form__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.form__tip {
  font-size: 11px;
  color: var(--color-text-muted);
}

.btn {
  padding: 7px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.04);
  color: var(--color-text);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  transition: border-color 160ms ease, background-color 160ms ease, color 160ms ease;
}

.btn:hover:not(:disabled) {
  border-color: rgba(255, 255, 255, 0.2);
  background: rgba(255, 255, 255, 0.08);
}

.btn:disabled {
  cursor: not-allowed;
  opacity: 0.55;
}

.btn--primary {
  border-color: rgba(255, 180, 58, 0.45);
  background: rgba(255, 180, 58, 0.14);
  color: var(--color-accent);
}

.btn--primary:hover:not(:disabled) {
  border-color: rgba(255, 180, 58, 0.7);
  background: rgba(255, 180, 58, 0.2);
}

.btn--danger {
  border-color: rgba(255, 107, 107, 0.45);
  background: rgba(255, 107, 107, 0.12);
  color: #ff9b9b;
}

.empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  min-height: 220px;
  padding: 28px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.015);
  color: var(--color-text-muted);
  font-size: 12px;
  text-align: center;
}

.empty__title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
}

.empty__hint {
  max-width: 380px;
  line-height: 1.7;
}

.list {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.card {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 14px 16px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: linear-gradient(180deg, rgba(30, 30, 38, 0.7) 0%, rgba(18, 18, 24, 0.7) 100%);
}

.card__main {
  min-width: 0;
}

.card__name {
  font-size: 14px;
  font-weight: 600;
}

.card__url {
  margin-top: 2px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--color-text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.card__meta {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 8px;
}

.tag {
  padding: 2px 8px;
  border: 1px solid rgba(255, 180, 58, 0.3);
  border-radius: 999px;
  background: rgba(255, 180, 58, 0.08);
  color: var(--color-accent);
  font-family: var(--font-mono);
  font-size: 11px;
}

.card__key {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--color-text-muted);
}

.card__actions {
  display: flex;
  flex: none;
  gap: 8px;
}
</style>
