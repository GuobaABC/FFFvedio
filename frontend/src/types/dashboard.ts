/** 首页大屏（模型用量监控 + 适配成熟度声明）的类型定义。 */

export type UsageKind = 'llm' | 'video'

export type AdapterMaturity = 'primary' | 'partial' | 'local'

export interface UsageTotals {
  totalTokens: number
  promptTokens: number
  outputTokens: number
  llmCalls: number
  videoCalls: number
  failedCalls: number
  unreportedCalls: number
  clips: number
  seconds: number
}

export interface UsageBreakdownRow {
  key: string
  totalTokens: number
  promptTokens: number
  outputTokens: number
  calls: number
  llmCalls: number
  videoCalls: number
  clips: number
  seconds: number
  failedCalls: number
  unreportedCalls: number
}

export interface UsageTrendPoint {
  date: string
  totalTokens: number
  llmCalls: number
  clips: number
  seconds: number
}

export interface UsageRecordRow {
  createdAt: string
  kind: UsageKind
  purpose: string
  displayName: string
  model: string
  totalTokens: number
  promptTokens: number
  outputTokens: number
  clips: number
  seconds: number
  latencyMs: number
  failed: boolean
  reported: boolean
  taskReference: string
}

export interface AdapterMaturityRow {
  id: string
  label: string
  description: string
  maturity: AdapterMaturity
  maturityLabel: string
  primary: boolean
  note: string
}

export interface DashboardBusinessCount {
  videoTasks: number
  videoTasksThisWeek: number
  crawlTasks: number
  crawlTasksThisWeek: number
  crawlItems: number
  tailFrameAssets: number
  templates: number
  providers: number
}

export interface DashboardSnapshot {
  generatedAt: string
  hasData: boolean
  today: UsageTotals
  week: UsageTotals
  all: UsageTotals
  trend: UsageTrendPoint[]
  byPurpose: UsageBreakdownRow[]
  byModel: UsageBreakdownRow[]
  recent: UsageRecordRow[]
  adapters: AdapterMaturityRow[]
  business: DashboardBusinessCount
  notes: string[]
}

/** 空快照：后端不可用（浏览器单独预览）时大屏仍能渲染骨架。 */
export function emptyDashboard(): DashboardSnapshot {
  const zero: UsageTotals = {
    totalTokens: 0,
    promptTokens: 0,
    outputTokens: 0,
    llmCalls: 0,
    videoCalls: 0,
    failedCalls: 0,
    unreportedCalls: 0,
    clips: 0,
    seconds: 0,
  }
  return {
    generatedAt: '',
    hasData: false,
    today: { ...zero },
    week: { ...zero },
    all: { ...zero },
    trend: [],
    byPurpose: [],
    byModel: [],
    recent: [],
    adapters: [],
    business: {
      videoTasks: 0,
      videoTasksThisWeek: 0,
      crawlTasks: 0,
      crawlTasksThisWeek: 0,
      crawlItems: 0,
      tailFrameAssets: 0,
      templates: 0,
      providers: 0,
    },
    notes: [],
  }
}

const NOUN_UNITS: [number, string][] = [
  [1_000_000_000, '十亿'],
  [1_000_000, '百万'],
  [1_000, '千'],
]

/** 把 token 数说成人话：大屏磁贴要一眼读完，不写 1234567 这种串。 */
export function formatTokens(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '0'
  }
  for (const [threshold, unit] of NOUN_UNITS) {
    if (value >= threshold * 10) {
      return `${(value / threshold).toFixed(1)} ${unit}`
    }
  }
  return value.toLocaleString('zh-CN')
}

export function formatSeconds(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '0s'
  }
  if (value < 60) {
    return `${value.toFixed(1)}s`
  }
  const minutes = Math.floor(value / 60)
  const rest = Math.round(value - minutes * 60)
  if (minutes < 60) {
    return `${minutes}m${rest}s`
  }
  return `${Math.floor(minutes / 60)}h${minutes % 60}m`
}

export function formatLatency(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) {
    return '—'
  }
  if (ms < 1000) {
    return `${Math.round(ms)}ms`
  }
  return `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)}s`
}

/** 时间戳只到秒：RFC3339 尾巴上的时区偏移在大屏上没有阅读价值。 */
export function formatCreatedAt(value: string): string {
  if (!value || value.length < 19) {
    return value || '—'
  }
  return `${value.slice(5, 10)} ${value.slice(11, 19)}`
}

export function formatTrendDate(value: string): string {
  return value.length >= 10 ? value.slice(5) : value
}
