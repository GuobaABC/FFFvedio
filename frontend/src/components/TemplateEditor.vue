<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import {
  TEMPLATE_SNIPPET_KEYS,
  TEMPLATE_TOKEN_KEYS,
  cloneTemplate,
  createEmptyTemplate,
  type VideoTemplate,
} from '../types/template'
import { REGION_OPTIONS, SUBTITLE_ALIGNMENTS, VERTICAL_OPTIONS, VIDEO_PLATFORMS } from '../types/video'

const props = defineProps<{
  template: VideoTemplate | null
  saving: boolean
  error: string
}>()

const emit = defineEmits<{
  (event: 'save', template: VideoTemplate): void
  (event: 'cancel'): void
}>()

const draft = ref<VideoTemplate>(createEmptyTemplate())
const localError = ref('')

watch(
  () => props.template,
  (template) => {
    draft.value = template ? cloneTemplate(template, { keepId: true }) : createEmptyTemplate()
    localError.value = ''
  },
  { immediate: true },
)

const isNew = computed(() => !draft.value.id)
const title = computed(() => (isNew.value ? '新建模板' : `编辑模板 · ${draft.value.name || draft.value.id}`))

const tagsText = computed({
  get: () => draft.value.tags.join(' '),
  set: (value: string) => {
    draft.value.tags = splitList(value)
  },
})

const varsText = computed({
  get: () => draft.value.opening.vars.join(' '),
  set: (value: string) => {
    draft.value.opening.vars = splitList(value)
  },
})

const boldChoice = computed({
  get: () => {
    const value = draft.value.subtitleStyle.bold
    return value === null || value === undefined ? '' : value ? 'true' : 'false'
  },
  set: (value: string) => {
    draft.value.subtitleStyle.bold = value === '' ? null : value === 'true'
  },
})

function splitList(value: string): string[] {
  return value
    .split(/[\s,，、;；]+/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function splitImages(value: string): string[] {
  return value
    .split(/[;\n]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function numberOrNull(value: unknown): number | null {
  if (value === null || value === undefined || value === '') {
    return null
  }
  const parsed = Number(value)
  return Number.isFinite(parsed) ? parsed : null
}

function addCharacter() {
  draft.value.characters.push({
    roleId: `char_${draft.value.characters.length + 1}`,
    name: '',
    images: [],
    appearanceBlock: '',
    voiceHint: '',
    perspective: '',
  })
}

function removeCharacter(index: number) {
  draft.value.characters.splice(index, 1)
}

function togglePlatform(id: string) {
  const index = draft.value.publishDefaults.platforms.indexOf(id)
  if (index >= 0) {
    draft.value.publishDefaults.platforms.splice(index, 1)
    return
  }
  draft.value.publishDefaults.platforms.push(id)
}

/** 提交前清洗：空串 → null / 空数组，避免 Go 侧指针字段解析失败 */
function buildPayload(): VideoTemplate {
  const source = draft.value
  const tokens: Record<string, string> = {}
  for (const [key, value] of Object.entries(source.style?.tokens ?? {})) {
    const trimmed = String(value ?? '').trim()
    if (trimmed) {
      tokens[key] = trimmed
    }
  }
  const snippets: Record<string, string> = {}
  for (const [key, value] of Object.entries(source.style?.promptSnippets ?? {})) {
    const trimmed = String(value ?? '').trim()
    if (trimmed) {
      snippets[key] = trimmed
    }
  }
  return {
    ...source,
    name: source.name.trim(),
    version: source.version.trim(),
    description: source.description.trim(),
    tags: splitList(source.tags.join(' ')),
    style: { tokens, promptSnippets: snippets },
    characters: source.characters
      .map((item, index) => ({
        roleId: item.roleId.trim() || `char_${index + 1}`,
        name: item.name.trim(),
        images: splitImages(Array.isArray(item.images) ? item.images.join(';') : ''),
        appearanceBlock: item.appearanceBlock.trim(),
        voiceHint: item.voiceHint.trim(),
        perspective: item.perspective.trim(),
      }))
      .filter((item) => item.name || item.appearanceBlock || item.images.length > 0),
    opening: {
      variants: source.opening.variants.map((item) => item.trim()).filter(Boolean),
      vars: splitList(source.opening.vars.join(' ')),
      speechRate: numberOrNull(source.opening.speechRate) ?? 0,
    },
    subtitleStyle: {
      fontName: source.subtitleStyle.fontName.trim(),
      fontSizePct: numberOrNull(source.subtitleStyle.fontSizePct),
      primaryColor: source.subtitleStyle.primaryColor.trim(),
      outlineColor: source.subtitleStyle.outlineColor.trim(),
      outlineWidth: numberOrNull(source.subtitleStyle.outlineWidth),
      alignment: Number(source.subtitleStyle.alignment) || 0,
      marginVpct: numberOrNull(source.subtitleStyle.marginVpct),
      bold: source.subtitleStyle.bold ?? null,
    },
    publishDefaults: {
      platform: '',
      platforms: [...source.publishDefaults.platforms],
      region: source.publishDefaults.region.trim(),
      vertical: source.publishDefaults.vertical.trim(),
    },
    defaults: {
      aspectRatio: source.defaults.aspectRatio,
      durationS: Number(source.defaults.durationS) || 0,
      fps: Number(source.defaults.fps) || 0,
      speechRate: numberOrNull(source.defaults.speechRate),
      continuationMode: source.defaults.continuationMode,
      language: source.defaults.language,
    },
    assets: {
      logo: source.assets.logo.trim(),
      watermark: source.assets.watermark.trim(),
      bgm: source.assets.bgm.trim(),
    },
  }
}

function submit() {
  localError.value = ''
  const payload = buildPayload()
  if (!payload.name) {
    localError.value = '请填写模板名称'
    return
  }
  emit('save', payload)
}

function cancel() {
  emit('cancel')
}
</script>

<template>
  <section class="editor">
    <header class="editor__head">
      <div>
        <h3 class="editor__title">{{ title }}</h3>
        <p class="hint">模板只提供默认值：选中后仍可在新建任务里逐项改，三道确认门照常生效。</p>
      </div>
      <div class="editor__actions">
        <button class="ui-btn" type="button" :disabled="saving" @click="cancel">取消</button>
        <button class="ui-btn ui-btn--primary" type="button" :disabled="saving" @click="submit">
          {{ saving ? '保存中…' : '保存模板' }}
        </button>
      </div>
    </header>

    <p v-if="localError" class="ui-notice ui-notice--error">{{ localError }}</p>
    <p v-if="error" class="ui-notice ui-notice--error">{{ error }}</p>

    <div class="editor__body">
      <section class="ui-card block">
        <h4 class="ui-h3">1. 基础信息</h4>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">模板名称 *</span>
            <input v-model="draft.name" class="ui-input" type="text" placeholder="品牌口播·暖调" />
          </label>
          <label class="ui-field">
            <span class="ui-label">版本</span>
            <input v-model="draft.version" class="ui-input" type="text" placeholder="1.0" />
          </label>
          <label class="ui-field">
            <span class="ui-label">作用域</span>
            <select v-model="draft.scope" class="ui-select">
              <option value="personal">个人</option>
              <option value="team">团队（需审核）</option>
            </select>
          </label>
        </div>
        <label class="ui-field">
          <span class="ui-label">描述</span>
          <input v-model="draft.description" class="ui-input" type="text" placeholder="一句话说明适用垂类与风格" />
        </label>
        <label class="ui-field">
          <span class="ui-label">标签（空格分隔，用于模板库检索）</span>
          <input v-model="tagsText" class="ui-input" type="text" placeholder="品牌 口播 美食 抖音" />
        </label>
      </section>

      <section class="ui-card block">
        <h4 class="ui-h3">2. 视频风格（受控词表）</h4>
        <div class="grid grid--3">
          <label v-for="item in TEMPLATE_TOKEN_KEYS" :key="item.key" class="ui-field">
            <span class="ui-label">{{ item.label }}</span>
            <input v-model="draft.style.tokens[item.key]" class="ui-input" type="text" :placeholder="item.placeholder" />
          </label>
        </div>
        <p class="hint">用「光线 / 色调 / 焦段 / 运镜 / 质感 / 情绪」的可度量描述，避免 cinematic、8K、高级感这类无增益词。</p>
        <div class="grid grid--2">
          <label v-for="item in TEMPLATE_SNIPPET_KEYS" :key="item.key" class="ui-field">
            <span class="ui-label">风格片段 · {{ item.label }}</span>
            <textarea
              v-model="draft.style.promptSnippets[item.key]"
              class="ui-textarea"
              rows="2"
              placeholder="同一份调性，按该模型改写一段固定槽位描述"
            ></textarea>
          </label>
        </div>
      </section>

      <section class="ui-card block">
        <div class="block__head">
          <h4 class="ui-h3">3. 角色插图（图文成对）</h4>
          <button class="ui-btn" type="button" @click="addCharacter">+ 添加角色</button>
        </div>
        <p v-if="draft.characters.length === 0" class="hint">
          不定义角色也可以：模板允许部分定义，主体由主题决定。添加后请同时给「定妆图 + 外观描述块」，不支持的模型会自动降级用文字。
          外观描述可写成猫 / 狗 / 猪 / 鸡等动物主体（可拟人化穿衣、站立），也可写成中性描述；未提供参考图时按此兜底，使用者上传参考图时以图为准。
        </p>
        <div v-for="(character, index) in draft.characters" :key="index" class="character">
          <div class="grid grid--3">
            <label class="ui-field">
              <span class="ui-label">role_id</span>
              <input v-model="character.roleId" class="ui-input" type="text" placeholder="host" />
            </label>
            <label class="ui-field">
              <span class="ui-label">名称</span>
              <input v-model="character.name" class="ui-input" type="text" placeholder="主理人（橘猫）" />
            </label>
            <label class="ui-field">
              <span class="ui-label">声音提示</span>
              <input v-model="character.voiceHint" class="ui-input" type="text" placeholder="温暖中音，4.2 字/秒" />
            </label>
          </div>
          <label class="ui-field">
            <span class="ui-label">外观描述块（可度量特征，作为跨段复制锚点）</span>
            <textarea
              v-model="character.appearanceBlock"
              class="ui-textarea"
              rows="2"
              placeholder="成年橘色短毛猫，圆脸、耳尖一撮白毛，脖子系米白针织围巾，亚麻小围裙挂在胸前"
            ></textarea>
          </label>
          <div class="grid grid--2">
            <label class="ui-field">
              <span class="ui-label">定妆图（多个用 ; 分隔）</span>
              <input
                class="ui-input"
                type="text"
                :value="character.images.join(';')"
                placeholder="C:\assets\host.png"
                @input="character.images = splitImages(($event.target as HTMLInputElement).value)"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">出场约束 / 视角</span>
              <input v-model="character.perspective" class="ui-input" type="text" placeholder="主视角，正对镜头半身" />
            </label>
          </div>
          <button class="ui-btn ui-btn--ghost" type="button" @click="removeCharacter(index)">移除角色</button>
        </div>
      </section>

      <section class="ui-card block">
        <h4 class="ui-h3">4. 默认开场白（shot#1 台词占位）</h4>
        <label v-for="(_, index) in draft.opening.variants" :key="index" class="ui-field">
          <span class="ui-label">变体 {{ index + 1 }}</span>
          <input
            v-model="draft.opening.variants[index]"
            class="ui-input"
            type="text"
            placeholder="{product}不是越贵越好，这三步就能喝出差别"
          />
        </label>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">变量（空格分隔，自动补全花括号占位符）</span>
            <input v-model="varsText" class="ui-input" type="text" placeholder="brand product topic" />
          </label>
          <label class="ui-field">
            <span class="ui-label">建议语速（0 = 不覆盖）</span>
            <input v-model.number="draft.opening.speechRate" class="ui-input" type="number" min="0" max="10" step="0.1" />
          </label>
          <div class="ui-field">
            <span class="ui-label">多样性护栏</span>
            <span class="ui-chip">同模板多任务按使用次数轮换变体</span>
          </div>
        </div>
      </section>

      <section class="ui-card block">
        <h4 class="ui-h3">5. 字幕与发布档</h4>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">字体</span>
            <input v-model="draft.subtitleStyle.fontName" class="ui-input" type="text" placeholder="思源黑体" />
          </label>
          <label class="ui-field">
            <span class="ui-label">字号（画面高 %，空 = 不覆盖）</span>
            <input v-model.number="draft.subtitleStyle.fontSizePct" class="ui-input" type="number" min="1.2" max="12" step="0.2" />
          </label>
          <label class="ui-field">
            <span class="ui-label">位置</span>
            <select v-model.number="draft.subtitleStyle.alignment" class="ui-select">
              <option :value="0">不指定</option>
              <option v-for="item in SUBTITLE_ALIGNMENTS" :key="item.value" :value="item.value">{{ item.label }}</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">主色</span>
            <input v-model="draft.subtitleStyle.primaryColor" class="ui-input" type="text" placeholder="#FFFFFF" />
          </label>
          <label class="ui-field">
            <span class="ui-label">描边色</span>
            <input v-model="draft.subtitleStyle.outlineColor" class="ui-input" type="text" placeholder="#000000" />
          </label>
          <label class="ui-field">
            <span class="ui-label">描边宽度</span>
            <input v-model.number="draft.subtitleStyle.outlineWidth" class="ui-input" type="number" min="0" max="8" step="0.5" />
          </label>
          <label class="ui-field">
            <span class="ui-label">底部边距（%）</span>
            <input v-model.number="draft.subtitleStyle.marginVpct" class="ui-input" type="number" min="0" max="30" step="0.5" />
          </label>
          <label class="ui-field">
            <span class="ui-label">加粗</span>
            <select v-model="boldChoice" class="ui-select">
              <option value="">不指定</option>
              <option value="true">是</option>
              <option value="false">否</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">受众时区</span>
            <select v-model="draft.publishDefaults.region" class="ui-select">
              <option value="">不指定</option>
              <option v-for="item in REGION_OPTIONS" :key="item.id" :value="item.id">{{ item.label }}</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">垂类</span>
            <select v-model="draft.publishDefaults.vertical" class="ui-select">
              <option value="">不指定</option>
              <option v-for="item in VERTICAL_OPTIONS" :key="item" :value="item">{{ item }}</option>
            </select>
          </label>
        </div>
        <div class="ui-field">
          <span class="ui-label">默认发布平台</span>
          <div class="chips">
            <button
              v-for="item in VIDEO_PLATFORMS"
              :key="item.id"
              class="chip"
              :class="{ 'chip--active': draft.publishDefaults.platforms.includes(item.id) }"
              type="button"
              @click="togglePlatform(item.id)"
            >
              {{ item.label }}
            </button>
          </div>
        </div>
      </section>

      <section class="ui-card block">
        <h4 class="ui-h3">6. 流水线默认值</h4>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">画幅</span>
            <select v-model="draft.defaults.aspectRatio" class="ui-select">
              <option value="">不指定</option>
              <option value="16:9">16:9</option>
              <option value="9:16">9:16</option>
              <option value="1:1">1:1</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">时长（秒，0 = 不覆盖）</span>
            <input v-model.number="draft.defaults.durationS" class="ui-input" type="number" min="0" max="600" />
          </label>
          <label class="ui-field">
            <span class="ui-label">帧率（0 = 不覆盖）</span>
            <input v-model.number="draft.defaults.fps" class="ui-input" type="number" min="0" max="60" />
          </label>
          <label class="ui-field">
            <span class="ui-label">语速（空 = 不覆盖）</span>
            <input v-model.number="draft.defaults.speechRate" class="ui-input" type="number" min="1" max="10" step="0.1" />
          </label>
          <label class="ui-field">
            <span class="ui-label">多段衔接</span>
            <select v-model="draft.defaults.continuationMode" class="ui-select">
              <option value="">不指定</option>
              <option value="last_frame">尾帧延续</option>
              <option value="overlap">重叠帧</option>
              <option value="none">硬切</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">语言</span>
            <select v-model="draft.defaults.language" class="ui-select">
              <option value="">不指定</option>
              <option value="zh-CN">中文</option>
              <option value="en-US">English</option>
            </select>
          </label>
        </div>
      </section>

      <section class="ui-card block">
        <h4 class="ui-h3">7. 品牌资产（合成阶段叠加）</h4>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">Logo（本地路径）</span>
            <input v-model="draft.assets.logo" class="ui-input" type="text" placeholder="C:\assets\logo.png" />
          </label>
          <label class="ui-field">
            <span class="ui-label">水印（本地路径，优先于 Logo）</span>
            <input v-model="draft.assets.watermark" class="ui-input" type="text" placeholder="C:\assets\wm.png" />
          </label>
          <label class="ui-field">
            <span class="ui-label">BGM（本地音频）</span>
            <input v-model="draft.assets.bgm" class="ui-input" type="text" placeholder="C:\assets\bgm.mp3" />
          </label>
        </div>
        <p class="hint">水印以画面宽度 16% 贴在右下角；BGM 循环混音至成片时长。文件缺失只提示不阻断成片。</p>
      </section>
    </div>
  </section>
</template>

<style scoped>
.editor {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 0;
}

.editor__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.editor__title {
  font-size: 15px;
  font-weight: 600;
}

.editor__actions {
  display: flex;
  gap: 8px;
}

.editor__body {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding-bottom: 12px;
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

.character {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
  align-self: flex-start;
  width: 100%;
}

.grid {
  display: grid;
  gap: 10px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
}

.grid--3 {
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
}

.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.chip {
  padding: 4px 10px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.03);
  color: var(--color-text-muted);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}

.chip--active {
  border-color: rgba(255, 180, 58, 0.6);
  background: rgba(255, 180, 58, 0.12);
  color: var(--color-accent);
}

.hint {
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}
</style>
