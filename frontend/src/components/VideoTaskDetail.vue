<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  TEMPLATE_FIELD_LABELS,
  TEMPLATE_SCOPE_LABELS,
  TEMPLATE_SOURCE_LABELS,
  templateScopeClass,
  templateSourceClass,
} from '../types/template'
import {
  CONTINUATION_LABELS,
  STAGE_LABELS,
  STATUS_LABELS,
  VIDEO_PLATFORMS,
  VIDEO_STAGES,
  estimateSpeech,
  formatClock,
  formatSeconds,
  type VideoPromptEdit,
  type VideoShot,
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
  return source.map((shot) => ({
    ...shot,
    timeRange: [shot.timeRange?.[0] ?? 0, shot.timeRange?.[1] ?? 0] as [number, number],
    roles: [...(shot.roles ?? [])],
  }))
}

function syncDrafts() {
  shotDrafts.value = cloneShots(props.task.run?.script?.shots ?? [])
  promptDrafts.value = (props.task.run?.prompts ?? []).map((item) => ({
    shotId: item.shotId,
    visualPrompt: item.visualPrompt,
    dialogue: item.dialogue,
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
    (props.task.stage === 'DRAFT_SCRIPT' ||
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

const summaryRows = computed(() => [
  { label: '主题', value: config.value.topic },
  { label: '时长 / 帧率', value: `${formatSeconds(config.value.durationS)} · ${config.value.fps}fps` },
  { label: '画幅', value: config.value.aspectRatio },
  { label: '衔接方式', value: CONTINUATION_LABELS[config.value.continuationMode] ?? config.value.continuationMode },
  { label: '语速', value: `${config.value.speechRate} ${config.value.language.startsWith('en') ? '词' : '字'}/秒` },
  { label: '字幕', value: config.value.subtitles ? '生成并烧录' : '不生成（默认）' },
  { label: '适配器', value: run.value?.adapter ? run.value.adapter : '自动推断' },
  { label: '首帧', value: config.value.firstFrame || '未指定（纯文生视频）' },
  {
    label: '角色参考',
    value:
      config.value.characterRefs.length > 0
        ? config.value.characterRefs.map((item) => item.name || item.roleId).join('、')
        : '无',
  },
])

function shotBudget(shot: VideoShot) {
  const estimate = estimateSpeech(shot.dialogue, config.value)
  const slot = Math.max(0, (shot.timeRange?.[1] ?? 0) - (shot.timeRange?.[0] ?? 0))
  return { units: estimate.units, seconds: estimate.seconds, window: slot, fits: estimate.seconds <= slot + 0.1 }
}

function timingSourceLabel(source: string): string {
  if (source === 'tts') {
    return 'TTS 实测对齐'
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
    await backend.saveVideoStageEdits(props.task.id, { shots: shotDrafts.value, prompts: [] })
    actionInfo.value = '分镜已保存，提示词 / 渲染结果已失效并等待重算'
  })
}

function savePrompts() {
  void guard('save-prompts', async () => {
    await backend.saveVideoStageEdits(props.task.id, { shots: [], prompts: promptDrafts.value })
    actionInfo.value = '提示词已保存，渲染计划已重算'
  })
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
        <span class="roles__label">人物插图</span>
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

    <!-- 关卡 ①：分镜确认 -->
    <section v-if="task.stage === 'CONFIRM_SCRIPT'" class="block">
      <header class="block__head">
        <h4 class="block__title">关卡 ① 分镜确认</h4>
        <span class="ui-chip" :class="gate1State === 'approved' ? 'ui-chip--ok' : 'ui-chip--warn'">
          {{ gate1State === 'approved' ? '已确认' : '待确认' }}
        </span>
      </header>
      <p class="hint">
        剧本来源：{{ run?.script?.source === 'llm' ? '文本模型（Agent1）' : '本地规则' }}；修改分镜会让提示词、字幕与渲染全部失效并重算。
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
        </header>
        <div class="grid grid--2">
          <label class="ui-field">
            <span class="ui-label">场景意图</span>
            <textarea v-model="shot.sceneIntent" class="ui-textarea" rows="2" :disabled="!canEditShots"></textarea>
          </label>
          <label class="ui-field">
            <span class="ui-label">台词（语速预算内）</span>
            <textarea v-model="shot.dialogue" class="ui-textarea" rows="2" :disabled="!canEditShots"></textarea>
          </label>
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
            <span class="ui-label">镜头时长（秒，保存后按总时长等比归一）</span>
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
      <div class="feedback">
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
        <span class="ui-chip">分段 {{ plan.clipCount }}</span>
        <span class="ui-chip">预计时长 {{ plan.totalS }}s</span>
        <span class="ui-chip">{{ plan.serial ? '串行生成' : '可并行' }}</span>
        <span class="ui-chip">预计积分 {{ plan.estCredits }}</span>
      </div>

      <div v-for="(prompt, index) in promptDrafts" :key="prompt.shotId || index" class="prompt">
        <header class="prompt__head">
          <span class="shot__name">{{ prompt.shotId }}</span>
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
        <label class="ui-field">
          <span class="ui-label">台词</span>
          <textarea v-model="prompt.dialogue" class="ui-textarea" rows="2" :disabled="!canEditPrompts"></textarea>
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
      </div>
    </section>

    <!-- 分段渲染 -->
    <section v-if="renders.length > 0 || task.stage === 'RENDER_VIDEO' || task.stage === 'COMPOSE' || task.stage === 'DONE'" class="block">
      <header class="block__head">
        <h4 class="block__title">分段渲染</h4>
        <span class="ui-chip">{{ renders.length }} 段</span>
      </header>
      <p v-if="renders.length === 0" class="hint">尚未产生片段；确认提示词后开始逐段生成。</p>
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
      <p v-else-if="plan" class="hint">
        本次未生成字幕（默认关闭）：成片不含字幕；需要时在新建任务勾选「生成字幕并烧录」。
      </p>
      <div v-if="subtitleCues.length > 0" class="cues">
        <div v-for="cue in subtitleCues" :key="cue.index" class="cue">
          <span class="cue__time">{{ formatSeconds(cue.start) }} → {{ formatSeconds(cue.end) }}</span>
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
  gap: 6px;
  margin: 0;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.02);
  list-style: none;
  overflow-x: auto;
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
  grid-template-columns: 130px minmax(0, 1fr);
  gap: 8px;
  font-size: 12px;
}

.cue__time {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--color-text-muted);
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
