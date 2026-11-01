package main

import (
	"context"
	"time"
)

// 模型用量监控：每一次对外计费调用（文本模型 token、视频渲染片段/秒数）都落一条 usage_events，
// 首页大屏按「今日 / 近 7 日 / 近 14 日趋势 / 用途 / 模型」聚合展示。
// 采集口径只写「实测到的量」：服务商响应里没有 usage 字段时记为未回报，不用字符数估算。
const (
	UsageKindLLM   = "llm"   // 文本模型调用，按 token 计
	UsageKindVideo = "video" // 视频渲染调用，按片段 / 秒数计
)

// 用途标签：直接给大屏分组，所以用中文，与任务日志口径一致。
const (
	UsagePurposeScript      = "分镜脚本"    // Agent1
	UsagePurposePrompt      = "画面提示词"   // Agent2
	UsagePurposeSegmentFix  = "片段提示词优化" // 逐段返工时的 AI 建议
	UsagePurposePublishMeta = "发布元数据"   // Agent4
	UsagePurposeHotExtract  = "热点抽取"    // 爬取 L3
	UsagePurposeSiteSelect  = "站点选择器"   // 爬取 L4
	UsagePurposeVideoRender = "视频渲染"    // Agent3
	UsagePurposeUnknown     = "未标注用途"
)

type usagePurposeKey struct{}

type usageTaskIDKey struct{}

// usageMeta 从 context 里读到的调用身份。
type usageMeta struct {
	purpose string
	taskID  string
}

// withUsagePurpose 给一次调用打用途标签，同一用途下的多次调用在聚合时合并。
// 只改用途，不动已有的任务 ID，所以可以在调用链深处补标签。
func withUsagePurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, usagePurposeKey{}, purpose)
}

// withUsageTask 带上任务身份，让一次调用能追溯到具体任务；不覆盖已标注的用途。
func withUsageTask(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, usageTaskIDKey{}, taskID)
}

// withUsage 一次性标注用途与任务（流水线阶段入口用）。
func withUsage(ctx context.Context, purpose, taskID string) context.Context {
	return withUsageTask(withUsagePurpose(ctx, purpose), taskID)
}

// usageMetaFrom 取调用点标注的用途与任务；没标注的归到「未标注用途」，
// 免得漏埋的调用在大屏上凭空消失，反而看不出问题。
func usageMetaFrom(ctx context.Context) usageMeta {
	meta := usageMeta{purpose: UsagePurposeUnknown}
	if purpose, ok := ctx.Value(usagePurposeKey{}).(string); ok && purpose != "" {
		meta.purpose = purpose
	}
	if taskID, ok := ctx.Value(usageTaskIDKey{}).(string); ok {
		meta.taskID = taskID
	}
	return meta
}

// llmTokenUsage 一次文本调用的 token 实测值。
type llmTokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// Reported 为 false 表示服务商没返回 usage 字段：总量记 0，
	// 大屏据此提示「厂商未回报」，而不是假装知道。
	Reported bool
}

func (u llmTokenUsage) total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// usageEvent 一条待写入的用量流水。
type usageEvent struct {
	CreatedAt     time.Time
	Kind          string
	Purpose       string
	ProviderName  string
	Model         string
	Dialect       string
	PromptTokens  int
	OutputTokens  int
	TotalTokens   int
	Clips         int
	Seconds       float64
	LatencyMS     int64
	Failed        bool
	Reported      bool
	TaskID        string
	ReferenceText string
}

// usageTotals 某个时间窗内的合计。
type usageTotals struct {
	TotalTokens  int     `json:"totalTokens"`
	PromptTokens int     `json:"promptTokens"`
	OutputTokens int     `json:"outputTokens"`
	LLMCalls     int     `json:"llmCalls"`
	VideoCalls   int     `json:"videoCalls"`
	FailedCalls  int     `json:"failedCalls"`
	Unreported   int     `json:"unreportedCalls"`
	Clips        int     `json:"clips"`
	Seconds      float64 `json:"seconds"`
}

// Calls 这一窗内发生过多少次计费调用（文本 + 视频）。
func (t usageTotals) Calls() int { return t.LLMCalls + t.VideoCalls }

// usageBreakdownRow 按维度（用途 / 模型）聚合的一行。
type usageBreakdownRow struct {
	Key          string  `json:"key"`
	TotalTokens  int     `json:"totalTokens"`
	PromptTokens int     `json:"promptTokens"`
	OutputTokens int     `json:"outputTokens"`
	Calls        int     `json:"calls"`
	LLMCalls     int     `json:"llmCalls"`
	VideoCalls   int     `json:"videoCalls"`
	Clips        int     `json:"clips"`
	Seconds      float64 `json:"seconds"`
	Failed       int     `json:"failedCalls"`
	Unreported   int     `json:"unreportedCalls"`
}

// usageTrendPoint 趋势图上的一天。
type usageTrendPoint struct {
	Date        string  `json:"date"`
	TotalTokens int     `json:"totalTokens"`
	LLMCalls    int     `json:"llmCalls"`
	Clips       int     `json:"clips"`
	Seconds     float64 `json:"seconds"`
}

// usageRecordRow 最近流水里的一行。
type usageRecordRow struct {
	CreatedAt     string  `json:"createdAt"`
	Kind          string  `json:"kind"`
	Purpose       string  `json:"purpose"`
	DisplayName   string  `json:"displayName"`
	Model         string  `json:"model"`
	TotalTokens   int     `json:"totalTokens"`
	PromptTokens  int     `json:"promptTokens"`
	OutputTokens  int     `json:"outputTokens"`
	Clips         int     `json:"clips"`
	Seconds       float64 `json:"seconds"`
	LatencyMS     int64   `json:"latencyMs"`
	Failed        bool    `json:"failed"`
	Reported      bool    `json:"reported"`
	TaskReference string  `json:"taskReference"`
}

// adapterMaturityRow 视频适配器的适配成熟度，供大屏的适配声明使用。
type adapterMaturityRow struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Description   string `json:"description"`
	Maturity      string `json:"maturity"`
	MaturityLabel string `json:"maturityLabel"`
	Primary       bool   `json:"primary"`
	Note          string `json:"note"`
}

// DashboardSnapshot 首页大屏的一次性快照。
type DashboardSnapshot struct {
	GeneratedAt string                 `json:"generatedAt"`
	HasData     bool                   `json:"hasData"`
	Today       usageTotals            `json:"today"`
	Week        usageTotals            `json:"week"`
	All         usageTotals            `json:"all"`
	Trend       []usageTrendPoint      `json:"trend"`
	ByPurpose   []usageBreakdownRow    `json:"byPurpose"`
	ByModel     []usageBreakdownRow    `json:"byModel"`
	Recent      []usageRecordRow       `json:"recent"`
	Adapters    []adapterMaturityRow   `json:"adapters"`
	Business    dashboardBusinessCount `json:"business"`
	Notes       []string               `json:"notes"`
}

// dashboardBusinessCount 业务量计数：让 token 消耗能对上「最近跑了多少活」。
type dashboardBusinessCount struct {
	VideoTasks         int `json:"videoTasks"`
	VideoTasksThisWeek int `json:"videoTasksThisWeek"`
	CrawlTasks         int `json:"crawlTasks"`
	CrawlTasksThisWeek int `json:"crawlTasksThisWeek"`
	CrawlItems         int `json:"crawlItems"`
	TailFrameAssets    int `json:"tailFrameAssets"`
	Templates          int `json:"templates"`
	Providers          int `json:"providers"`
}

// dashboardNotes 大屏底部的口径说明：把「数据从哪来、哪里不可信」写在数据旁边。
func dashboardNotes() []string {
	return []string{
		"token 取模型响应里的 usage 字段实测值；厂商未回报的调用只计次不计量，不会用字符数估算。",
		"视频渲染按片段数与出片秒数计（视频模型不回报 token），失败片段同样入账——它可能已经扣了额度。",
		"趋势窗口为含今日在内的近 14 个本地日；「近 7 日」为其中最后 7 天。",
	}
}
