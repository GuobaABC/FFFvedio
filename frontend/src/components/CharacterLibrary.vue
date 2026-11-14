<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'

import { backend, errorText } from '../services/backend'
import { formatClock, type VideoCharacter, type VideoVoiceProfile } from '../types/video'

/**
 * 角色库：保存可跨任务复用的角色档案（外观锚点 / 人设 / 负向约束 / 音色 / 默认定妆图）。
 * 新建任务时选中的档案会被后端快照进任务，之后改角色库不会改写旧任务。
 */

interface DraftCharacter extends Omit<VideoCharacter, 'voiceProfile' | 'images' | 'tags'> {
  voiceProfile: VideoVoiceProfile
  imagesText: string
  tagsText: string
}

const characters = ref<VideoCharacter[]>([])
const selectedId = ref('')
const keyword = ref('')
const loading = ref(false)
const loadError = ref('')
const actionError = ref('')

const editing = ref(false)
const draft = ref<DraftCharacter>(emptyCharacter())
const saving = ref(false)

function emptyCharacter(): DraftCharacter {
  return {
    id: '',
    name: '',
    roleId: '',
    appearanceBlock: '',
    persona: '',
    negativePrompt: '',
    voiceHint: '',
    voiceProfile: { voiceId: '', rate: 0, pitch: 0, volume: 0, emotion: '' },
    perspective: '',
    imagesText: '',
    tagsText: '',
    note: '',
    createdAt: '',
    updatedAt: '',
  }
}

function cloneCharacter(source: VideoCharacter): DraftCharacter {
  return {
    ...source,
    voiceProfile: source.voiceProfile
      ? { ...source.voiceProfile }
      : { voiceId: '', rate: 0, pitch: 0, volume: 0, emotion: '' },
    imagesText: (source.images ?? []).join(';'),
    tagsText: (source.tags ?? []).join('；'),
  }
}

const selected = computed(
  () => characters.value.find((character) => character.id === selectedId.value) ?? null,
)

const filtered = computed(() => {
  const text = keyword.value.trim().toLowerCase()
  if (!text) {
    return characters.value
  }
  return characters.value.filter((character) => {
    const haystack = [
      character.name,
      character.roleId,
      character.appearanceBlock,
      character.persona,
      character.note,
      ...(character.tags ?? []),
    ]
      .join(' ')
      .toLowerCase()
    return haystack.includes(text)
  })
})

function splitList(value: string): string[] {
  return value
    .split(/[;；,\n，]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

async function load(preferId = '') {
  loading.value = true
  try {
    const list = await backend.listVideoCharacters()
    characters.value = list ?? []
    loadError.value = ''

    const target = preferId || selectedId.value
    if (target && characters.value.some((item) => item.id === target)) {
      selectedId.value = target
    } else {
      selectedId.value = characters.value.length > 0 ? characters.value[0].id : ''
    }
  } catch (error) {
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
})

function startCreate() {
  draft.value = emptyCharacter()
  editing.value = true
  actionError.value = ''
}

function startEdit() {
  if (!selected.value) {
    return
  }
  draft.value = cloneCharacter(selected.value)
  editing.value = true
  actionError.value = ''
}

function cancelEdit() {
  editing.value = false
  actionError.value = ''
}

async function save() {
  actionError.value = ''
  const name = draft.value.name.trim()
  if (!name) {
    actionError.value = '请先填写角色名称'
    return
  }
  saving.value = true
  try {
    const { imagesText, tagsText, ...rest } = draft.value
    const voice = rest.voiceProfile
    const hasVoice = voice.voiceId || voice.rate || voice.pitch || voice.volume || voice.emotion
    const payload: VideoCharacter = {
      ...rest,
      name,
      roleId: draft.value.roleId.trim(),
      appearanceBlock: draft.value.appearanceBlock.trim(),
      persona: draft.value.persona.trim(),
      negativePrompt: draft.value.negativePrompt.trim(),
      voiceHint: draft.value.voiceHint.trim(),
      perspective: draft.value.perspective.trim(),
      note: draft.value.note.trim(),
      images: splitList(imagesText),
      tags: splitList(tagsText),
      voiceProfile: hasVoice ? { ...voice } : null,
    }
    const saved = await backend.saveVideoCharacter(payload)
    editing.value = false
    await load(saved.id)
  } catch (error) {
    actionError.value = errorText(error)
  } finally {
    saving.value = false
  }
}

async function removeSelected() {
  const character = selected.value
  if (!character) {
    return
  }
  if (!window.confirm(`删除角色「${character.name}」？已创建的任务仍保留角色快照。`)) {
    return
  }
  actionError.value = ''
  try {
    await backend.deleteVideoCharacter(character.id)
    selectedId.value = ''
    await load()
  } catch (error) {
    actionError.value = errorText(error)
  }
}

async function pickDraftImages() {
  try {
    const files = await backend.pickImageFiles()
    if (files.length > 0) {
      draft.value.imagesText = files.join(';')
    }
  } catch (error) {
    actionError.value = errorText(error)
  }
}

function characterSummary(character: VideoCharacter): string {
  const parts: string[] = []
  if (character.appearanceBlock) {
    parts.push('有外观锚点')
  }
  if (character.persona) {
    parts.push('有人设')
  }
  if ((character.images ?? []).length > 0) {
    parts.push(`${character.images.length} 张定妆图`)
  }
  return parts.join(' · ') || '仅基础信息'
}
</script>

<template>
  <section class="library">
    <header class="library__head">
      <div>
        <h2 class="library__title">角色库</h2>
        <p class="library__subtitle">
          把角色的外观锚点、人设、负向约束、音色与默认定妆图保存下来，新建任务时直接引用。
          任务会保存角色档案快照：之后修改角色库不会影响已经创建的任务。
        </p>
      </div>
      <div class="library__stats">
        <span class="ui-chip">共 {{ characters.length }} 个</span>
        <button class="ui-btn ui-btn--primary" type="button" @click="startCreate">+ 新建角色</button>
        <button class="ui-btn" type="button" :disabled="loading" @click="load()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>
    <p v-if="actionError" class="ui-notice ui-notice--error">{{ actionError }}</p>

    <div class="library__body">
      <aside class="list">
        <input v-model="keyword" class="ui-input" type="text" placeholder="搜索角色名 / 外观 / 标签" />
        <p v-if="filtered.length === 0 && !loading" class="list__empty">
          角色库还是空的。点右上角「+ 新建角色」，把常驻角色沉淀下来，后续任务可直接复用。
        </p>
        <button
          v-for="character in filtered"
          :key="character.id"
          type="button"
          class="item"
          :class="{ 'item--active': character.id === selectedId }"
          @click="selectedId = character.id"
        >
          <span class="item__top">
            <span class="item__name" :title="character.name">{{ character.name }}</span>
            <span class="ui-chip">{{ character.roleId }}</span>
          </span>
          <span class="item__desc">{{ characterSummary(character) }}</span>
          <span class="item__meta">{{ formatClock(character.updatedAt) }}</span>
        </button>
      </aside>

      <div class="detail">
        <form v-if="editing" class="ui-card block" @submit.prevent="save">
          <div class="block__head">
            <h3 class="ui-h3">{{ draft.id ? '编辑角色' : '新建角色' }}</h3>
            <div class="row">
              <button class="ui-btn ui-btn--primary" type="submit" :disabled="saving">
                {{ saving ? '保存中…' : '保存角色' }}
              </button>
              <button class="ui-btn ui-btn--ghost" type="button" @click="cancelEdit">取消</button>
            </div>
          </div>

          <div class="grid grid--2">
            <label class="ui-field">
              <span class="ui-label">角色名（必填）</span>
              <input v-model="draft.name" class="ui-input" type="text" placeholder="如 橘座（猫）" />
            </label>
            <label class="ui-field">
              <span class="ui-label">分镜角色标识（role_id，可留空自动生成）</span>
              <input v-model="draft.roleId" class="ui-input" type="text" placeholder="host" />
            </label>
          </div>

          <label class="ui-field">
            <span class="ui-label">外观锚点（会写进每段提示词，建议 2 个以上可辨识特征）</span>
            <textarea
              v-model="draft.appearanceBlock"
              class="ui-textarea"
              rows="3"
              placeholder="成年橘色短毛猫，圆脸，耳尖一小撮白毛，脖子系米白色针织围巾"
            ></textarea>
          </label>

          <label class="ui-field">
            <span class="ui-label">人设 / 口吻（只影响台词与表演，不进逐段画面）</span>
            <textarea
              v-model="draft.persona"
              class="ui-textarea"
              rows="2"
              placeholder="慢热、嘴硬心软；说话短句，不堆成语"
            ></textarea>
          </label>

          <label class="ui-field">
            <span class="ui-label">角色负向约束（全片避开，如外貌禁忌）</span>
            <textarea
              v-model="draft.negativePrompt"
              class="ui-textarea"
              rows="2"
              placeholder="不要出现项圈铃铛；不要人类手指"
            ></textarea>
          </label>

          <div class="grid grid--2">
            <label class="ui-field">
              <span class="ui-label">声音提示（一行，给原生音频模型参考）</span>
              <input v-model="draft.voiceHint" class="ui-input" type="text" placeholder="温暖中音，4.2 字/秒" />
            </label>
            <label class="ui-field">
              <span class="ui-label">视角约定</span>
              <input v-model="draft.perspective" class="ui-input" type="text" placeholder="主视角，正对镜头半身" />
            </label>
          </div>

          <div class="grid grid--3">
            <label class="ui-field">
              <span class="ui-label">音色 ID（TTS 预留）</span>
              <input
                v-model="draft.voiceProfile.voiceId"
                class="ui-input"
                type="text"
                placeholder="warm-male"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">语速倍率（1 = 正常）</span>
              <input v-model.number="draft.voiceProfile.rate" class="ui-input" type="number" step="0.1" min="0.5" max="2" />
            </label>
            <label class="ui-field">
              <span class="ui-label">情绪</span>
              <input v-model="draft.voiceProfile.emotion" class="ui-input" type="text" placeholder="calm" />
            </label>
          </div>

          <label class="ui-field">
            <span class="ui-label">默认定妆图（多个用 ; 分隔）</span>
            <div class="row">
              <input
                v-model="draft.imagesText"
                class="ui-input"
                type="text"
                spellcheck="false"
                placeholder="参考图路径，多个用 ; 分隔"
              />
              <button class="ui-btn" type="button" @click="pickDraftImages">选择…</button>
            </div>
          </label>

          <label class="ui-field">
            <span class="ui-label">标签（顿号 / 逗号分隔，用于检索）</span>
            <input v-model="draft.tagsText" class="ui-input" type="text" placeholder="主角、猫、咖啡馆系列" />
          </label>

          <label class="ui-field">
            <span class="ui-label">备注</span>
            <input v-model="draft.note" class="ui-input" type="text" placeholder="仅自己可见的说明" />
          </label>
        </form>

        <template v-else-if="selected">
          <section class="ui-card detail__head">
            <div class="detail__title-row">
              <div>
                <h3 class="detail__title">
                  {{ selected.name }}
                  <span class="ui-chip">{{ selected.roleId }}</span>
                  <span v-if="selected.voiceHint" class="ui-chip ui-chip--ok">{{ selected.voiceHint }}</span>
                </h3>
                <p v-if="selected.note" class="detail__desc">{{ selected.note }}</p>
              </div>
              <div class="detail__actions">
                <button class="ui-btn ui-btn--primary" type="button" @click="startEdit">编辑</button>
                <button class="ui-btn ui-btn--danger" type="button" @click="removeSelected">删除</button>
              </div>
            </div>
            <p class="hint">
              新建任务时在「角色参考图」区选择本角色即可复用；任务会保存档案快照，删除角色不会影响已创建的任务。
            </p>
          </section>

          <section class="ui-card block">
            <h4 class="ui-h3">外观锚点</h4>
            <p class="detail__block">{{ selected.appearanceBlock || '（未填写，模型只能靠定妆图保持形象）' }}</p>
          </section>

          <section v-if="selected.persona || selected.negativePrompt" class="ui-card block">
            <h4 class="ui-h3">人设与约束</h4>
            <dl class="summary">
              <div v-if="selected.persona" class="summary__row">
                <dt>人设 / 口吻</dt>
                <dd>{{ selected.persona }}</dd>
              </div>
              <div v-if="selected.negativePrompt" class="summary__row">
                <dt>角色负向约束</dt>
                <dd>{{ selected.negativePrompt }}</dd>
              </div>
            </dl>
          </section>

          <section class="ui-card block">
            <h4 class="ui-h3">定妆图与标签</h4>
            <p class="detail__desc">
              定妆图共 {{ (selected.images ?? []).length }} 张；未在任务里另选图片时，会自动使用这里的图。
            </p>
            <p v-if="(selected.images ?? []).length > 0" class="detail__block">{{ selected.images.join('\n') }}</p>
            <div v-if="(selected.tags ?? []).length > 0" class="tags">
              <span v-for="tag in selected.tags" :key="tag" class="ui-chip">#{{ tag }}</span>
            </div>
            <p v-if="selected.voiceProfile" class="hint">
              音色：{{ selected.voiceProfile.voiceId || '未命名' }} / 语速 {{ selected.voiceProfile.rate || 1 }}x
              / 情绪 {{ selected.voiceProfile.emotion || '未设置' }}
            </p>
          </section>
        </template>

        <div v-else class="ui-card detail__empty">
          <p class="empty__title">选择左侧角色查看档案</p>
          <p class="empty__desc">
            角色库保存的是可复用设定；新建任务时选中角色，后端会把档案与默认参考图快照进任务。
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
  max-width: 720px;
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

.list__empty {
  padding: 18px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-md);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
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
  font-size: 11px;
  color: var(--color-text-muted);
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

.detail__block {
  font-size: 12px;
  line-height: 1.8;
  white-space: pre-wrap;
  color: var(--color-text);
}

.detail__empty {
  padding: 22px;
}

.empty__title {
  font-size: 14px;
  font-weight: 600;
}

.empty__desc {
  margin-top: 6px;
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.grid {
  display: grid;
  gap: 10px;
}

.grid--2 {
  grid-template-columns: repeat(2, minmax(0, 1fr));
}

.grid--3 {
  grid-template-columns: repeat(3, minmax(0, 1fr));
}

.summary {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.summary__row {
  display: grid;
  grid-template-columns: 110px minmax(0, 1fr);
  gap: 8px;
  font-size: 12px;
  line-height: 1.7;
}

.summary__row dt {
  color: var(--color-text-muted);
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 8px;
}

@media (max-width: 900px) {
  .library__body {
    grid-template-columns: minmax(0, 1fr);
  }

  .grid--2,
  .grid--3 {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>