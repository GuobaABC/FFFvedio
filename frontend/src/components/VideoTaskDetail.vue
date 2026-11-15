<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { backend, errorText } from '../services/backend'
import SubtitleConfirm from './SubtitleConfirm.vue'
import {
  TEMPLATE_FIELD_LABELS,
  TEMPLATE_SCOPE_LABELS,
  TEMPLATE_SOURCE_LABELS,
  templateScopeClass,
  templateSourceClass,
} from '../types/template'
import {
  CONTINUATION_LABELS,
  MANUAL_SHOT_MAX_S,
  MANUAL_SHOT_MIN_S,
  STAGE_LABELS,
  STATUS_LABELS,
  SUBTITLE_ALIGNMENTS,
  VIDEO_PLATFORMS,
  VIDEO_STAGES,
  estimateSpeech,
  formatClock,
  formatSeconds,
  type TailFrameAsset,
  type VideoDialogueLine,
  type VideoPromptEdit,
  type VideoSegment,
  type VideoShot,
  type VideoSubtitleCue,
  type VideoTask,
} from '../types/video'

const props = defineProps<{ task: VideoTask }>()
const emit = defineEmits<{
  (event: 'changed'): void
  (event: 'deleted', taskId: string): void
}>()

/* 关卡编辑草稿：进入关卡时从 RunContext 复制一份，保存时才回写后端 */
const shotDrafts = ref<VideoShot[]>([])
const promptDrafts = ref<VideoPromptEdit[]>([])
const feedback = ref('')
const busy = ref('')
const actionError = ref('')
const actionInfo = ref('')
const confirmDelete = ref(false)
const showLogs = ref(false)

/* 关卡③：发布元数据草稿 */
const metaTitle = ref('')
const metaHashtags = ref('')
const metaKeywords = ref('')
const metaDescription = ref('')
const metaSlot = ref('')
const metaPlatforms = ref<string[]>([])
const metaFeedback = ref('')

function cloneShots(source: VideoShot[]): VideoShot[] {
  return source.map((shot) => {
    const dialogueLines = (shot.dialogueLines ?? []).map((line) => ({ ...line }))
    // 旧任务只有 dialogue/speaker：升级成一行，编辑时不再分叉成两套数据
    if (dialogueLines.length === 0 && (shot.dialogue ?? '').trim()) {
      dialogueLines.push({ speaker: shot.speaker ?? '', text: (shot.dialogue ?? '').trim() })
    }
    return {
      ...shot,
      timeRange: [shot.timeRange?.[0] ?? 0, shot.timeRange?.[1] ?? 0] as [number, number],
      roles: [...(shot.roles ?? [])],
      dialogueLines,
    }
  })
}

function syncDrafts() {
  shotDrafts.value = cloneShots(props.task.run?.script?.shots ?? [])
  promptDrafts.value = (props.task.run?.prompts ?? []).map((item) => ({
    shotId: item.shotId,
    visualPrompt: item.visualPrompt,
    dialogue: item.dialogue,
    speaker: item.speaker ?? '',
    dialogueLines: (item.dialogueLines ?? []).map((line) => ({ ...line })),
    durationS: Math.max(0, (item.end ?? 0) - (item.start ?? 0)),
  }))

  const meta = props.task.run?.publishMeta ?? null
  metaTitle.value = meta?.title?.primary ?? ''
  metaHashtags.value = (meta?.tags?.hashtags ?? []).join(' ')
  metaKeywords.value = (meta?.tags?.keywords ?? []).join(' ')
  metaDescription.value = meta?.description ?? ''
  metaSlot.value = meta?.publishTime?.candidates?.[0]?.slot ?? ''
  metaPlatforms.value = [...(props.task.config.platforms ?? [])]
}

watch(() => `${props.task.id}:${props.task.updatedAt}`, syncDrafts, { immediate: true })

/** 台词归属可选值：旁白 + 任务里的角色表；空值表示交给后端按出场角色推断 */
const speakerOptions = computed(() => {
  const options = [
    { value: '', label: '自动（本段唯一角色，否则旁白）' },
    { value: 'narration', label: '旁白（画外音）' },
  ]
  for (const ref of props.task.config?.characterRefs ?? []) {
    if (!ref.roleId) continue
    options.push({ value: ref.roleId, label: ref.name || ref.roleId })
  }
  return options
})

/** 说话人下拉的完整标签：选择框宽度有限，被截断时靠 title 兜底 */
function speakerOptionLabel(value?: string): string {
  const option = speakerOptions.value.find((item) => item.value === (value ?? ''))
  return option?.label ?? '自动（本段唯一角色，否则旁白）'
}

/** 把归属值翻译成人看得懂的名字：说话人写错时，用户要能一眼发现 */
function speakerLabelOf(speaker?: string): string {
  if (!speaker) return '归属待定'
  if (speaker === 'narration') return '旁白'
  const ref = (props.task.config?.characterRefs ?? []).find((item) => item.roleId === speaker)
  return ref?.name || speaker
}

type DialogueCarrier = { dialogue?: string; speaker?: string; dialogueLines?: VideoDialogueLine[] }

/** 取台词行：以 dialogueLines 为准，旧数据回落到 dialogue/speaker。 */
function dialogueLinesOf(target: DialogueCarrier): VideoDialogueLine[] {
  if (!target.dialogueLines) {
    target.dialogueLines = []
  }
  if (target.dialogueLines.length === 0 && (target.dialogue ?? '').trim()) {
    target.dialogueLines.push({ speaker: target.speaker ?? '', text: (target.dialogue ?? '').trim() })
  }
  return target.dialogueLines
}

/** 当前编辑中的台词全文：预算估算与保存都以它为准。 */
function dialogueTextOf(target: DialogueCarrier): string {
  const lines = (target.dialogueLines ?? []).map((line) => (line.text ?? '').trim()).filter(Boolean)
  return lines.length > 0 ? lines.join('\n') : (target.dialogue ?? '')
}

/** 台词行同步回兼容字段：dialogue 是全文，speaker 只在单说话人时保留。 */
function syncDialogueCompat(target: DialogueCarrier) {
  const lines = (target.dialogueLines ?? [])
    .map((line) => ({ speaker: (line.speaker ?? '').trim(), text: (line.text ?? '').trim() }))
    .filter((line) => line.text)
  target.dialogueLines = lines
  target.dialogue = lines.map((line) => line.text).join('\n')
  target.speaker = lines.length === 1 ? lines[0].speaker : ''
}

/** 一款多说话人台词编辑器：加一行 / 删一行，段与提示词关卡共用。 */
function addDialogueLine(target: DialogueCarrier, fallbackRoles: string[] = []) {
  const lines = dialogueLinesOf(target)
  const fallback = fallbackRoles.length === 1 ? fallbackRoles[0] : ''
  lines.push({ speaker: fallback, text: '' })
}

function removeDialogueLine(target: DialogueCarrier, index: number) {
  dialogueLinesOf(target).splice(index, 1)
  syncDialogueCompat(target)
}

function onDialogueEdited(target: DialogueCarrier) {
  syncDialogueCompat(target)
}

/** 多说话人的可读归属：多个用“、”连接，避免只显示兼容字段的空值。 */
function dialogueSpeakerLabelOf(target: DialogueCarrier): string {
  const speakers = [
    ...new Set(
      (target.dialogueLines ?? [])
        .map((line) => (line.speaker ?? '').trim())
        .filter(Boolean),
    ),
  ]
  if (speakers.length === 0) {
    return speakerLabelOf(target.speaker)
  }
  return speakers.map((speaker) => speakerLabelOf(speaker)).join('、')
}

const run = computed(() => props.task.run)
const config = computed(() => props.task.config)
const isRunning = computed(() => props.task.status === 'running')
const stageIndex = computed(() => VIDEO_STAGES.findIndex((item) => item.id === props.task.stage))
const canEditShots = computed(() => props.task.stage === 'CONFIRM_SCRIPT' && !isRunning.value)
const canEditPrompts = computed(() => props.task.stage === 'CONFIRM_PROMPT' && !isRunning.value)
const canEditMeta = computed(() => props.task.stage === 'CONFIRM_META' && !isRunning.value)

const plan = computed(() => run.value?.plan ?? null)
const renders = computed(() => {
  const segments = new Map((plan.value?.segments ?? []).map((segment) => [segment.id, segment]))
  return [...(run.value?.renders ?? [])]
    .map((render) => ({ render, segment: segments.get(render.segmentId) ?? null }))
    .sort((left, right) => (left.segment?.index ?? 0) - (right.segment?.index ?? 0))
})
const output = computed(() => run.value?.output ?? null)
const subtitleCues = computed(() => run.value?.subtitles?.cues ?? [])
const promptRows = computed(() => run.value?.prompts ?? [])
const warnings = computed(() => run.value?.warnings ?? [])
const logs = computed(() => run.value?.logs ?? [])

const gate1State = computed(() => run.value?.gates?.gate1 ?? 'pending')
const gate2State = computed(() => run.value?.gates?.gate2 ?? 'pending')
const gate3State = computed(() => run.value?.gates?.gate3 ?? 'pending')
const publishMeta = computed(() => run.value?.publishMeta ?? null)

/* 模板来源：任务内锁定的模板快照 + 每字段来源标注（template / override / default） */
const templateRun = computed(() => run.value?.template ?? null)
const sourceEntries = computed(() =>
  Object.entries(run.value?.configSources ?? {}).map(([key, source]) => ({
    key,
    label: TEMPLATE_FIELD_LABELS[key] ?? key,
    source,
  })),
)
const overrideCount = computed(() => sourceEntries.value.filter((row) => row.source === 'override').length)
const templateName = computed(() => templateRun.value?.name || props.task.templateId || '未命名模板')

const canRun = computed(
  () =>
    !isRunning.value &&
    !reviewPendingSegment.value &&
    (props.task.stage === 'DRAFT_SCRIPT' ||
      props.task.stage === 'BUILD_EDIT' ||
      props.task.stage === 'RENDER_VIDEO' ||
      props.task.stage === 'COMPOSE' ||
      props.task.stage === 'BUILD_PUBLISH_META'),
)

const runActionLabel = computed(() => {
  if (props.task.status === 'failed') {
    return '继续执行'
  }
  if (props.task.stage === 'DRAFT_SCRIPT') {
    return '执行任务'
  }
  return '继续执行'
})

/* 分镜来源：手动分段（跳过 Agent1）/ AI 自动生成 */
const scriptMode = computed(() => (config.value.scriptMode === 'manual' ? 'manual' : 'auto'))
const scriptSourceLabel = computed(() => {
  if (scriptMode.value === 'manual') {
    const count = run.value?.script?.shots?.length ?? config.value.manualShots?.length ?? 0
    return `手动分段（你填的 ${count} 段，未调 Agent1）`
  }
  if (run.value?.script?.source === 'llm_locked' || run.value?.script?.treatment?.mode === 'script_locked') {
    return '锁定剧本（台词原文，只做分镜）'
  }
  if (run.value?.script?.treatment) {
    return '文本模型（企划→分镜）'
  }
  return run.value?.script?.source === 'llm' ? '文本模型（Agent1）' : '本地规则'
})

/** 企划→分镜第一阶段的产物；旧任务为空 */
const treatment = computed(() => run.value?.script?.treatment ?? null)

const summaryRows = computed(() => [
  { label: '主题', value: config.value.topic },
  { label: '分镜来源', value: scriptSourceLabel.value },
  { label: '时长 / 帧率', value: `${formatSeconds(config.value.durationS)} · ${config.value.fps}fps` },
  { label: '画幅', value: config.value.aspectRatio },
  { label: '衔接方式', value: CONTINUATION_LABELS[config.value.continuationMode] ?? config.value.continuationMode },
  { label: '语速', value: `${config.value.speechRate} ${config.value.language.startsWith('en') ? '词' : '字'}/秒` },
  { label: '字幕', value: config.value.subtitles ? '生成并烧录（默认）' : '本任务不生成' },
  { label: '适配器', value: run.value?.adapter ? run.value.adapter : '自动推断' },
  { label: '首帧', value: config.value.firstFrame || '未指定（纯文生视频）' },
  {
    label: '首帧约束',
    value:
      config.value.strictFirstFrame === false
        ? '宽松（与角色图一起走全模态参考，锁帧靠提示词里的「图片1」约定）'
        : '严格（有首帧的片段必须从这张图起片，角色参考图让位）',
  },
  {
    label: '角色参考',
    value:
      config.value.characterRefs.length > 0
        ? config.value.characterRefs
            .map((item) => {
              const images = item.images ?? []
              const source =
                item.imageSource === 'template' ? '模板定妆图' : item.imageSource === 'user' ? '用户上传' : '未标注来源'
              const auto = item.autoCreated ? '，企划自动创建' : ''
              const species = item.species ? `，${item.species}` : ''
              return `${item.name || item.roleId}（${source}，${images.length} 张${item.appearanceBlock ? '，有外观锚点' : ''}${species}${auto}）`
            })
            .join('、')
        : '无',
  },
])

function shotBudget(shot: VideoShot) {
  const estimate = estimateSpeech(dialogueTextOf(shot), config.value)
  const slot = Math.max(0, (shot.timeRange?.[1] ?? 0) - (shot.timeRange?.[0] ?? 0))
  return { units: estimate.units, seconds: estimate.seconds, window: slot, fits: estimate.seconds <= slot + 0.1 }
}

function timingSourceLabel(source: string): string {
  if (source === 'tts') {
    return 'TTS 实测对齐'
  }
  if (source === 'aligned') {
    return '按音频活动对齐（提取音频→说话区间→台词对齐）'
  }
  if (source === 'measured') {
    return '片段实测对齐（与对白同步）'
  }
  if (source === 'estimate') {
    return '语速模型估算'
  }
  return source || '语速模型估算'
}

function promptNote(shotId: string): string {
  return promptRows.value.find((item) => item.shotId === shotId)?.note ?? ''
}

/** 该镜头在当前渲染计划里实际占用的时长（模型档位取整后） */
function promptPlanDuration(shotId: string): number {
  return (plan.value?.segments ?? [])
    .filter((segment) => segment.shotId === shotId)
    .reduce((sum, segment) => sum + segment.durationS, 0)
}

/** 提示词关卡里加台词行时的默认说话人：本镜头只有一个角色就直接选他。 */
function shotRolesForPrompt(shotId: string): string[] {
  return shotDrafts.value.find((shot) => shot.id === shotId)?.roles ?? []
}

/** 生成前展示本段的起片来源：用户首帧 / 上一段尾帧 / 无首帧 */
function segmentFirstFrameSource(segment: VideoSegment): string {
  if (segment.index > 0 && config.value.continuationMode === 'last_frame') {
    return '上一段尾帧'
  }
  return config.value.firstFrame ? '用户首帧' : '无首帧（文生视频）'
}

function setRoles(shot: VideoShot, value: string) {
  shot.roles = value
    .split(/[、,，;；\s]+/)
    .map((item) => item.trim())
    .filter(Boolean)
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

async function guard(label: string, task: () => Promise<void>) {
  actionError.value = ''
  actionInfo.value = ''
  busy.value = label
  try {
    await task()
    emit('changed')
  } catch (error) {
    actionError.value = errorText(error)
  } finally {
    busy.value = ''
  }
}

function runTask() {
  void guard('run', async () => {
    await backend.runVideoTask(props.task.id)
    actionInfo.value = '已进入执行队列，阶段与日志会自动刷新'
  })
}

function approveGate() {
  void guard('approve', async () => {
    await backend.approveVideoGate(props.task.id)
    actionInfo.value = '关卡已确认，进入下一阶段'
  })
}

function regenerate() {
  void guard('regenerate', async () => {
    await backend.regenerateVideoStage(props.task.id, feedback.value.trim())
    feedback.value = ''
    actionInfo.value = '已重新生成当前阶段，下游产物按需重算'
  })
}

function saveShots() {
  void guard('save-shots', async () => {
    for (const shot of shotDrafts.value) {
      syncDialogueCompat(shot)
    }
    await backend.saveVideoStageEdits(props.task.id, { shots: shotDrafts.value, prompts: [] })
    actionInfo.value = '分镜已保存，提示词 / 渲染结果已失效并等待重算'
  })
}

function savePrompts() {
  void guard('save-prompts', async () => {
    for (const prompt of promptDrafts.value) {
      syncDialogueCompat(prompt)
    }
    await backend.saveVideoStageEdits(props.task.id, { shots: [], prompts: promptDrafts.value })
    actionInfo.value = '提示词与段时长已保存；请核对下方计划，再点「确认并开始生成视频」'
  })
}

/* ---------------- 字幕摘要 ---------------- */
/** 把锚点值翻译成人看得懂的位置（0 = 沿用全局样式） */
function subtitleAnchorLabel(anchor?: number): string {
  if (!anchor) return '沿用全局位置'
  return SUBTITLE_ALIGNMENTS.find((item) => item.value === anchor)?.label ?? `锚点 ${anchor}`
}

/** 逐条布局摘要：只列用户改过的项，没改就不噪音 */
function cueLayoutNote(cue: VideoSubtitleCue): string {
  const parts: string[] = []
  if (cue.anchor) parts.push(subtitleAnchorLabel(cue.anchor))
  if (cue.marginVpct) parts.push(`留白 ${cue.marginVpct}%`)
  if (cue.fontSizePct) parts.push(`字号 ${cue.fontSizePct}%`)
  return parts.length > 0 ? ` · ${parts.join(' / ')}` : ''
}

/** 标签文本框支持空格 / 逗号 / 顿号 / 分号 / # 分隔 */
function splitTags(value: string): string[] {
  return value
    .split(/[\s,，、;；]+/)
    .map((item) => item.trim().replace(/^#+/, ''))
    .filter(Boolean)
}

function toggleMetaPlatform(id: string) {
  const index = metaPlatforms.value.indexOf(id)
  if (index >= 0) {
    metaPlatforms.value.splice(index, 1)
    return
  }
  metaPlatforms.value.push(id)
}

function saveMeta() {
  void guard('save-meta', async () => {
    await backend.saveVideoStageEdits(props.task.id, {
      shots: [],
      prompts: [],
      meta: {
        title: metaTitle.value,
        hashtags: splitTags(metaHashtags.value),
        keywords: splitTags(metaKeywords.value),
        description: metaDescription.value,
        slot: metaSlot.value,
        coverTs: null,
      },
    })
    actionInfo.value = '发布元数据已保存，等待确认'
  })
}

function regenerateMeta() {
  void guard('regen-meta', async () => {
    await backend.regenerateVideoMeta(props.task.id, metaPlatforms.value, metaFeedback.value.trim())
    metaFeedback.value = ''
    actionInfo.value = '已按当前平台重新生成发布元数据（视频不变）'
  })
}

function removeTask() {
  if (!confirmDelete.value) {
    confirmDelete.value = true
    return
  }
  void guard('delete', async () => {
    await backend.deleteVideoTask(props.task.id)
    emit('deleted', props.task.id)
  })
  confirmDelete.value = false
}

/* ---------------- 逐段确认关卡 ---------------- */
const segPromptDraft = ref('')
const segDialogueDrafts = ref<VideoDialogueLine[]>([])
const segFeedbackDraft = ref('')

/** 待确认的片段（开启逐段确认且本段新渲染成功） */
const reviewPendingSegment = computed<VideoSegment | null>(() => {
  const pendingId = run.value?.reviewPendingSegment ?? ''
  if (!pendingId || props.task.stage !== 'RENDER_VIDEO') {
    return null
  }
  return (plan.value?.segments ?? []).find((segment) => segment.id === pendingId) ?? null
})

/** 生成前的提示词确认：下一段尚未提交视频模型，先让用户过目/改写（需求 11） */
const pendingPromptSegment = computed<VideoSegment | null>(() => {
  const pendingId = run.value?.reviewPendingPromptSegment ?? ''
  if (!pendingId || props.task.stage !== 'RENDER_VIDEO') {
    return null
  }
  return (plan.value?.segments ?? []).find((segment) => segment.id === pendingId) ?? null
})

/** 渲染失败的片段：同样支持改提示词后只重试这一段 */
const failedSegment = computed<VideoSegment | null>(() => {
  if (reviewPendingSegment.value || pendingPromptSegment.value || props.task.stage !== 'RENDER_VIDEO') {
    return null
  }
  const failedRender = (run.value?.renders ?? []).find((render) => render.status === 'failed')
  if (!failedRender) {
    return null
  }
  return (plan.value?.segments ?? []).find((segment) => segment.id === failedRender.segmentId) ?? null
})

const reviewSegment = computed(() => reviewPendingSegment.value ?? failedSegment.value)
/** 当前正在审核的片段：生成后确认优先，否则只是生成前的提示词确认 */
const activeReviewSegment = computed(() => reviewSegment.value ?? pendingPromptSegment.value)
const reviewIsLast = computed(
  () => reviewSegment.value != null && reviewSegment.value.index >= (plan.value?.clipCount ?? 1) - 1,
)

/** 长镜头被拆成多段时，台词只属于首段：后续段不允许再填台词。 */
const segIsFirstOfShot = computed(() => {
  const segment = activeReviewSegment.value
  if (!segment) {
    return false
  }
  return !(plan.value?.segments ?? []).some(
    (item) => item.shotId === segment.shotId && item.index < segment.index,
  )
})

/** 切换待审片段时才重置草稿，避免 2s 轮询覆盖用户正在输入的内容 */
watch(
  () => `${props.task.id}:${activeReviewSegment.value?.id ?? ''}:${reviewPendingSegment.value ? 'after' : 'before'}`,
  () => {
    const segment = activeReviewSegment.value
    segPromptDraft.value = segment?.prompt ?? ''
    segDialogueDrafts.value = segment
      ? (segment.dialogueLines ?? []).length > 0
        ? (segment.dialogueLines ?? []).map((line) => ({ ...line }))
        : (segment.dialogue ?? '').trim()
          ? [{ speaker: segment.speaker ?? '', text: (segment.dialogue ?? '').trim() }]
          : []
      : []
    segFeedbackDraft.value = ''
  },
  { immediate: true },
)

/** 提交给后端的台词载荷：只保留有文本的行，并同步兼容字段。 */
function segmentDialoguePayload(): { dialogue: string; dialogueLines: VideoDialogueLine[]; speaker: string } {
  const lines = segIsFirstOfShot.value
    ? segDialogueDrafts.value
        .map((line) => ({ speaker: (line.speaker ?? '').trim(), text: (line.text ?? '').trim() }))
        .filter((line) => line.text)
    : []
  return {
    dialogue: lines.map((line) => line.text).join('\n'),
    dialogueLines: lines,
    speaker: lines.length === 1 ? lines[0].speaker : '',
  }
}

function addSegmentDialogueLine() {
  if (!segIsFirstOfShot.value) {
    return
  }
  segDialogueDrafts.value.push({ speaker: '', text: '' })
}

function removeSegmentDialogueLine(index: number) {
  segDialogueDrafts.value.splice(index, 1)
}

function approveSegment() {
  void guard('approve-segment', async () => {
    await backend.approveVideoSegment(props.task.id)
    actionInfo.value = reviewIsLast.value ? '片段已确认，进入合成阶段' : '片段已确认，开始生成下一段'
  })
}

/** 生成前确认提示词：可原样放行，也可先在面板里改写再提交模型 */
function approveSegmentPrompt() {
  const segment = pendingPromptSegment.value
  if (!segment) {
    return
  }
  void guard('approve-prompt', async () => {
    await backend.approveVideoSegmentPrompt(props.task.id, {
      segmentId: segment.id,
      visualPrompt: segPromptDraft.value.trim(),
      ...segmentDialoguePayload(),
    })
    actionInfo.value = `第 ${segment.index + 1} 段提示词已确认，开始生成本段`
  })
}

function regenerateSegment() {
  const segment = reviewSegment.value
  if (!segment) {
    return
  }
  void guard('regen-segment', async () => {
    await backend.regenerateVideoSegment(props.task.id, {
      segmentId: segment.id,
      visualPrompt: segPromptDraft.value.trim(),
      ...segmentDialoguePayload(),
    })
    segFeedbackDraft.value = ''
    actionInfo.value = '已按修改后的提示词重新生成该段'
  })
}

function suggestSegmentPrompt() {
  const segment = activeReviewSegment.value
  if (!segment) {
    return
  }
  void guard('ai-suggest', async () => {
    const suggestion = await backend.suggestSegmentPrompt(
      props.task.id,
      segment.id,
      segFeedbackDraft.value.trim(),
    )
    segPromptDraft.value = suggestion
    actionInfo.value = 'AI 已按意见重写提示词，可直接确认生成或继续微调'
  })
}

/* ---------------- 尾帧资产（成片输出区展示） ---------------- */
const tailAssets = ref<TailFrameAsset[]>([])
const tailAsset = computed(() => tailAssets.value.find((item) => item.taskId === props.task.id) ?? null)

watch(
  () => `${props.task.id}:${props.task.stage}`,
  async () => {
    if (!props.task.config.keepLastFrame) {
      return
    }
    try {
      tailAssets.value = await backend.listTailFrameAssets()
    } catch {
      tailAssets.value = []
    }
  },
  { immediate: true },
)

function openFolder() {
  void guard('folder', async () => {
    await backend.openVideoTaskFolder(props.task.id)
  })
}

function revealOutput() {
  void guard('reveal', async () => {
    await backend.revealVideoOutput(props.task.id)
  })
}
</script>

<template>
  <article class="detail-card">
    <header class="head">
      <div class="head__main">
        <div class="head__title-row">
          <h3 class="head__title">{{ task.title }}</h3>
          <span class="ui-chip" :class="statusClass(task.status)">{{ STATUS_LABELS[task.status] }}</span>
          <span class="ui-chip">{{ STAGE_LABELS[task.stage] ?? task.stage }}</span>
        </div>
        <p class="head__meta">
          任务 {{ task.id }} · 创建 {{ formatClock(task.createdAt) }} · 更新 {{ formatClock(task.updatedAt) }}
        </p>
      </div>
      <div class="head__actions">
        <button v-if="canRun" class="ui-btn ui-btn--primary" type="button" :disabled="busy !== ''" @click="runTask">
          {{ busy === 'run' ? '提交中…' : runActionLabel }}
        </button>
        <button v-if="task.stage === 'DONE'" class="ui-btn" type="button" :disabled="busy !== ''" @click="regenerate">
          重新生成
        </button>
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="openFolder">任务目录</button>
        <button class="ui-btn ui-btn--danger" type="button" :disabled="busy !== ''" @click="removeTask">
          {{ confirmDelete ? '确认删除？' : '删除任务' }}
        </button>
      </div>
    </header>

    <ol class="timeline">
      <li
        v-for="(stage, index) in VIDEO_STAGES"
        :key="stage.id"
        class="timeline__item"
        :class="{
          'timeline__item--done': index < stageIndex,
          'timeline__item--active': index === stageIndex,
        }"
        :title="stage.hint"
      >
        <span class="timeline__dot" aria-hidden="true"></span>
        <span class="timeline__label">{{ stage.label }}</span>
      </li>
    </ol>

    <p v-if="task.error" class="ui-notice ui-notice--error">{{ task.error }}</p>
    <p v-if="actionError" class="ui-notice ui-notice--error">{{ actionError }}</p>
    <p v-if="actionInfo" class="ui-notice ui-notice--ok">{{ actionInfo }}</p>
    <p v-for="warning in warnings" :key="warning" class="ui-notice ui-notice--warn">{{ warning }}</p>
    <p v-if="isRunning" class="ui-notice ui-notice--warn">
      任务执行中：{{ STAGE_LABELS[task.stage] }}。页面每 2 秒自动刷新，可直接查看下方进度与日志。
    </p>

    <section class="block">
      <h4 class="block__title">输入契约</h4>
      <dl class="summary">
        <div v-for="row in summaryRows" :key="row.label" class="summary__row">
          <dt>{{ row.label }}</dt>
          <dd>{{ row.value }}</dd>
        </div>
      </dl>
    </section>

    <!-- 模板来源：默认值来自哪一层，以及任务内锁定的版本快照 -->
    <section v-if="templateRun" class="block">
      <header class="block__head">
        <h4 class="block__title">模板来源</h4>
        <div class="chips">
          <span class="ui-chip" :class="templateScopeClass(templateRun.scope)">
            {{ TEMPLATE_SCOPE_LABELS[templateRun.scope] }}
          </span>
          <span class="ui-chip">{{ templateName }} · v{{ templateRun.version }}</span>
          <span class="ui-chip">{{ sourceEntries.length }} 个字段分层覆盖（本次改动 {{ overrideCount }}）</span>
        </div>
      </header>
      <p class="hint">
        模板只提供默认值：任务内保存了 v{{ templateRun.version }} 的快照，之后修改或删除模板不影响本任务；三道确认门仍按阶段逐道生效。
      </p>
      <p v-if="templateRun.openingLine" class="opening">开场白（shot#1 台词底稿）：{{ templateRun.openingLine }}</p>
      <p v-if="templateRun.styleSnippet" class="hint">风格片段（按模型注入 Agent1 / Agent2）：{{ templateRun.styleSnippet }}</p>
      <p v-if="templateRun.note" class="hint">{{ templateRun.note }}</p>
      <p v-for="(item, index) in templateRun.compliance ?? []" :key="`tc${index}`" class="ui-notice ui-notice--error">
        {{ item }}
      </p>

      <div v-if="(templateRun.characters ?? []).length > 0" class="roles">
        <span class="roles__label">角色插图</span>
        <div v-for="character in templateRun.characters" :key="character.roleId" class="role">
          <span class="role__name">{{ character.name || character.roleId }}</span>
          <span class="role__meta">{{ (character.images ?? []).length }} 张定妆图</span>
          <span v-if="character.voiceHint" class="role__meta">{{ character.voiceHint }}</span>
          <span v-if="character.appearanceBlock" class="role__block">{{ character.appearanceBlock }}</span>
        </div>
      </div>

      <dl v-if="sourceEntries.length > 0" class="summary">
        <div v-for="row in sourceEntries" :key="row.key" class="summary__row">
          <dt>{{ row.label }}</dt>
          <dd>
            <span class="src" :class="templateSourceClass(row.source)">
              {{ TEMPLATE_SOURCE_LABELS[row.source] ?? row.source }}
            </span>
          </dd>
        </div>
      </dl>
    </section>

    <!-- 角色档案：角色库 / 模板快照都会在任务创建时固化，审查时可直接核对外观锚点 -->
    <section v-if="(config?.characterRefs ?? []).length > 0" class="block">
      <header class="block__head">
        <h4 class="block__title">角色档案</h4>
        <span class="ui-chip">{{ config.characterRefs.length }} 个角色</span>
      </header>
      <p class="hint">
        外观锚点会统一写进 Agent1 / Agent2 与本地兜底；任务保存的是创建时的档案快照，之后修改角色库不会改写本任务。
      </p>
      <div class="roles">
        <div v-for="ref in config.characterRefs" :key="ref.roleId" class="role">
          <span class="role__name">{{ ref.name || ref.roleId }}</span>
          <span class="role__meta">{{ ref.roleId }}</span>
          <span v-if="ref.species" class="ui-chip">{{ ref.species }}</span>
          <span v-if="ref.autoCreated" class="ui-chip ui-chip--warn">企划自动创建</span>
          <span class="role__meta">{{ (ref.images ?? []).length }} 张定妆图</span>
          <span v-if="ref.characterId" class="ui-chip ui-chip--ok">角色库快照</span>
          <span v-if="ref.appearanceBlock" class="role__block">外观：{{ ref.appearanceBlock }}</span>
          <span v-if="ref.persona" class="role__block">人设：{{ ref.persona }}</span>
          <span v-if="ref.negativePrompt" class="role__block">禁止：{{ ref.negativePrompt }}</span>
          <span v-if="ref.voiceHint" class="role__meta">声音：{{ ref.voiceHint }}</span>
        </div>
      </div>
    </section>

    <!-- 故事企划：两阶段流程的角色、节拍与连续性约定；旧任务没有该产物 -->
    <section v-if="treatment" class="block">
      <header class="block__head">
        <h4 class="block__title">故事企划</h4>
        <span class="ui-chip" :class="treatment.mode === 'script_locked' ? 'ui-chip--ok' : 'ui-chip--warn'">
          {{ treatment.mode === 'script_locked' ? '锁定台词' : '原创企划' }}
        </span>
        <span v-if="treatment.model" class="ui-chip">模型 {{ treatment.model }}</span>
        <span class="ui-chip">{{ (treatment.beats ?? []).length }} 个节拍 · {{ (treatment.cast ?? []).length }} 个角色</span>
        <span v-if="(treatment.lockedLines ?? []).length > 0" class="ui-chip ui-chip--ok">
          锁定 {{ treatment.lockedLines?.length }} 句台词
        </span>
      </header>
      <p class="opening">立意：{{ treatment.premise }}</p>
      <div v-if="(treatment.cast ?? []).length > 0" class="treatment__cast">
        <div v-for="member in treatment.cast ?? []" :key="member.roleId" class="treatment__cast-item">
          <span class="role__name">{{ member.name }}</span>
          <span class="role__meta">{{ member.roleId }}</span>
          <span v-if="member.species" class="ui-chip">{{ member.species }}</span>
          <span v-if="member.autoCreated" class="ui-chip ui-chip--warn">自动创建</span>
          <span class="treatment__voice">声音：{{ member.voiceHint || '未绑定' }}</span>
          <span v-if="member.appearance" class="role__block">{{ member.appearance }}</span>
        </div>
      </div>
      <div class="treatment__beats">
        <div v-for="beat in treatment.beats ?? []" :key="beat.id" class="treatment__beat">
          <div class="treatment__beat-head">
            <span class="ui-chip">{{ beat.id }}</span>
            <span class="ui-chip ui-chip--ok">{{ beat.function }}</span>
            <span class="treatment__beat-title">{{ beat.summary }}</span>
          </div>
          <p v-if="beat.newInfo" class="hint">新信息：{{ beat.newInfo }}</p>
          <p v-if="beat.link" class="hint">衔接：{{ beat.link }}</p>
        </div>
      </div>
      <p v-if="treatment.continuity?.length" class="hint">
        连续性：{{ treatment.continuity.join('；') }}
      </p>
      <p v-if="treatment.bible" class="hint">
        连续性圣经：
        <template v-if="treatment.bible.locations?.length">地点 {{ treatment.bible.locations.join('、') }}；</template>
        <template v-if="treatment.bible.props?.length">道具 {{ treatment.bible.props.join('、') }}；</template>
        <template v-if="treatment.bible.timeline?.length">时间线 {{ treatment.bible.timeline.join(' → ') }}；</template>
        <template v-if="treatment.bible.style">风格 {{ treatment.bible.style }}</template>
      </p>
      <p v-if="treatment.memoryHits?.length" class="hint">
        已参考项目记忆：{{ treatment.memoryHits.join('；') }}
      </p>
    </section>

    <!-- 关卡 ①：分镜确认 -->
    <section v-if="task.stage === 'CONFIRM_SCRIPT'" class="block">
      <header class="block__head">
        <h4 class="block__title">关卡 ① 分镜确认</h4>
        <span class="ui-chip" :class="gate1State === 'approved' ? 'ui-chip--ok' : 'ui-chip--warn'">
          {{ gate1State === 'approved' ? '已确认' : '待确认' }}
        </span>
      </header>
      <p class="hint">
        剧本来源：{{ scriptSourceLabel }}；修改分镜会让提示词、字幕与渲染全部失效并重算。
      </p>
      <p v-if="scriptMode === 'manual'" class="hint hint--warn">
        手动分段：这里的 {{ shotDrafts.length }} 段就是最终分镜。保存时每段时长保真，只把
        {{ MANUAL_SHOT_MIN_S }}–{{ MANUAL_SHOT_MAX_S }} 秒之外的段夹到边界并告警；目标时长按各段之和回写，
        模型档位对不齐时由渲染阶段的切分逻辑兜底。
      </p>
      <p v-if="run?.script?.logline" class="logline">{{ run.script.logline }}</p>

      <div v-for="(shot, index) in shotDrafts" :key="shot.id || index" class="shot">
        <header class="shot__head">
          <span class="shot__name">镜头 {{ index + 1 }} · {{ shot.id }}</span>
          <span class="ui-chip">
            {{ formatSeconds(shot.timeRange[0]) }} → {{ formatSeconds(shot.timeRange[1]) }}
          </span>
          <span class="ui-chip" :class="shotBudget(shot).fits ? 'ui-chip--ok' : 'ui-chip--error'">
            台词 ≈ {{ shotBudget(shot).seconds }}s / {{ formatSeconds(shotBudget(shot).window) }}
          </span>
          <span class="ui-chip">{{ dialogueSpeakerLabelOf(shot) }}</span>
        </header>
        <label class="ui-field">
          <span class="ui-label">场景意图</span>
          <textarea v-model="shot.sceneIntent" class="ui-textarea" rows="2" :disabled="!canEditShots"></textarea>
        </label>
        <div class="ui-field">
          <div class="block__head">
            <span class="ui-label">台词与说话人（语速预算内，可多行）</span>
            <button
              class="ui-btn ui-btn--ghost"
              type="button"
              :disabled="!canEditShots"
              @click="addDialogueLine(shot, shot.roles ?? [])"
            >+ 加一句</button>
          </div>
          <p v-if="dialogueLinesOf(shot).length === 0" class="hint">
            本段无台词：画面内不会安排任何对口型动作。
          </p>
          <div v-for="(line, lineIndex) in dialogueLinesOf(shot)" :key="lineIndex" class="row dialogue-line">
            <select
              v-model="line.speaker"
              class="ui-select"
              :title="speakerOptionLabel(line.speaker)"
              :disabled="!canEditShots"
              @change="onDialogueEdited(shot)"
            >
              <option v-for="option in speakerOptions" :key="option.value" :value="option.value">
                {{ option.label }}
              </option>
            </select>
            <input
              v-model="line.text"
              class="ui-input"
              type="text"
              :title="line.text"
              :disabled="!canEditShots"
              placeholder="这一句台词"
              @input="onDialogueEdited(shot)"
            />
            <button
              class="ui-btn ui-btn--ghost"
              type="button"
              title="删除这句台词"
              aria-label="删除这句台词"
              :disabled="!canEditShots"
              @click="removeDialogueLine(shot, lineIndex)"
            >删除</button>
          </div>
          <span class="hint">按开口顺序排列；轮到谁开口，镜头就给到谁，其他角色嘴唇闭合。</span>
        </div>
        <div class="grid grid--2">
          <label class="ui-field">
            <span class="ui-label">角色（顿号 / 逗号分隔）</span>
            <input
              class="ui-input"
              type="text"
              :value="(shot.roles ?? []).join('、')"
              :disabled="!canEditShots"
              @input="setRoles(shot, ($event.target as HTMLInputElement).value)"
            />
          </label>
          <label class="ui-field">
            <span class="ui-label">
              镜头时长（秒，直接采用，总时长按各段之和回写）
            </span>
            <input
              v-model.number="shot.durationS"
              class="ui-input"
              type="number"
              min="0.5"
              step="0.5"
              :disabled="!canEditShots"
            />
          </label>
          <label class="ui-field">
            <span class="ui-label">运镜</span>
            <input v-model="shot.camera" class="ui-input" type="text" :disabled="!canEditShots" />
          </label>
          <label class="ui-field">
            <span class="ui-label">情绪</span>
            <input v-model="shot.mood" class="ui-input" type="text" :disabled="!canEditShots" />
          </label>
        </div>
        <p v-if="!shotBudget(shot).fits" class="hint hint--warn">
          台词约 {{ shotBudget(shot).seconds }}s（{{ shotBudget(shot).units }} 个语速单位），超出本镜头
          {{ formatSeconds(shotBudget(shot).window) }}，建议精简台词或延长镜头。
        </p>
      </div>

      <div class="actions">
        <button class="ui-btn" type="button" :disabled="busy !== '' || !canEditShots" @click="saveShots">
          {{ busy === 'save-shots' ? '保存中…' : '保存分镜修改' }}
        </button>
        <button class="ui-btn ui-btn--primary" type="button" :disabled="busy !== ''" @click="approveGate">
          {{ busy === 'approve' ? '提交中…' : '确认分镜，生成提示词' }}
        </button>
      </div>
      <div v-if="scriptMode === 'manual'" class="hint">
        手动分段模式下不提供「按意见重写」：段结构是你定的，要改就直接改上面的表单再保存；想交回 Agent1 需新建自动模式任务。
      </div>
      <div v-else class="feedback">
        <label class="ui-field">
          <span class="ui-label">重新生成意见（可选）</span>
          <input
            v-model="feedback"
            class="ui-input"
            type="text"
            placeholder="例：镜头 2 太快，改成缓慢推近；结尾加一句品牌口号"
          />
        </label>
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="regenerate">
          {{ busy === 'regenerate' ? '重算中…' : '按意见重新生成分镜' }}
        </button>
      </div>
    </section>

    <!-- 关卡 ②：提示词确认 -->
    <section v-if="task.stage === 'CONFIRM_PROMPT'" class="block">
      <header class="block__head">
        <h4 class="block__title">关卡 ② 提示词确认（成本拍板）</h4>
        <span class="ui-chip" :class="gate2State === 'approved' ? 'ui-chip--ok' : 'ui-chip--warn'">
          {{ gate2State === 'approved' ? '已确认' : '待确认' }}
        </span>
      </header>
      <p class="hint">
        i2v 提示词只描述「首帧之后发生的变化」，并要求画面内不出现文字；台词是否写入提示词取决于模型是否支持原生音频。
      </p>

      <div v-if="plan" class="cost">
        <span class="ui-chip">适配器 {{ plan.adapter }}</span>
        <span class="ui-chip">目标 {{ config.durationS }}s</span>
        <span class="ui-chip">分段 {{ plan.clipCount }}</span>
        <span class="ui-chip">预计时长 {{ plan.totalS }}s</span>
        <span v-if="Math.abs(plan.totalS - config.durationS) >= 0.5" class="ui-chip ui-chip--warn">
          档位差额 {{ (plan.totalS - config.durationS).toFixed(1) }}s
        </span>
        <span class="ui-chip">{{ plan.serial ? '串行生成' : '可并行' }}</span>
        <span class="ui-chip">预计积分 {{ plan.estCredits }}</span>
      </div>

      <div v-for="(prompt, index) in promptDrafts" :key="prompt.shotId || index" class="prompt">
        <header class="prompt__head">
          <span class="shot__name">{{ prompt.shotId }}</span>
          <span class="ui-chip">{{ dialogueSpeakerLabelOf(prompt) }}</span>
          <span v-if="promptNote(prompt.shotId)" class="ui-chip ui-chip--error">{{ promptNote(prompt.shotId) }}</span>
        </header>
        <label class="ui-field">
          <span class="ui-label">画面提示词</span>
          <textarea
            v-model="prompt.visualPrompt"
            class="ui-textarea"
            rows="4"
            :disabled="!canEditPrompts"
          ></textarea>
        </label>
        <div class="ui-field">
          <div class="block__head">
            <span class="ui-label">台词与说话人（可多行，按开口顺序排列）</span>
            <button
              class="ui-btn ui-btn--ghost"
              type="button"
              :disabled="!canEditPrompts"
              @click="addDialogueLine(prompt, shotRolesForPrompt(prompt.shotId))"
            >+ 加一句</button>
          </div>
          <p v-if="dialogueLinesOf(prompt).length === 0" class="hint">
            本段无台词：画面内不会安排任何对口型动作。
          </p>
          <div v-for="(line, lineIndex) in dialogueLinesOf(prompt)" :key="lineIndex" class="row dialogue-line">
            <select
              v-model="line.speaker"
              class="ui-select"
              :title="speakerOptionLabel(line.speaker)"
              :disabled="!canEditPrompts"
              @change="onDialogueEdited(prompt)"
            >
              <option v-for="option in speakerOptions" :key="option.value" :value="option.value">
                {{ option.label }}
              </option>
            </select>
            <input
              v-model="line.text"
              class="ui-input"
              type="text"
              :title="line.text"
              :disabled="!canEditPrompts"
              placeholder="这一句台词"
              @input="onDialogueEdited(prompt)"
            />
            <button
              class="ui-btn ui-btn--ghost"
              type="button"
              title="删除这句台词"
              aria-label="删除这句台词"
              :disabled="!canEditPrompts"
              @click="removeDialogueLine(prompt, lineIndex)"
            >删除</button>
          </div>
          <span class="hint">轮到谁开口，镜头就给到谁；旁白段会在提示词里显式写明“画面内无人开口”。</span>
        </div>
        <label class="ui-field">
          <span class="ui-label">
            本段时长（秒）· 当前计划 {{ promptPlanDuration(prompt.shotId) }}s
          </span>
          <input
            v-model.number="prompt.durationS"
            class="ui-input"
            type="number"
            min="0.5"
            max="600"
            step="0.5"
            :disabled="!canEditPrompts"
          />
          <span class="hint">保存后按模型档位切分；实际总时长与档位差额会显示在计划里。</span>
        </label>
      </div>

      <div class="actions">
        <button class="ui-btn" type="button" :disabled="busy !== '' || !canEditPrompts" @click="savePrompts">
          {{ busy === 'save-prompts' ? '保存中…' : '保存提示词修改' }}
        </button>
        <button class="ui-btn ui-btn--primary" type="button" :disabled="busy !== ''" @click="approveGate">
          {{ busy === 'approve' ? '提交中…' : '确认并开始生成视频' }}
        </button>
      </div>
      <div class="feedback">
        <label class="ui-field">
          <span class="ui-label">重新生成意见（可选）</span>
          <input
            v-model="feedback"
            class="ui-input"
            type="text"
            placeholder="例：整体偏向冷色调，主体动作更克制"
          />
        </label>
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="regenerate">
          {{ busy === 'regenerate' ? '重算中…' : '按意见重新生成提示词' }}
        </button>
        <p class="hint">整批重新生成会覆盖你在上面手工改过的提示词；只想改某一段时优先用逐段确认里的单段重渲。</p>
      </div>
    </section>

    <!-- 字幕确认关卡：排在视频生成之后，调完字幕才进合成 -->
    <SubtitleConfirm v-if="task.stage === 'CONFIRM_EDIT'" :task="task" @changed="emit('changed')" />

    <!-- 分段渲染 -->
    <section v-if="renders.length > 0 || task.stage === 'RENDER_VIDEO' || task.stage === 'COMPOSE' || task.stage === 'DONE'" class="block">
      <header class="block__head">
        <h4 class="block__title">分段渲染</h4>
        <span class="ui-chip">{{ renders.length }} 段</span>
        <span v-if="reviewPendingSegment" class="ui-chip ui-chip--warn">
          第 {{ reviewPendingSegment.index + 1 }} 段待确认
        </span>
        <span v-if="pendingPromptSegment" class="ui-chip ui-chip--warn">
          第 {{ pendingPromptSegment.index + 1 }} 段待生成
        </span>
      </header>
      <p v-if="renders.length === 0" class="hint">尚未产生片段；确认提示词后开始逐段生成。</p>
      <p v-if="reviewPendingSegment" class="ui-notice ui-notice--warn">
        逐段确认已开启：第 {{ reviewPendingSegment.index + 1 }} 段已生成，确认后才开始生成下一段；效果不佳可修改提示词（或让 AI 重写）后只重生成这一段。
      </p>

      <!-- 生成前提示词确认（需求 11）：本段尚未提交视频模型，先确认再花钱 -->
      <div v-if="pendingPromptSegment" class="review review--before">
        <p class="review__title">
          第 {{ pendingPromptSegment.index + 1 }} / {{ plan?.clipCount ?? 1 }} 段生成前确认 · 本段尚未提交视频模型，不会消耗额度
        </p>
        <div class="cost">
          <span class="ui-chip">{{ pendingPromptSegment.durationS }}s</span>
          <span class="ui-chip">起片来源：{{ segmentFirstFrameSource(pendingPromptSegment) }}</span>
        </div>
        <p class="hint">
          下面就是即将发给视频模型的画面提示词。可直接确认生成，也可先改写提示词/台词（台词改动只影响本段）；生成后如需返工仍可逐段重渲。
        </p>
        <label class="ui-field">
          <span class="ui-label">画面提示词</span>
          <textarea v-model="segPromptDraft" class="ui-textarea" rows="4"></textarea>
        </label>
        <div class="ui-field">
          <div class="block__head">
            <span class="ui-label">台词与说话人（改动后该段重渲，字幕时间轴在视频生成后重铺）</span>
            <button
              class="ui-btn ui-btn--ghost"
              type="button"
              :disabled="!segIsFirstOfShot"
              @click="addSegmentDialogueLine"
            >+ 加一句</button>
          </div>
          <p v-if="!segIsFirstOfShot" class="hint">
            本段属于长镜头的后续段：台词只属于镜头首段，不能在这里单独增改。
          </p>
          <p v-else-if="segDialogueDrafts.length === 0" class="hint">
            本段无台词：画面内不会安排任何对口型动作。
          </p>
          <div v-for="(line, lineIndex) in segDialogueDrafts" :key="lineIndex" class="row dialogue-line">
            <select v-model="line.speaker" class="ui-select" :title="speakerOptionLabel(line.speaker)" :disabled="!segIsFirstOfShot">
              <option v-for="option in speakerOptions" :key="option.value" :value="option.value">
                {{ option.label }}
              </option>
            </select>
            <input
              v-model="line.text"
              class="ui-input"
              type="text"
              :title="line.text"
              :disabled="!segIsFirstOfShot"
              placeholder="这一句台词"
            />
            <button
              class="ui-btn ui-btn--ghost"
              type="button"
              title="删除这句台词"
              aria-label="删除这句台词"
              :disabled="!segIsFirstOfShot"
              @click="removeSegmentDialogueLine(lineIndex)"
            >删除</button>
          </div>
        </div>
        <div class="feedback">
          <label class="ui-field">
            <span class="ui-label">哪里想调整？（供 AI 重写提示词参考）</span>
            <input
              v-model="segFeedbackDraft"
              class="ui-input"
              type="text"
              placeholder="例：主体动作更克制 / 光线更暖 / 镜头推进慢一点"
            />
          </label>
          <button class="ui-btn" type="button" :disabled="busy !== ''" @click="suggestSegmentPrompt">
            {{ busy === 'ai-suggest' ? 'AI 撰写中…' : 'AI 优化提示词' }}
          </button>
        </div>
        <div class="actions">
          <button
            class="ui-btn ui-btn--primary"
            type="button"
            :disabled="busy !== '' || !segPromptDraft.trim()"
            @click="approveSegmentPrompt"
          >
            {{ busy === 'approve-prompt' ? '提交中…' : '确认提示词，生成本段' }}
          </button>
        </div>
      </div>

      <div v-for="row in renders" :key="row.render.segmentId" class="render">
        <header class="render__head">
          <span class="shot__name">
            第 {{ (row.segment?.index ?? 0) + 1 }} 段 · {{ row.render.shotId }}
          </span>
          <span class="ui-chip" :class="row.render.status === 'succeeded' ? 'ui-chip--ok' : 'ui-chip--error'">
            {{ row.render.status }}
          </span>
          <span class="ui-chip">{{ row.render.durationS }}s</span>
        </header>
        <p v-if="row.render.note" class="hint">{{ row.render.note }}</p>
        <video v-if="row.render.mediaUrl" class="video" :src="row.render.mediaUrl" controls preload="metadata"></video>
        <p v-else class="hint">模型返回：{{ row.render.videoUri || '无本地文件' }}</p>

        <!-- 逐段确认面板：待确认或失败的片段 -->
        <div v-if="reviewSegment && row.render.segmentId === reviewSegment.id" class="review">
          <p class="review__title">
            {{ reviewPendingSegment ? '确认本段效果，或修改提示词后重新生成' : '本段生成失败：可修改提示词后仅重试这一段' }}
          </p>
          <label class="ui-field">
            <span class="ui-label">画面提示词</span>
            <textarea v-model="segPromptDraft" class="ui-textarea" rows="4"></textarea>
          </label>
          <div class="ui-field">
            <div class="block__head">
              <span class="ui-label">台词与说话人（改动后该段重渲，字幕时间轴在视频生成后重铺）</span>
              <button
                class="ui-btn ui-btn--ghost"
                type="button"
                :disabled="!segIsFirstOfShot"
                @click="addSegmentDialogueLine"
              >+ 加一句</button>
            </div>
            <p v-if="!segIsFirstOfShot" class="hint">
              本段属于长镜头的后续段：台词只属于镜头首段，不能在这里单独增改。
            </p>
            <p v-else-if="segDialogueDrafts.length === 0" class="hint">
              本段无台词：画面内不会安排任何对口型动作。
            </p>
            <div v-for="(line, lineIndex) in segDialogueDrafts" :key="lineIndex" class="row dialogue-line">
              <select v-model="line.speaker" class="ui-select" :title="speakerOptionLabel(line.speaker)" :disabled="!segIsFirstOfShot">
                <option v-for="option in speakerOptions" :key="option.value" :value="option.value">
                  {{ option.label }}
                </option>
              </select>
              <input
                v-model="line.text"
                class="ui-input"
                type="text"
                :title="line.text"
                :disabled="!segIsFirstOfShot"
                placeholder="这一句台词"
              />
              <button
                class="ui-btn ui-btn--ghost"
                type="button"
                title="删除这句台词"
                aria-label="删除这句台词"
                :disabled="!segIsFirstOfShot"
                @click="removeSegmentDialogueLine(lineIndex)"
              >删除</button>
            </div>
          </div>
          <div class="feedback">
            <label class="ui-field">
              <span class="ui-label">哪里不满意？（供 AI 重写提示词参考）</span>
              <input
                v-model="segFeedbackDraft"
                class="ui-input"
                type="text"
                placeholder="例：主体动作僵硬 / 光线太暗 / 镜头推进太快"
              />
            </label>
            <button class="ui-btn" type="button" :disabled="busy !== ''" @click="suggestSegmentPrompt">
              {{ busy === 'ai-suggest' ? 'AI 撰写中…' : 'AI 优化提示词' }}
            </button>
          </div>
          <div class="actions">
            <button
              v-if="reviewPendingSegment"
              class="ui-btn ui-btn--primary"
              type="button"
              :disabled="busy !== ''"
              @click="approveSegment"
            >
              {{ busy === 'approve-segment' ? '提交中…' : reviewIsLast ? '确认，进入合成' : '确认，生成下一段' }}
            </button>
            <button class="ui-btn" type="button" :disabled="busy !== ''" @click="regenerateSegment">
              {{ busy === 'regen-segment' ? '重渲中…' : reviewPendingSegment ? '改提示词并重生成本段' : '修改提示词并重试本段' }}
            </button>
          </div>
        </div>
      </div>
    </section>

    <!-- 输出物 -->
    <section v-if="output" class="block">
      <header class="block__head">
        <h4 class="block__title">成片输出</h4>
        <span class="ui-chip" :class="output.finalPath ? 'ui-chip--ok' : 'ui-chip--warn'">
          {{ output.finalPath ? '已产出' : '未合成' }}
        </span>
      </header>
      <p v-if="output.note" class="ui-notice ui-notice--warn">{{ output.note }}</p>
      <p v-if="!output.finalPath && output.mergedPath" class="hint">
        已保留分段视频：{{ output.mergedPath }}（可在上方分段渲染中逐个播放）
      </p>
      <video v-if="output.mediaUrl" class="video" :src="output.mediaUrl" controls preload="metadata"></video>
      <p class="hint">
        成片路径：{{ output.finalPath || '—' }} · 字幕{{ output.burnSubtitles ? '已烧录' : '未烧录' }} ·
        {{ output.finishedAt ? formatClock(output.finishedAt) : '' }}
      </p>
      <div class="actions">
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="revealOutput">在文件夹中定位成片</button>
        <button class="ui-btn" type="button" :disabled="busy !== ''" @click="openFolder">打开任务目录</button>
      </div>
      <div v-if="config.keepLastFrame" class="tail">
        <template v-if="tailAsset">
          <p class="hint">
            尾帧已存入资产库（{{ formatClock(tailAsset.createdAt) }}），可在新建任务的「从尾帧资产选择」中用作首帧继续创作。
          </p>
          <img v-if="tailAsset.mediaUrl" class="tail__img" :src="tailAsset.mediaUrl" alt="尾帧" />
          <p v-else class="hint hint--warn">尾帧文件已不在任务目录中，可能已被手动清理。</p>
        </template>
        <p v-else class="hint">本任务开启了保留尾帧，最后一段生成成功后会自动存入资产库。</p>
      </div>
    </section>

    <!-- 关卡③：发布元数据 -->
    <section v-if="publishMeta" class="block">
      <header class="block__head">
        <h4 class="block__title">发布元数据（关卡 ③）</h4>
        <span class="ui-chip">{{ publishMeta.platformLabel }}</span>
        <span class="ui-chip">{{ publishMeta.source === 'llm' ? '文本模型生成' : '本地规则生成' }}</span>
        <span class="ui-chip" :class="gate3State === 'approved' ? 'ui-chip--ok' : 'ui-chip--warn'">
          {{ gate3State === 'approved' ? '已确认' : '待确认' }}
        </span>
      </header>

      <p v-if="publishMeta.note" class="ui-notice ui-notice--warn">{{ publishMeta.note }}</p>

      <div class="meta">
        <header class="meta__head">
          <span class="meta__label">标题</span>
          <span
            class="ui-chip"
            :class="publishMeta.title.charCount > publishMeta.title.limit ? 'ui-chip--error' : 'ui-chip--ok'"
          >
            {{ publishMeta.title.charCount }} / {{ publishMeta.title.limit }} 字
          </span>
          <span class="ui-chip">首屏可见 {{ publishMeta.title.visibleLimit }} 字</span>
          <span class="ui-chip">{{ publishMeta.title.hookType }}</span>
        </header>
        <input v-if="canEditMeta" v-model="metaTitle" class="ui-input" type="text" />
        <p v-else class="meta__value">{{ publishMeta.title.primary }}</p>
        <div v-if="(publishMeta.title.alternatives ?? []).length > 0" class="chips">
          <span v-for="alt in publishMeta.title.alternatives ?? []" :key="alt" class="ui-chip">
            备选：{{ alt }}
          </span>
        </div>
        <p v-for="hit in publishMeta.title.compliance ?? []" :key="hit" class="hint hint--warn">
          合规：{{ hit }}
        </p>
      </div>

      <div class="meta">
        <header class="meta__head">
          <span class="meta__label">话题 / 搜索关键词</span>
          <span class="ui-chip">
            {{ publishMeta.tags.counts.hashtag }} 话题 · {{ publishMeta.tags.counts.keyword }} 关键词
          </span>
        </header>
        <template v-if="canEditMeta">
          <label class="ui-field">
            <span class="ui-label">话题（空格 / 逗号分隔，# 可省略）</span>
            <input v-model="metaHashtags" class="ui-input" type="text" />
          </label>
          <label class="ui-field">
            <span class="ui-label">搜索关键词</span>
            <input v-model="metaKeywords" class="ui-input" type="text" />
          </label>
        </template>
        <template v-else>
          <div class="chips">
            <span v-for="tag in publishMeta.tags.hashtags ?? []" :key="tag" class="ui-chip">{{ tag }}</span>
          </div>
          <p class="hint">关键词：{{ (publishMeta.tags.keywords ?? []).join(' / ') || '—' }}</p>
        </template>
        <p class="hint">
          来源：提示词槽位 {{ (publishMeta.tags.source.fromPromptSlots ?? []).join('、') || '—' }} ·
          主题词 {{ (publishMeta.tags.source.fromTopic ?? []).join('、') || '—' }}
        </p>
      </div>

      <div class="meta">
        <header class="meta__head">
          <span class="meta__label">发布时段（受众本地时间）</span>
          <span class="ui-chip">{{ publishMeta.publishTime.timezone }}</span>
        </header>
        <label v-for="item in publishMeta.publishTime.candidates ?? []" :key="item.slot" class="slot">
          <input v-if="canEditMeta" v-model="metaSlot" type="radio" :value="item.slot" />
          <span class="slot__time">{{ item.slot }}</span>
          <span class="slot__reason">{{ item.reason }}</span>
          <span class="ui-chip">评分 {{ item.score.toFixed(2) }}</span>
        </label>
        <p class="hint">{{ publishMeta.publishTime.bufferNote }}</p>
      </div>

      <div class="meta">
        <header class="meta__head">
          <span class="meta__label">封面与正文</span>
          <span class="ui-chip">封面 {{ publishMeta.cover.ratio }} · 第 {{ publishMeta.cover.frameTs }}s</span>
          <span class="ui-chip">{{ publishMeta.category }}</span>
        </header>
        <p class="hint">封面文案：{{ publishMeta.cover.textOverlay || '—' }}</p>
        <label v-if="canEditMeta" class="ui-field">
          <span class="ui-label">发布正文</span>
          <textarea v-model="metaDescription" class="ui-textarea" rows="4"></textarea>
        </label>
        <p v-else class="meta__value meta__value--pre">{{ publishMeta.description || '—' }}</p>
      </div>

      <template v-if="canEditMeta">
        <div class="meta">
          <header class="meta__head">
            <span class="meta__label">发布平台（切换后仅重算元数据，不重跑视频）</span>
          </header>
          <div class="chips">
            <button
              v-for="item in VIDEO_PLATFORMS"
              :key="item.id"
              type="button"
              class="platform"
              :class="{ 'platform--active': metaPlatforms.includes(item.id) }"
              @click="toggleMetaPlatform(item.id)"
            >
              {{ item.label }}
            </button>
          </div>
        </div>
        <div class="actions">
          <button class="ui-btn" type="button" :disabled="busy !== ''" @click="saveMeta">
            {{ busy === 'save-meta' ? '保存中…' : '保存文案修改' }}
          </button>
          <button class="ui-btn ui-btn--primary" type="button" :disabled="busy !== ''" @click="approveGate">
            {{ busy === 'approve' ? '提交中…' : '确认发布并完成' }}
          </button>
        </div>
        <div class="feedback">
          <label class="ui-field">
            <span class="ui-label">重生成意见（如：标签太多 / 换个更晚的时段）</span>
            <input v-model="metaFeedback" class="ui-input" type="text" placeholder="标签太多，标题再钩一点" />
          </label>
          <button class="ui-btn" type="button" :disabled="busy !== ''" @click="regenerateMeta">
            {{ busy === 'regen-meta' ? '重算中…' : '按当前平台重算' }}
          </button>
        </div>
      </template>
    </section>

    <!-- 字幕与制作计划 -->
    <section v-if="run?.subtitles || plan" class="block">
      <header class="block__head">
        <h4 class="block__title">字幕与渲染计划</h4>
        <span v-if="run?.subtitles" class="ui-chip">
          时间轴来源：{{ timingSourceLabel(run.subtitles.timingSource) }}
        </span>
        <span
          v-if="run?.subtitles"
          class="ui-chip"
          :class="run.subtitles.confirmed ? 'ui-chip--ok' : 'ui-chip--warn'"
        >
          {{ run.subtitles.confirmed ? '已确认：合成不再重排' : '待确认' }}
        </span>
      </header>
      <div v-if="plan" class="segments">
        <div v-for="segment in plan.segments" :key="segment.id" class="segment">
          <span class="segment__name">{{ segment.id }}</span>
          <span class="segment__meta">
            {{ formatSeconds(segment.start) }} → {{ formatSeconds(segment.end) }}（{{ segment.durationS }}s）
          </span>
          <span class="segment__prompt">{{ segment.prompt }}</span>
        </div>
      </div>
      <p v-for="item in plan?.warnings ?? []" :key="item" class="hint hint--warn">
        {{ item }}
      </p>
      <p v-if="run?.subtitles" class="hint">
        字幕文件：{{ run.subtitles.srtPath || '—' }}
      </p>
      <p v-else-if="plan && config.subtitles" class="hint">
        字幕将在视频生成后铺轴：拿拼接好的成片画布按片段实测时长与音频活动对齐，可在字幕关卡逐条改时间/位置/字号。
      </p>
      <p v-else-if="plan" class="hint">
        本次未生成字幕（默认关闭）：成片不含字幕；需要时在新建任务勾选「生成字幕并烧录」。
      </p>
      <p v-if="run?.subtitles?.timingTablePath" class="hint">
        字幕/时间对应表：{{ run.subtitles.timingTablePath }}
      </p>
      <div v-if="subtitleCues.length > 0" class="cues">
        <div v-for="cue in subtitleCues" :key="cue.index" class="cue">
          <span class="cue__time">
            {{ formatSeconds(cue.start) }} → {{ formatSeconds(cue.end) }}{{ cueLayoutNote(cue) }}
          </span>
          <span class="cue__text">{{ cue.text }}</span>
        </div>
      </div>
    </section>

    <section class="block">
      <header class="block__head">
        <h4 class="block__title">执行日志</h4>
        <button class="ui-btn ui-btn--ghost" type="button" @click="showLogs = !showLogs">
          {{ showLogs ? '收起' : `展开（${logs.length}）` }}
        </button>
      </header>
      <pre v-if="showLogs && logs.length > 0" class="logs">{{ logs.join('\n') }}</pre>
      <p v-else-if="logs.length === 0" class="hint">还没有日志，执行任务后这里会出现每个阶段的关键信息。</p>
    </section>
  </article>
</template>

<style scoped>
.detail-card {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding-bottom: 12px;
}

/* 台词行：说话人自适应宽度；容器变窄时自动折成「说话人」+「台词 / 删除」两行，
   避免固定 190px 的下拉把台词输入挤到只剩几个字 */
.dialogue-line {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.dialogue-line .ui-select {
  flex: 1 1 170px;
  min-width: 150px;
  max-width: 260px;
}

.dialogue-line .ui-input {
  flex: 1 1 220px;
}

.dialogue-line .ui-btn {
  flex: 0 0 auto;
}

.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
}

.head__main {
  min-width: 0;
}

.head__title-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.head__title {
  font-size: 15px;
  font-weight: 600;
}

.head__meta {
  margin-top: 4px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--color-text-muted);
  overflow-wrap: anywhere;
}

.head__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.timeline {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  margin: 0;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
  list-style: none;
}

.timeline__item {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11.5px;
  color: var(--color-text-muted);
  white-space: nowrap;
}

.timeline__item::after {
  content: '';
  width: 14px;
  height: 1px;
  margin: 0 2px;
  background: var(--color-border);
}

.timeline__item:last-child::after {
  display: none;
}

.timeline__dot {
  width: 7px;
  height: 7px;
  border: 1px solid currentColor;
  border-radius: 50%;
}

.timeline__item--done {
  color: #7ee2a8;
}

.timeline__item--done .timeline__dot {
  background: currentColor;
}

.timeline__item--active {
  color: var(--color-accent);
  font-weight: 600;
}

.timeline__item--active .timeline__dot {
  background: currentColor;
  box-shadow: 0 0 0 3px rgba(255, 180, 58, 0.18);
}

.block {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
}

.block__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}

.block__title {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
}

.summary {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 8px 16px;
  margin: 0;
}

.summary__row {
  display: flex;
  gap: 8px;
  min-width: 0;
  font-size: 12px;
}

.summary__row dt {
  flex: none;
  color: var(--color-text-muted);
}

.summary__row dd {
  margin: 0;
  min-width: 0;
  overflow-wrap: anywhere;
}

.logline {
  padding: 8px 10px;
  border-left: 2px solid rgba(255, 180, 58, 0.5);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text);
}

.shot,
.prompt,
.render {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.shot__head,
.prompt__head,
.render__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.shot__name {
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--color-accent);
}

.grid {
  display: grid;
  gap: 10px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
}

.cost {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.meta {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.meta__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.meta__label {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text);
}

.meta__value {
  font-size: 12.5px;
  line-height: 1.8;
  overflow-wrap: anywhere;
}

.meta__value--pre {
  white-space: pre-wrap;
  color: var(--color-text-muted);
}

.chips {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.slot {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 12px;
  cursor: pointer;
}

.slot__time {
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: var(--color-accent);
}

.slot__reason {
  color: var(--color-text-muted);
}

.platform {
  padding: 4px 11px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.03);
  color: var(--color-text-muted);
  font: inherit;
  font-size: 11.5px;
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

.actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.feedback {
  display: flex;
  align-items: flex-end;
  gap: 8px;
  flex-wrap: wrap;
}

.feedback .ui-field {
  flex: 1;
  min-width: 240px;
}

.video {
  width: 100%;
  max-height: 380px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: #000;
}

.review {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px;
  border: 1px dashed rgba(255, 180, 58, 0.55);
  border-radius: var(--radius-sm);
  background: rgba(255, 180, 58, 0.05);
}

.review__title {
  margin: 0;
  font-size: 12px;
  font-weight: 600;
  color: var(--color-accent);
}

.tail {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.tail__img {
  max-width: 260px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: #000;
}

.segments,
.cues {
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-height: 240px;
  overflow: auto;
}

.segment {
  display: grid;
  grid-template-columns: 64px 160px minmax(0, 1fr);
  gap: 8px;
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.segment__name {
  font-family: var(--font-mono);
  color: var(--color-accent);
}

.segment__prompt {
  overflow-wrap: anywhere;
}

.cue {
  display: grid;
  grid-template-columns: minmax(120px, 150px) minmax(0, 1fr);
  gap: 8px;
  align-items: start;
  font-size: 12px;
}

.cue__time {
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.7;
  color: var(--color-text-muted);
  overflow-wrap: anywhere;
}

.cue__text {
  overflow-wrap: anywhere;
}

.logs {
  max-height: 240px;
  margin: 0;
  padding: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.32);
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.7;
  color: #c9ccd4;
  white-space: pre-wrap;
  overflow: auto;
}

.hint {
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.hint--warn {
  color: #ffca7a;
}

.opening {
  padding: 8px 10px;
  border-left: 2px solid rgba(88, 214, 141, 0.55);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text);
}

.roles {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

/* 故事企划：角色卡 + 节拍卡 */
.treatment__cast,
.treatment__beats {
  display: grid;
  gap: 8px;
}

.treatment__cast {
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
}

.treatment__cast-item {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
  font-size: 12px;
}

.treatment__voice {
  flex-basis: 100%;
  font-size: 11.5px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.treatment__beat {
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.treatment__beat-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.treatment__beat-title {
  font-size: 12.5px;
  font-weight: 600;
}

.roles__label {
  font-size: 12px;
  color: var(--color-text-muted);
}

.role {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
  font-size: 12px;
}

.role__name {
  font-weight: 600;
}

.role__meta {
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.role__block {
  flex-basis: 100%;
  font-size: 11.5px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.src {
  padding: 1px 8px;
  border: 1px solid var(--color-border);
  border-radius: 999px;
  font-size: 10.5px;
  line-height: 1.7;
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
  .segment {
    grid-template-columns: 1fr;
  }

  .cue {
    grid-template-columns: 1fr;
  }
}
</style>
