package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newUsageStub 起一个 OpenAI 兼容的假服务，响应体由用例给定（用来验证 usage 解析）。
func newUsageStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func queryUsageEvents(t *testing.T, app *App) []usageRow {
	t.Helper()
	rows, err := app.queryUsageRecentRows(50)
	if err != nil {
		t.Fatalf("读取用量流水失败: %v", err)
	}
	return rows
}

func TestChatCompletionRecordsTokenUsage(t *testing.T) {
	server := newUsageStub(t, `{"choices":[{"message":{"content":"ok"}}],
		"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500}}`)
	app := newTestApp(t)

	ctx := withUsage(context.Background(), UsagePurposeScript, "task-usage-1")
	if _, err := app.chatCompletion(ctx, Provider{
		Name:    "火山方舟",
		BaseURL: server.URL,
		APIKey:  "k",
		Model:   "doubao-seed-2-5",
	}, "system", "user"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	rows := queryUsageEvents(t, app)
	if len(rows) != 1 {
		t.Fatalf("应落一条用量流水，实际 %d 条", len(rows))
	}
	row := rows[0]
	if row.Kind != UsageKindLLM || row.Purpose != UsagePurposeScript || row.TaskID != "task-usage-1" {
		t.Fatalf("流水身份标注错误: %+v", row)
	}
	if row.PromptTokens != 1200 || row.OutputTokens != 300 || row.TotalTokens != 1500 {
		t.Fatalf("token 解析错误: %+v", row)
	}
	if !row.Reported || row.Failed {
		t.Fatalf("回报状态错误: reported=%v failed=%v", row.Reported, row.Failed)
	}
	if row.Dialect != DialectOpenAI {
		t.Fatalf("方言未记录: %q", row.Dialect)
	}
}

func TestChatCompletionRecordsAnthropicCacheTokens(t *testing.T) {
	// Anthropic 命中缓存的输入不写在 input_tokens 里，漏掉就会低估消耗
	server := newUsageStub(t, `{"content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":600}}`)
	app := newTestApp(t)

	if _, err := app.chatCompletion(context.Background(), Provider{
		BaseURL: server.URL, APIKey: "k", Model: "claude-sonnet", Dialect: DialectAnthropic,
	}, "system", "user"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	rows := queryUsageEvents(t, app)
	if len(rows) != 1 {
		t.Fatalf("应落一条流水，实际 %d 条", len(rows))
	}
	if rows[0].PromptTokens != 700 || rows[0].TotalTokens != 740 {
		t.Fatalf("Anthropic 输入量应含缓存读: %+v", rows[0])
	}
	if rows[0].Purpose != UsagePurposeUnknown {
		t.Fatalf("没标注用途时应归到未标注，实际 %q", rows[0].Purpose)
	}
}

func TestChatCompletionRecordsUnreportedAndFailedCalls(t *testing.T) {
	noUsage := newUsageStub(t, `{"choices":[{"message":{"content":"ok"}}]}`)
	app := newTestApp(t)
	if _, err := app.chatCompletion(context.Background(), Provider{
		BaseURL: noUsage.URL, APIKey: "k", Model: "glm-4",
	}, "system", "user"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	t.Cleanup(failing.Close)
	if _, err := app.chatCompletion(context.Background(), Provider{
		BaseURL: failing.URL, APIKey: "k", Model: "glm-4",
	}, "system", "user"); err == nil {
		t.Fatal("429 应当报错")
	}

	rows := queryUsageEvents(t, app)
	if len(rows) != 2 {
		t.Fatalf("成功与失败都应入账，实际 %d 条", len(rows))
	}
	var unreported, failed usageRow
	for _, row := range rows {
		switch {
		case row.Failed:
			failed = row
		default:
			unreported = row
		}
	}
	// 厂商没回报时记 0 而不是估算，大屏靠 reported 字段说明口径
	if unreported.Reported || unreported.TotalTokens != 0 || unreported.ReferenceText != "" {
		t.Fatalf("未回报的调用应只计次不计量: %+v", unreported)
	}
	if !failed.Failed || failed.Reported || failed.ReferenceText == "" {
		t.Fatalf("失败调用应带错误摘要: %+v", failed)
	}
}

func TestRecordVideoUsageCountsClipsAndSeconds(t *testing.T) {
	app := newTestApp(t)
	ctx := withUsage(context.Background(), UsagePurposeVideoRender, "task-video-9")

	app.recordVideoUsage(ctx, Provider{Name: "火山方舟"}, "ark", "doubao-seedance-2-5", 10, 3*time.Minute, nil)
	app.recordVideoUsage(ctx, Provider{Name: "火山方舟"}, "ark", "", 0, time.Minute, context.DeadlineExceeded)

	rows := queryUsageEvents(t, app)
	if len(rows) != 2 {
		t.Fatalf("视频渲染成功与失败都应入账，实际 %d 条", len(rows))
	}
	var ok, bad usageRow
	for _, row := range rows {
		if row.Failed {
			bad = row
		} else {
			ok = row
		}
	}
	if ok.Kind != UsageKindVideo || ok.Clips != 1 || ok.Seconds != 10 || ok.Model != "doubao-seedance-2-5" {
		t.Fatalf("成功片段记账错误: %+v", ok)
	}
	if ok.Reported {
		t.Fatal("视频接口不回报 token，reported 必须为 false")
	}
	// 失败时拿不到模型与时长，用适配器 ID 兜底，仍要能看出是哪个任务
	if bad.Model != "ark" || bad.Clips != 1 || bad.TaskID != "task-video-9" || bad.ReferenceText == "" {
		t.Fatalf("失败片段记账错误: %+v", bad)
	}
}

func TestGetDashboardAggregatesWindowsAndBreakdown(t *testing.T) {
	app := newTestApp(t)
	today := time.Now()

	app.recordUsageEvent(usageEvent{Kind: UsageKindLLM, Purpose: UsagePurposeScript,
		ProviderName: "火山方舟", Model: "doubao-seed-2-5", PromptTokens: 700, OutputTokens: 300,
		TotalTokens: 1000, Reported: true, TaskID: "t1"})
	app.recordUsageEvent(usageEvent{Kind: UsageKindVideo, Purpose: UsagePurposeVideoRender,
		ProviderName: "火山方舟", Model: "doubao-seedance-2-5", Clips: 1, Seconds: 10, TaskID: "t1"})
	app.recordUsageEvent(usageEvent{Kind: UsageKindLLM, Purpose: UsagePurposePrompt,
		Model: "doubao-seed-2-5", TotalTokens: 500, Reported: true,
		CreatedAt: today.AddDate(0, 0, -3)})
	// 窗口外的记录：只进累计，不进今日/近 7 日/趋势
	app.recordUsageEvent(usageEvent{Kind: UsageKindLLM, Purpose: UsagePurposePublishMeta,
		Model: "glm-4", TotalTokens: 9999, Reported: true, CreatedAt: today.AddDate(0, 0, -20)})

	snapshot, err := app.GetDashboard()
	if err != nil {
		t.Fatalf("大屏快照失败: %v", err)
	}
	if !snapshot.HasData {
		t.Fatal("有用量记录时 hasData 应为 true")
	}
	if snapshot.Today.TotalTokens != 1000 || snapshot.Today.Clips != 1 || snapshot.Today.Seconds != 10 {
		t.Fatalf("今日窗口聚合错误: %+v", snapshot.Today)
	}
	if snapshot.Today.LLMCalls != 1 || snapshot.Today.VideoCalls != 1 || snapshot.Today.Calls() != 2 {
		t.Fatalf("今日调用数错误: %+v", snapshot.Today)
	}
	if snapshot.Week.TotalTokens != 1500 || snapshot.Week.LLMCalls != 2 {
		t.Fatalf("近 7 日窗口聚合错误: %+v", snapshot.Week)
	}
	if snapshot.All.TotalTokens != 11499 || snapshot.All.Calls() != 4 {
		t.Fatalf("累计应含窗口外记录: %+v", snapshot.All)
	}

	if len(snapshot.Trend) != dashboardTrendDays {
		t.Fatalf("趋势天数应为 %d，实际 %d", dashboardTrendDays, len(snapshot.Trend))
	}
	if last := snapshot.Trend[len(snapshot.Trend)-1]; last.Date != today.Format("2006-01-02") || last.TotalTokens != 1000 || last.Clips != 1 {
		t.Fatalf("趋势最后一天应是今天: %+v", last)
	}
	if snapshot.Trend[0].TotalTokens != 0 || len(snapshot.Trend[0].Date) != 10 {
		t.Fatalf("空白日应保留占位: %+v", snapshot.Trend[0])
	}

	// 窗口内的三个用途都应在，窗口外的「发布元数据」不能进分布
	if len(snapshot.ByPurpose) != 3 || snapshot.ByPurpose[0].Key != UsagePurposeScript {
		t.Fatalf("用途分布应只含窗口内记录且按消耗倒序: %+v", snapshot.ByPurpose)
	}
	for _, row := range snapshot.ByPurpose {
		if row.Key == UsagePurposePublishMeta {
			t.Fatalf("窗口外的用途不应出现在趋势分布里: %+v", snapshot.ByPurpose)
		}
		if row.Key == UsagePurposeVideoRender && (row.Clips != 1 || row.Seconds != 10 || row.LLMCalls != 0) {
			t.Fatalf("视频渲染应单独成一个用途桶: %+v", row)
		}
	}
	if snapshot.ByPurpose[1].TotalTokens != 500 {
		t.Fatalf("用途分布数值错误: %+v", snapshot.ByPurpose)
	}
	if len(snapshot.ByModel) == 0 || snapshot.ByModel[0].TotalTokens < snapshot.ByModel[len(snapshot.ByModel)-1].TotalTokens {
		t.Fatalf("模型分布排序错误: %+v", snapshot.ByModel)
	}
	if len(snapshot.Recent) == 0 || snapshot.Recent[0].Purpose == "" {
		t.Fatalf("最近流水缺少用途标注: %+v", snapshot.Recent)
	}
	if len(snapshot.Notes) == 0 {
		t.Fatal("大屏必须带口径说明")
	}
}

func TestDashboardAdaptersDeclareByteDanceAsPrimary(t *testing.T) {
	snapshot, err := newTestApp(t).GetDashboard()
	if err != nil {
		t.Fatalf("大屏快照失败: %v", err)
	}

	primary := 0
	for index, row := range snapshot.Adapters {
		if row.Maturity == AdapterMaturityPrimary {
			primary++
			if row.ID != "ark" || index != 0 || !row.Primary {
				t.Fatalf("主力适配只能是方舟且必须排首位: %+v (index %d)", row, index)
			}
			continue
		}
		if row.Primary || row.MaturityLabel == "" || row.Note == "" {
			t.Fatalf("非主力通道应说明缺口: %+v", row)
		}
	}
	if primary != 1 {
		t.Fatalf("只能有一个主力适配，实际 %d 个", primary)
	}
	// 方舟是字节跳动（火山方舟 / 豆包 Seedance），其余通道不得被声明成已完整适配
	if snapshot.Adapters[0].Label == "" {
		t.Fatal("主力适配缺少展示名")
	}
	for _, row := range snapshot.Adapters[1:] {
		if row.Maturity == AdapterMaturityPrimary {
			t.Fatalf("%s 被误标为主力适配", row.ID)
		}
	}
}

func TestAdapterInfoCarriesMaturityForCreatePage(t *testing.T) {
	for _, info := range adapterInfos() {
		if info.Maturity == "" || info.MaturityLabel == "" {
			t.Fatalf("%s 缺少适配成熟度声明", info.ID)
		}
		switch info.Maturity {
		case AdapterMaturityPrimary, AdapterMaturityPartial, AdapterMaturityLocal:
		default:
			t.Fatalf("%s 成熟度取值非法: %s", info.ID, info.Maturity)
		}
	}
}

func TestUsageContextTagsAreIndependent(t *testing.T) {
	// 上游先带任务 ID，深处再补用途，两者不能互相覆盖
	ctx := withUsageTask(context.Background(), "task-a")
	ctx = withUsagePurpose(ctx, UsagePurposeHotExtract)
	meta := usageMetaFrom(ctx)
	if meta.taskID != "task-a" || meta.purpose != UsagePurposeHotExtract {
		t.Fatalf("标注被覆盖: %+v", meta)
	}
	if got := usageMetaFrom(context.Background()); got.purpose != UsagePurposeUnknown {
		t.Fatalf("未标注时应归到未标注用途: %+v", got)
	}
}
