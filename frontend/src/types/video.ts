/** 视频生成模块的数据结构，与 Go 侧 video_models.go 的 JSON 字段一一对应 */

import type { VideoTemplateRef, VideoTemplateRun } from './template'

export type VideoStage =
  | 'DRAFT_SCRIPT'
  | 'CONFIRM_SCRIPT'
  | 'BUILD_PROMPT_SUBS'
  | 'CONFIRM_PROMPT'
  | 'RENDER_VIDEO'
  | 'COMPOSE'
  | 'BUILD_PUBLISH_META'
  | 'CONFIRM_META'
  | 'DONE'

export type VideoStatus = 'draft' | 'running' | 'waiting' | 'done' | 'failed'

export type VideoAdapterId = 'auto' | 'dashscope' | 'runway' | 'veo' | 'generic' | 'mock'

export type ContinuationMode = 'last_frame' | 'overlap' | 'none'

export interface VideoCharacterRef {
  roleId: string
  name: string
  images: string[]
  voiceHint: string
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

export interface VideoShot {
  id: string
  timeRange: [number, number]
  roles: string[]
  sceneIntent: string
  dialogue: string
  camera: string
  mood: string
  audioCue: string
  durationS: number
}

export interface VideoScript {
  logline: string
  language: string
  source: string
  shots: VideoShot[]
}

export interface VideoPromptItem {
  shotId: string
  visualPrompt: string
  dialogue: string
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
}

export interface VideoSubtitles {
  srtPath: string
  assPath: string
  srtUrl: string
  assUrl: string
  timingSource: string
  cues: VideoSubtitleCue[]
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
  subtitles: VideoSubtitles | null
  renders: VideoRender[] | null
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
}

export interface VideoTask {
  id: string
  title: string
  stage: VideoStage
  status: VideoStatus
  error: string
  /** 任务引用的模板 ID（空 = 未使用模板） */
  templateId: string
  config: VideoRunConfig
  run: VideoRunContext
  createdAt: string
  updatedAt: string
}

export interface VideoTaskInput {
  title: string
  config: VideoRunConfig
}

export interface VideoPromptEdit {
  shotId: string
  visualPrompt: string
  dialogue: string
}

export interface VideoGateEdits {
  shots: VideoShot[]
  prompts: VideoPromptEdit[]
  meta?: VideoPublishEdit | null
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
  { id: 'BUILD_PROMPT_SUBS', label: '提示词与字幕', hint: 'Agent2 展开提示词、字幕时间轴' },
  { id: 'CONFIRM_PROMPT', label: '确认提示词', hint: '人工确认关卡 ②（成本拍板）' },
  { id: 'RENDER_VIDEO', label: '生成视频', hint: 'Agent3 适配器逐段生成' },
  { id: 'COMPOSE', label: '合成成片', hint: 'ffmpeg 拼接 + 烧录字幕' },
  { id: 'BUILD_PUBLISH_META', label: '发布元数据', hint: 'Agent4 标题 / 标签 / 发布时段' },
  { id: 'CONFIRM_META', label: '确认发布', hint: '人工确认关卡 ③' },
  { id: 'DONE', label: '完成', hint: '输出物与发布元数据已产出' },
]

export const STAGE_LABELS: Record<string, string> = {
  DRAFT_SCRIPT: '剧本分镜',
  CONFIRM_SCRIPT: '确认分镜',
  BUILD_PROMPT_SUBS: '提示词与字幕',
  CONFIRM_PROMPT: '确认提示词',
  RENDER_VIDEO: '生成视频',
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
    firstFrame: '',
    characterRefs: [],
    durationS: 10,
    fps: 24,
    aspectRatio: '16:9',
    continuationMode: 'last_frame',
    speechRate: 4,
    language: 'zh-CN',
    subtitles: false,
    videoParams: {
      style: '写实电影感',
      camera: '',
      mood: '',
      extraPrompt: '',
      negativePrompt: '',
      seed: 0,
    },
    subtitleStyle: {
      fontName: '思源黑体',
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
