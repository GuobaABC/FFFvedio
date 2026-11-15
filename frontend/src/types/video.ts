/** 视频生成模块的数据结构，与 Go 侧 video_models.go 的 JSON 字段一一对应 */

import type { VideoTemplateRef, VideoTemplateRun } from './template'

export type VideoStage =
  | 'DRAFT_SCRIPT'
  | 'CONFIRM_SCRIPT'
  | 'BUILD_PROMPT'
  | 'CONFIRM_PROMPT'
  | 'RENDER_VIDEO'
  | 'BUILD_EDIT'
  | 'CONFIRM_EDIT'
  | 'COMPOSE'
  | 'BUILD_PUBLISH_META'
  | 'CONFIRM_META'
  | 'DONE'

export type VideoStatus = 'draft' | 'running' | 'waiting' | 'done' | 'failed'

/** 分镜来源：AI 自动生成 / 用户手动分段 */
export type ScriptMode = 'auto' | 'manual'

export type VideoAdapterId = 'auto' | 'dashscope' | 'runway' | 'veo' | 'generic' | 'mock'

export type ContinuationMode = 'last_frame' | 'overlap' | 'none'

export interface VideoVoiceProfile {
  voiceId: string
  /** 语速倍率（1 = 正常） */
  rate: number
  /** 音高偏移（半音） */
  pitch: number
  /** 音量（0-2，1 = 原始音量） */
  volume: number
  emotion: string
}

/** 角色库条目：跨任务复用的角色档案；任务创建时后端会快照进 characterRefs */
export interface VideoCharacter {
  id: string
  name: string
  roleId: string
  appearanceBlock: string
  persona: string
  negativePrompt: string
  voiceHint: string
  voiceProfile: VideoVoiceProfile | null
  perspective: string
  images: string[]
  tags: string[]
  note: string
  createdAt: string
  updatedAt: string
}

export interface VideoCharacterRef {
  roleId: string
  name: string
  /** 自动创建角色的动物种类；用户角色可为空 */
  species?: string
  /** Agent1 企划自动创建的任务内临时角色 */
  autoCreated?: boolean
  /** 绑定的角色库条目；旧任务没有该字段 */
  characterId?: string
  images: string[]
  /** 图片来源：user = 用户上传（优先），template = 模板定妆图；留空后端按 user 处理 */
  imageSource?: string
  appearanceBlock?: string
  persona?: string
  negativePrompt?: string
  perspective?: string
  /** 声音风格提示词；新流程要求每个角色必须绑定 */
  voiceHint: string
  voiceProfile?: VideoVoiceProfile | null
}

export interface VideoSubtitleStyle {
  fontName: string
  fontSizePct: number
  primaryColor: string
  outlineColor: string
  outlineWidth: number
  alignment: number
  marginVpct: number
  bold: boolean
}

export interface VideoParams {
  style: string
  camera: string
  mood: string
  extraPrompt: string
  negativePrompt: string
  seed: number
}

export interface VideoRunConfig {
  topic: string
  /** 分镜来源：auto = Agent1 自动生成；manual = 用户逐段填写（跳过 Agent1） */
  scriptMode?: ScriptMode
  /** 用户提供了完整剧本：锁定台词，只做分镜化 */
  scriptLocked?: boolean
  /** 手动分段：scriptMode=manual 时直接当分镜用，段数与段时长以填写为准 */
  manualShots?: VideoShot[]
  firstFrame: string
  characterRefs: VideoCharacterRef[]
  durationS: number
  fps: number
  aspectRatio: string
  continuationMode: ContinuationMode
  speechRate: number
  language: string
  /** 是否生成并烧录字幕；默认关闭，开启后才产出 SRT/ASS 并烧进成片 */
  subtitles: boolean
  /** 逐段确认：每段生成后停住等用户确认/重生成，默认开启 */
  segmentReview: boolean
  /** 严格首帧：有首帧（含尾帧资产带入）的片段必须从这张图起片，默认开启 */
  strictFirstFrame: boolean
  /** 保留最后一段尾帧到资产库，供其他任务选作首帧 */
  keepLastFrame: boolean
  videoParams: VideoParams
  subtitleStyle: VideoSubtitleStyle
  adapter: VideoAdapterId | string
  providerId: string
  textProviderId: string
  localFallback: boolean
  /** Agent4 发布平台（可多选，首个为主平台） */
  platforms: string[]
  /** 受众时区，默认 Asia/Shanghai */
  region: string
  /** 垂类：美食/游戏/母婴/知识… */
  vertical: string
  /** 选中的模板：只预填默认值，不绕过三道确认门 */
  templateRef?: VideoTemplateRef | null
}

export interface VideoDialogueLine {
  /** 说话人：roleId 或 narration（旁白）；空字符串表示自动推断 */
  speaker: string
  text: string
}

export interface VideoShot {
  id: string
  timeRange: [number, number]
  roles: string[]
  sceneIntent: string
  /** 所有台词按行拼接后的兼容字段；多说话人时以 dialogueLines 为准 */
  dialogue: string
  /** 只有单一说话人时才保留；多说话人时为空 */
  speaker?: string
  dialogueLines?: VideoDialogueLine[]
  camera: string
  mood: string
  audioCue: string
  durationS: number
  /** 企划→分镜两阶段的新字段；旧任务为空 */
  beatId?: string
  function?: string
  newInfo?: string
  reaction?: string
  transition?: string
}

export interface VideoCastMember {
  roleId: string
  name: string
  species?: string
  appearance?: string
  persona?: string
  /** 声音风格提示词；新流程要求非空 */
  voiceHint: string
  autoCreated?: boolean
  ephemeral?: boolean
}

export interface VideoBeat {
  id: string
  function: string
  summary: string
  newInfo: string
  emotion?: string
  location?: string
  characters?: string[]
  props?: string[]
  link?: string
}

export interface VideoLockedLine {
  id: string
  speaker: string
  text: string
  scene?: number
}

export interface VideoBible {
  locations?: string[]
  props?: string[]
  timeline?: string[]
  style?: string
}

export interface VideoTreatment {
  mode: 'original' | 'script_locked' | string
  premise: string
  cast: VideoCastMember[]
  beats: VideoBeat[]
  continuity?: string[]
  bible?: VideoBible | null
  lockedLines?: VideoLockedLine[]
  memoryHits?: string[]
  model?: string
  revision?: number
}

export interface VideoScript {
  logline: string
  language: string
  source: string
  /** original / script_locked；旧任务为空 */
  mode?: string
  /** 企划→分镜两阶段产物；旧任务为空 */
  treatment?: VideoTreatment | null
  shots: VideoShot[]
}

export interface VideoPromptItem {
  shotId: string
  visualPrompt: string
  dialogue: string
  /** 只有单一说话人时才保留；多说话人时为空 */
  speaker?: string
  dialogueLines?: VideoDialogueLine[]
  start: number
  end: number
  charCount: number
  estSpeechS: number
  fits: boolean
  renderDurationS: number
  note: string
}

export interface VideoSegment {
  id: string
  shotId: string
  index: number
  prompt: string
  dialogue: string
  /** 台词归属（roleId / narration）与写进提示词的显示名；多说话人时 speaker 为空 */
  speaker?: string
  speakerName?: string
  dialogueLines?: VideoDialogueLine[]
  start: number
  end: number
  durationS: number
}

export interface VideoPlan {
  adapter: string
  clipCount: number
  serial: boolean
  estCredits: number
  totalS: number
  segments: VideoSegment[]
  warnings: string[]
}

export interface VideoSubtitleCue {
  index: number
  start: number
  end: number
  text: string
  /** 逐条对齐锚点（ASS numpad 1-9）；0 = 沿用全局样式 */
  anchor?: number
  /** 逐条垂直留白（占画面高度百分比）；0 = 沿用全局样式 */
  marginVpct?: number
  /** 逐条字号（占画面高度百分比）；0 = 沿用全局样式 */
  fontSizePct?: number
  /** 自动生成的字幕归属片段；剪辑重排/裁剪时按它做段内重映射 */
  segmentId?: string
  /** 相对片段起点的偏移（秒），保存时按当前剪辑计划重算 */
  offsetS?: number
}

/** 剪辑计划里的一个片段：顺序、启停与入出点 */
export interface VideoEditClip {
  segmentId: string
  shotId?: string
  /** 0-based，越小越靠前 */
  order: number
  enabled: boolean
  /** 相对源片段起点，秒 */
  inPoint: number
  /** 相对源片段起点，秒；0 表示到片尾 */
  outPoint: number
  /** 只读：probe 出来的源时长 */
  sourceDurationS: number
}

/** 用户可编辑的剪辑计划；revision 只在顺序/启停/入出点变化时 +1 */
export interface VideoEditPlan {
  clips: VideoEditClip[]
  revision: number
  updatedAt?: string
}

/** 字幕校验问题：error 级禁止确认合成，warning 只提示 */
export interface VideoSubtitleIssue {
  index: number
  level: 'error' | 'warning'
  code: string
  message: string
}

export interface VideoSubtitles {
  srtPath: string
  assPath: string
  srtUrl: string
  assUrl: string
  /** 字幕/时间对应表（JSON）：含对齐依据与逐条 cue */
  timingTablePath: string
  timingTableUrl: string
  /** estimate | measured | tts | aligned（按音频活动对齐） */
  timingSource: string
  cues: VideoSubtitleCue[]
  /** 用户在字幕关卡确认过：合成阶段只烧录，不再重排时间轴 */
  confirmed: boolean
  editedAt?: string
}

/** 合成前的剪辑预处理产物（选段/拼接/画幅归一），字幕与合成共用同一块画布 */
export interface VideoComposePrep {
  mergedPath: string
  canvasPath: string
  /** 成片画布的 /media 预览地址（字幕关卡里对着它调位置） */
  canvasUrl: string
  canvasWidth: number
  canvasHeight: number
  composedBySegment: Record<string, string> | null
  segmentIds: string[] | null
  /** 与剪辑计划 Revision 对齐；不等就说明画布已过期 */
  editRevision: number
  /** 画布实测总时长（秒），前端时间轴上限 */
  durationS: number
  hasFFmpeg: boolean
  updatedAt: string
}

export interface VideoRender {
  segmentId: string
  shotId: string
  taskId: string
  videoUri: string
  mediaUrl: string
  status: string
  note: string
  seed: number
  modelVer: string
  durationS: number
  firstFrame: string
  updatedAt: string
}

export interface VideoOutput {
  mergedPath: string
  finalPath: string
  mediaUrl: string
  burnSubtitles: boolean
  note: string
  finishedAt: string
}

export interface VideoRunContext {
  adapter: string
  script: VideoScript | null
  prompts: VideoPromptItem[] | null
  plan: VideoPlan | null
  /** 剪辑计划（BUILD_EDIT 自动生成，CONFIRM_EDIT 人工调整） */
  edit?: VideoEditPlan | null
  subtitles: VideoSubtitles | null
  /** 字幕校验问题：error 级禁止确认合成 */
  subtitleIssues?: VideoSubtitleIssue[] | null
  renders: VideoRender[] | null
  /** 合成前预处理缓存（BUILD_EDIT 算一次，合成阶段复用） */
  compose: VideoComposePrep | null
  output: VideoOutput | null
  publishMeta: PublishMeta | null
  /** 任务锁定的模板快照（含生效字段来源） */
  template: VideoTemplateRun | null
  /** 字段名 → template / override / default */
  configSources: Record<string, string> | null
  gates: Record<string, string>
  warnings: string[] | null
  logs: string[] | null
  feedback: string
  /** 逐段确认关卡：等待用户确认的片段 ID（空 = 无待确认） */
  reviewPendingSegment: string
  /** 逐段确认的「生成前」关卡：等待用户确认提示词的片段 ID（空 = 无待确认） */
  reviewPendingPromptSegment: string
  /** 已确认过提示词、可直接生成的片段 ID */
  approvedSegmentPrompts?: string[] | null
}

export interface VideoTask {
  id: string
  title: string
  stage: VideoStage
  status: VideoStatus
  error: string
  /** 任务引用的模板 ID（空 = 未使用模板） */
  templateId: string
  /** 归属项目 ID（空 = 未归类） */
  projectId: string
  /** 集数（0 = 未分集） */
  episode: number
  config: VideoRunConfig
  run: VideoRunContext
  createdAt: string
  updatedAt: string
}

export interface VideoTaskInput {
  title: string
  projectId: string
  episode?: number
  config: VideoRunConfig
}

export interface ProjectMemory {
  id: string
  projectId: string
  episode: number
  kind: string
  title: string
  content: string
  payload: string
  sourceTask: string
  createdAt: string
  updatedAt: string
  score?: number
}

export interface VideoPromptEdit {
  shotId: string
  visualPrompt: string
  dialogue: string
  /** 台词归属：roleId 或 narration；留空表示不改 */
  speaker?: string
  /** 多说话人台词；提交后以它为准 */
  dialogueLines?: VideoDialogueLine[]
  /** 本段目标时长（秒）；0 或省略表示沿用原时长 */
  durationS?: number
}

/** 字幕关卡的修改：提交完整的 cue 列表，增删都体现在列表里 */
export interface VideoSubtitleEdit {
  cues: VideoSubtitleCue[]
}

/** 剪辑页保存/校验的整表提交：片段计划 + 字幕草稿 */
export interface VideoEditEdit {
  clips: VideoEditClip[]
  cues: VideoSubtitleCue[]
}

export interface VideoGateEdits {
  shots: VideoShot[]
  prompts: VideoPromptEdit[]
  meta?: VideoPublishEdit | null
  subtitles?: VideoSubtitleEdit | null
}

/** 单段重生成入参：改该段提示词/台词后只重渲这一段及其后 */
export interface VideoSegmentRegen {
  segmentId: string
  visualPrompt: string
  dialogue: string
  /** 多说话人台词；提交后以它为准 */
  dialogueLines?: VideoDialogueLine[]
  /** 留空表示沿用原归属 */
  speaker?: string
}

/** 尾帧资产：任务最后一段的尾帧，可在新任务里选作首帧 */
export interface TailFrameAsset {
  id: string
  taskId: string
  taskTitle: string
  segmentId: string
  /** 跟随来源任务的归属项目（任务被删后为空 = 未归类） */
  projectId: string
  imagePath: string
  mediaUrl: string
  createdAt: string
}

/** 项目：任务的归组容器，项目视图按它看任务与尾帧 */
export interface VideoProject {
  id: string
  name: string
  note: string
  /** 以下统计仅列表查询时后端填充 */
  taskCount: number
  tailCount: number
  createdAt: string
  updatedAt: string
}

/* ---------------- Agent4 发布元数据（gate③） ---------------- */

export interface PublishTitle {
  primary: string
  alternatives: string[] | null
  alternativeHooks: string[] | null
  charCount: number
  limit: number
  visibleLimit: number
  hookType: string
  compliance: string[] | null
}

export interface PublishTagSource {
  fromPromptSlots: string[] | null
  fromTopic: string[] | null
  fromVertical: string[] | null
}

export interface PublishTags {
  hashtags: string[] | null
  keywords: string[] | null
  counts: { hashtag: number; keyword: number }
  source: PublishTagSource
}

export interface PublishTimeCandidate {
  slot: string
  reason: string
  score: number
}

export interface PublishTime {
  candidates: PublishTimeCandidate[] | null
  timezone: string
  bufferNote: string
}

export interface PublishCover {
  frameTs: number
  textOverlay: string
  ratio: string
}

export interface PublishMeta {
  platform: string
  platformLabel: string
  platforms: string[] | null
  title: PublishTitle
  tags: PublishTags
  publishTime: PublishTime
  cover: PublishCover
  description: string
  category: string
  source: string
  note: string
  generatedAt: string
}

/** gate③ 的局部修改：只改文案与时段，不动视频 */
export interface VideoPublishEdit {
  title: string
  hashtags: string[]
  keywords: string[]
  description: string
  slot: string
  coverTs: number | null
}

export interface VideoCapabilities {
  i2vFirstFrame: boolean
  lastFrame: boolean
  characterReference: boolean
  nativeAudio: boolean
  allowedDurations: number[]
  maxClipS: number
  allowedAspectRatios: string[]
  maxResolution: string
  promptStyle: string
  creditsPerClip: number
}

export interface VideoAdapterInfo {
  id: string
  label: string
  description: string
  /** 适配成熟度：primary = 字节跳动火山方舟 Seedance（主力适配），partial = 基础可用但未完整适配，local = 本地联调不计费 */
  maturity: 'primary' | 'partial' | 'local'
  maturityLabel: string
  maturityNote: string
  capabilities: VideoCapabilities
}

export interface VideoEnvironment {
  ffmpegAvailable: boolean
  ffmpegPath: string
  ffprobeAvailable: boolean
  workRoot: string
  adapters: VideoAdapterInfo[]
}

export const VIDEO_STAGES: { id: VideoStage; label: string; hint: string }[] = [
  { id: 'DRAFT_SCRIPT', label: '剧本分镜', hint: 'Agent1 生成结构化分镜' },
  { id: 'CONFIRM_SCRIPT', label: '确认分镜', hint: '人工确认关卡 ①' },
  { id: 'BUILD_PROMPT', label: '提示词', hint: 'Agent2 展开画面提示词' },
  { id: 'CONFIRM_PROMPT', label: '确认提示词', hint: '人工确认关卡 ②（成本拍板）' },
  { id: 'RENDER_VIDEO', label: '生成视频', hint: 'Agent3 适配器逐段生成' },
  { id: 'BUILD_EDIT', label: '生成字幕', hint: '生成字幕时间轴与预览画布' },
  { id: 'CONFIRM_EDIT', label: '确认字幕', hint: '人工确认关卡：调字幕位置、字号与时间' },
  { id: 'COMPOSE', label: '合成成片', hint: 'ffmpeg 拼接 + 烧录字幕' },
  { id: 'BUILD_PUBLISH_META', label: '发布元数据', hint: 'Agent4 标题 / 标签 / 发布时段' },
  { id: 'CONFIRM_META', label: '确认发布', hint: '人工确认关卡 ③' },
  { id: 'DONE', label: '完成', hint: '输出物与发布元数据已产出' },
]

export const STAGE_LABELS: Record<string, string> = {
  DRAFT_SCRIPT: '剧本分镜',
  CONFIRM_SCRIPT: '确认分镜',
  BUILD_PROMPT: '提示词',
  CONFIRM_PROMPT: '确认提示词',
  RENDER_VIDEO: '生成视频',
  BUILD_EDIT: '生成字幕',
  CONFIRM_EDIT: '确认字幕',
  COMPOSE: '合成成片',
  BUILD_PUBLISH_META: '发布元数据',
  CONFIRM_META: '确认发布',
  DONE: '完成',
}

/** 发布平台档位：与 Go 侧 PlatformProfile 对齐，仅用于前端展示约束 */
export const VIDEO_PLATFORMS: { id: string; label: string; titleMaxLen: number; hashtagMax: number }[] = [
  { id: 'douyin', label: '抖音', titleMaxLen: 30, hashtagMax: 5 },
  { id: 'channels', label: '视频号', titleMaxLen: 40, hashtagMax: 4 },
  { id: 'xiaohongshu', label: '小红书', titleMaxLen: 20, hashtagMax: 10 },
  { id: 'bilibili', label: 'B站', titleMaxLen: 80, hashtagMax: 15 },
  { id: 'kuaishou', label: '快手', titleMaxLen: 30, hashtagMax: 5 },
  { id: 'youtube_shorts', label: 'YouTube Shorts', titleMaxLen: 60, hashtagMax: 3 },
  { id: 'tiktok', label: 'TikTok', titleMaxLen: 100, hashtagMax: 5 },
]

export const VERTICAL_OPTIONS: string[] = ['美食', '游戏', '母婴', '知识', '职场', '健身', '旅行', '数码', '美妆', '生活']

export const REGION_OPTIONS: { id: string; label: string }[] = [
  { id: 'Asia/Shanghai', label: '中国大陆（Asia/Shanghai）' },
  { id: 'Asia/Hong_Kong', label: '中国香港（Asia/Hong_Kong）' },
  { id: 'Asia/Tokyo', label: '日本（Asia/Tokyo）' },
  { id: 'America/Los_Angeles', label: '美西（America/Los_Angeles）' },
  { id: 'Europe/London', label: '英国（Europe/London）' },
]

export const STATUS_LABELS: Record<VideoStatus, string> = {
  draft: '待执行',
  running: '执行中',
  waiting: '待确认',
  done: '已完成',
  failed: '失败',
}

export const CONTINUATION_LABELS: Record<string, string> = {
  last_frame: '尾帧延续',
  overlap: '重叠帧',
  none: '硬切',
}

export const SUBTITLE_ALIGNMENTS: { value: number; label: string }[] = [
  { value: 1, label: '左下' },
  { value: 2, label: '中下' },
  { value: 3, label: '右下' },
  { value: 4, label: '左中' },
  { value: 5, label: '居中' },
  { value: 6, label: '右中' },
  { value: 7, label: '左上' },
  { value: 8, label: '中上' },
  { value: 9, label: '右上' },
]

export function createEmptyVideoConfig(): VideoRunConfig {
  return {
    topic: '',
    scriptMode: 'auto',
    scriptLocked: false,
    manualShots: [],
    firstFrame: '',
    characterRefs: [],
    durationS: 10,
    fps: 24,
    aspectRatio: '16:9',
    continuationMode: 'last_frame',
    speechRate: 4,
    language: 'zh-CN',
    subtitles: true,
    segmentReview: true,
    strictFirstFrame: true,
    keepLastFrame: false,
    videoParams: {
      style: '写实电影感',
      camera: '',
      mood: '',
      extraPrompt: '',
      negativePrompt: '',
      seed: 0,
    },
    subtitleStyle: {
      fontName: 'Noto Sans SC',
      fontSizePct: 4.2,
      primaryColor: '#FFFFFF',
      outlineColor: '#000000',
      outlineWidth: 2,
      alignment: 2,
      marginVpct: 5,
      bold: false,
    },
    adapter: 'auto',
    providerId: '',
    textProviderId: '',
    localFallback: true,
    platforms: [],
    region: 'Asia/Shanghai',
    vertical: '',
    templateRef: null,
  }
}

export function isEnglish(config: VideoRunConfig): boolean {
  return config.language.toLowerCase().startsWith('en')
}

/** 手动分段的单段时长边界，与 Go 侧 manualShotMinS / manualShotMaxS 保持一致 */
export const MANUAL_SHOT_MIN_S = 4
export const MANUAL_SHOT_MAX_S = 15

/** 新建一段手动分段：默认 6 秒、空内容，段号按当前段数顺延 */
export function createManualShot(index: number): VideoShot {
  return {
    id: `s${index + 1}`,
    timeRange: [0, 0],
    roles: [],
    sceneIntent: '',
    dialogue: '',
    speaker: '',
    dialogueLines: [],
    camera: '',
    mood: '',
    audioCue: '',
    durationS: 6,
  }
}

/** 台词时长估算，与 Go 侧 estimateSpeechSeconds 口径一致（前端只用于确认门展示） */
export function estimateSpeech(text: string, config: VideoRunConfig): { units: number; seconds: number } {
  const trimmed = text.trim()
  if (!trimmed) {
    return { units: 0, seconds: 0 }
  }
  const units = isEnglish(config)
    ? trimmed.split(/\s+/).filter(Boolean).length
    : trimmed.replace(/[\s，。！？；、：,.!?;:]/g, '').length
  const rate = config.speechRate > 0 ? config.speechRate : isEnglish(config) ? 2.6 : 4
  return { units, seconds: Math.round((units / rate + 0.3) * 10) / 10 }
}

export function formatClock(value: string): string {
  if (!value) {
    return '—'
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return value
  }
  const pad = (input: number) => String(input).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export function formatSeconds(value: number): string {
  return Number.isInteger(value) ? `${value}s` : `${value.toFixed(1)}s`
}
