/**
 * 视频模板（Template）数据结构，与 Go 侧 template_models.go 的 JSON 字段一一对应。
 * 模板只提供默认值：选中模板会预填 RunConfig，三道确认关卡与各字段仍可改。
 */

export type TemplateScope = 'builtin' | 'team' | 'personal'

/** 字段来源：模板带入 / 本次改动 / 默认值 */
export type TemplateSource = 'template' | 'override' | 'default'

export interface TemplateStyle {
  /** 受控词表：lighting / grade / lens / camera / texture / mood */
  tokens: Record<string, string>
  /** 按模型的 prompt_style 分装的风格片段：kling / veo / runway / generic */
  promptSnippets: Record<string, string>
}

export interface TemplateCharacter {
  roleId: string
  name: string
  images: string[]
  appearanceBlock: string
  voiceHint: string
  perspective: string
}

export interface TemplateOpening {
  variants: string[]
  vars: string[]
  speechRate: number
}

export interface TemplateSubtitleStyle {
  fontName: string
  fontSizePct: number | null
  primaryColor: string
  outlineColor: string
  outlineWidth: number | null
  alignment: number
  marginVpct: number | null
  bold: boolean | null
}

export interface TemplatePublishDefaults {
  platform: string
  platforms: string[]
  region: string
  vertical: string
}

export interface TemplateDefaults {
  aspectRatio: string
  durationS: number
  fps: number
  speechRate: number | null
  continuationMode: string
  language: string
}

export interface TemplateAssets {
  logo: string
  watermark: string
  bgm: string
}

export interface VideoTemplate {
  id: string
  name: string
  version: string
  scope: TemplateScope
  description: string
  tags: string[]
  style: TemplateStyle
  characters: TemplateCharacter[]
  opening: TemplateOpening
  subtitleStyle: TemplateSubtitleStyle
  publishDefaults: TemplatePublishDefaults
  defaults: TemplateDefaults
  assets: TemplateAssets
  createdAt: string
  updatedAt: string
}

/** 任务对模板的引用：锁版本 + 记录本次显式改动 + 开场白变量 */
export interface VideoTemplateRef {
  id: string
  version: string
  /** 键为 RunConfig 字段名（如 durationS / aspectRatio），值非空表示本次改过 */
  overrides: Record<string, string>
  openingVars: Record<string, string>
}

/** 任务内锁定的模板快照（可复现、可审计） */
export interface VideoTemplateRun extends VideoTemplate {
  sources: Record<string, string> | null
  overrides: string[] | null
  openingLine: string
  openingAll: string[] | null
  styleSnippet: string
  compliance: string[] | null
  note: string
}

/** 选模板时的「预览与最小样」 */
export interface VideoTemplatePreview {
  opening: string
  openingAll: string[] | null
  styleSummary: string
  styleSnippet: string
  styleTokens: Record<string, string> | null
  characters: string[] | null
  subtitle: string
  publish: string
  defaults: string
  assets: string[] | null
  compliance: string[] | null
  warnings: string[] | null
}

export const TEMPLATE_SCOPE_LABELS: Record<TemplateScope, string> = {
  builtin: '内置',
  team: '团队',
  personal: '个人',
}

export const TEMPLATE_SOURCE_LABELS: Record<string, string> = {
  template: '模板',
  override: '本次改动',
  default: '默认',
}

/** 字段名 → 中文标签，任务视图与来源标注共用 */
export const TEMPLATE_FIELD_LABELS: Record<string, string> = {
  durationS: '时长',
  fps: '帧率',
  aspectRatio: '画幅',
  continuationMode: '衔接方式',
  language: '语言',
  speechRate: '语速',
  'videoParams.style': '风格',
  'videoParams.camera': '运镜',
  'videoParams.mood': '情绪',
  'videoParams.extraPrompt': '额外提示',
  'subtitleStyle.fontName': '字幕字体',
  'subtitleStyle.fontSizePct': '字幕字号',
  'subtitleStyle.primaryColor': '字幕主色',
  'subtitleStyle.outlineColor': '字幕描边色',
  'subtitleStyle.outlineWidth': '字幕描边宽',
  'subtitleStyle.alignment': '字幕位置',
  'subtitleStyle.marginVpct': '字幕边距',
  'subtitleStyle.bold': '字幕加粗',
  platforms: '发布平台',
  region: '受众时区',
  vertical: '垂类',
  characterRefs: '角色参考',
}

/** 风格 token 的受控键，编辑区按固定顺序展示 */
export const TEMPLATE_TOKEN_KEYS: { key: string; label: string; placeholder: string }[] = [
  { key: 'lighting', label: '光线方向', placeholder: '侧逆光 45°，暖调反光板补面光' },
  { key: 'grade', label: '色调 grade', placeholder: '暖琥珀色调，暗部保留细节' },
  { key: 'lens', label: '镜头焦段', placeholder: '35mm 定焦，f/2.0 浅景深' },
  { key: 'camera', label: '运镜', placeholder: '手持轻微呼吸感，缓慢推近' },
  { key: 'texture', label: '胶片质感', placeholder: '细颗粒胶片质感' },
  { key: 'mood', label: '情绪', placeholder: '温暖、松弛、有食欲' },
]

/** 风格片段的模型槽位 */
export const TEMPLATE_SNIPPET_KEYS: { key: string; label: string }[] = [
  { key: 'kling', label: '可灵 / kling' },
  { key: 'veo', label: 'Veo' },
  { key: 'runway', label: 'Runway' },
  { key: 'generic', label: '通用' },
]

export function createEmptyTemplate(scope: TemplateScope = 'personal'): VideoTemplate {
  const tokens: Record<string, string> = {}
  for (const item of TEMPLATE_TOKEN_KEYS) {
    tokens[item.key] = ''
  }
  const snippets: Record<string, string> = {}
  for (const item of TEMPLATE_SNIPPET_KEYS) {
    snippets[item.key] = ''
  }
  return {
    id: '',
    name: '',
    version: '1.0',
    scope,
    description: '',
    tags: [],
    style: { tokens, promptSnippets: snippets },
    characters: [],
    opening: { variants: [''], vars: [], speechRate: 0 },
    subtitleStyle: {
      fontName: '',
      fontSizePct: null,
      primaryColor: '',
      outlineColor: '',
      outlineWidth: null,
      alignment: 0,
      marginVpct: null,
      bold: null,
    },
    publishDefaults: { platform: '', platforms: [], region: '', vertical: '' },
    defaults: {
      aspectRatio: '',
      durationS: 0,
      fps: 0,
      speechRate: null,
      continuationMode: '',
      language: '',
    },
    assets: { logo: '', watermark: '', bgm: '' },
    createdAt: '',
    updatedAt: '',
  }
}

/**
 * 把已有模板复制成可编辑草稿。
 * 「另存为」时清空 ID，并把 scope 从 builtin 降为 personal——内置作用域由后端保留，
 * 直接提交会得到「内置作用域由系统保留」的报错。
 */
export function cloneTemplate(template: VideoTemplate, options?: { keepId?: boolean }): VideoTemplate {
  const keepId = options?.keepId === true
  const scope: TemplateScope = !keepId && template.scope === 'builtin' ? 'personal' : template.scope
  return {
    ...template,
    id: keepId ? template.id : '',
    scope,
    style: {
      tokens: { ...template.style?.tokens },
      promptSnippets: { ...template.style?.promptSnippets },
    },
    characters: (template.characters ?? []).map((item) => ({ ...item, images: [...(item.images ?? [])] })),
    opening: {
      variants: [...(template.opening?.variants ?? [])],
      vars: [...(template.opening?.vars ?? [])],
      speechRate: template.opening?.speechRate ?? 0,
    },
    tags: [...(template.tags ?? [])],
  }
}

export function templateScopeClass(scope: string): string {
  switch (scope) {
    case 'builtin':
      return 'ui-chip--ok'
    case 'team':
      return 'ui-chip--warn'
    default:
      return ''
  }
}

export function templateSourceClass(source: string): string {
  switch (source) {
    case 'template':
      return 'src--template'
    case 'override':
      return 'src--override'
    default:
      return 'src--default'
  }
}
