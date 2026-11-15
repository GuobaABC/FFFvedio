package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"
)

// dashboardTrendDays 趋势窗口天数：今日 / 近 7 日都从这份按天聚合的数据里推，
// 不在 SQL 里用 date() 做时间换算（SQLite 按 UTC 解析带时区的文本，跨时区会错一天）。
const dashboardTrendDays = 14

const dashboardRecentLimit = 12

// usageRow 一行用量明细（与 usage_events 表列一一对应）。
type usageRow struct {
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
	CreatedAt     string
}

const usageSelectColumns = `kind, purpose, provider_name, model, dialect,
	prompt_tokens, output_tokens, total_tokens, clips, seconds,
	latency_ms, failed, reported, task_id, reference_text, created_at`

// recordUsageEvent 写一条用量流水。
// 采集失败绝不影响主流程：用量是大屏观测数据，不值得让一次已经付费成功的调用作废。
func (a *App) recordUsageEvent(event usageEvent) {
	if a == nil || a.db == nil {
		return
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if event.Purpose == "" {
		event.Purpose = UsagePurposeUnknown
	}
	if _, err := a.db.Exec(`
INSERT INTO usage_events (created_at, kind, purpose, provider_name, model, dialect,
	prompt_tokens, output_tokens, total_tokens, clips, seconds,
	latency_ms, failed, reported, task_id, reference_text)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.CreatedAt.Format(time.RFC3339), event.Kind, event.Purpose, event.ProviderName,
		event.Model, event.Dialect, event.PromptTokens, event.OutputTokens, event.TotalTokens,
		event.Clips, event.Seconds, event.LatencyMS, boolToInt(event.Failed), boolToInt(event.Reported),
		event.TaskID, truncateRunes(event.ReferenceText, 500)); err != nil {
		log.Printf("用量记账失败（不影响业务流程）: %v", err)
	}
}

// recordLLMUsage 记录一次文本模型调用：用途与任务身份来自 context 标注，
// token 数来自服务商响应里的 usage 字段。
func (a *App) recordLLMUsage(ctx context.Context, provider Provider, dialect string, usage llmTokenUsage, latency time.Duration, callErr error) {
	meta := usageMetaFrom(ctx)
	event := usageEvent{
		Kind:         UsageKindLLM,
		Purpose:      meta.purpose,
		ProviderName: provider.Name,
		Model:        provider.Model,
		Dialect:      dialect,
		PromptTokens: usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.total(),
		LatencyMS:    latency.Milliseconds(),
		Failed:       callErr != nil,
		Reported:     usage.Reported,
		TaskID:       meta.taskID,
	}
	if callErr != nil {
		event.ReferenceText = callErr.Error()
	}
	a.recordUsageEvent(event)
}

// recordVideoUsage 记录一次视频渲染调用：视频模型按片段与出片秒数计费，不回报 token。
// 失败也记：失败的片段同样可能已经扣了额度，大屏要能看到这次动作。
// seconds 传服务商实际返回的时长（失败时为 0），它比请求值更接近计费口径。
func (a *App) recordVideoUsage(ctx context.Context, provider Provider, adapterID, model string, seconds float64, latency time.Duration, callErr error) {
	meta := usageMetaFrom(ctx)
	event := usageEvent{
		Kind:         UsageKindVideo,
		Purpose:      firstNonEmpty(meta.purpose, UsagePurposeVideoRender),
		ProviderName: provider.Name,
		Model:        firstNonEmpty(model, adapterID),
		Dialect:      adapterID,
		Clips:        1,
		Seconds:      roundSeconds(seconds),
		LatencyMS:    latency.Milliseconds(),
		Failed:       callErr != nil,
		// 视频接口不回报 token，Reported 保持 false：大屏把这次调用计入次数而不计入量
		TaskID: meta.taskID,
	}
	if callErr != nil {
		event.ReferenceText = callErr.Error()
	}
	a.recordUsageEvent(event)
}

/* ------------------------------ 大屏聚合 ------------------------------ */

// GetDashboard 首页大屏快照：token / 视频用量 + 业务量计数 + 适配成熟度声明。
func (a *App) GetDashboard() (DashboardSnapshot, error) {
	snapshot := DashboardSnapshot{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Trend:       emptyUsageTrend(dashboardTrendDays),
		Adapters:    adapterMaturityRows(),
		Notes:       dashboardNotes(),
	}
	if a == nil || a.db == nil {
		snapshot.Recent = []usageRecordRow{}
		return snapshot, nil
	}

	rows, err := a.queryUsageWithinDays(dashboardTrendDays)
	if err != nil {
		return snapshot, err
	}
	agg := aggregateUsage(rows, dashboardTrendDays)
	snapshot.Trend = agg.Trend
	snapshot.ByPurpose = agg.ByPurpose
	snapshot.ByModel = agg.ByModel
	snapshot.Today = agg.Today
	snapshot.Week = agg.Week
	snapshot.HasData = agg.Calls > 0

	all, err := a.queryUsageTotals()
	if err != nil {
		return snapshot, err
	}
	snapshot.All = all
	if all.Calls() > 0 {
		snapshot.HasData = true
	}

	recent, err := a.queryUsageRecentRows(dashboardRecentLimit)
	if err != nil {
		return snapshot, err
	}
	snapshot.Recent = make([]usageRecordRow, 0, len(recent))
	for _, row := range recent {
		snapshot.Recent = append(snapshot.Recent, row.toRecord())
	}

	if err := a.fillBusinessCounts(&snapshot.Business); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (a *App) queryUsageWithinDays(days int) ([]usageRow, error) {
	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	// created_at 前 10 位即写入时的本地日期（RFC3339 文本），按前缀比较就是按本地日切窗
	rows, err := a.db.Query(`SELECT `+usageSelectColumns+`
FROM usage_events WHERE substr(created_at, 1, 10) >= ? ORDER BY created_at`, cutoff)
	if err != nil {
		return nil, usageError("读取用量明细失败", err)
	}
	defer rows.Close()
	return scanUsageRows(rows)
}

func (a *App) queryUsageRecentRows(limit int) ([]usageRow, error) {
	rows, err := a.db.Query(`SELECT `+usageSelectColumns+`
FROM usage_events ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, usageError("读取最近流水失败", err)
	}
	defer rows.Close()
	return scanUsageRows(rows)
}

func scanUsageRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]usageRow, error) {
	out := make([]usageRow, 0, 16)
	for rows.Next() {
		var row usageRow
		var failed, reported int
		if err := rows.Scan(&row.Kind, &row.Purpose, &row.ProviderName, &row.Model, &row.Dialect,
			&row.PromptTokens, &row.OutputTokens, &row.TotalTokens, &row.Clips, &row.Seconds,
			&row.LatencyMS, &failed, &reported, &row.TaskID, &row.ReferenceText, &row.CreatedAt); err != nil {
			return nil, usageError("解析用量流水失败", err)
		}
		row.Failed = failed != 0
		row.Reported = reported != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

func (a *App) queryUsageTotals() (usageTotals, error) {
	var totals usageTotals
	var llmCalls, videoCalls int
	err := a.db.QueryRow(`
SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(clips), 0), COALESCE(SUM(seconds), 0),
	-- 空库时 SUM 给 NULL 而不是 0，不兜住会让首次打开大屏直接报错
	COALESCE(SUM(CASE WHEN kind = 'llm' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN kind = 'video' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN kind = 'llm' AND reported = 0 AND failed = 0 THEN 1 ELSE 0 END), 0)
FROM usage_events`).Scan(&totals.TotalTokens, &totals.PromptTokens, &totals.OutputTokens,
		&totals.Clips, &totals.Seconds, &llmCalls, &videoCalls, &totals.FailedCalls, &totals.Unreported)
	if err != nil {
		return totals, usageError("读取用量合计失败", err)
	}
	totals.LLMCalls = llmCalls
	totals.VideoCalls = videoCalls
	return totals, nil
}

// fillBusinessCounts 补业务量：只看 token 不知道换了多少产出，加一排任务与条目计数。
func (a *App) fillBusinessCounts(business *dashboardBusinessCount) error {
	weekCutoff := time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	single := []struct {
		dst   *int
		query string
	}{
		{&business.TailFrameAssets, `SELECT COUNT(1) FROM tail_frame_assets`},
		{&business.Templates, `SELECT COUNT(1) FROM video_templates`},
		{&business.Providers, `SELECT COUNT(1) FROM providers`},
	}
	for _, counter := range single {
		if err := a.db.QueryRow(counter.query).Scan(counter.dst); err != nil {
			return usageError("统计业务数据失败", err)
		}
	}

	windowed := []struct {
		total  *int
		recent *int
		query  string
	}{
		{&business.VideoTasks, &business.VideoTasksThisWeek, `video_tasks`},
	}
	for _, counter := range windowed {
		if err := a.db.QueryRow(`SELECT COUNT(1) FROM ` + counter.query).Scan(counter.total); err != nil {
			return usageError("统计业务数据失败", err)
		}
		if err := a.db.QueryRow(`SELECT COUNT(1) FROM `+counter.query+` WHERE substr(created_at, 1, 10) >= ?`,
			weekCutoff).Scan(counter.recent); err != nil {
			return usageError("统计近 7 日业务数据失败", err)
		}
	}
	return nil
}

/* ------------------------------ 内存聚合 ------------------------------ */

type usageAggregation struct {
	Trend     []usageTrendPoint
	ByPurpose []usageBreakdownRow
	ByModel   []usageBreakdownRow
	Today     usageTotals
	Week      usageTotals
	Calls     int
}

// aggregateUsage 按天聚合近 days 日的用量明细，并推出今日 / 近 7 日两个窗口。
// 时间桶在 Go 侧按本地日期字符串比较，避免时区换算把记录挪到隔壁一天。
func aggregateUsage(rows []usageRow, days int) usageAggregation {
	trend := emptyUsageTrend(days)
	today := trend[len(trend)-1].Date
	weekStart := trend[len(trend)-min(days, 7)].Date
	indexByDate := make(map[string]int, len(trend))
	for i, point := range trend {
		indexByDate[point.Date] = i
	}

	byPurpose := make(map[string]*usageBreakdownRow)
	byModel := make(map[string]*usageBreakdownRow)
	var result usageAggregation

	for _, row := range rows {
		date := row.dateKey()
		if idx, ok := indexByDate[date]; ok {
			point := &trend[idx]
			point.TotalTokens += row.TotalTokens
			point.LLMCalls += boolToInt(row.Kind == UsageKindLLM)
			point.Clips += row.Clips
			point.Seconds = roundSeconds(point.Seconds + row.Seconds)
		}
		if date == today {
			addUsageTotals(&result.Today, row)
		}
		if date >= weekStart {
			addUsageTotals(&result.Week, row)
		}
		// 窗口外的记录（理论上不会有）仍要计入调用数，免得聚合口径漏项
		addBreakdown(byPurpose, firstNonEmpty(row.Purpose, UsagePurposeUnknown), row)
		addBreakdown(byModel, row.modelKey(), row)
		result.Calls++
	}

	result.Trend = trend
	result.ByPurpose = flattenBreakdown(byPurpose)
	result.ByModel = flattenBreakdown(byModel)
	return result
}

func addUsageTotals(totals *usageTotals, row usageRow) {
	totals.TotalTokens += row.TotalTokens
	totals.PromptTokens += row.PromptTokens
	totals.OutputTokens += row.OutputTokens
	totals.Clips += row.Clips
	totals.Seconds = roundSeconds(totals.Seconds + row.Seconds)
	totals.FailedCalls += boolToInt(row.Failed)
	if row.Kind == UsageKindVideo {
		totals.VideoCalls++
		return
	}
	totals.LLMCalls++
	totals.Unreported += boolToInt(!row.Reported && !row.Failed)
}

func addBreakdown(rows map[string]*usageBreakdownRow, key string, row usageRow) {
	entry, ok := rows[key]
	if !ok {
		entry = &usageBreakdownRow{Key: key}
		rows[key] = entry
	}
	entry.TotalTokens += row.TotalTokens
	entry.PromptTokens += row.PromptTokens
	entry.OutputTokens += row.OutputTokens
	entry.Clips += row.Clips
	entry.Seconds = roundSeconds(entry.Seconds + row.Seconds)
	entry.Calls++
	entry.Failed += boolToInt(row.Failed)
	entry.Unreported += boolToInt(!row.Reported && !row.Failed && row.Kind == UsageKindLLM)
	if row.Kind == UsageKindVideo {
		entry.VideoCalls++
	} else {
		entry.LLMCalls++
	}
}

func flattenBreakdown(rows map[string]*usageBreakdownRow) []usageBreakdownRow {
	out := make([]usageBreakdownRow, 0, len(rows))
	for _, entry := range rows {
		out = append(out, *entry)
	}
	// 消耗高的排前面：大屏一屏就要看见钱花在哪
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Clips != out[j].Clips {
			return out[i].Clips > out[j].Clips
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// emptyUsageTrend 生成含今天在内的近 days 天占位序列，保证图表横轴稳定。
func emptyUsageTrend(days int) []usageTrendPoint {
	if days <= 0 {
		days = 1
	}
	out := make([]usageTrendPoint, 0, days)
	today := time.Now()
	for offset := days - 1; offset >= 0; offset-- {
		out = append(out, usageTrendPoint{Date: today.AddDate(0, 0, -offset).Format("2006-01-02")})
	}
	return out
}

func roundSeconds(value float64) float64 {
	return float64(int64(value*10+0.5)) / 10
}

func (r usageRow) dateKey() string {
	if len(r.CreatedAt) >= 10 {
		return r.CreatedAt[:10]
	}
	return r.CreatedAt
}

// modelKey 聚合用的模型名：视频调用可能只填了适配器 ID。
func (r usageRow) modelKey() string {
	if r.Model != "" {
		return r.Model
	}
	if r.Kind == UsageKindVideo {
		return firstNonEmpty(r.Dialect, "未命名模型")
	}
	return firstNonEmpty(r.ProviderName, "未命名模型")
}

// displayName 流水行展示名：文本调用看提供商，视频调用看模型 / 适配器。
func (r usageRow) displayName() string {
	if r.Kind == UsageKindVideo {
		return firstNonEmpty(r.Model, r.Dialect, "视频模型")
	}
	return firstNonEmpty(r.ProviderName, r.Model, "文本模型")
}

func (r usageRow) toRecord() usageRecordRow {
	reference := firstNonEmpty(r.TaskID, r.ReferenceText)
	return usageRecordRow{
		CreatedAt:     r.CreatedAt,
		Kind:          r.Kind,
		Purpose:       firstNonEmpty(r.Purpose, UsagePurposeUnknown),
		DisplayName:   r.displayName(),
		Model:         r.Model,
		TotalTokens:   r.TotalTokens,
		PromptTokens:  r.PromptTokens,
		OutputTokens:  r.OutputTokens,
		Clips:         r.Clips,
		Seconds:       r.Seconds,
		LatencyMS:     r.LatencyMS,
		Failed:        r.Failed,
		Reported:      r.Reported,
		TaskReference: truncateRunes(reference, 80),
	}
}

// usageError 用量查询的错误出口：读表失败只影响大屏，日志里留一条线索即可。
func usageError(action string, err error) error {
	return fmt.Errorf("%s: %w", action, err)
}
