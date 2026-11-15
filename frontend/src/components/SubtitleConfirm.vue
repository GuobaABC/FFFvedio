<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  SUBTITLE_ALIGNMENTS,
  formatClock,
  formatSeconds,
  type VideoSubtitleCue,
  type VideoSubtitleIssue,
  type VideoTask,
} from '../types/video'

const props = defineProps<{ task: VideoTask }>()
const emit = defineEmits<{ (event: 'changed'): void }>()

const cues = ref<VideoSubtitleCue[]>([])
const issues = ref<VideoSubtitleIssue[]>([])
const busy = ref('')
const actionError = ref('')
const actionInfo = ref('')
const relayoutConfirm = ref(false)
const currentTime = ref(0)
const selectedCue = ref(-1)
const videoEl = ref<HTMLVideoElement | null>(null)
const previewHeight = ref(0)

const run = computed(() => props.task.run)
const config = computed(() => props.task.config)
const editable = computed(() => props.task.stage === 'CONFIRM_EDIT' && props.task.status !== 'running')
const subtitlesEnabled = computed(() => config.value.subtitles)
const canvasUrl = computed(
  () => run.value?.compose?.canvasUrl || run.value?.output?.mediaUrl || '',
)
const canvasDurationS = computed(() => run.value?.compose?.durationS || config.value.durationS || 0)
const segments = computed(() => run.value?.plan?.segments ?? [])
const errorIssues = computed(() => issues.value.filter((issue) => issue.level === 'error'))
const canApprove = computed(() => editable.value && errorIssues.value.length === 0 && busy.value === '')

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
  cues.value = cloneCues(props.task.run?.subtitles?.cues ?? [])
  issues.value = props.task.run?.subtitleIssues ?? []
  relayoutConfirm.value = false
  currentTime.value = 0
  selectedCue.value = -1
}

watch(
  () => `${props.task.id}:${props.task.updatedAt}:${props.task.run?.subtitles?.editedAt ?? ''}`,
  syncDrafts,
  { immediate: true },
)

function updatePreviewHeight() {
  previewHeight.value = videoEl.value?.clientHeight ?? 0
}

onMounted(() => {
  window.addEventListener('resize', updatePreviewHeight)
})

onUnmounted(() => {
  window.removeEventListener('resize', updatePreviewHeight)
})

function onTimeUpdate() {
  currentTime.value = videoEl.value?.currentTime ?? 0
}

function seekTo(seconds: number) {
  if (videoEl.value) {
    videoEl.value.currentTime = Math.max(0, Math.min(seconds, canvasDurationS.value))
  }
  currentTime.value = seconds
}

function focusCue(index: number) {
  selectedCue.value = index
  const cue = cues.value[index]
  if (!cue) {
    return
  }
  videoEl.value?.pause()
  seekTo(cue.start)
}

function addCue() {
  const last = cues.value[cues.value.length - 1]
  const start = last ? Math.round((last.end + 0.2) * 10) / 10 : 0
  cues.value.push({
    index: cues.value.length + 1,
    start,
    end: Math.min(start + 2.5, canvasDurationS.value || start + 2.5),
    text: '',
    anchor: 0,
    marginVpct: 0,
    fontSizePct: 0,
    segmentId: '',
    offsetS: 0,
  })
  focusCue(cues.value.length - 1)
}

function removeCue(index: number) {
  cues.value.splice(index, 1)
  selectedCue.value = -1
}

const activeCues = computed(() =>
  cues.value.filter((cue) => currentTime.value >= cue.start && currentTime.value < cue.end),
)

/**
 * 字幕预览样式：用 CSS 近似 ASS 的样式与九宫格位置。
 * 预览是纯前端的，不触发任何重编码；保存时才写 ASS/SRT。
 */
function overlayStyle(cue: VideoSubtitleCue): Record<string, string> {
  const style = config.value.subtitleStyle
  const anchor = cue.anchor || style?.alignment || 2
  const margin = cue.marginVpct || style?.marginVpct || 5
  const size = cue.fontSizePct || style?.fontSizePct || 4.2
  const outline = Math.max(0, style?.outlineWidth ?? 2)
  const result: Record<string, string> = {
    color: style?.primaryColor || '#FFFFFF',
    fontFamily: style?.fontName || 'Noto Sans SC',
    fontWeight: style?.bold ? '700' : '400',
    // 外层 preview-shell 的 font-size = 1% 画布高度，所以这里直接用 em。
    fontSize: `${size}em`,
    WebkitTextStroke: outline > 0 ? `${outline / 2}px ${style?.outlineColor || '#000000'}` : '0',
    textShadow: outline > 0 ? `0 0 ${Math.max(1, outline)}px ${style?.outlineColor || '#000000'}` : 'none',
  }
  const horizontal = anchor % 3
  result.textAlign = horizontal === 1 ? 'left' : horizontal === 0 ? 'right' : 'center'
  result.left = horizontal === 1 ? '8%' : horizontal === 0 ? '8%' : '0'
  result.right = horizontal === 1 ? '8%' : horizontal === 0 ? '8%' : '0'
  if (anchor <= 3) {
    result.bottom = `${margin}%`
    result.top = 'auto'
    result.transform = 'none'
  } else if (anchor >= 7) {
    result.top = `${margin}%`
    result.bottom = 'auto'
    result.transform = 'none'
  } else {
    result.top = '50%'
    result.bottom = 'auto'
    result.transform = 'translateY(-50%)'
  }
  return result
}

function cueLayoutNote(cue: VideoSubtitleCue): string {
  const anchor = cue.anchor || config.value.subtitleStyle.alignment
  const label = SUBTITLE_ALIGNMENTS.find((item) => item.value === anchor)?.label ?? '中下'
  const size = cue.fontSizePct || config.value.subtitleStyle.fontSizePct
  const margin = cue.marginVpct || config.value.subtitleStyle.marginVpct
  return ` · ${label} / 字号 ${size}% / 留白 ${margin}%`
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

function saveSubtitles(): Promise<VideoTask> {
  return backend.saveVideoSubtitles(props.task.id, { cues: cues.value })
}

function saveOnly() {
  void guard('save', async () => {
    await saveSubtitles()
    return '字幕已保存：SRT/ASS 已按当前参数写入，确认合成时烧录'
  })
}

function approve() {
  void guard('approve', async () => {
    await saveSubtitles()
    await backend.approveVideoGate(props.task.id)
    return '字幕已确认，开始合成成片'
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
    return '已按当前时间轴重铺字幕，手工修改会被覆盖'
  })
}

function toggleSubtitles() {
  void guard('subtitles', async () => {
    await backend.setVideoSubtitlesEnabled(props.task.id, !subtitlesEnabled.value)
    return subtitlesEnabled.value ? '已关闭本任务字幕' : '已为本次开启字幕并生成时间轴'
  })
}

function regenerateSubtitles() {
  void guard('regenerate', async () => {
    await backend.regenerateVideoStage(props.task.id, '')
    return '已丢弃当前字幕草稿，按视频重新生成默认时间轴'
  })
}
</script>

<template>
  <section class="block">
    <header class="block__head">
      <h4 class="block__title">字幕确认（视频已生成）</h4>
      <span class="ui-chip" :class="run?.gates?.gate_edit === 'approved' ? 'ui-chip--ok' : 'ui-chip--warn'">
        {{ run?.gates?.gate_edit === 'approved' ? '已确认' : '待确认' }}
      </span>
      <span class="ui-chip">{{ cues.length }} 条字幕</span>
      <span v-if="run?.subtitles" class="ui-chip">时间轴来源：{{ run.subtitles.timingSource }}</span>
    </header>
    <p class="hint">
      左边是成片画布与字幕预览，右边是字幕表。改位置、字号、文本、时间会立即反映在预览里；
      点「保存字幕」才会写入 SRT/ASS，确认合成时才烧录。剪辑已固定为渲染顺序，不再支持裁剪/重排。
    </p>
    <p v-if="run?.subtitles?.editedAt" class="hint">上次修改：{{ formatClock(run.subtitles.editedAt) }}</p>

    <div v-if="!subtitlesEnabled" class="hint">
      本任务曾关闭字幕。需要字幕时可
      <button class="ui-btn" type="button" :disabled="busy !== ''" @click="toggleSubtitles">
        为本次开启字幕并生成
      </button>
    </div>

    <div class="subtitle-layout">
      <div class="subtitle-preview">
        <div
          v-if="canvasUrl"
          class="preview-shell"
          :style="{ fontSize: `${previewHeight > 0 ? previewHeight / 100 : 6}px` }"
        >
          <video
            ref="videoEl"
            class="video"
            :src="canvasUrl"
            controls
            preload="metadata"
            @loadedmetadata="updatePreviewHeight"
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
        <p v-else class="hint">预览画布尚未生成：确认提示词并生成视频后会自动出现。</p>

        <div class="timeline">
          <div class="timeline__ruler">
            <span>{{ formatSeconds(0) }}</span>
            <span>{{ formatSeconds(canvasDurationS) }}</span>
          </div>
          <div class="timeline__track">
            <button
              v-for="segment in segments"
              :key="segment.id"
              class="timeline__segment"
              :style="{
                left: `${canvasDurationS > 0 ? (segment.start / canvasDurationS) * 100 : 0}%`,
                width: `${canvasDurationS > 0 ? Math.max(1, ((segment.end - segment.start) / canvasDurationS) * 100) : 0}%`,
              }"
              type="button"
              @click="seekTo(segment.start)"
            >
              <span>{{ segment.id }}</span>
            </button>
          </div>
        </div>
      </div>

      <div class="subtitle-subs">
        <div class="subtitle-subs__head">
          <h5 class="ui-h3">字幕时间轴</h5>
          <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="addCue">添加字幕</button>
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

        <div
          v-for="(cue, index) in cues"
          :key="`cue-${index}`"
          class="cue-row"
          :class="{ 'cue-row--active': selectedCue === index }"
          @click="focusCue(index)"
        >
          <header class="cue-row__head">
            <span class="cue-row__name">字幕 {{ index + 1 }}</span>
            <span class="ui-chip">{{ formatSeconds(cue.start) }} → {{ formatSeconds(cue.end) }}</span>
            <span class="ui-chip">{{ cueLayoutNote(cue) }}</span>
            <button
              class="ui-btn ui-btn--danger"
              type="button"
              :disabled="!editable"
              @click.stop="removeCue(index)"
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
                :max="canvasDurationS"
                step="0.1"
                :disabled="!editable"
                @focus="focusCue(index)"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">结束（秒，最短 0.4s）</span>
              <input
                v-model.number="cue.end"
                class="ui-input"
                type="number"
                min="0"
                :max="canvasDurationS"
                step="0.1"
                :disabled="!editable"
                @focus="focusCue(index)"
              />
            </label>
            <label class="ui-field">
              <span class="ui-label">位置（屏幕九宫格）</span>
              <select v-model.number="cue.anchor" class="ui-select" :disabled="!editable" @focus="focusCue(index)">
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
                @focus="focusCue(index)"
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
                @focus="focusCue(index)"
              />
            </label>
          </div>
          <label class="ui-field">
            <span class="ui-label">字幕文本（留空则本条不写入）</span>
            <textarea
              v-model="cue.text"
              class="ui-textarea"
              rows="2"
              :disabled="!editable"
              @focus="focusCue(index)"
            ></textarea>
          </label>
        </div>
      </div>
    </div>

    <div class="actions">
      <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="saveOnly">
        {{ busy === 'save' ? '保存中…' : '保存字幕' }}
      </button>
      <button class="ui-btn" type="button" :disabled="!editable || busy !== ''" @click="regenerateSubtitles">
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
        {{ busy === 'approve' ? '提交中…' : '确认字幕，开始合成' }}
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
.subtitle-layout {
  display: grid;
  grid-template-columns: minmax(0, 1.1fr) minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}

@media (max-width: 1180px) {
  .subtitle-layout {
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
  line-height: 1.35;
  white-space: pre-line;
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

.cue-row {
  border: 1px solid rgba(127, 127, 127, 0.18);
  border-radius: 8px;
  padding: 10px;
  margin-bottom: 10px;
  cursor: pointer;
}

.cue-row--active {
  border-color: rgba(255, 180, 58, 0.55);
  background: rgba(255, 180, 58, 0.05);
}

.cue-row__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}

.cue-row__name {
  font-weight: 600;
}

.subtitle-subs__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}

.subtitle-subs__head .ui-h3 {
  margin: 0 auto 0 0;
}

.issues {
  margin: 0 0 10px;
  padding-left: 18px;
  font-size: 12px;
}

.issues__item--error {
  color: #ff9b9b;
}

.issues__item--warn {
  color: #ffca7a;
}

.hint--error {
  color: #ff9b9b;
}

.grid {
  display: grid;
  gap: 10px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
}
</style>
