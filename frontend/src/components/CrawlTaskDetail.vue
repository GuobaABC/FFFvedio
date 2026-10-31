<script setup lang="ts">
import { computed, ref } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  CRAWL_ERROR_LABELS,
  CRAWL_STAGES,
  CRAWL_STAGE_LABELS,
  CRAWL_STATE_LABELS,
  CRAWL_STATUS_LABELS,
  crawlStateClass,
  crawlStatusClass,
  formatConfidence,
  formatCrawlClock,
  formatDuration,
  formatSecondsLabel,
  tierLabel,
  type CrawlItem,
  type CrawlTask,
} from '../types/crawl'

const props = defineProps<{ task: CrawlTask }>()
const emit = defineEmits<{
  (event: 'changed'): void
  (event: 'deleted', taskId: string): void
  (event: 'to-video', item: CrawlItem): void
}>()

const busy = ref(false)
const actionError = ref('')

const results = computed(() => props.task.run?.results ?? [])
const items = computed(() => props.task.run?.items ?? [])
const stats = computed(() => props.task.run?.stats)
const warnings = computed(() => props.task.run?.warnings ?? [])
const logs = computed(() => (props.task.run?.logs ?? []).slice(-16))
const running = computed(() => props.task.status === 'running')

const stageIndex = computed(() => {
  const index = CRAWL_STAGES.findIndex((stage) => stage.id === props.task.stage)
  return index < 0 ? 0 : index
})

const tierHits = computed(() => {
  const hits = stats.value?.tierHits ?? {}
  return Object.entries(hits)
    .filter(([, count]) => count > 0)
    .sort((left, right) => right[1] - left[1])
})

/** 失败重试：只有可重试的条目才允许「执行任务」，其余情况引导用「强制重爬」 */
const hasRetryable = computed(() =>
  results.value.some((result) =>
    [
      'CREATED',
      'ROUTED',
      'FETCHED',
      'PARSED',
      'VALIDATED',
      'NEEDS_LLM',
      'LLM_EXTRACTED',
      'FAILED_RETRY',
      'BLOCKED',
    ].includes(result.state),
  ),
)

async function run(force: boolean) {
  if (busy.value) {
    return
  }
  actionError.value = ''
  busy.value = true
  try {
    if (force) {
      await backend.recrawlCrawlTask(props.task.id)
    } else {
      await backend.runCrawlTask(props.task.id)
    }
    emit('changed')
  } catch (error) {
    actionError.value = errorText(error)
  } finally {
    busy.value = false
  }
}

async function remove() {
  if (busy.value) {
    return
  }
  if (!window.confirm(`删除任务「${props.task.title}」？条目库与站点规则会保留。`)) {
    return
  }
  actionError.value = ''
  busy.value = true
  try {
    await backend.deleteCrawlTask(props.task.id)
    emit('deleted', props.task.id)
  } catch (error) {
    actionError.value = errorText(error)
  } finally {
    busy.value = false
  }
}

async function copyURL(url: string) {
  try {
    await navigator.clipboard.writeText(url)
  } catch {
    actionError.value = '复制失败：当前环境不允许访问剪贴板'
  }
}

function errorLabel(code: string): string {
  return CRAWL_ERROR_LABELS[code] ?? code
}
</script>

<template>
  <article class="detail">
    <header class="detail__head">
      <div class="detail__headline">
        <h3 class="detail__title">{{ task.title }}</h3>
        <span class="ui-chip" :class="crawlStatusClass(task.status)">
          {{ CRAWL_STATUS_LABELS[task.status] ?? task.status }}
        </span>
        <span class="ui-chip">{{ CRAWL_STAGE_LABELS[task.stage] ?? task.stage }}</span>
        <span v-if="task.run.lastRunAt" class="ui-chip">上次执行 {{ formatCrawlClock(task.run.lastRunAt) }}</span>
      </div>
      <div class="detail__actions">
        <button
          class="ui-btn ui-btn--primary"
          type="button"
          :disabled="busy || running || !hasRetryable"
          @click="run(false)"
        >
          {{ running ? '执行中…' : '执行任务' }}
        </button>
        <button class="ui-btn" type="button" :disabled="busy || running" @click="run(true)">
          强制重爬
        </button>
        <button class="ui-btn ui-btn--danger" type="button" :disabled="busy || running" @click="remove">
          删除
        </button>
      </div>
    </header>

    <p class="detail__meta">
      <span class="mono">{{ task.id }}</span>
      <span>·</span>
      <span>创建 {{ formatCrawlClock(task.createdAt) }}</span>
      <span>·</span>
      <span>更新 {{ formatCrawlClock(task.updatedAt) }}</span>
      <span v-if="task.run.finishedAt">· 完成 {{ formatCrawlClock(task.run.finishedAt) }}</span>
    </p>

    <p v-if="actionError" class="ui-notice ui-notice--error">{{ actionError }}</p>
    <p v-if="task.error" class="ui-notice ui-notice--error">{{ task.error }}</p>
    <p v-for="warning in warnings" :key="warning" class="ui-notice ui-notice--warn">{{ warning }}</p>

    <div class="chips">
      <span class="ui-chip">共 {{ stats?.total ?? results.length }} 条</span>
      <span class="ui-chip ui-chip--ok">入库 {{ stats?.stored ?? 0 }}</span>
      <span class="ui-chip">缓存 {{ stats?.cached ?? 0 }}</span>
      <span class="ui-chip ui-chip--error">失败 {{ stats?.failed ?? 0 }}</span>
      <span class="ui-chip ui-chip--warn">拦截 {{ stats?.blocked ?? 0 }}</span>
      <span class="ui-chip">LLM 调用 {{ stats?.llmCalls ?? 0 }}</span>
      <span class="ui-chip">耗时 {{ formatDuration(stats?.durationMs ?? 0) }}</span>
      <span v-for="[tier, count] in tierHits" :key="tier" class="ui-chip">
        {{ tierLabel(tier) }} × {{ count }}
      </span>
    </div>

    <ol class="timeline">
      <li
        v-for="(stage, index) in CRAWL_STAGES"
        :key="stage.id"
        class="timeline__item"
        :class="{
          'timeline__item--active': index === stageIndex && task.status !== 'done',
          'timeline__item--done': index < stageIndex || task.status === 'done',
        }"
      >
        <span class="timeline__dot" aria-hidden="true"></span>
        <span class="timeline__label">{{ stage.label }}</span>
        <span class="timeline__hint">{{ stage.hint }}</span>
      </li>
    </ol>

    <section class="section">
      <div class="section__head">
        <h4 class="section__title">爬取结果 · {{ results.length }} 条 URL</h4>
        <span class="ui-chip">{{ items.length }} 条入库</span>
      </div>

      <p v-if="results.length === 0" class="section__empty">这个任务里没有 URL。</p>

      <div v-for="(result, index) in results" :key="result.canonicalUrl + index" class="result">
        <div class="result__head">
          <span class="ui-chip" :class="crawlStateClass(result.state)">
            {{ CRAWL_STATE_LABELS[result.state] ?? result.state }}
          </span>
          <span class="result__site">{{ result.siteKey || '未知站点' }}</span>
          <span v-if="result.tier" class="ui-chip">{{ tierLabel(result.tier) }}</span>
          <span v-if="result.cached" class="ui-chip ui-chip--ok">缓存</span>
          <span v-if="result.httpStatus" class="ui-chip">HTTP {{ result.httpStatus }}</span>
          <span class="ui-chip">置信度 {{ formatConfidence(result.confidence) }}</span>
          <span v-if="result.latencyMs" class="ui-chip">{{ formatDuration(result.latencyMs) }}</span>
          <span v-if="result.errorCode" class="ui-chip ui-chip--warn">{{ errorLabel(result.errorCode) }}</span>
        </div>
        <p class="result__url mono">{{ result.canonicalUrl }}</p>
        <p v-if="result.originalUrl && result.originalUrl !== result.canonicalUrl" class="result__from">
          原始输入 {{ result.originalUrl }}
        </p>
        <p v-if="result.message" class="result__message">{{ result.message }}</p>
        <details v-if="result.steps?.length" class="steps">
          <summary>审计步骤 {{ result.steps.length }} 步</summary>
          <ul class="steps__list">
            <li v-for="(step, stepIndex) in result.steps" :key="stepIndex" class="steps__item">
              <span class="steps__name">{{ step.step }}</span>
              <span class="steps__status">{{ step.status }}</span>
              <span v-if="step.tier" class="ui-chip">{{ tierLabel(step.tier) }}</span>
              <span v-if="step.latencyMs" class="steps__latency">{{ formatDuration(step.latencyMs) }}</span>
              <span class="steps__note">{{ step.note }}</span>
            </li>
          </ul>
        </details>
      </div>
    </section>

    <section class="section">
      <div class="section__head">
        <h4 class="section__title">条目库 · {{ items.length }} 条</h4>
        <span class="hint">标题 / 简介 / 封面 URL 三要素，可直接喂给视频生成</span>
      </div>

      <p v-if="items.length === 0" class="section__empty">
        还没有条目：执行任务后，抽到完整三要素的视频会自动入库（同一条视频跨任务只留一份）。
      </p>

      <div class="items">
        <article v-for="item in items" :key="item.id" class="item">
          <div class="item__thumb">
            <img
              v-if="item.thumbnail.url"
              :src="item.thumbnail.url"
              :alt="item.title"
              referrerpolicy="no-referrer"
              loading="lazy"
            />
            <span v-else class="item__thumb-empty">无封面</span>
          </div>
          <div class="item__body">
            <p class="item__title">{{ item.title }}</p>
            <p v-if="item.description" class="item__desc">{{ item.description }}</p>
            <div class="item__chips">
              <span class="ui-chip">{{ item.siteKey || '站点' }}</span>
              <span class="ui-chip">{{ tierLabel(item.meta.extractTier) }}</span>
              <span class="ui-chip">置信度 {{ formatConfidence(item.meta.confidence) }}</span>
              <span v-if="item.optional.durationSec" class="ui-chip">
                时长 {{ formatSecondsLabel(item.optional.durationSec) }}
              </span>
              <span v-if="item.optional.author" class="ui-chip">作者 {{ item.optional.author }}</span>
              <span v-if="item.meta.siteProfile" class="ui-chip ui-chip--ok">站点规则命中</span>
            </div>
            <p class="item__url mono">{{ item.canonicalUrl }}</p>
            <div class="item__actions">
              <button class="ui-btn ui-btn--primary" type="button" @click="emit('to-video', item)">
                带入视频生成
              </button>
              <button class="ui-btn" type="button" @click="copyURL(item.canonicalUrl)">复制链接</button>
            </div>
          </div>
        </article>
      </div>
    </section>

    <details class="logs">
      <summary>执行日志（最近 {{ logs.length }} 条）</summary>
      <ul class="logs__list">
        <li v-for="(log, index) in logs" :key="index" class="logs__item">{{ log }}</li>
      </ul>
    </details>
  </article>
</template>

<style scoped>
.detail {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
}

.detail__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.detail__headline {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  min-width: 0;
}

.detail__title {
  font-size: 15px;
  font-weight: 600;
}

.detail__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.detail__meta {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 11px;
  color: var(--color-text-muted);
}

.mono {
  font-family: var(--font-mono);
  letter-spacing: 0.02em;
}

.chips {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.timeline {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.timeline__item {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.015);
  font-size: 11px;
  color: var(--color-text-muted);
}

.timeline__item--active {
  border-color: rgba(96, 165, 250, 0.5);
  background: rgba(96, 165, 250, 0.08);
}

.timeline__item--done {
  border-color: rgba(88, 214, 141, 0.32);
  background: rgba(88, 214, 141, 0.06);
}

.timeline__dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
  opacity: 0.5;
}

.timeline__item--active .timeline__dot {
  background: #93c5fd;
  opacity: 1;
}

.timeline__item--done .timeline__dot {
  background: #7ee2a8;
  opacity: 1;
}

.timeline__label {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text);
}

.timeline__hint {
  line-height: 1.6;
}

.section {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: rgba(255, 255, 255, 0.015);
}

.section__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}

.section__title {
  font-size: 13px;
  font-weight: 600;
}

.section__empty {
  padding: 14px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
  font-size: 12px;
  line-height: 1.8;
  color: var(--color-text-muted);
}

.result {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.result__head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.result__site {
  font-size: 12px;
  font-weight: 600;
}

.result__url {
  font-size: 11.5px;
  color: var(--color-text);
  word-break: break-all;
}

.result__from {
  font-size: 11px;
  color: var(--color-text-muted);
  word-break: break-all;
}

.result__message {
  font-size: 12px;
  line-height: 1.6;
  color: #ffca7a;
}

.steps {
  margin-top: 4px;
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.steps summary {
  cursor: pointer;
}

.steps__list {
  display: flex;
  flex-direction: column;
  gap: 3px;
  margin: 6px 0 0;
  padding: 0;
  list-style: none;
}

.steps__item {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  padding: 3px 0;
  border-top: 1px dashed rgba(255, 255, 255, 0.06);
}

.steps__name {
  min-width: 68px;
  font-family: var(--font-mono);
  color: var(--color-text);
}

.steps__status {
  min-width: 60px;
  font-family: var(--font-mono);
}

.steps__note {
  flex: 1;
  min-width: 140px;
  word-break: break-all;
}

.items {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 10px;
}

.item {
  display: grid;
  grid-template-columns: 104px minmax(0, 1fr);
  gap: 10px;
  padding: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.18);
}

.item__thumb {
  display: grid;
  place-items: center;
  width: 104px;
  height: 78px;
  overflow: hidden;
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.04);
  color: var(--color-text-muted);
  font-size: 11px;
}

.item__thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.item__body {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.item__title {
  font-size: 12.5px;
  font-weight: 600;
  line-height: 1.5;
}

.item__desc {
  display: -webkit-box;
  overflow: hidden;
  font-size: 11.5px;
  line-height: 1.6;
  color: var(--color-text-muted);
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 3;
}

.item__chips {
  display: flex;
  align-items: center;
  gap: 5px;
  flex-wrap: wrap;
}

.item__url {
  font-size: 10.5px;
  color: var(--color-text-muted);
  word-break: break-all;
}

.item__actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 2px;
}

.logs {
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.logs summary {
  cursor: pointer;
}

.logs__list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin: 6px 0 0;
  padding: 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(0, 0, 0, 0.24);
  list-style: none;
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.7;
}

.logs__item {
  word-break: break-all;
}

.hint {
  font-size: 12px;
  color: var(--color-text-muted);
}
</style>
