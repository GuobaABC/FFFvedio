<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  SUBTITLE_ALIGNMENTS,
  formatClock,
  formatSeconds,
  type VideoEditClip,
  type VideoSubtitleCue,
  type VideoSubtitleIssue,
  type VideoTask,
} from '../types/video'

const props = defineProps<{ task: VideoTask }>()
const emit = defineEmits<{ (event: 'changed'): void }>()

/* 剪辑草稿：进入关卡时从 RunContext 复制，保存后才回写后端 */
const clips = ref<VideoEditClip[]>([])
const cues = ref<VideoSubtitleCue[]>([])
const issues = ref<VideoSubtitleIssue[]>([])
const busy = ref('')
const actionError = ref('')
const actionInfo = ref('')
const relayoutConfirm = ref(false)
const currentTime = ref(0)
const videoEl = ref<HTMLVideoElement | null>(null)

const run = computed(() => props.task.run)
const config = computed(() => props.task.config)
const editable = computed(() => props.task.stage === 'CONFIRM_EDIT' && props.task.status !== 'running')
const lastFrameMode = computed(() => config.value.continuationMode === 'last_frame')
const subtitlesEnabled = computed(() => config.value.subtitles)

function cloneClips(source: VideoEditClip[]): VideoEditClip[] {
  return [...source]
    .sort((left, right) => (left.order ?? 0) - (right.order ?? 0))
    .map((clip, index) => ({
      segmentId: clip.segmentId,
      shotId: clip.shotId ?? '',
      order: index,
      enabled: clip.enabled !== false,
      inPoint: clip.inPoint ?? 0,
      outPoint: clip.outPoint ?? 0,
      sourceDurationS: clip.sourceDurationS ?? 0,
    }))
}

function cloneCues(source: VideoSubtitleCue[]): VideoSubtitleCue[] {
  return source.map((cue, index) => ({
    index: cue.index ?? index + 1,
    start: cue.start ?? 0,
    end: cue.end ?? 0,
    text: cue.text ?? '',
    anchor: cue.anchor ?? 0,
    marginVpct: cue.marginVpct ?? 0,
    fontSizePct: cue.fontSizePct ?? 0,
    segmentId: cue.segmentId ?? '',
    offsetS: cue.offsetS ?? 0,
  }))
}

function syncDrafts() {
  clips.value = cloneClips(props.task.run?.edit?.clips ?? [])
  cues.value = cloneCues(props.task.run?.subtitles?.cues ?? [])
  issues.value = props.task.run?.subtitleIssues ?? []
  relayoutConfirm.value = false
  currentTime.value = 0
}

watch(
  () => `${props.task.id}:${props.task.updatedAt}:${props.task.run?.edit?.revision ?? 0}`,
  syncDrafts,
  { immediate: true },
)

const sortedClips = computed(() =>
  [...clips.value].sort((left, right) => left.order - right.order),
)

/** 每个片段裁剪后的有效时长 */
function clipDuration(clip: VideoEditClip): number {
  const source = clip.sourceDurationS || clip.outPoint || 0
  const out = clip.outPoint > 0 ? Math.min(clip.outPoint, source) : source
  return Math.max(0, out - Math.max(0, clip.inPoint))
}

const editTotalS = computed(() =>
  sortedClips.value
    .filter((clip) => clip.enabled)
    .reduce((sum, clip) => sum + clipDuration(clip), 0),
)

const canvasDurationS = computed(() => {
  const duration = run.value?.compose?.durationS ?? 0
  return duration > 0 ? duration : editTotalS.value
})

const subtitleMaxTime = computed(() => {
  const fromCues = cues.value.reduce((max, cue) => Math.max(max, cue.end ?? 0), 0)
  return Math.max(canvasDurationS.value, editTotalS.value, fromCues, config.value.durationS ?? 0)
})

const canvasUrl = computed(
  () => run.value?.compose?.canvasUrl || run.value?.output?.mediaUrl || '',
)

const errorIssues = computed(() => issues.value.filter((issue) => issue.level === 'error'))
const warningIssues = computed(() => issues.value.filter((issue) => issue.level === 'warning'))
const canApprove = computed(
  () => editable.value && errorIssues.value.length === 0 && busy.value === '',
)

/** 时间轴上每个片段的位置与宽度（按启用片段的实际时长比例） */
const trackSegments = computed(() => {
  const total = editTotalS.value || 1
  let cursor = 0
  return sortedClips.value.map((clip) => {
    const duration = clip.enabled ? clipDuration(clip) : 0
    const segment = {
      clip,
      leftPct: (cursor / total) * 100,
      widthPct: (duration / total) * 100,
    }
    cursor += duration
    return segment
  })
})

const activeCues = computed(() =>
  cues.value.filter((cue) => currentTime.value >= cue.start && currentTime.value < cue.end),
)

function overlayStyle(cue: VideoSubtitleCue): Record<string, string> {
  const anchor = cue.anchor || config.value.subtitleStyle?.alignment || 2
  const margin = cue.marginVpct || config.value.subtitleStyle?.marginVpct || 5
  const size = cue.fontSizePct || config.value.subtitleStyle?.fontSizePct || 4.2
  const horizontal = anchor % 3 === 1 ? '8%' : anchor % 3 === 0 ? '8%' : '0'
  const style: Record<string, string> = {
    fontSize: `${size}%`,
    marginLeft: horizontal,
    marginRight: horizontal,
  }
  if (anchor <= 3) {
    style.bottom = `${margin}%`
    style.top = 'auto'
  } else if (anchor >= 7) {
    style.top = `${margin}%`
    style.bottom = 'auto'
  } else {
    style.top = `${50 - margin}%`
    style.bottom = 'auto'
  }
  style.textAlign = anchor % 3 === 1 ? 'left' : anchor % 3 === 0 ? 'right' : 'center'
  return style
}

function onTimeUpdate() {
  currentTime.value = videoEl.value?.currentTime ?? 0
}

function seekTo(seconds: number) {
  if (videoEl.value) {
    videoEl.value.currentTime = Math.max(0, Math.min(seconds, subtitleMaxTime.value))
  }
  currentTime.value = seconds
}

function moveClip(index: number, delta: number) {
  if (!editable.value || lastFrameMode.value) return
  const ordered = sortedClips.value
  const target = index + delta
  if (target < 0 || target >= ordered.length) return
  const [moved] = ordered.splice(index, 1)
  ordered.splice(target, 0, moved)
  ordered.forEach((clip, position) => {
    clip.order = position
  })
  clips.value = ordered
}

function toggleClip(clip: VideoEditClip) {
  if (!editable.value) return
  clip.enabled = !clip.enabled
}

function addCue() {
  const last = cues.value[cues.value.length - 1]
  const start = last ? Math.round((last.end + 0.2) * 10) / 10 : 0
  cues.value.push({
    index: cues.value.length + 1,
    start,
    end: Math.min(start + 2.5, subtitleMaxTime.value || start + 2.5),
    text: '',
    anchor: 0,
    marginVpct: 0,
    fontSizePct: 0,
    segmentId: '',
    offsetS: 0,
  })
}

function removeCue(index: number) {
  cues.value.splice(index, 1)
}

function payload() {
  return {
    clips: sortedClips.value.map((clip, index) => ({ ...clip, order: index })),
    cues: cues.value,
  }
}

async function guard(label: string, task: () => Promise<string>) {
  actionError.value = ''
  actionInfo.value = ''
  busy.value = label
  try {
    actionInfo.value = await task()
    emit('changed')
  } catch (error) {
    actionError.value = errorText(error)
  } finally {
    busy.value = ''
  }
}

function saveDraft(): Promise<VideoTask> {
  return backend.saveVideoEdit(props.task.id, payload())
}

function saveEdit() {
  void guard('save', async () => {
    await saveDraft()
    return '剪辑草稿已保存（尚未应用，预览画布与字幕时间轴暂不改变）'
  })
}

function applyEdit() {
  void guard('apply', async () => {
    await saveDraft()
    await backend.applyVideoEdit(props.task.id)
    return '已开始应用剪辑：预览画布会在后台重建，完成后回到本页'
  })
}

function relayoutSubtitles() {
  if (!relayoutConfirm.value) {
    relayoutConfirm.value = true
    return
  }
  relayoutConfirm.value = false
  void guard('relayout', async () => {
    await backend.relayoutVideoSubtitles(props.task.id)
    return '已开始按当前时间轴重铺字幕，手工修改会被覆盖'
  })
}

function toggleSubtitles() {
  void guard('subtitles', async () => {
    await backend.setVideoSubtitlesEnabled(props.task.id, !subtitlesEnabled.value)
    return subtitlesEnabled.value ? '已关闭本任务字幕' : '已为本次开启字幕并生成时间轴'
  })
}

function approve() {
  void guard('approve', async () => {
    await saveDraft()
    await backend.approveVideoGate(props.task.id)
    return '剪辑与字幕已确认，开始合成成片'
  })
}

function regenerateEdit() {
  void guard('regenerate', async () => {
    await backend.regenerateVideoStage(props.task.id, '')
    return '已丢弃当前剪辑草稿，按视频重新生成默认剪辑计划与字幕'
  })
}

function validate() {
  void guard('validate', async () => {
    issues.value = await backend.validateVideoEdit(props.task.id, payload())
    if (errorIssues.value.length > 0) {
      throw new Error(`字幕存在 ${errorIssues.value.length} 个错误，请按下方提示修正后再确认`)
    }
    return `校验通过：${warningIssues.value.length} 条提醒`
  })
}
</script>

<template>
  <section class="block">
    <header class="block__head">
      <h4 class="block__title">剪辑与字幕（视频已生成）</h4>
      <span class="ui-chip" :class="run?.gates?.gate_edit === 'approved' ? 'ui-chip--ok' : 'ui-chip--warn'">
        {{ run?.gates?.gate_edit === 'approved' ? '已确认' : '待确认' }}
      </span>
      <span class="ui-chip">{{ sortedClips.filter((clip) => clip.enabled).length }}/{{ sortedClips.length }} 段</span>
      <span class="ui-chip">{{ cues.length }} 条字幕</span>
      <span v-if="run?.subtitles" class="ui-chip">时间轴来源：{{ run.subtitles.timingSource }}</span>
    </header>
    <p class="hint">
      左边是预览画布与时间轴，右边是字幕表。调整片段启停/入出点后点「应用剪辑」重建预览；
      字幕保存后不会立即烧录，确认合成时才写入成片。尾帧延续模式下片段顺序必须与生成顺序一致。
    </p>
    <p v-if="run?.subtitles?.editedAt" class="hint">上次修改：{{ formatClock(run.subtitles.editedAt) }}</p>

    <div v-if="!subtitlesEnabled" class="hint">
      本任务曾关闭字幕。需要字幕时可
      <button class="ui-btn" type="button" :disabled="busy !== ''" @click="toggleSubtitles">
        为本次开启字幕并生成
      </button>
    </div>

    <div class="edit-layout">
      <div class="edit-preview">
        <div v-if="canvasUrl" class="preview-shell">
          <video
            ref="videoEl"
            class="video"
            :src="canvasUrl"
            controls
            preload="metadata"
            @timeupdate="onTimeUpdate"
            @seeked="onTimeUpdate"
          ></video>
          <div class="preview-overlay">
            <div
              v-for="cue in activeCues"
              :key="`overlay-${cue.index}`"
              class="preview-cue"
              :style="overlayStyle(cue)"
            >
              {{ cue.text }}
            </div>
          </div>
        </div>
        <p v-else class="hint">预览画布尚未生成：保存后点「应用剪辑」重建。</p>

        <div class="timeline">
          <div class="timeline__ruler">
            <span>{{ formatSeconds(0) }}</span>
            <span>{{ formatSeconds(canvasDurationS) }}</span>
          </div>
          <div class="timeline__track">
            <button
              v-for="segment in trackSegments"
              :key="segment.clip.segmentId"
              class="timeline__segment"
              :class="{ 'timeline__segment--off': !segment.clip.enabled }"
              :style="{ left: `${segment.leftPct}%`, width: `${segment.widthPct}%` }"
              type="button"
              @click="seekTo((segment.leftPct / 100) * editTotalS)"
            >
              <span>{{ segment.clip.shotId || segment.clip.segmentId }}</span>
            </button>
          </div>
        </div>

        <div v-for="(clip, index) in sortedClips" :key="clip.segmentId" class="clip-row">
          <header class="clip-row__head">
            <label class="check check--inline">
              <input
                type="checkbox"
                :checked="clip.enabled"
                :disabled="!editable"
                @change="toggleClip(clip)"
              />
              启用
            </label>
            <span class="clip-row__name">{{ clip.shotId || clip.segmentId }}</span>
            <span class="ui-chip">源 {{ formatSeconds(clip.sourceDurationS) }}</span>
            <span class="ui-chip">入片 {{ formatSeconds(clipDuration(clip)) }}</span>
            <button
              class="ui-btn"
              type="button"
              :disabled="!editable || lastFrameMode || index === 0"
              :title="lastFrameMode ? '尾帧延续模式下不能调整片段顺序' : '上移'"
              @click="moveClip(index, -1)"
            >
              ↑
            </button>
            <button
              class="ui-btn"
              type="button"
              :disabled="!editable || lastFrameMode || index === sortedClips.length - 1"
              :title="lastFrameMode ? '尾帧延续模式下不能调整片段顺序' : '下移'"
              @click="moveClip(index, 1)"
            >
              ↓
            </button>
          </header>
          <div class="grid grid--2">
            <label class="ui-field">
              <span class="ui-label">入点（秒）</span>
              <input
                v-model.number="clip.inPoint"
                class="ui-input"
                type="number"
                min="0"
                :max="Math.max(0, clip.sourceDurationS - 0.25)"
                step="0.1"
                :disabled="!editable"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">出点（秒，0 = 到片尾）</span>
              <input
                v-model.number="clip.outPoint"
                class="ui-input"
                type="number"
                min="0"
                :max="clip.sourceDurationS"
                step="0.1"
                :disabled="!editable"
              />
            </label>
          </div>
        </div>
      </div>

      <div class="edit-subs">
        <div class="edit-subs__head">
          <h5 class="ui-h3">字幕时间轴</h5>
          <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="addCue">添加字幕</button>
          <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="validate">校验</button>
          <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="relayoutSubtitles">
            {{ relayoutConfirm ? '确认覆盖手工修改？' : '按时间轴重铺字幕' }}
          </button>
        </div>

        <ul v-if="issues.length > 0" class="issues">
          <li
            v-for="(issue, index) in issues"
            :key="`issue-${index}`"
            :class="issue.level === 'error' ? 'issues__item--error' : 'issues__item--warn'"
          >
            {{ issue.message }}
          </li>
        </ul>

        <p v-if="run?.subtitles" class="hint">
          保存后字幕文件会按当前草稿重写；确认合成前仍有 error 级问题时不能提交。
        </p>

        <div v-for="(cue, index) in cues" :key="`cue-${index}`" class="cue-row">
          <header class="cue-row__head">
            <span class="cue-row__name">字幕 {{ index + 1 }}</span>
            <span class="ui-chip">{{ formatSeconds(cue.start) }} → {{ formatSeconds(cue.end) }}</span>
            <span class="ui-chip" :class="cue.end - cue.start < 0.4 ? 'ui-chip--error' : ''">
              停留 {{ Math.max(0, cue.end - cue.start).toFixed(1) }}s
            </span>
            <button
              class="ui-btn ui-btn--danger"
              type="button"
              :disabled="!editable"
              @click="removeCue(index)"
            >
              删除
            </button>
          </header>
          <div class="grid grid--2">
            <label class="ui-field">
              <span class="ui-label">起始（秒）</span>
              <input
                v-model.number="cue.start"
                class="ui-input"
                type="number"
                min="0"
                :max="subtitleMaxTime"
                step="0.1"
                :disabled="!editable"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">结束（秒，最短 0.4s）</span>
              <input
                v-model.number="cue.end"
                class="ui-input"
                type="number"
                min="0"
                :max="subtitleMaxTime"
                step="0.1"
                :disabled="!editable"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">位置（屏幕九宫格）</span>
              <select v-model.number="cue.anchor" class="ui-input" :disabled="!editable">
                <option :value="0">沿用全局</option>
                <option v-for="option in SUBTITLE_ALIGNMENTS" :key="option.value" :value="option.value">
                  {{ option.label }}
                </option>
              </select>
            </label>
            <label class="ui-field">
              <span class="ui-label">垂直留白（%，0 = 全局 {{ config.subtitleStyle.marginVpct }}%）</span>
              <input
                v-model.number="cue.marginVpct"
                class="ui-input"
                type="number"
                min="0"
                max="90"
                step="0.5"
                :disabled="!editable"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">字号（%，0 = 全局 {{ config.subtitleStyle.fontSizePct }}%）</span>
              <input
                v-model.number="cue.fontSizePct"
                class="ui-input"
                type="number"
                min="0"
                max="30"
                step="0.1"
                :disabled="!editable"
              />
            </label>
          </div>
          <label class="ui-field">
            <span class="ui-label">字幕文本（留空则本条不写入）</span>
            <textarea v-model="cue.text" class="ui-textarea" rows="2" :disabled="!editable"></textarea>
          </label>
        </div>
      </div>
    </div>

    <div class="actions">
      <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="saveEdit">
        {{ busy === 'save' ? '保存中…' : '保存草稿' }}
      </button>
      <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="applyEdit">
        {{ busy === 'apply' ? '应用中…' : '应用剪辑（重建预览）' }}
      </button>
      <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="regenerateEdit">
        {{ busy === 'regenerate' ? '重算中…' : '丢弃草稿并重新生成' }}
      </button>
      <button
        v-if="subtitlesEnabled"
        class="ui-btn"
        type="button"
        :disabled="!editable || busy !== ''"
        @click="toggleSubtitles"
      >
        关闭本任务字幕
      </button>
      <button class="ui-btn ui-btn--primary" type="button" :disabled="!canApprove" @click="approve">
        {{ busy === 'approve' ? '提交中…' : '确认剪辑与字幕，开始合成' }}
      </button>
    </div>
    <p v-if="errorIssues.length > 0" class="hint hint--error">
      有 {{ errorIssues.length }} 个 error 级字幕问题，修正或删除对应字幕后才能确认合成。
    </p>
    <p v-if="actionError" class="hint hint--error">{{ actionError }}</p>
    <p v-if="actionInfo" class="hint">{{ actionInfo }}</p>
  </section>
</template>

<style scoped>
.edit-layout {
  display: grid;
  grid-template-columns: minmax(0, 1.15fr) minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}

@media (max-width: 1180px) {
  .edit-layout {
    grid-template-columns: minmax(0, 1fr);
  }
}

.preview-shell {
  position: relative;
  background: #000;
  border-radius: 10px;
  overflow: hidden;
}

.preview-shell .video {
  display: block;
  width: 100%;
}

.preview-overlay {
  position: absolute;
  inset: 0;
  pointer-events: none;
}

.preview-cue {
  position: absolute;
  left: 0;
  right: 0;
  color: #fff;
  font-weight: 700;
  line-height: 1.35;
  white-space: pre-line;
  text-shadow:
    -2px -2px 0 #000,
    2px -2px 0 #000,
    -2px 2px 0 #000,
    2px 2px 0 #000;
}

.timeline {
  margin: 12px 0;
}

.timeline__ruler {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  opacity: 0.7;
}

.timeline__track {
  position: relative;
  height: 34px;
  background: rgba(127, 127, 127, 0.16);
  border-radius: 8px;
  overflow: hidden;
}

.timeline__segment {
  position: absolute;
  top: 3px;
  bottom: 3px;
  border: 0;
  border-radius: 6px;
  background: #5b8def;
  color: #fff;
  font-size: 11px;
  padding: 0 6px;
  overflow: hidden;
  white-space: nowrap;
  cursor: pointer;
}

.timeline__segment + .timeline__segment {
  box-shadow: inset 2px 0 0 rgba(0, 0, 0, 0.25);
}

.timeline__segment--off {
  background: rgba(120, 120, 120, 0.25);
  color: inherit;
  text-decoration: line-through;
}

.clip-row,
.cue-row {
  border: 1px solid rgba(127, 127, 127, 0.18);
  border-radius: 8px;
  padding: 10px;
  margin-bottom: 10px;
}

.clip-row__head,
.cue-row__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}

.clip-row__name,
.cue-row__name {
  font-weight: 600;
}

.edit-subs__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}

.edit-subs__head .ui-h3 {
  margin: 0 auto 0 0;
}

.issues {
  margin: 0 0 10px;
  padding-left: 18px;
  font-size: 12px;
}

.issues__item--error {
  color: #d64545;
}

.issues__item--warn {
  color: #b7791f;
}

.hint--error {
  color: #d64545;
}
</style>