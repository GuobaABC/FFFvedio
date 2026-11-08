<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'

import { consumeCrawlDraft, type CrawlVideoDraft } from '../composables/crawlNav'
import { consumePendingTemplate } from '../composables/templateNav'
import { videoNav } from '../composables/videoNav'
import { backend, errorText } from '../services/backend'
import type { Provider } from '../types/provider'
import {
  TEMPLATE_FIELD_LABELS,
  TEMPLATE_SCOPE_LABELS,
  TEMPLATE_SOURCE_LABELS,
  templateScopeClass,
  templateSourceClass,
  type VideoTemplate,
  type VideoTemplatePreview,
  type VideoTemplateRef,
} from '../types/template'
import {
  CONTINUATION_LABELS,
  MANUAL_SHOT_MAX_S,
  MANUAL_SHOT_MIN_S,
  REGION_OPTIONS,
  SUBTITLE_ALIGNMENTS,
  VERTICAL_OPTIONS,
  VIDEO_PLATFORMS,
  createEmptyVideoConfig,
  createManualShot,
  formatClock,
  type ScriptMode,
  type TailFrameAsset,
  type VideoCharacter,
  type VideoDialogueLine,
  type VideoEnvironment,
  type VideoProject,
  type VideoRunConfig,
  type VideoShot,
} from '../types/video'

const emit = defineEmits<{ (event: 'created', taskId: string): void }>()

const form = reactive<VideoRunConfig>(createEmptyVideoConfig())
const title = ref('')
const submitting = ref(false)
const submitError = ref('')
const loadError = ref('')
const providers = ref<Provider[]>([])
const environment = ref<VideoEnvironment | null>(null)

/* ---------------- 所属项目：任务归组，项目视图按它聚合 ---------------- */
const projects = ref<VideoProject[]>([])
const projectId = ref('')
/** 项目视图「用为新建首帧」交接来的尾帧路径提示 */
const tailHandoffHint = ref('')

/** 从「热点爬取 → 爬取结果」带入的素材草稿（只消费一次） */
const sourceDraft = ref<CrawlVideoDraft | null>(null)

/** 角色参考图以「; 分隔的路径文本」编辑，提交时再转成 images 数组 */
const characterImagesText = ref<string[]>([])
/** 模板带进来的那一份图路径：用来区分「模板定妆图」与「我自己选的图」 */
const templateCharacterImages = ref<string[]>([])
const characterUploadNote = ref('')

/* ---------------- 角色库：跨任务复用角色档案 ---------------- */
const characterLibrary = ref<VideoCharacter[]>([])
const characterLibraryError = ref('')
const characterLibraryId = ref('')

async function loadCharacterLibrary() {
  try {
    characterLibrary.value = await backend.listVideoCharacters()
    characterLibraryError.value = ''
  } catch (error) {
    characterLibrary.value = []
    characterLibraryError.value = errorText(error)
  }
}

/** 把角色库档案加入本任务：留空字段由后端再按角色库补全，这里先填好便于确认。 */
function addCharacterFromLibrary() {
  const character = characterLibrary.value.find((item) => item.id === characterLibraryId.value)
  if (!character) {
    return
  }
  form.characterRefs.push({
    characterId: character.id,
    roleId: character.roleId || character.id,
    name: character.name,
    images: [...(character.images ?? [])],
    imageSource: (character.images ?? []).length > 0 ? 'user' : '',
    appearanceBlock: character.appearanceBlock,
    persona: character.persona,
    negativePrompt: character.negativePrompt,
    perspective: character.perspective,
    voiceHint: character.voiceHint,
    voiceProfile: character.voiceProfile ? { ...character.voiceProfile } : null,
  })
  characterImagesText.value.push((character.images ?? []).join(';'))
  // 角色库的图是用户维护的权威定妆图，不参与「模板图被覆盖」的提示
  templateCharacterImages.value.push('')
  syncRoleIDSnapshot()
  characterUploadNote.value = `已从角色库加入「${character.name}」：外观锚点、人设、负向约束与音色会随任务一起快照。`
}

/** 把当前任务里的角色设定存回角色库，便于下次直接复用。 */
async function saveCharacterToLibrary(index: number) {
  const ref = form.characterRefs[index]
  if (!ref || !ref.name.trim()) {
    characterLibraryError.value = '保存到角色库前请先填写角色名称'
    return
  }
  try {
    const saved = await backend.saveVideoCharacter({
      id: ref.characterId ?? '',
      name: ref.name.trim(),
      roleId: ref.roleId.trim(),
      appearanceBlock: (ref.appearanceBlock ?? '').trim(),
      persona: (ref.persona ?? '').trim(),
      negativePrompt: (ref.negativePrompt ?? '').trim(),
      voiceHint: ref.voiceHint.trim(),
      voiceProfile: ref.voiceProfile ?? null,
      perspective: (ref.perspective ?? '').trim(),
      images: splitImages(characterImagesText.value[index] ?? ''),
      tags: [],
      note: '',
      createdAt: '',
      updatedAt: '',
    })
    ref.characterId = saved.id
    await loadCharacterLibrary()
    characterUploadNote.value = `角色「${saved.name}」已保存到角色库，下次新建任务可直接选择。`
  } catch (error) {
    characterLibraryError.value = errorText(error)
  }
}

/* ---------------- 尾帧资产：历史任务的尾帧可直接选作本任务首帧 ---------------- */
const tailAssets = ref<TailFrameAsset[]>([])
const showTailAssets = ref(false)
const tailAssetsError = ref('')

async function loadTailAssets() {
  try {
    tailAssets.value = await backend.listTailFrameAssets()
    tailAssetsError.value = ''
  } catch (error) {
    tailAssets.value = []
    tailAssetsError.value = errorText(error)
  }
}

async function toggleTailAssetPicker() {
  showTailAssets.value = !showTailAssets.value
  if (showTailAssets.value) {
    await loadTailAssets()
  }
}

function useTailFrame(asset: TailFrameAsset) {
  form.firstFrame = asset.imagePath
  showTailAssets.value = false
}

async function removeTailAsset(assetId: string) {
  try {
    await backend.deleteTailFrameAsset(assetId)
    await loadTailAssets()
  } catch (error) {
    tailAssetsError.value = errorText(error)
  }
}

/* ---------------- 模板：只预填默认值，不绕过三道确认门 ---------------- */

/** 模板可覆盖的字段白名单与展示顺序（与 Go 侧 templateField* 常量一致） */
const TEMPLATE_FIELDS = [
  'durationS',
  'fps',
  'aspectRatio',
  'continuationMode',
  'language',
  'speechRate',
  'videoParams.style',
  'videoParams.camera',
  'videoParams.mood',
  'subtitleStyle.fontName',
  'subtitleStyle.fontSizePct',
  'subtitleStyle.primaryColor',
  'subtitleStyle.outlineColor',
  'subtitleStyle.outlineWidth',
  'subtitleStyle.alignment',
  'subtitleStyle.marginVpct',
  'subtitleStyle.bold',
  'platforms',
  'region',
  'vertical',
  'characterRefs',
]

/** 风格 token 的输出顺序（与 Go 侧 templateTokenOrder 一致） */
const STYLE_TOKEN_ORDER = ['lighting', 'grade', 'lens', 'camera', 'texture', 'mood']

const templates = ref<VideoTemplate[]>([])
const selectedTemplateId = ref('')
const templatePreview = ref<VideoTemplatePreview | null>(null)
const templateError = ref('')
const templateLoading = ref(false)
/** 模板实际带入的字段基线：当前值等于基线即「模板」，不等于则「本次改动」 */
const templateBaseline = ref<Record<string, string>>({})
const openingVars = ref<Record<string, string>>({})

const selectedTemplate = computed(
  () => templates.value.find((item) => item.id === selectedTemplateId.value) ?? null,
)

/** 读取表单字段的字符串快照，仅用于来源比对与 overrides 上报 */
function readPath(path: string): string {
  // 角色是对象数组，直接 String() 会变成 [object Object]；
  // 图片路径必须一并入快照，否则只换了图不改角色名时会被当成「没动过」
  if (path === 'characterRefs') {
    const names = form.characterRefs.map((ref) => ref.name || ref.roleId).join('、')
    const images = form.characterRefs
      .map((_, index) => characterImagesText.value[index] ?? '')
      .join(';')
    // 档案字段也算“改过”：模板带外观、用户改人设时不能误判成没动过
    const profiles = form.characterRefs
      .map((ref) =>
        [ref.appearanceBlock ?? '', ref.persona ?? '', ref.negativePrompt ?? '', ref.perspective ?? '', ref.voiceHint].join('|'),
      )
      .join('、')
    return `${names}|${images}|${profiles}`
  }
  const [head, tail] = path.split('.')
  const record = form as unknown as Record<string, unknown>
  const value = tail ? (record[head] as Record<string, unknown> | undefined)?.[tail] : record[head]
  return Array.isArray(value) ? value.join(',') : String(value ?? '')
}

const sourceRows = computed(() => {
  if (!selectedTemplate.value) {
    return []
  }
  return TEMPLATE_FIELDS.map((key) => {
    const base = templateBaseline.value[key]
    const current = readPath(key)
    const source = base === undefined ? 'default' : current === base ? 'template' : 'override'
    return {
      key,
      label: TEMPLATE_FIELD_LABELS[key] ?? key,
      value: current,
      display: displayValue(key, current),
      source,
    }
  })
})

/** 枚举 / 布尔字段展示为中文，避免在来源表里出现 last_frame、true 这类原始值 */
function displayValue(key: string, value: string): string {
  if (!value) {
    return '—'
  }
  switch (key) {
    case 'continuationMode':
      return CONTINUATION_LABELS[value] ?? value
    case 'subtitleStyle.alignment':
      return SUBTITLE_ALIGNMENTS.find((item) => String(item.value) === value)?.label ?? value
    case 'subtitleStyle.bold':
      return value === 'true' ? '是' : '否'
    case 'platforms':
      return value
        .split(',')
        .map((id) => VIDEO_PLATFORMS.find((item) => item.id === id)?.label ?? id)
        .join('、')
    case 'characterRefs': {
      // 快照里带完整路径（用于比对），表格里只展示名字与张数，免得一串路径挤爆单元格
      const [names, images] = value.split('|')
      const count = (images ?? '').split(';').filter(Boolean).length
      return `${names}（共 ${count} 张图）`
    }
    default:
      return value
  }
}

const overriddenRows = computed(() => sourceRows.value.filter((row) => row.source === 'override'))

function styleSummary(template: VideoTemplate): string {
  const tokens = template.style?.tokens ?? {}
  const parts: string[] = []
  const used = new Set<string>()
  for (const key of STYLE_TOKEN_ORDER) {
    used.add(key)
    const value = String(tokens[key] ?? '').trim()
    if (value) {
      parts.push(value)
    }
  }
  for (const key of Object.keys(tokens).sort()) {
    if (used.has(key)) {
      continue
    }
    const value = String(tokens[key] ?? '').trim()
    if (value) {
      parts.push(value)
    }
  }
  return parts.join('；')
}

/** 把模板默认值填进表单，并记下「哪些字段真的来自模板」用于来源标注 */
function applyTemplate(template: VideoTemplate) {
  const baseline: Record<string, string> = {}
  templateCharacterImages.value = []
  const mark = (key: string) => {
    baseline[key] = readPath(key)
  }

  const defaults = template.defaults
  if (defaults.aspectRatio) {
    form.aspectRatio = defaults.aspectRatio
    mark('aspectRatio')
  }
  if (defaults.durationS > 0) {
    form.durationS = defaults.durationS
    mark('durationS')
  }
  if (defaults.fps > 0) {
    form.fps = defaults.fps
    mark('fps')
  }
  if (defaults.language) {
    form.language = defaults.language
    mark('language')
  }
  if (defaults.continuationMode) {
    form.continuationMode = defaults.continuationMode as VideoRunConfig['continuationMode']
    mark('continuationMode')
  }
  // 开场白建议语速优先于流水线默认语速（与 Go 侧 templateSpeechRate 一致）
  const rate = template.opening.speechRate > 0 ? template.opening.speechRate : (defaults.speechRate ?? 0)
  if (rate > 0) {
    form.speechRate = rate
    mark('speechRate')
  }

  const summary = styleSummary(template)
  if (summary) {
    form.videoParams.style = summary
    mark('videoParams.style')
  }
  const camera = String(template.style?.tokens?.camera ?? '').trim()
  if (camera) {
    form.videoParams.camera = camera
    mark('videoParams.camera')
  }
  const mood = String(template.style?.tokens?.mood ?? '').trim()
  if (mood) {
    form.videoParams.mood = mood
    mark('videoParams.mood')
  }

  const subtitle = template.subtitleStyle
  if (subtitle.fontName) {
    form.subtitleStyle.fontName = subtitle.fontName
    mark('subtitleStyle.fontName')
  }
  if (subtitle.fontSizePct !== null && subtitle.fontSizePct !== undefined) {
    form.subtitleStyle.fontSizePct = subtitle.fontSizePct
    mark('subtitleStyle.fontSizePct')
  }
  if (subtitle.primaryColor) {
    form.subtitleStyle.primaryColor = subtitle.primaryColor
    mark('subtitleStyle.primaryColor')
  }
  if (subtitle.outlineColor) {
    form.subtitleStyle.outlineColor = subtitle.outlineColor
    mark('subtitleStyle.outlineColor')
  }
  if (subtitle.outlineWidth !== null && subtitle.outlineWidth !== undefined) {
    form.subtitleStyle.outlineWidth = subtitle.outlineWidth
    mark('subtitleStyle.outlineWidth')
  }
  if (subtitle.alignment >= 1 && subtitle.alignment <= 9) {
    form.subtitleStyle.alignment = subtitle.alignment
    mark('subtitleStyle.alignment')
  }
  if (subtitle.marginVpct !== null && subtitle.marginVpct !== undefined) {
    form.subtitleStyle.marginVpct = subtitle.marginVpct
    mark('subtitleStyle.marginVpct')
  }
  if (subtitle.bold !== null && subtitle.bold !== undefined) {
    form.subtitleStyle.bold = subtitle.bold
    mark('subtitleStyle.bold')
  }

  const publish = template.publishDefaults
  if ((publish.platforms ?? []).length > 0) {
    form.platforms = [...publish.platforms]
    mark('platforms')
  }
  if (publish.region) {
    form.region = publish.region
    mark('region')
  }
  if (publish.vertical) {
    form.vertical = publish.vertical
    mark('vertical')
  }

  if ((template.characters ?? []).length > 0) {
    form.characterRefs = template.characters.map((character, index) => ({
      roleId: character.roleId || `char_${index + 1}`,
      name: character.name,
      images: [...(character.images ?? [])],
      appearanceBlock: character.appearanceBlock,
      persona: '',
      negativePrompt: '',
      perspective: character.perspective,
      voiceHint: character.voiceHint,
    }))
    characterImagesText.value = form.characterRefs.map((ref) => ref.images.join(';'))
    templateCharacterImages.value = [...characterImagesText.value]
    syncRoleIDSnapshot()
    mark('characterRefs')
  }

  const nextVars: Record<string, string> = {}
  for (const name of template.opening.vars ?? []) {
    nextVars[name] = openingVars.value[name] ?? ''
  }
  openingVars.value = nextVars
  templateBaseline.value = baseline
}

async function refreshTemplatePreview() {
  const template = selectedTemplate.value
  if (!template) {
    templatePreview.value = null
    return
  }
  try {
    templatePreview.value = await backend.previewVideoTemplate(template.id, form.topic.trim(), {
      ...openingVars.value,
    })
  } catch (error) {
    templatePreview.value = null
    templateError.value = errorText(error)
  }
}

async function selectTemplate(templateId: string) {
  selectedTemplateId.value = templateId
  templateError.value = ''
  const template = templates.value.find((item) => item.id === templateId)
  if (!template) {
    templateBaseline.value = {}
    templateCharacterImages.value = []
    openingVars.value = {}
    templatePreview.value = null
    return
  }
  templateLoading.value = true
  try {
    applyTemplate(template)
    await refreshTemplatePreview()
  } finally {
    templateLoading.value = false
  }
}

function clearTemplate() {
  selectedTemplateId.value = ''
  templateBaseline.value = {}
  templateCharacterImages.value = []
  openingVars.value = {}
  templatePreview.value = null
  templateError.value = ''
}

async function loadTemplates(preferredId = '') {
  try {
    const list = await backend.listVideoTemplates()
    templates.value = list ?? []
    if (preferredId && templates.value.some((item) => item.id === preferredId)) {
      await selectTemplate(preferredId)
    }
  } catch (error) {
    templateError.value = errorText(error)
  }
}

/** 提交时上报「本次改过」的字段，后端据此把来源标成 override 而不是 template */
function buildTemplateRef(): VideoTemplateRef | null {
  const template = selectedTemplate.value
  if (!template) {
    return null
  }
  const overrides: Record<string, string> = {}
  for (const row of overriddenRows.value) {
    if (row.value.trim()) {
      overrides[row.key] = row.value
    }
  }
  return {
    id: template.id,
    version: template.version,
    overrides,
    openingVars: { ...openingVars.value },
  }
}

const genProviders = computed(() => providers.value.filter((item) => item.category === 'multimodal_gen'))
const textProviders = computed(() =>
  providers.value.filter((item) => item.category === 'text' || item.category === 'multimodal'),
)

const adapterOptions = computed(() => [
  { id: 'auto', label: '自动（按提供商推断）' },
  ...(environment.value?.adapters ?? []).map((item) => ({ id: item.id, label: item.label })),
])

const selectedAdapter = computed(() =>
  (environment.value?.adapters ?? []).find((item) => item.id === form.adapter),
)

const derivedFrames = computed(() => {
  const duration = Number(form.durationS) || 0
  const fps = Number(form.fps) || 0
  return Math.max(0, Math.round(duration * fps))
})

const unitLabel = computed(() => (form.language.startsWith('en') ? '词' : '字'))

/** 任务是否已有任何图片输入：有图时主体以图为准，不再默认动物 */
const hasImageInput = computed(
  () =>
    form.firstFrame.trim() !== '' ||
    characterImagesText.value.some((text) => text.trim() !== ''),
)

const platformHint = computed(() => {
  const primary = VIDEO_PLATFORMS.find((item) => item.id === form.platforms[0])
  if (!primary) {
    return '未选择：按通用默认档（标题 ≤40 字、话题 ≤5 个、晚间高峰）'
  }
  const overflow = form.platforms.length > 4 ? ' · 仅前 4 个平台生效' : ''
  return `${primary.label}：标题 ≤${primary.titleMaxLen} 字、话题 ≤${primary.hashtagMax} 个${overflow}`
})

// 连续整数档位折叠成区间，避免方舟这类「支持任意整数秒」的模型刷屏
function formatDurations(durations: number[]): string {
  if (durations.length === 0) {
    return '—'
  }
  const sorted = [...durations].sort((left, right) => left - right)
  const contiguous = sorted.every((value, index) => index === 0 || value === sorted[index - 1] + 1)
  if (contiguous && sorted.length > 4) {
    return `${sorted[0]}–${sorted[sorted.length - 1]} 秒（整数）`
  }
  return `${sorted.join(' / ')} 秒`
}

const adapterHint = computed(() => {
  const maturity = selectedAdapter.value?.maturityNote
    ? `适配状态：${selectedAdapter.value?.maturityLabel}——${selectedAdapter.value?.maturityNote}。`
    : ''
  const caps = selectedAdapter.value?.capabilities
  if (!caps) {
    return (
      maturity +
      '适配器将根据已登记提供商的 Base URL / 模型名自动推断；也可显式选择「本地联调占位」跑通流水线。'
    )
  }
  return (
    maturity +
    `时长档位 ${formatDurations(caps.allowedDurations)}，单段上限 ${caps.maxClipS}s；` +
    `画幅 ${caps.allowedAspectRatios.join(' / ')}；尾帧条件 ${caps.lastFrame ? '支持' : '不支持'}；` +
    `原生音频 ${caps.nativeAudio ? '支持（台词写入提示词）' : '不支持（字幕按语速估算）'}。`
  )
})

const providerHint = computed(() => {
  if (genProviders.value.length === 0) {
    return '还没有「多模态生成模型」提供商：真实生成会失败，可先用「本地联调占位」验证整条流水线。'
  }
  return '首帧/参考图是本地文件时，火山方舟 Seedance 会自动转成 base64 内联；DashScope / Runway 仍要求公网 URL。'
})

// 画幅与字幕的对齐策略：服务商不总按请求出片，合成阶段会兜底
const aspectHint = '画幅以任务选择为准：火山方舟 Seedance 的首帧/首尾帧任务锁定输出比例跟随首帧图（ratio 只能为 adaptive），'
  + '素材比例不符时合成阶段会用模糊填充统一画幅。开启字幕时，时间轴按各片段实测时长重排，一句一条，跟对白对齐。'

onMounted(async () => {
  // 热点爬取带入的素材：先落到表单，再拉提供商能力
  const draft = consumeCrawlDraft()
  if (draft) {
    sourceDraft.value = draft
    form.topic = draft.topic
  }
  // 项目视图尾帧「用为新建首帧」交接：填入首帧并提示来源（只消费一次）
  if (videoNav.pendingFirstFramePath) {
    form.firstFrame = videoNav.pendingFirstFramePath
    tailHandoffHint.value = videoNav.pendingFirstFramePath
    videoNav.pendingFirstFramePath = ''
  }
  // 来自模板库的「用此模板新建任务」（只消费一次）
  void loadTemplates(consumePendingTemplate())
  try {
    const [providerList, env, projectList] = await Promise.all([
      backend.listProviders().catch((error) => {
        loadError.value = errorText(error)
        return [] as Provider[]
      }),
      backend.getVideoEnvironment().catch((error) => {
        loadError.value = errorText(error)
        return null
      }),
      backend.listVideoProjects().catch(() => [] as VideoProject[]),
    ])
    providers.value = providerList
    environment.value = env
    projects.value = projectList ?? []
  } catch (error) {
    loadError.value = errorText(error)
  }
  await loadCharacterLibrary()
})

function addCharacter() {
  form.characterRefs.push({
    roleId: nextCharacterRoleID(),
    name: '',
    images: [],
    appearanceBlock: '',
    persona: '',
    negativePrompt: '',
    perspective: '',
    voiceHint: '',
  })
  characterImagesText.value.push('')
  templateCharacterImages.value.push('')
  syncRoleIDSnapshot()
}

/** 一个角色的图到底是谁给的：上传图优先，模板生成图只作退路 */
function characterImageBadge(index: number): string {
  const current = (characterImagesText.value[index] ?? '').trim()
  if (!current) {
    return ''
  }
  return current === (templateCharacterImages.value[index] ?? '').trim()
    ? '模板定妆图'
    : '上传图（优先）'
}

function characterImageBadgeClass(index: number): string {
  return characterImageBadge(index) === '模板定妆图' ? 'src src--template' : 'src src--override'
}

function removeCharacter(index: number) {
  const removed = (form.characterRefs[index]?.roleId ?? '').trim()
  form.characterRefs.splice(index, 1)
  characterImagesText.value.splice(index, 1)
  templateCharacterImages.value.splice(index, 1)
  if (removed) {
    // 角色被删掉后手动分段里不能还留着他的台词归属与出场勾选
    for (const shot of form.manualShots ?? []) {
      shot.roles = shot.roles.filter((role) => role !== removed)
      if (shot.dialogueLines) {
        shot.dialogueLines = shot.dialogueLines.filter((line) => line.speaker !== removed)
      }
      if ((shot.speaker ?? '') === removed) {
        shot.speaker = ''
      }
      syncShotDialogue(shot)
    }
  }
  syncRoleIDSnapshot()
}

/* ---------------- 角色标识：唯一性与引用同步 ---------------- */

/** 下一个可用的角色标识：扫描已有 char_N 的最大后缀，删除后再添加不会撞号。 */
function nextCharacterRoleID(): string {
  let max = 0
  for (const ref of form.characterRefs) {
    const match = /^char_(\d+)$/i.exec((ref.roleId ?? '').trim())
    if (match) {
      max = Math.max(max, Number(match[1]))
    }
  }
  const used = new Set(
    form.characterRefs.map((ref) => (ref.roleId ?? '').trim().toLowerCase()).filter(Boolean),
  )
  let candidate = `char_${max + 1}`
  while (used.has(candidate.toLowerCase())) {
    max += 1
    candidate = `char_${max + 1}`
  }
  return candidate
}

/** 角色标识重复清单：重复会让台词归属永远绑不到确定的人，提交前必须挡住。 */
const duplicateRoleIDs = computed(() => {
  const seen = new Set<string>()
  const duplicates = new Set<string>()
  for (const ref of form.characterRefs) {
    const roleId = (ref.roleId ?? '').trim().toLowerCase()
    if (!roleId) {
      continue
    }
    if (seen.has(roleId)) {
      duplicates.add(roleId)
    }
    seen.add(roleId)
  }
  return [...duplicates]
})

/** 角色标识改名前后的快照：用户直接编辑输入框时，把引用一起搬走。 */
const roleIDSnapshot = ref<string[]>([])

function syncRoleIDSnapshot() {
  roleIDSnapshot.value = form.characterRefs.map((ref) => (ref.roleId ?? '').trim())
}

function retargetRole(oldRoleID: string, newRoleID: string) {
  if (!oldRoleID || !newRoleID || oldRoleID === newRoleID) {
    return
  }
  for (const shot of form.manualShots ?? []) {
    shot.roles = [...new Set(shot.roles.map((role) => (role === oldRoleID ? newRoleID : role)))]
    for (const line of shotDialogueLines(shot)) {
      if (line.speaker === oldRoleID) {
        line.speaker = newRoleID
      }
    }
    if ((shot.speaker ?? '') === oldRoleID) {
      shot.speaker = newRoleID
    }
    syncShotDialogue(shot)
  }
}

watch(
  () => form.characterRefs.map((ref) => ref.roleId),
  (next) => {
    const previous = roleIDSnapshot.value
    next.forEach((roleId, index) => {
      const old = (previous[index] ?? '').trim()
      const current = (roleId ?? '').trim()
      if (old && old !== current) {
        retargetRole(old, current)
      }
    })
    roleIDSnapshot.value = next.map((roleId) => (roleId ?? '').trim())
  },
)

/* ---------------- 手动分段：多说话人台词 ---------------- */

/** 取某段的台词行；旧路径只有 dialogue/speaker 时自动升级成一行。 */
function shotDialogueLines(shot: VideoShot): VideoDialogueLine[] {
  if (!shot.dialogueLines) {
    shot.dialogueLines = []
  }
  if (shot.dialogueLines.length === 0 && (shot.dialogue ?? '').trim()) {
    shot.dialogueLines.push({ speaker: (shot.speaker ?? '').trim(), text: (shot.dialogue ?? '').trim() })
  }
  return shot.dialogueLines
}

/** 把台词行同步回兼容字段：dialogue 是全部文本，speaker 只在单说话人时保留。 */
function syncShotDialogue(shot: VideoShot) {
  const lines = (shot.dialogueLines ?? [])
    .map((line) => ({ speaker: (line.speaker ?? '').trim(), text: (line.text ?? '').trim() }))
    .filter((line) => line.text)
  shot.dialogueLines = lines
  shot.dialogue = lines.map((line) => line.text).join('\n')
  shot.speaker = lines.length === 1 ? lines[0].speaker : ''
  // 角色开口就必须算作出场，否则参考图与提示词都找不到这张脸。
  for (const line of lines) {
    if (!line.speaker || line.speaker === 'narration') {
      continue
    }
    if (!shot.roles.includes(line.speaker)) {
      shot.roles.push(line.speaker)
    }
  }
}

function addDialogueLine(index: number) {
  const shot = form.manualShots?.[index]
  if (!shot) {
    return
  }
  const lines = shotDialogueLines(shot)
  const fallbackSpeaker = shot.roles.length === 1 ? shot.roles[0] : ''
  lines.push({ speaker: fallbackSpeaker, text: '' })
}

function removeDialogueLine(index: number, lineIndex: number) {
  const shot = form.manualShots?.[index]
  if (!shot) {
    return
  }
  shotDialogueLines(shot).splice(lineIndex, 1)
  syncShotDialogue(shot)
}

function onDialogueSpeakerChange(index: number) {
  const shot = form.manualShots?.[index]
  if (shot) {
    syncShotDialogue(shot)
  }
}

async function pickFirstFrame() {
  try {
    const path = await backend.pickImageFile()
    if (path) {
      form.firstFrame = path
    }
  } catch (error) {
    submitError.value = errorText(error)
  }
}

async function pickCharacterImages(index: number) {
  try {
    const files = await backend.pickImageFiles()
    if (files.length > 0) {
      const previous = (characterImagesText.value[index] ?? '').trim()
      characterImagesText.value[index] = files.join(';')
      characterUploadNote.value = previous
        ? `已用你选的 ${files.length} 张图替掉该角色原有的定妆图：同一角色只用一份来源，模板里的生成图不会再提交给模型。`
        : `已为该角色选了 ${files.length} 张上传图：它们会排在参考图最前面，并在提示词里标为形象基准。`
    }
  } catch (error) {
    submitError.value = errorText(error)
  }
}

function splitImages(value: string): string[] {
  return value
    .split(/[;\n]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

/** 把爬取条目的封面填进首帧（远程 URL 需模型侧能公网访问） */
function useCrawlThumbnail() {
  if (sourceDraft.value?.thumbnail) {
    form.firstFrame = sourceDraft.value.thumbnail
  }
}

/** 把爬取时存的选题标签并入创作要求：extraPrompt 会作为“必须落实的额外要求”进 Agent1/Agent2 */
function applyDraftTags() {
  const tags = sourceDraft.value?.tags ?? []
  if (tags.length === 0) {
    return
  }
  const line = `选题标签：${tags.join('、')}`
  const current = form.videoParams.extraPrompt.trim()
  form.videoParams.extraPrompt = current ? `${current}；${line}` : line
}

function dismissCrawlDraft() {
  sourceDraft.value = null
}

function togglePlatform(id: string) {
  const index = form.platforms.indexOf(id)
  if (index >= 0) {
    form.platforms.splice(index, 1)
    return
  }
  form.platforms.push(id)
}

/* ---------------- 分镜来源：AI 自动生成 vs 手动分段 ---------------- */
const scriptMode = computed<ScriptMode>({
  get: () => (form.scriptMode === 'manual' ? 'manual' : 'auto'),
  set: (value) => {
    form.scriptMode = value
    // 切到手动时先给一段空模板，用户不用先点「添加一段」
    if (value === 'manual' && manualShots.value.length === 0) {
      addManualShot()
    }
  },
})

const manualShots = computed(() => form.manualShots ?? [])
const manualTotalS = computed(
  () => Math.round(manualShots.value.reduce((sum, shot) => sum + (Number(shot.durationS) || 0), 0) * 10) / 10,
)

/** 说话人候选：旁白 + 已登记的角色（没登记角色时只能选旁白） */
const speakerOptions = computed(() => [
  { value: '', label: '自动（本段唯一角色→他，否则旁白）' },
  { value: 'narration', label: '旁白（画外音）' },
  ...form.characterRefs
    .filter((ref) => ref.roleId)
    .map((ref) => ({ value: ref.roleId, label: ref.name || ref.roleId })),
])

function addManualShot() {
  if (!form.manualShots) {
    form.manualShots = []
  }
  form.manualShots.push(createManualShot(form.manualShots.length))
}

function removeManualShot(index: number) {
  form.manualShots?.splice(index, 1)
}

function moveManualShot(index: number, delta: number) {
  const shots = form.manualShots
  if (!shots) {
    return
  }
  const target = index + delta
  if (target < 0 || target >= shots.length) {
    return
  }
  const [moved] = shots.splice(index, 1)
  shots.splice(target, 0, moved)
}

function toggleManualShotRole(index: number, roleId: string) {
  const shot = form.manualShots?.[index]
  if (!shot) {
    return
  }
  const at = shot.roles.indexOf(roleId)
  if (at >= 0) {
    shot.roles.splice(at, 1)
    return
  }
  shot.roles.push(roleId)
}

async function submit() {
  submitError.value = ''
  if (!form.topic.trim()) {
    submitError.value = '请先填写视频主题'
    return
  }

  // 手动分段：画面内容是必填项，先在前端挡一遍，别等到后端报错
  if (scriptMode.value === 'manual') {
    const shots = form.manualShots ?? []
    if (shots.length === 0) {
      submitError.value = '手动分段至少要填一段'
      return
    }
    const missing = shots.findIndex((shot) => !shot.sceneIntent.trim())
    if (missing >= 0) {
      submitError.value = `第 ${missing + 1} 段还没写画面内容（分段必填）`
      return
    }
    // 台词行与兼容字段先对齐，免得提交的还是上一版文本
    for (const shot of shots) {
      syncShotDialogue(shot)
    }
  }

  // 空标识先补成不撞号的 char_N，再做唯一性校验
  for (const ref of form.characterRefs) {
    if (!(ref.roleId ?? '').trim()) {
      ref.roleId = nextCharacterRoleID()
    }
  }
  if (duplicateRoleIDs.value.length > 0) {
    submitError.value = `角色标识重复：${duplicateRoleIDs.value.join('、')}。每个角色的 role_id 必须唯一，否则台词归属绑不到确定的人`
    return
  }

  const config: VideoRunConfig = {
    ...form,
    topic: form.topic.trim(),
    // 手动分段：目标时长跟着分段之和走，不再另外填
    durationS: scriptMode.value === 'manual' ? Math.max(1, Math.round(manualTotalS.value)) : form.durationS,
    templateRef: buildTemplateRef(),
    characterRefs: form.characterRefs
      .map((ref, index) => {
        const images = splitImages(characterImagesText.value[index] ?? '')
        // 没动过的模板行标 template、换过的标 user：后端靠这个判定谁能优先提交
        const inherited =
          images.length > 0 &&
          (characterImagesText.value[index] ?? '').trim() === (templateCharacterImages.value[index] ?? '').trim()
        return {
          ...ref,
          name: ref.name.trim(),
          roleId: ref.roleId.trim() || nextCharacterRoleID(),
          images,
          imageSource: images.length > 0 ? (inherited ? 'template' : 'user') : '',
        }
      })
      .filter((ref) => ref.name || ref.images.length > 0),
  }

  submitting.value = true
  try {
    const task = await backend.createVideoTask({
      title: title.value.trim(),
      projectId: projectId.value,
      config,
    })
    emit('created', task.id)
  } catch (error) {
    submitError.value = errorText(error)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="create">
    <header class="create__head">
      <div>
        <h2 class="create__title">新建视频任务</h2>
        <p class="create__subtitle">
          主题 + 首帧 → 分镜 → 提示词/字幕 → 逐段生成 → 合成成片 → 发布元数据；
          三道确认关卡（分镜 / 提示词 / 发布）分别挡在付费生成与发布之前
        </p>
      </div>
      <span class="ui-chip">总帧数 {{ derivedFrames }}（时长 × 帧率）</span>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>
    <p v-if="submitError" class="ui-notice ui-notice--error">{{ submitError }}</p>
    <p v-if="environment && !environment.ffmpegAvailable" class="ui-notice ui-notice--warn">
      未检测到 ffmpeg：合成时会保留分段视频，跳过拼接与字幕烧录。安装 ffmpeg 并加入 PATH 后可重试。
    </p>

    <div v-if="sourceDraft" class="source">
      <div class="source__body">
        <span class="ui-chip ui-chip--ok">已带入爬取素材</span>
        <span class="source__topic">{{ sourceDraft.note }}</span>
        <span v-for="tag in sourceDraft.tags ?? []" :key="tag" class="ui-chip">#{{ tag }}</span>
      </div>
      <div class="source__actions">
        <button v-if="sourceDraft.thumbnail" class="ui-btn" type="button" @click="useCrawlThumbnail">
          用封面作首帧
        </button>
        <button
          v-if="sourceDraft.tags?.length"
          class="ui-btn"
          type="button"
          title="写进「额外创作要求」，分镜与提示词会落实这几个标签"
          @click="applyDraftTags"
        >
          标签并入创作要求
        </button>
        <button class="ui-btn ui-btn--ghost" type="button" @click="dismissCrawlDraft">仅保留主题</button>
      </div>
    </div>

    <form class="create__form" @submit.prevent="submit">
      <section class="ui-card block">
        <div class="block__head">
          <h3 class="ui-h3">0. 模板（可选，只提供默认值）</h3>
          <div class="row">
            <span v-if="selectedTemplate" class="ui-chip" :class="templateScopeClass(selectedTemplate.scope)">
              {{ TEMPLATE_SCOPE_LABELS[selectedTemplate.scope] }} · v{{ selectedTemplate.version }}
            </span>
            <span v-if="templateLoading" class="ui-chip ui-chip--running">预览中…</span>
            <button v-if="selectedTemplate" class="ui-btn ui-btn--ghost" type="button" @click="clearTemplate">
              清除模板
            </button>
          </div>
        </div>
        <div class="grid grid--2">
          <label class="ui-field">
            <span class="ui-label">选择模板</span>
            <select
              class="ui-select"
              :value="selectedTemplateId"
              @change="selectTemplate(($event.target as HTMLSelectElement).value)"
            >
              <option value="">不使用模板（全部手填）</option>
              <option v-for="item in templates" :key="item.id" :value="item.id">
                {{ item.name }} · v{{ item.version }} · {{ TEMPLATE_SCOPE_LABELS[item.scope] }}
              </option>
            </select>
          </label>
          <label v-for="(_, name) in openingVars" :key="name" class="ui-field">
            <span class="ui-label">开场白变量 {{ '{' + name + '}' }}</span>
            <input v-model="openingVars[name]" class="ui-input" type="text" @change="refreshTemplatePreview" />
          </label>
        </div>
        <p v-if="templates.length === 0" class="hint">
          还没有模板：到「视频生成 → 模板库」新建，或直接按默认值手填。
        </p>
        <p v-if="templateError" class="ui-notice ui-notice--error">{{ templateError }}</p>

        <template v-if="selectedTemplate">
          <p class="hint">
            模板只预填默认值：标「模板」的字段来自 {{ selectedTemplate.name }} v{{ selectedTemplate.version }}，改动后会变成「本次改动」；
            分镜 / 提示词 / 发布三道确认门照常生效。
          </p>
          <div v-if="templatePreview" class="preview">
            <p class="preview__line">
              <span class="preview__tag">开场白</span>
              {{ templatePreview.opening || '（模板未定义开场白）' }}
            </p>
            <p v-if="templatePreview.styleSummary" class="preview__line">
              <span class="preview__tag">风格</span>{{ templatePreview.styleSummary }}
            </p>
            <p v-if="templatePreview.styleSnippet" class="preview__line">
              <span class="preview__tag">片段</span>{{ templatePreview.styleSnippet }}
            </p>
            <p v-for="(item, index) in templatePreview.characters ?? []" :key="`ch${index}`" class="preview__line">
              <span class="preview__tag">人物</span>{{ item }}
            </p>
            <p v-if="templatePreview.subtitle" class="preview__line">
              <span class="preview__tag">字幕</span>{{ templatePreview.subtitle }}
            </p>
            <p v-if="templatePreview.publish" class="preview__line">
              <span class="preview__tag">发布</span>{{ templatePreview.publish }}
            </p>
            <p v-if="templatePreview.defaults" class="preview__line">
              <span class="preview__tag">默认值</span>{{ templatePreview.defaults }}
            </p>
            <p v-for="(item, index) in templatePreview.assets ?? []" :key="`as${index}`" class="preview__line">
              <span class="preview__tag">资产</span>{{ item }}
            </p>
          </div>
          <p v-for="(item, index) in templatePreview?.compliance ?? []" :key="`tc${index}`" class="ui-notice ui-notice--error">
            {{ item }}（命中违禁词会阻止创建任务）
          </p>
          <p v-for="(item, index) in templatePreview?.warnings ?? []" :key="`tw${index}`" class="ui-notice ui-notice--warn">
            {{ item }}
          </p>

          <details class="sources" open>
            <summary class="sources__summary">
              字段来源：{{ overriddenRows.length }} 项本次改动，其余为模板或默认
            </summary>
            <div class="sources__grid">
              <div v-for="row in sourceRows" :key="row.key" class="sources__row">
                <span class="sources__label">{{ row.label }}</span>
                <span class="sources__value">{{ row.display }}</span>
                <span class="src" :class="templateSourceClass(row.source)">
                  {{ TEMPLATE_SOURCE_LABELS[row.source] }}
                </span>
              </div>
            </div>
          </details>
        </template>
      </section>

      <section class="ui-card block">
        <h3 class="ui-h3">1. 主题与首帧</h3>
        <label class="ui-field">
          <span class="ui-label">视频主题 / 诉求 *</span>
          <textarea
            v-model="form.topic"
            class="ui-textarea"
            rows="3"
            placeholder="例：清晨咖啡馆里，一杯手冲咖啡唤醒整座城市（温暖、写实电影感）"
            required
          ></textarea>
        </label>
        <div class="grid grid--2">
          <label class="ui-field">
            <span class="ui-label">任务标题（可选）</span>
            <input v-model="title" class="ui-input" type="text" placeholder="留空则用主题自动生成" />
          </label>
          <label class="ui-field">
            <span class="ui-label">所属项目</span>
            <select v-model="projectId" class="ui-select">
              <option value="">未归类</option>
              <option v-for="project in projects" :key="project.id" :value="project.id">
                {{ project.name }}
              </option>
            </select>
          </label>
          <div class="ui-field">
            <span class="ui-label">首帧图片（本地路径或 URL）</span>
            <span class="row">
              <input
                v-model="form.firstFrame"
                class="ui-input"
                type="text"
                spellcheck="false"
                placeholder="C:\images\first.png 或 https://..."
              />
              <button class="ui-btn" type="button" @click="pickFirstFrame">选择…</button>
              <button class="ui-btn ui-btn--ghost" type="button" @click="toggleTailAssetPicker">
                {{ showTailAssets ? '收起尾帧库' : '从尾帧资产选择' }}
              </button>
            </span>
            <p v-if="tailHandoffHint" class="hint hint--warn">
              已从项目视图的尾帧资产带入首帧：{{ tailHandoffHint }}
            </p>
            <div v-if="showTailAssets" class="tail-picker">
              <p v-if="tailAssetsError" class="hint hint--warn">{{ tailAssetsError }}</p>
              <p v-else-if="tailAssets.length === 0" class="hint">
                尾帧资产库为空：在任务创建页或设置中开启「保留最后一段尾帧到资产库」后，完成的任务会在这里出现尾帧图。
              </p>
              <div v-for="asset in tailAssets" :key="asset.id" class="tail-item">
                <img v-if="asset.mediaUrl" class="tail-item__img" :src="asset.mediaUrl" alt="尾帧" />
                <div class="tail-item__info">
                  <span class="tail-item__title">{{ asset.taskTitle || asset.taskId }}</span>
                  <span class="tail-item__meta">{{ formatClock(asset.createdAt) }}</span>
                </div>
                <button class="ui-btn" type="button" @click="useTailFrame(asset)">用作首帧</button>
                <button class="ui-btn ui-btn--ghost" type="button" @click="removeTailAsset(asset.id)">移除</button>
              </div>
            </div>
          </div>
        </div>
        <p class="hint">
          首帧驱动叙事：Agent1 会把画面基调、主体、光线与首帧对齐；i2v 提示词只描述「从首帧开始发生的变化」。
        </p>
        <p v-if="form.firstFrame" class="hint hint--warn">
          严格锁帧已开启（默认）：新视频一定从这张图开始；与角色参考图冲突时首帧优先，主体以首帧为准，不再强制动物。
          若连通道本身不支持以首帧起片，任务会直接报错而不是静默忽略。
        </p>
      </section>

      <section class="ui-card block">
        <div class="block__head">
          <h3 class="ui-h3">2. 角色参考图（可选）</h3>
          <button class="ui-btn" type="button" @click="addCharacter">+ 添加角色</button>
        </div>
        <p class="hint">
          角色图用于跨段保持主体外观一致；不支持主体参考的模型会把统一外观描述写进每段提示词。
          <template v-if="hasImageInput">
            已提供首帧或角色参考图：出镜主体以图为准，不再强制动物；请自行确认图片的肖像与使用授权。
          </template>
          <template v-else>
            当前没有首帧或角色参考图：出镜主体默认使用猫 / 狗 / 猪 / 鸡等动物（可拟人化），避开真人形象。
          </template>
        </p>
        <div v-if="characterLibrary.length > 0" class="character-picker">
          <select v-model="characterLibraryId" class="ui-select">
            <option value="">从角色库选择…</option>
            <option v-for="character in characterLibrary" :key="character.id" :value="character.id">
              {{ character.name }}（{{ character.roleId }}）
            </option>
          </select>
          <button class="ui-btn" type="button" :disabled="!characterLibraryId" @click="addCharacterFromLibrary">
            加入本任务
          </button>
          <span class="hint">选中后会把外观锚点、人设、负向约束、音色与默认定妆图一起带入。</span>
        </div>
        <p v-else class="hint">
          角色库为空：可先到「视频生成 → 角色库」沉淀常驻角色，或直接在下面添加并存回角色库。
        </p>
        <p v-if="characterLibraryError" class="hint hint--warn">{{ characterLibraryError }}</p>
        <div v-for="(ref, index) in form.characterRefs" :key="index" class="character">
          <input v-model="ref.name" class="ui-input" type="text" placeholder="角色名称，如 橘座（猫）" />
          <input
            v-model="characterImagesText[index]"
            class="ui-input"
            type="text"
            spellcheck="false"
            placeholder="参考图路径，多个用 ; 分隔"
          />
          <span v-if="characterImageBadge(index)" :class="characterImageBadgeClass(index)">{{
            characterImageBadge(index)
          }}</span>
          <button class="ui-btn" type="button" @click="pickCharacterImages(index)">选择…</button>
          <button class="ui-btn ui-btn--ghost" type="button" @click="removeCharacter(index)">移除</button>
          <details class="character__profile">
            <summary>角色档案：外观锚点 / 人设 / 负向约束 / 声音</summary>
            <label class="ui-field">
              <span class="ui-label">外观锚点（会写进每段提示词；有上传图时以图为准）</span>
              <textarea
                v-model="ref.appearanceBlock"
                class="ui-textarea"
                rows="2"
                placeholder="成年橘色短毛猫，圆脸，耳尖一小撮白毛，脖子系米白色针织围巾"
              ></textarea>
            </label>
            <label class="ui-field">
              <span class="ui-label">人设 / 口吻</span>
              <textarea
                v-model="ref.persona"
                class="ui-textarea"
                rows="2"
                placeholder="慢热、嘴硬心软；说话短句，不堆成语"
              ></textarea>
            </label>
            <div class="grid grid--2">
              <label class="ui-field">
                <span class="ui-label">角色负向约束</span>
                <input
                  v-model="ref.negativePrompt"
                  class="ui-input"
                  type="text"
                  placeholder="不要出现项圈铃铛"
                />
              </label>
              <label class="ui-field">
                <span class="ui-label">声音提示</span>
                <input v-model="ref.voiceHint" class="ui-input" type="text" placeholder="温暖中音，4.2 字/秒" />
              </label>
              <label class="ui-field">
                <span class="ui-label">视角约定</span>
                <input v-model="ref.perspective" class="ui-input" type="text" placeholder="主视角，正对镜头半身" />
              </label>
              <label class="ui-field">
                <span class="ui-label">分镜角色标识（role_id）</span>
                <input v-model="ref.roleId" class="ui-input" type="text" placeholder="host" />
              </label>
            </div>
            <div class="character__actions">
              <span v-if="ref.characterId" class="ui-chip ui-chip--ok">已绑定角色库</span>
              <button class="ui-btn" type="button" @click="saveCharacterToLibrary(index)">保存到角色库</button>
            </div>
          </details>
        </div>
        <p v-if="duplicateRoleIDs.length > 0" class="ui-notice ui-notice--error">
          角色标识重复：{{ duplicateRoleIDs.join('、') }}。每个角色的 role_id 必须唯一，否则台词归属绑不到确定的人。
        </p>
        <p v-if="characterUploadNote" class="hint">{{ characterUploadNote }}</p>
        <p v-if="form.characterRefs.length > 0" class="hint">
          默认严格锁帧：有首帧（含上一段尾帧延续出的首帧）的片段按「图生视频-首帧」提交，角色图会让位（官方规则：首帧与参考图两类任务互斥），
          形象靠首帧画面与提示词里的外观锚点保持；在「3. 时长、画幅与衔接」里关掉「严格首帧」后，才把首帧图与角色图一起按「全模态参考」提交，
          并在提示词里按「图片N」逐张绑定（需 Seedance 2.0 系列 / 2.5，Seedance 1.x 仅能走首帧延续）。
        </p>
        <p v-if="form.characterRefs.length > 0" class="hint">
          图片来源优先级：你选的上传图 &gt; 模板定妆图。同一角色只用一份来源（两张不同的图丢给模型只会平均出第三个形象），
          所以下拉模板后直接点「选择…」就能逐角色覆盖模板图，没覆盖的角色继续用模板图；参考图超模型上限时先丢模板图。
        </p>
      </section>

      <section class="ui-card block">
        <h3 class="ui-h3">3. 时长、画幅与衔接</h3>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">目标时长（秒）</span>
            <input
              v-model.number="form.durationS"
              class="ui-input"
              type="number"
              min="2"
              max="600"
              :disabled="scriptMode === 'manual'"
            />
          </label>
          <label class="ui-field">
            <span class="ui-label">帧率 fps</span>
            <input v-model.number="form.fps" class="ui-input" type="number" min="12" max="60" />
          </label>
          <label class="ui-field">
            <span class="ui-label">画幅</span>
            <select v-model="form.aspectRatio" class="ui-select">
              <option value="16:9">16:9</option>
              <option value="9:16">9:16</option>
              <option value="1:1">1:1</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">多段衔接方式</span>
            <select v-model="form.continuationMode" class="ui-select">
              <option value="last_frame">尾帧延续（画面最连续，串行较慢）</option>
              <option value="overlap">重叠帧（当前按硬切合成）</option>
              <option value="none">硬切（适合分镜切换）</option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">语速（{{ unitLabel }}/秒）</span>
            <input v-model.number="form.speechRate" class="ui-input" type="number" min="1" max="10" step="0.1" />
          </label>
          <label class="ui-field">
            <span class="ui-label">语言</span>
            <select v-model="form.language" class="ui-select">
              <option value="zh-CN">中文（默认 4 字/秒）</option>
              <option value="en-US">English（默认 2.6 词/秒）</option>
            </select>
          </label>
        </div>
        <p v-if="scriptMode === 'manual'" class="hint hint--warn">
          手动分段模式下这个目标时长不生效：总时长以第 4 节各段之和（{{ manualTotalS }}s）为准，切回自动模式时再用这里的值。
        </p>
        <div class="chips chips--options">
          <label class="check check--inline">
            <input v-model="form.segmentReview" type="checkbox" />
            逐段确认（每段生成后先预览确认，再生成下一段）
          </label>
          <label class="check check--inline">
            <input v-model="form.keepLastFrame" type="checkbox" />
            保留最后一段尾帧到资产库
          </label>
          <label class="check check--inline">
            <input v-model="form.strictFirstFrame" type="checkbox" />
            严格首帧（有首帧的片段必须从这张图起片）
          </label>
        </div>
        <p class="hint">
          逐段确认开启后，渲染阶段每生成一段就停住等你验收：不满意可修改提示词（或让 AI 重写）只重生成这一段；
          关闭则一次串行生成全部片段。开启保留尾帧后，最后一段的尾帧会存入资产库，其他任务可选作首帧续写。
        </p>
        <p class="hint">
          严格首帧（默认开）：你填的首帧或从尾帧资产带入的那张图，会作为「图生视频-首帧」任务的 first_frame 提交，画面硬从这张图开始。
          关掉它才能与角色参考图共用（走全模态参考，锁帧降为提示词里的「图片1」软约定，模型可能只做近似开场）。
        </p>
        <p class="hint">{{ adapterHint }}</p>
        <p class="hint">{{ aspectHint }}</p>
      </section>

      <section class="ui-card block">
        <div class="block__head">
          <h3 class="ui-h3">4. 分镜来源</h3>
          <span class="ui-chip">
            {{ scriptMode === 'manual' ? `${manualShots.length} 段 · 合计 ${manualTotalS}s` : 'AI 自动生成' }}
          </span>
        </div>
        <div class="chips chips--options">
          <label class="check check--inline">
            <input v-model="scriptMode" type="radio" value="auto" />
            AI 自动生成分镜（Agent1 按主题、风格与角色切段）
          </label>
          <label class="check check--inline">
            <input v-model="scriptMode" type="radio" value="manual" />
            手动分段（自己填段数、时长与台词，跳过 Agent1）
          </label>
        </div>
        <p v-if="scriptMode === 'auto'" class="hint">
          自动模式：Agent1 按目标时长（{{ form.durationS }} 秒）规划段数与节奏，关卡①里可再改。
        </p>
        <p v-else class="hint">
          手动模式：你填的 {{ manualShots.length }} 段直接当最终分镜，Agent1 不再改结构；目标时长改为按各段之和（{{ manualTotalS }}s）回写，
          段时长保真、不按总时长缩放。每段 {{ MANUAL_SHOT_MIN_S }}–{{ MANUAL_SHOT_MAX_S }} 秒（越界会被夹到边界并告警），最多 12 段；
          模型档位对不齐时由渲染阶段的切分逻辑兜底。
        </p>
        <template v-if="scriptMode === 'manual'">
          <div class="block__head">
            <span class="hint">画面内容是必填项；角色只表示「本段出场」，台词按开口顺序一行一句，可以多个人轮流说。</span>
            <button class="ui-btn" type="button" @click="addManualShot">+ 添加一段</button>
          </div>
          <div v-for="(shot, index) in manualShots" :key="index" class="manual">
            <div class="manual__head">
              <span class="manual__no">第 {{ index + 1 }} 段</span>
              <label class="manual__dur">
                <span class="ui-label">时长（秒）</span>
                <input
                  v-model.number="shot.durationS"
                  class="ui-input"
                  type="number"
                  :min="MANUAL_SHOT_MIN_S"
                  :max="MANUAL_SHOT_MAX_S"
                  step="1"
                />
              </label>
              <span class="row">
                <button
                  class="ui-btn ui-btn--ghost"
                  type="button"
                  :disabled="index === 0"
                  @click="moveManualShot(index, -1)"
                >上移</button>
                <button
                  class="ui-btn ui-btn--ghost"
                  type="button"
                  :disabled="index === manualShots.length - 1"
                  @click="moveManualShot(index, 1)"
                >下移</button>
                <button class="ui-btn ui-btn--ghost" type="button" @click="removeManualShot(index)">删除</button>
              </span>
            </div>
            <label class="ui-field">
              <span class="ui-label">画面内容 *</span>
              <textarea
                v-model="shot.sceneIntent"
                class="ui-textarea"
                rows="2"
                placeholder="例：清晨窗边，橘座伸爪推开窗帘，光斑落在木桌上"
              ></textarea>
            </label>
            <div class="ui-field">
              <div class="block__head">
                <span class="ui-label">台词与说话人（可空，可多行）</span>
                <button class="ui-btn ui-btn--ghost" type="button" @click="addDialogueLine(index)">+ 加一句</button>
              </div>
              <p v-if="shotDialogueLines(shot).length === 0" class="hint">
                本段无台词：画面内不会安排任何对口型动作。
              </p>
              <div v-for="(line, lineIndex) in shotDialogueLines(shot)" :key="lineIndex" class="row dialogue-line">
                <select
                  v-model="line.speaker"
                  class="ui-select"
                  @change="onDialogueSpeakerChange(index)"
                >
                  <option v-for="option in speakerOptions" :key="option.value" :value="option.value">
                    {{ option.label }}
                  </option>
                </select>
                <input
                  v-model="line.text"
                  class="ui-input"
                  type="text"
                  placeholder="这一句台词"
                  @input="syncShotDialogue(shot)"
                />
                <button
                  class="ui-btn ui-btn--ghost"
                  type="button"
                  @click="removeDialogueLine(index, lineIndex)"
                >删除</button>
              </div>
              <span class="hint">按开口顺序排列；轮到谁开口，镜头就给到谁，其他角色嘴唇闭合。</span>
            </div>
            <div class="ui-field">
              <span class="ui-label">本段出场角色（勾了就算出场：参考图与提示词会带上）</span>
              <div v-if="form.characterRefs.length > 0" class="chips chips--options">
                <label
                  v-for="ref in form.characterRefs.filter((item) => item.roleId)"
                  :key="ref.roleId"
                  class="check check--inline"
                >
                  <input
                    type="checkbox"
                    :checked="shot.roles.includes(ref.roleId)"
                    @change="toggleManualShotRole(index, ref.roleId)"
                  />
                  {{ ref.name || ref.roleId }}
                </label>
              </div>
              <p v-else class="hint">还没登记角色：到上面「2. 角色参考图」加一个，这里就能勾了。</p>
            </div>
            <div class="grid grid--3">
              <label class="ui-field">
                <span class="ui-label">运镜（可空）</span>
                <input v-model="shot.camera" class="ui-input" type="text" placeholder="缓慢推近 / 环绕主体" />
              </label>
              <label class="ui-field">
                <span class="ui-label">情绪（可空）</span>
                <input v-model="shot.mood" class="ui-input" type="text" placeholder="温暖 / 期待" />
              </label>
              <label class="ui-field">
                <span class="ui-label">环境音（可空）</span>
                <input v-model="shot.audioCue" class="ui-input" type="text" placeholder="咖啡机蒸汽声" />
              </label>
            </div>
          </div>
          <p class="hint">
            说话人留空时：本段唯一角色自动当说话人，否则按旁白处理。每行台词独立归属，一段可以有多个角色轮流开口；
            纯旁白段落会在提示词里显式写明“画面内无人开口”。台词与角色都填了却缺画面内容，会在后端创建任务时直接报错。
          </p>
        </template>
      </section>

      <section class="ui-card block">
        <h3 class="ui-h3">5. 模型与提供商</h3>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">视频适配器</span>
            <select v-model="form.adapter" class="ui-select">
              <option v-for="option in adapterOptions" :key="option.id" :value="option.id">
                {{ option.label }}
              </option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">视频提供商（多模态生成）</span>
            <select v-model="form.providerId" class="ui-select">
              <option value="">自动选择第一个</option>
              <option v-for="provider in genProviders" :key="provider.id" :value="provider.id">
                {{ provider.name }} · {{ provider.model }}
              </option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">文本提供商（Agent1 / Agent2）</span>
            <select v-model="form.textProviderId" class="ui-select">
              <option value="">自动选择文本模型</option>
              <option v-for="provider in textProviders" :key="provider.id" :value="provider.id">
                {{ provider.name }} · {{ provider.model }}
              </option>
            </select>
          </label>
        </div>
        <p v-if="selectedAdapter" class="hint">{{ selectedAdapter.description }}</p>
        <p class="hint">{{ providerHint }}</p>
        <label class="check">
          <input v-model="form.localFallback" type="checkbox" />
          未配置文本模型时用本地规则生成剧本与提示词（不产生文本模型费用）
        </label>
      </section>

      <section class="ui-card block">
        <h3 class="ui-h3">6. 画面参数</h3>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">风格</span>
            <input v-model="form.videoParams.style" class="ui-input" type="text" placeholder="写实电影感" />
          </label>
          <label class="ui-field">
            <span class="ui-label">运镜偏好</span>
            <input v-model="form.videoParams.camera" class="ui-input" type="text" placeholder="缓慢推近 / 环绕主体" />
          </label>
          <label class="ui-field">
            <span class="ui-label">情绪</span>
            <input v-model="form.videoParams.mood" class="ui-input" type="text" placeholder="温暖 / 期待" />
          </label>
          <label class="ui-field">
            <span class="ui-label">额外提示</span>
            <input v-model="form.videoParams.extraPrompt" class="ui-input" type="text" placeholder="补充可控的光线/材质要求" />
          </label>
          <label class="ui-field">
            <span class="ui-label">负向提示</span>
            <input v-model="form.videoParams.negativePrompt" class="ui-input" type="text" placeholder="避免出现：文字水印、畸形手指" />
          </label>
          <label class="ui-field">
            <span class="ui-label">随机种子（0 = 随机）</span>
            <input v-model.number="form.videoParams.seed" class="ui-input" type="number" min="0" step="1" />
          </label>
        </div>
      </section>

      <section class="ui-card block">
        <div class="block__head">
          <h3 class="ui-h3">7. 字幕（默认生成）</h3>
        </div>
        <p class="hint">
          新任务默认生成字幕：视频生成后自动铺时间轴，可在剪辑页预览、逐条调整时间与位置，确认后再烧录。
          画面提示词始终要求模型不把文字画进画面。旧任务若曾关闭字幕，可在剪辑页为本次开启。
        </p>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">字体（内置 Noto Sans SC，跨机器可用）</span>
            <input v-model="form.subtitleStyle.fontName" class="ui-input" type="text" placeholder="Noto Sans SC" />
          </label>
          <label class="ui-field">
            <span class="ui-label">字号（画面高度 %）</span>
            <input
              v-model.number="form.subtitleStyle.fontSizePct"
              class="ui-input"
              type="number"
              min="1.2"
              max="12"
              step="0.2"
            />
          </label>
          <label class="ui-field">
            <span class="ui-label">位置</span>
            <select v-model.number="form.subtitleStyle.alignment" class="ui-select">
              <option v-for="item in SUBTITLE_ALIGNMENTS" :key="item.value" :value="item.value">
                {{ item.label }}
              </option>
            </select>
          </label>
          <div class="ui-field">
            <span class="ui-label">主色</span>
            <span class="row">
              <input v-model="form.subtitleStyle.primaryColor" class="ui-input" type="text" />
              <input v-model="form.subtitleStyle.primaryColor" class="color" type="color" />
            </span>
          </div>
          <div class="ui-field">
            <span class="ui-label">描边色</span>
            <span class="row">
              <input v-model="form.subtitleStyle.outlineColor" class="ui-input" type="text" />
              <input v-model="form.subtitleStyle.outlineColor" class="color" type="color" />
            </span>
          </div>
          <label class="ui-field">
            <span class="ui-label">描边宽度</span>
            <input
              v-model.number="form.subtitleStyle.outlineWidth"
              class="ui-input"
              type="number"
              min="0"
              max="8"
              step="0.5"
            />
          </label>
          <label class="ui-field">
            <span class="ui-label">底部边距（画面高度 %）</span>
            <input
              v-model.number="form.subtitleStyle.marginVpct"
              class="ui-input"
              type="number"
              min="0"
              max="30"
              step="0.5"
            />
          </label>
          <label class="check check--inline">
            <input v-model="form.subtitleStyle.bold" type="checkbox" />
            字幕加粗
          </label>
        </div>
        <p class="hint">
          字号按画面高度百分比换算；ASS PlayRes 与视频分辨率一致，跨 720P / 1080P 观感一致。
        </p>
      </section>

      <section class="ui-card block">
        <div class="block__head">
          <h3 class="ui-h3">8. 发布平台（Agent4 发布元数据）</h3>
          <span class="ui-chip">{{ form.platforms.length }} 个平台</span>
        </div>
        <div class="platforms">
          <button
            v-for="item in VIDEO_PLATFORMS"
            :key="item.id"
            class="platform"
            :class="{ 'platform--active': form.platforms.includes(item.id) }"
            type="button"
            @click="togglePlatform(item.id)"
          >
            {{ item.label }}
          </button>
        </div>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">受众时区</span>
            <select v-model="form.region" class="ui-select">
              <option v-for="item in REGION_OPTIONS" :key="item.id" :value="item.id">
                {{ item.label }}
              </option>
            </select>
          </label>
          <label class="ui-field">
            <span class="ui-label">垂类（影响发布时段偏移）</span>
            <select v-model="form.vertical" class="ui-select">
              <option value="">不指定（通用）</option>
              <option v-for="item in VERTICAL_OPTIONS" :key="item" :value="item">{{ item }}</option>
            </select>
          </label>
          <div class="ui-field">
            <span class="ui-label">主平台约束</span>
            <span class="ui-chip">{{ platformHint }}</span>
          </div>
        </div>
        <p class="hint">
          Agent4 只依赖已确认的提示词与剧本，确认门②通过后与渲染同时准备：关键词前置的标题 + 钩子、
          话题/搜索关键词、受众本地时间的 Top-N 发布时段与封面帧建议；确认门③只改文案，不重跑视频。
        </p>
      </section>

      <div class="create__actions">
        <button class="ui-btn ui-btn--primary" type="submit" :disabled="submitting">
          {{ submitting ? '创建中…' : '创建任务' }}
        </button>
        <span class="hint">创建后跳到「视频生成结果」，点击「执行任务」开始 Agent1 剧本分镜。</span>
      </div>
    </form>
  </section>
</template>

<style scoped>
.create {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.create__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.create__title {
  font-size: 16px;
  font-weight: 600;
}

.create__subtitle {
  max-width: 620px;
  margin-top: 4px;
  font-size: 12px;
  color: var(--color-text-muted);
}

.create__form {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding-bottom: 8px;
}

.block {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
}

.block__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.grid {
  display: grid;
  gap: 12px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
}

.grid--3 {
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
}

.row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

/* 台词行：说话人选择固定宽度，台词输入吃掉剩余空间 */
.dialogue-line .ui-select {
  flex: 0 0 190px;
}

.dialogue-line .ui-input {
  flex: 1 1 auto;
}

.dialogue-line .ui-btn {
  flex: 0 0 auto;
}

.character {
  display: grid;
  /* 新增一列给来源标签：没图时 v-if 不渲染，auto 轨道自然为 0 */
  grid-template-columns: minmax(120px, 0.6fr) minmax(220px, 2fr) auto auto auto;
  gap: 8px;
  align-items: center;
}

.character-picker {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}

.character__profile {
  grid-column: 1 / -1;
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 2px;
  padding: 10px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.02);
}

.character__profile summary {
  cursor: pointer;
  font-size: 12px;
  color: var(--color-text-muted);
}

.character__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

/* 手动分段：一段一张卡片，段头放时长与排序 */
.manual {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px;
  margin-top: 10px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
}

.manual__head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.manual__no {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-accent);
}

.manual__dur {
  display: flex;
  align-items: center;
  gap: 6px;
}

.manual__dur .ui-input {
  width: 88px;
}

.platforms {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.platform {
  padding: 5px 12px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.03);
  color: var(--color-text-muted);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  transition: border-color 160ms ease, background-color 160ms ease, color 160ms ease;
}

.platform:hover {
  border-color: rgba(255, 255, 255, 0.22);
  color: var(--color-text);
}

.platform--active {
  border-color: rgba(255, 180, 58, 0.6);
  background: rgba(255, 180, 58, 0.12);
  color: var(--color-accent);
}

.color {
  flex: none;
  width: 38px;
  height: 34px;
  padding: 2px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.28);
  cursor: pointer;
}

.check {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  color: var(--color-text-muted);
  cursor: pointer;
}

.check--inline {
  align-self: end;
  padding-bottom: 8px;
}

.hint {
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

/* 字幕关闭时样式区域降透明度提示“暂不生效”，但仍可提前调好 */
.subs--off {
  opacity: 0.55;
}

.create__actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  padding: 4px 2px 12px;
}

.source {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  padding: 8px 12px;
  border: 1px solid rgba(88, 214, 141, 0.28);
  border-radius: var(--radius-sm);
  background: rgba(88, 214, 141, 0.06);
}

.source__body {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  flex-wrap: wrap;
}

.source__topic {
  font-size: 11.5px;
  color: var(--color-text-muted);
  word-break: break-all;
}

.source__actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.preview {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.02);
}

.preview__line {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
  word-break: break-word;
}

.preview__tag {
  flex: none;
  min-width: 48px;
  padding: 1px 6px;
  border: 1px solid var(--color-border);
  border-radius: 4px;
  font-size: 11px;
  text-align: center;
  color: var(--color-text);
}

.sources {
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
  padding: 8px 10px;
}

.sources__summary {
  font-size: 12px;
  color: var(--color-text-muted);
  cursor: pointer;
}

.sources__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 4px 14px;
  margin-top: 8px;
}

.sources__row {
  display: grid;
  grid-template-columns: 88px minmax(0, 1fr) auto;
  gap: 8px;
  align-items: center;
  font-size: 11.5px;
}

.sources__label {
  color: var(--color-text-muted);
}

.sources__value {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.src {
  padding: 1px 7px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  font-size: 10.5px;
  line-height: 1.6;
  color: var(--color-text-muted);
}

.src--template {
  border-color: rgba(88, 214, 141, 0.45);
  color: #58d68d;
}

.src--override {
  border-color: rgba(255, 180, 58, 0.55);
  color: var(--color-accent);
}

.src--default {
  opacity: 0.75;
}

@media (max-width: 900px) {
  .character {
    grid-template-columns: 1fr;
  }
}

.chips--options {
  display: flex;
  align-items: center;
  gap: 18px;
  flex-wrap: wrap;
  margin-top: 4px;
}

.chips--options .check--inline {
  align-self: center;
  padding-bottom: 0;
}

.tail-picker {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 8px;
  padding: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.tail-item {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.tail-item__img {
  width: 96px;
  height: 54px;
  object-fit: cover;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: #000;
}

.tail-item__info {
  display: flex;
  flex-direction: column;
  min-width: 120px;
  flex: 1;
}

.tail-item__title {
  font-size: 12px;
  overflow-wrap: anywhere;
}

.tail-item__meta {
  font-size: 11px;
  color: var(--color-text-muted);
}
</style>
