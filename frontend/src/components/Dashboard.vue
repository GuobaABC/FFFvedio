<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  emptyDashboard,
  formatCreatedAt,
  formatLatency,
  formatSeconds,
  formatTokens,
  formatTrendDate,
  type AdapterMaturityRow,
  type DashboardSnapshot,
  type UsageBreakdownRow,
  type UsageTotals,
} from '../types/dashboard'

const snapshot = ref<DashboardSnapshot>(emptyDashboard())
const loading = ref(false)
const loadError = ref('')

const refreshTimerMs = 30000
let timerId: number | undefined

async function load() {
  loading.value = true
  try {
    snapshot.value = await backend.getDashboard()
    loadError.value = ''
  } catch (error) {
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  // 大屏是持续观测：30s 自动刷一次，不用手动点
  timerId = window.setInterval(load, refreshTimerMs)
})

onUnmounted(() => {
  if (timerId !== undefined) {
    window.clearInterval(timerId)
  }
})

const primaryAdapter = computed<AdapterMaturityRow | undefined>(() =>
  snapshot.value.adapters.find((item) => item.primary),
)
const partialAdapters = computed(() =>
  snapshot.value.adapters.filter((item) => !item.primary && item.maturity === 'partial'),
)
const localAdapters = computed(() =>
  snapshot.value.adapters.filter((item) => item.maturity === 'local'),
)

/** KPI 磁贴：token 为主，视频按片段/秒数，异常项单独露出来 */
const tiles = computed(() => {
  const today: UsageTotals = snapshot.value.today
  const week = snapshot.value.week
  const all = snapshot.value.all
  return [
    {
      label: '今日 token',
      value: formatTokens(today.totalTokens),
      hint: `输入 ${today.promptTokens.toLocaleString()}｜输出 ${today.outputTokens.toLocaleString()}`,
      tone: 'accent' as const,
    },
    {
      label: '今日调用',
      value: `${today.llmCalls + today.videoCalls}`,
      hint: `文本 ${today.llmCalls} 次｜视频片段 ${today.clips} 段 / ${formatSeconds(today.seconds)}`,
      tone: 'plain' as const,
    },
    {
      label: '近 7 日 token',
      value: formatTokens(week.totalTokens),
      hint: `文本 ${week.llmCalls} 次｜视频片段 ${week.clips} 段`,
      tone: 'plain' as const,
    },
    {
      label: '累计 token',
      value: formatTokens(all.totalTokens),
      hint: `共 ${all.llmCalls + all.videoCalls} 次计费调用`,
      tone: 'plain' as const,
    },
    {
      label: '累计视频产出',
      value: `${all.clips} 段`,
      hint: formatSeconds(all.seconds),
      tone: 'plain' as const,
    },
    {
      label: '失败调用',
      value: `${all.failedCalls}`,
      hint: '失败也可能已扣额度，一并入账',
      tone: all.failedCalls > 0 ? ('danger' as const) : ('plain' as const),
    },
    {
      label: '厂商未回报',
      value: `${all.unreportedCalls}`,
      hint: '这些调用只计次，token 无法得知',
      tone: all.unreportedCalls > 0 ? ('warn' as const) : ('plain' as const),
    },
  ]
})

const business = computed(() => snapshot.value.business)

const trendMax = computed(() =>
  Math.max(1, ...snapshot.value.trend.map((point) => point.totalTokens)),
)

function trendHeight(tokens: number): string {
  return `${Math.max(2, Math.round((tokens / trendMax.value) * 100))}%`
}

function breakdownRows(rows: UsageBreakdownRow[]): (UsageBreakdownRow & { width: string })[] {
  const max = Math.max(1, ...rows.map((row) => row.totalTokens || row.clips))
  return rows.map((row) => ({
    ...row,
    width: `${Math.max(3, Math.round(((row.totalTokens || row.clips) / max) * 100))}%`,
  }))
}

const purposeRows = computed(() => breakdownRows(snapshot.value.byPurpose))
const modelRows = computed(() => breakdownRows(snapshot.value.byModel))

function rowValue(row: UsageBreakdownRow): string {
  if (row.totalTokens > 0) {
    return `${formatTokens(row.totalTokens)} tokens`
  }
  if (row.clips > 0) {
    return `${row.clips} 段 / ${formatSeconds(row.seconds)}`
  }
  return `${row.calls} 次`
}

function rowBadge(row: UsageBreakdownRow): string {
  const parts: string[] = []
  if (row.videoCalls > 0) {
    parts.push(`视频 ${row.videoCalls}`)
  }
  if (row.llmCalls > 0) {
    parts.push(`文本 ${row.llmCalls}`)
  }
  if (row.failedCalls > 0) {
    parts.push(`失败 ${row.failedCalls}`)
  }
  if (row.unreportedCalls > 0) {
    parts.push(`未回报 ${row.unreportedCalls}`)
  }
  return parts.join('｜')
}

const generatedAtText = computed(() => formatCreatedAt(snapshot.value.generatedAt))
</script>

<template>
  <div class="dash">
    <header class="dash__head">
      <div>
        <h2 class="dash__title">模型用量大屏</h2>
        <p class="dash__sub">
          token 与视频片段消耗｜{{ generatedAtText || '尚未加载' }}
        </p>
      </div>
      <div class="dash__head-actions">
        <span v-if="loading" class="dash__pulse">采集中…</span>
        <button type="button" class="ui-btn ui-btn--ghost" :disabled="loading" @click="load">
          刷新
        </button>
      </div>
    </header>

    <p v-if="loadError" class="dash__error">{{ loadError }}</p>

    <!-- 适配声明：主力是字节跳动火山方舟 Seedance，其余通道未完整适配 -->
    <section class="dash__banner">
      <div class="dash__banner-main">
        <span class="dash__banner-tag">适配声明</span>
        <p class="dash__banner-text">
          目前<b>主要适配的视频生成模型是字节跳动 · {{ primaryAdapter?.label || '火山方舟 Seedance（豆包视频）' }}</b>：
          {{
            primaryAdapter?.note ||
            '首帧延续、角色参考图（全模态参考）、「图片N」素材引用、画幅锁定与时长档位都按官方文档核对过'
          }}。
          <template v-if="partialAdapters.length">
            其余通道
            <b>{{ partialAdapters.map((item) => item.label).join('、') }}</b>
            只打通了基础生视频链路，<b>尚未完整适配</b>；本地联调占位不计费、不进消耗统计。
          </template>
        </p>
      </div>
      <ul class="dash__adapters">
        <li
          v-for="adapter in snapshot.adapters"
          :key="adapter.id"
          class="dash__adapter"
          :class="{
            'dash__adapter--primary': adapter.primary,
            'dash__adapter--local': adapter.maturity === 'local',
          }"
          :title="adapter.description"
        >
          <span class="dash__adapter-name">{{ adapter.label }}</span>
          <span class="dash__adapter-badge">{{ adapter.maturityLabel }}</span>
          <span v-if="adapter.note" class="dash__adapter-note">{{ adapter.note }}</span>
        </li>
      </ul>
    </section>

    <p v-if="!snapshot.hasData" class="dash__empty">
      还没有计费调用记录：跑一次视频任务或热点爬取后，这里会出现 token 与片段消耗。
    </p>

    <!-- KPI 磁贴 -->
    <section class="dash__tiles">
      <div
        v-for="tile in tiles"
        :key="tile.label"
        class="dash__tile"
        :class="`dash__tile--${tile.tone}`"
      >
        <p class="dash__tile-label">{{ tile.label }}</p>
        <p class="dash__tile-value">{{ tile.value }}</p>
        <p class="dash__tile-hint">{{ tile.hint }}</p>
      </div>
    </section>

    <div class="dash__grid">
      <!-- 14 日趋势 -->
      <section class="dash__card">
        <h3 class="ui-h3">近 14 日 token 趋势</h3>
        <div class="dash__chart">
          <div
            v-for="point in snapshot.trend"
            :key="point.date"
            class="dash__bar-col"
            :title="`${point.date}｜${point.totalTokens.toLocaleString()} tokens｜文本 ${point.llmCalls} 次｜片段 ${point.clips} 段`"
          >
            <div class="dash__bar-track">
              <div class="dash__bar" :style="{ height: trendHeight(point.totalTokens) }"></div>
            </div>
            <span class="dash__bar-label">{{ formatTrendDate(point.date) }}</span>
          </div>
        </div>
        <p class="hint">柱高按当日 token 相对峰值折算；空白日表示没有计费调用。</p>
      </section>

      <!-- 按用途 -->
      <section class="dash__card">
        <h3 class="ui-h3">按用途拆分（近 14 日）</h3>
        <div v-if="!purposeRows.length" class="dash__card-empty">暂无数据</div>
        <ul v-else class="dash__rows">
          <li v-for="row in purposeRows" :key="row.key" class="dash__row">
            <div class="dash__row-top">
              <span class="dash__row-key">{{ row.key }}</span>
              <span class="dash__row-value">{{ rowValue(row) }}</span>
            </div>
            <div class="dash__row-track">
              <div class="dash__row-fill" :style="{ width: row.width }"></div>
            </div>
            <p class="dash__row-badge">{{ rowBadge(row) }}</p>
          </li>
        </ul>
      </section>

      <!-- 按模型 -->
      <section class="dash__card">
        <h3 class="ui-h3">按模型拆分（近 14 日）</h3>
        <div v-if="!modelRows.length" class="dash__card-empty">暂无数据</div>
        <ul v-else class="dash__rows">
          <li v-for="row in modelRows" :key="row.key" class="dash__row">
            <div class="dash__row-top">
              <span class="dash__row-key">{{ row.key }}</span>
              <span class="dash__row-value">{{ rowValue(row) }}</span>
            </div>
            <div class="dash__row-track">
              <div
                class="dash__row-fill"
                :class="{ 'dash__row-fill--video': row.videoCalls > 0 && row.totalTokens === 0 }"
                :style="{ width: row.width }"
              ></div>
            </div>
            <p class="dash__row-badge">{{ rowBadge(row) }}</p>
          </li>
        </ul>
      </section>

      <!-- 业务量 -->
      <section class="dash__card">
        <h3 class="ui-h3">业务量</h3>
        <div class="dash__biz">
          <p class="dash__biz-item">
            <span>视频任务</span>
            <b>{{ business.videoTasks }}</b>
            <em>近 7 日 +{{ business.videoTasksThisWeek }}</em>
          </p>
          <p class="dash__biz-item">
            <span>爬取任务</span>
            <b>{{ business.crawlTasks }}</b>
            <em>近 7 日 +{{ business.crawlTasksThisWeek }}</em>
          </p>
          <p class="dash__biz-item">
            <span>热点条目</span>
            <b>{{ business.crawlItems }}</b>
          </p>
          <p class="dash__biz-item">
            <span>尾帧资产</span>
            <b>{{ business.tailFrameAssets }}</b>
          </p>
          <p class="dash__biz-item">
            <span>模板</span>
            <b>{{ business.templates }}</b>
          </p>
          <p class="dash__biz-item">
            <span>提供商</span>
            <b>{{ business.providers }}</b>
          </p>
        </div>
      </section>
    </div>

    <!-- 最近流水 -->
    <section class="dash__card dash__card--wide">
      <h3 class="ui-h3">最近调用</h3>
      <div v-if="!snapshot.recent.length" class="dash__card-empty">暂无记录</div>
      <table v-else class="dash__table">
        <thead>
          <tr>
            <th>时间</th>
            <th>用途</th>
            <th>模型 / 通道</th>
            <th class="dash__num">token</th>
            <th class="dash__num">片段</th>
            <th class="dash__num">耗时</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, index) in snapshot.recent" :key="`${row.createdAt}-${index}`">
            <td class="dash__mono">{{ formatCreatedAt(row.createdAt) }}</td>
            <td>{{ row.purpose }}</td>
            <td>
              {{ row.displayName }}
              <span v-if="row.model" class="dash__model">{{ row.model }}</span>
            </td>
            <td class="dash__num">
              {{ row.totalTokens ? row.totalTokens.toLocaleString() : '—' }}
            </td>
            <td class="dash__num">
              {{ row.clips ? `${row.clips} / ${formatSeconds(row.seconds)}` : '—' }}
            </td>
            <td class="dash__num">{{ formatLatency(row.latencyMs) }}</td>
            <td>
              <span v-if="row.failed" class="dash__state dash__state--fail">失败</span>
              <span v-else-if="row.kind === 'llm' && !row.reported" class="dash__state dash__state--unreported">
                未回报
              </span>
              <span v-else class="dash__state dash__state--ok">成功</span>
            </td>
          </tr>
        </tbody>
      </table>
    </section>

    <ul v-if="snapshot.notes.length" class="dash__notes">
      <li v-for="note in snapshot.notes" :key="note">{{ note }}</li>
    </ul>
    <p v-if="localAdapters.length" class="hint">
      本地联调通道：{{ localAdapters.map((item) => item.label).join('、') }}（不计费、不入库消耗）
    </p>
  </div>
</template>

<style scoped>
.dash {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1320px;
  margin: 0 auto;
  width: 100%;
}

.dash__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

.dash__title {
  font-size: 20px;
  font-weight: 600;
  letter-spacing: 0.01em;
}

.dash__sub {
  margin-top: 2px;
  font-size: 12px;
  color: var(--color-text-muted);
  font-family: var(--font-mono);
}

.dash__head-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}

.dash__pulse {
  font-size: 12px;
  color: var(--color-accent);
  animation: dash-pulse 1.4s ease-in-out infinite;
}

@keyframes dash-pulse {
  0%,
  100% {
    opacity: 0.4;
  }
  50% {
    opacity: 1;
  }
}

.dash__error {
  padding: 8px 12px;
  border: 1px solid rgba(255, 92, 92, 0.4);
  border-radius: var(--radius-sm);
  background: rgba(255, 92, 92, 0.08);
  color: #ff8f8f;
  font-size: 12px;
}

/* 适配声明 */
.dash__banner {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 14px 16px;
  border: 1px solid rgba(255, 180, 58, 0.32);
  border-radius: var(--radius-md);
  background: linear-gradient(120deg, rgba(255, 180, 58, 0.1), rgba(255, 138, 61, 0.04));
}

.dash__banner-main {
  display: flex;
  gap: 10px;
  align-items: flex-start;
}

.dash__banner-tag {
  flex: none;
  padding: 3px 9px;
  border-radius: 999px;
  background: var(--color-accent);
  color: #1a1207;
  font-size: 11px;
  font-weight: 700;
}

.dash__banner-text {
  font-size: 12.5px;
  line-height: 1.75;
  color: var(--color-text);
}

.dash__banner-text b {
  color: var(--color-accent);
  font-weight: 600;
}

.dash__adapters {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(230px, 1fr));
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.dash__adapter {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.02);
}

.dash__adapter--primary {
  border-color: rgba(255, 180, 58, 0.5);
  background: rgba(255, 180, 58, 0.08);
}

.dash__adapter--local {
  opacity: 0.6;
}

.dash__adapter-name {
  font-size: 12.5px;
  font-weight: 600;
}

.dash__adapter-badge {
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--color-border);
  font-size: 10.5px;
  color: var(--color-text-muted);
  white-space: nowrap;
}

.dash__adapter--primary .dash__adapter-badge {
  border-color: rgba(255, 180, 58, 0.5);
  color: var(--color-accent);
}

.dash__adapter-note {
  flex-basis: 100%;
  font-size: 11px;
  line-height: 1.6;
  color: var(--color-text-muted);
}

.dash__empty {
  padding: 10px 12px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
  color: var(--color-text-muted);
  font-size: 12px;
}

/* KPI */
.dash__tiles {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(168px, 1fr));
  gap: 10px;
}

.dash__tile {
  padding: 12px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: var(--color-surface);
}

.dash__tile-label {
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.dash__tile-value {
  margin-top: 4px;
  font-family: var(--font-mono);
  font-size: 22px;
  font-weight: 600;
  line-height: 1.2;
  font-variant-numeric: tabular-nums;
}

.dash__tile-hint {
  margin-top: 4px;
  font-size: 11px;
  color: var(--color-text-muted);
}

.dash__tile--accent {
  border-color: rgba(255, 180, 58, 0.4);
  background: linear-gradient(150deg, rgba(255, 180, 58, 0.12), var(--color-surface));
}

.dash__tile--accent .dash__tile-value {
  color: var(--color-accent);
}

.dash__tile--danger .dash__tile-value {
  color: #ff8f8f;
}

.dash__tile--warn .dash__tile-value {
  color: var(--color-accent-strong);
}

/* 卡片区 */
.dash__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 12px;
}

.dash__card {
  padding: 14px 16px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: var(--color-surface);
}

.dash__card--wide {
  grid-column: 1 / -1;
}

.dash__card-empty {
  padding: 18px 0;
  text-align: center;
  color: var(--color-text-muted);
  font-size: 12px;
}

/* 趋势柱 */
.dash__chart {
  display: flex;
  align-items: flex-end;
  gap: 6px;
  height: 168px;
  margin-top: 10px;
}

.dash__bar-col {
  display: flex;
  flex: 1;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  min-width: 0;
  height: 100%;
}

.dash__bar-track {
  display: flex;
  flex: 1;
  width: 100%;
  align-items: flex-end;
  border-radius: var(--radius-sm);
  background: rgba(255, 255, 255, 0.03);
}

.dash__bar {
  width: 100%;
  border-radius: 4px 4px 0 0;
  background: linear-gradient(180deg, var(--color-accent) 0%, rgba(255, 138, 61, 0.55) 100%);
}

.dash__bar-label {
  font-family: var(--font-mono);
  font-size: 9.5px;
  color: var(--color-text-muted);
  white-space: nowrap;
}

/* 分布条 */
.dash__rows {
  display: flex;
  flex-direction: column;
  gap: 12px;
  margin: 12px 0 0;
  padding: 0;
  list-style: none;
}

.dash__row-top {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  font-size: 12px;
}

.dash__row-key {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dash__row-value {
  flex: none;
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
  color: var(--color-accent);
}

.dash__row-track {
  height: 6px;
  margin-top: 5px;
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.05);
  overflow: hidden;
}

.dash__row-fill {
  height: 100%;
  border-radius: 999px;
  background: linear-gradient(90deg, var(--color-accent) 0%, var(--color-accent-strong) 100%);
}

.dash__row-fill--video {
  background: linear-gradient(90deg, #5fd0ff 0%, #3a8bff 100%);
}

.dash__row-badge {
  margin-top: 3px;
  font-size: 10.5px;
  color: var(--color-text-muted);
}

/* 业务量 */
.dash__biz {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 8px;
  margin-top: 10px;
}

.dash__biz-item {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 6px;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  font-size: 11.5px;
  color: var(--color-text-muted);
}

.dash__biz-item b {
  font-family: var(--font-mono);
  font-size: 16px;
  color: var(--color-text);
}

.dash__biz-item em {
  flex-basis: 100%;
  font-style: normal;
  font-size: 10.5px;
  color: var(--color-accent);
}

/* 流水表 */
.dash__table {
  width: 100%;
  margin-top: 10px;
  border-collapse: collapse;
  font-size: 12px;
}

.dash__table th,
.dash__table td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--color-border);
  text-align: left;
  vertical-align: top;
}

.dash__table th {
  color: var(--color-text-muted);
  font-weight: 500;
  font-size: 11px;
  white-space: nowrap;
}

.dash__num {
  text-align: right;
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.dash__mono {
  font-family: var(--font-mono);
  white-space: nowrap;
}

.dash__model {
  margin-left: 6px;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--color-text-muted);
}

.dash__state {
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--color-border);
  font-size: 10.5px;
  white-space: nowrap;
}

.dash__state--ok {
  color: #7ee2a8;
}

.dash__state--fail {
  border-color: rgba(255, 92, 92, 0.45);
  color: #ff8f8f;
}

.dash__state--unreported {
  border-color: rgba(255, 180, 58, 0.4);
  color: var(--color-accent);
}

.dash__notes {
  margin: 0;
  padding-left: 18px;
  color: var(--color-text-muted);
  font-size: 11px;
  line-height: 1.8;
}
</style>
