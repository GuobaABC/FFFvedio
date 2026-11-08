package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForVideoTask 轮询任务状态直到 predicate 命中（用于走后台 goroutine 的关卡动作）。
func waitForVideoTask(t *testing.T, app *App, id string, predicate func(VideoTask) bool, reason string) VideoTask {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last VideoTask
	for time.Now().Before(deadline) {
		task, err := app.videoTaskByID(id)
		if err == nil {
			last = task
			if predicate(task) {
				return task
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待任务状态超时（%s）: stage=%s status=%s error=%s pending=%s pendingPrompt=%s logs=%v",
		reason, last.Stage, last.Status, last.Error, last.Run.ReviewPendingSegment, last.Run.ReviewPendingPromptSegment, last.Run.Logs)
	return last
}

// waitForVideoTaskIdle 等待后台流水线真正退出：避免紧接着的关卡操作撞上任务锁。
func waitForVideoTaskIdle(t *testing.T, app *App, id string, reason string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !app.isVideoTaskRunning(id) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待任务退出执行状态超时（%s）", reason)
}

func TestSegmentReviewGuards(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "逐段确认边界", DurationS: 6, Adapter: "mock",
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageRender
	task.Status = VideoStatusWaiting
	task.Run.Plan = &VideoPlan{Segments: []VideoSegment{{ID: "s1-1", Index: 0, DurationS: 3}}}
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	// 有待确认片段时，通用「继续执行」必须挡在关卡外
	task.Run.ReviewPendingSegment = "s1-1"
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := app.RunVideoTask(created.ID); err == nil || !strings.Contains(err.Error(), "片段确认关卡") {
		t.Fatalf("待确认片段时 RunVideoTask 应报错: %v", err)
	}

	// 生成前的提示词确认同样挡在关卡外
	task.Run.ReviewPendingSegment = ""
	task.Run.ReviewPendingPromptSegment = "s1-1"
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := app.RunVideoTask(created.ID); err == nil || !strings.Contains(err.Error(), "分段提示词确认关卡") {
		t.Fatalf("待确认提示词时 RunVideoTask 应报错: %v", err)
	}
	if err := app.ApproveVideoSegmentPrompt(created.ID, VideoSegmentRegen{SegmentID: "s9-9"}); err == nil ||
		!strings.Contains(err.Error(), "不是当前待确认") {
		t.Fatalf("确认非当前片段应报错: %v", err)
	}

	// 没有待确认片段时不能凭空确认
	task.Run.ReviewPendingSegment = ""
	task.Run.ReviewPendingPromptSegment = ""
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := app.ApproveVideoSegment(created.ID); err == nil || !strings.Contains(err.Error(), "没有待确认") {
		t.Fatalf("无待确认片段时 ApproveVideoSegment 应报错: %v", err)
	}
	if err := app.ApproveVideoSegmentPrompt(created.ID, VideoSegmentRegen{SegmentID: "s1-1"}); err == nil ||
		!strings.Contains(err.Error(), "没有待确认") {
		t.Fatalf("无待确认提示词时 ApproveVideoSegmentPrompt 应报错: %v", err)
	}

	// 重生成必须命中渲染计划里的片段
	if err := app.RegenerateVideoSegment(created.ID, VideoSegmentRegen{SegmentID: "not-exist"}); err == nil ||
		!strings.Contains(err.Error(), "不在渲染计划中") {
		t.Fatalf("未知片段重生成应报错: %v", err)
	}

	// 非渲染阶段不允许单段重渲
	task.Stage = VideoStageConfirmScript
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := app.RegenerateVideoSegment(created.ID, VideoSegmentRegen{SegmentID: "s1-1"}); err == nil ||
		!strings.Contains(err.Error(), "仅可在视频生成阶段") {
		t.Fatalf("非渲染阶段重渲应报错: %v", err)
	}
}

// 逐段确认的「生成前」关卡：第 2 段提交模型前必须先让用户看到并确认提示词；
// 已生成成功的第一段不能重复暂停，确认后改动要真的写进渲染计划。
func TestSegmentPromptReviewPausesBeforeRender(t *testing.T) {
	if _, ok := ffmpegPath(); !ok {
		t.Skip("本地联调占位需要 ffmpeg，跳过逐段确认渲染集成测试")
	}
	withTempVideoRoot(t)
	app := newTestApp(t)

	reviewOn := true
	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "逐段提示词确认", DurationS: 8, Adapter: "mock", SegmentReview: &reviewOn,
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageRender
	task.Status = VideoStatusWaiting
	// 预置发布元数据，避免渲染阶段顺带调用文本模型
	task.Run.PublishMeta = &PublishMeta{}
	task.Run.Script = &VideoScript{Shots: []VideoShot{
		{ID: "s1", TimeRange: [2]float64{0, 4}, DurationS: 4},
		{ID: "s2", TimeRange: [2]float64{4, 8}, DurationS: 4},
	}}
	task.Run.Prompts = []VideoPromptItem{
		{ShotID: "s1", VisualPrompt: "第一段画面", Start: 0, End: 4},
		{ShotID: "s2", VisualPrompt: "第二段画面", Start: 4, End: 8},
	}
	task.Run.Plan = &VideoPlan{Adapter: "mock", ClipCount: 2, TotalS: 2, Segments: []VideoSegment{
		{ID: "s1-1", ShotID: "s1", Index: 0, Prompt: "第一段画面", DurationS: 1, Start: 0, End: 1},
		{ID: "s2-1", ShotID: "s2", Index: 1, Prompt: "第二段画面", DurationS: 1, Start: 1, End: 2},
	}}
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	// 第一段渲染完成后先停在后置确认关卡
	app.runVideoPipeline(created.ID, VideoStageRender, "")
	task = waitForVideoTask(t, app, created.ID, func(item VideoTask) bool {
		return item.Run.ReviewPendingSegment == "s1-1"
	}, "第一段生成后应停在片段确认")
	if task.Run.ReviewPendingPromptSegment != "" {
		t.Fatalf("第一段不该弹生成前确认: %q", task.Run.ReviewPendingPromptSegment)
	}
	if len(task.Run.Renders) != 1 || task.Run.Renders[0].Status != "succeeded" {
		t.Fatalf("第一段应已生成成功: %+v", task.Run.Renders)
	}
	waitForVideoTaskIdle(t, app, created.ID, "第一段确认关卡")

	// 确认第一段后，第二段应在渲染前停住等提示词确认
	if err := app.ApproveVideoSegment(created.ID); err != nil {
		t.Fatalf("确认第一段失败: %v", err)
	}
	task = waitForVideoTask(t, app, created.ID, func(item VideoTask) bool {
		return item.Run.ReviewPendingPromptSegment == "s2-1"
	}, "第二段生成前应停在提示词确认")
	if task.Stage != VideoStageRender || task.Status != VideoStatusWaiting {
		t.Fatalf("生成前确认应停在渲染阶段等待: stage=%s status=%s", task.Stage, task.Status)
	}
	for _, render := range task.Run.Renders {
		if render.SegmentID == "s2-1" {
			t.Fatalf("第二段尚未确认提示词就提交了模型: %+v", render)
		}
	}
	waitForVideoTaskIdle(t, app, created.ID, "第二段提示词确认关卡")

	// 确认时改写提示词/台词，改动必须落进渲染计划与提示词权威源
	if err := app.ApproveVideoSegmentPrompt(created.ID, VideoSegmentRegen{
		SegmentID:    "s2-1",
		VisualPrompt: "第二段手改后的画面",
		Dialogue:     "手改台词",
	}); err != nil {
		t.Fatalf("确认段落提示词失败: %v", err)
	}
	task = waitForVideoTask(t, app, created.ID, func(item VideoTask) bool {
		return item.Run.ReviewPendingSegment == "s2-1"
	}, "第二段生成后应停在片段确认")
	if task.Run.Plan.Segments[1].Prompt != "第二段手改后的画面" {
		t.Fatalf("确认时改写的提示词没进渲染计划: %q", task.Run.Plan.Segments[1].Prompt)
	}
	if task.Run.Prompts[1].VisualPrompt != "第二段手改后的画面" || task.Run.Prompts[1].Dialogue != "手改台词" {
		t.Fatalf("确认时的改写没回写提示词关卡: %+v", task.Run.Prompts[1])
	}
	if !segmentPromptApproved(&task.Run, "s2-1") {
		t.Fatal("确认后应记住该段提示词已确认，断点续跑不再重复暂停")
	}
}

func TestTailFrameAssetStore(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	workDir, err := app.ensureTaskWorkDir("vt-asset")
	if err != nil {
		t.Fatalf("创建任务目录失败: %v", err)
	}
	clipsDir := filepath.Join(workDir, "clips")
	if err := os.MkdirAll(clipsDir, 0o755); err != nil {
		t.Fatalf("创建 clips 目录失败: %v", err)
	}
	first := filepath.Join(clipsDir, "s2-1-final-tail.png")
	second := filepath.Join(clipsDir, "s2-2-final-tail.png")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
			t.Fatalf("写入尾帧文件失败: %v", err)
		}
	}

	if err := app.replaceTailFrameAsset("vt-asset", "任务A", "s2-1", first); err != nil {
		t.Fatalf("登记尾帧资产失败: %v", err)
	}
	// 同任务重生成最后一段后覆盖旧资产，库里始终只保留一条
	if err := app.replaceTailFrameAsset("vt-asset", "任务A", "s2-2", second); err != nil {
		t.Fatalf("覆盖尾帧资产失败: %v", err)
	}
	assets, err := app.ListTailFrameAssets()
	if err != nil {
		t.Fatalf("读取尾帧资产失败: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("同任务应只保留一条尾帧资产: %+v", assets)
	}
	if assets[0].SegmentID != "s2-2" || assets[0].ImagePath != second {
		t.Fatalf("资产应保留最新一条: %+v", assets[0])
	}
	if assets[0].MediaURL != videoMediaURL("vt-asset", "clips/s2-2-final-tail.png") {
		t.Fatalf("mediaUrl 未按任务目录生成: %s", assets[0].MediaURL)
	}

	if err := app.DeleteTailFrameAsset(assets[0].ID); err != nil {
		t.Fatalf("删除尾帧资产失败: %v", err)
	}
	if list, err := app.ListTailFrameAssets(); err != nil || len(list) != 0 {
		t.Fatalf("删除后资产列表应为空: %v %+v", err, list)
	}
	if err := app.DeleteTailFrameAsset(assets[0].ID); err == nil {
		t.Fatal("重复删除应报错")
	}
}

func TestSuggestSegmentPrompt(t *testing.T) {
	withTempVideoRoot(t)

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		// 模型偶发回“引号包裹的代码块”，验证清理能力
		content := "\"```\n缓慢横移镜头，主体抬手示意，暖色逆光\n```\""
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
		_, _ = io.WriteString(w, string(payload))
	}))
	defer server.Close()

	app := newTestApp(t)

	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "AI 重写单段提示词", DurationS: 6, Adapter: "mock",
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Run.Script = &VideoScript{Shots: []VideoShot{{ID: "s1", SceneIntent: "咖啡馆清晨", Camera: "推近", Mood: "温暖"}}}
	task.Run.Plan = &VideoPlan{Segments: []VideoSegment{
		{ID: "s1-1", ShotID: "s1", Index: 0, Prompt: "旧提示词", Dialogue: "第一句", DurationS: 3},
	}}
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	// 没有文本模型时必须给出厂商指引
	if _, err := app.SuggestSegmentPrompt(created.ID, "s1-1", "太暗"); err == nil ||
		!strings.Contains(err.Error(), "系统设置") {
		t.Fatalf("缺少文本模型应提示去提供商配置: %v", err)
	}

	if _, err := app.SaveProvider(Provider{
		Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model",
	}); err != nil {
		t.Fatalf("保存提供商失败: %v", err)
	}

	suggestion, err := app.SuggestSegmentPrompt(created.ID, "s1-1", "光线太暗")
	if err != nil {
		t.Fatalf("AI 生成提示词失败: %v", err)
	}
	if strings.Contains(suggestion, "```") || strings.HasPrefix(suggestion, "\"") {
		t.Fatalf("建议词应剥掉代码块与引号: %q", suggestion)
	}
	if !strings.Contains(suggestion, "缓慢横移镜头") {
		t.Fatalf("建议词内容不符: %q", suggestion)
	}
	body, _ := json.Marshal(gotBody)
	if !strings.Contains(string(body), "光线太暗") || !strings.Contains(string(body), "旧提示词") {
		t.Fatal("请求应带上当前提示词与用户反馈")
	}

	// 片段不在计划中时报错
	if _, err := app.SuggestSegmentPrompt(created.ID, "s9-9", "意见"); err == nil {
		t.Fatal("未知片段应报错")
	}
}

func TestNormalizeSegmentReviewDefaults(t *testing.T) {
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "默认逐段确认"})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if !cfg.segmentReviewEnabled() {
		t.Fatal("逐段确认应默认开启")
	}

	disabled := false
	explicit, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "关闭逐段确认", SegmentReview: &disabled})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if explicit.segmentReviewEnabled() {
		t.Fatal("显式关闭后应保持关闭")
	}
	zero := VideoRunConfig{}
	if explicit.KeepLastFrame || zero.KeepLastFrame {
		t.Fatal("保留尾帧应默认关闭")
	}
}

// 严格首帧默认开启：用户给的首帧/尾帧必须作为新视频的第一帧，
// 只有显式关掉才允许让角色参考图挤掉锁帧（方舟两类任务互斥）。
func TestStrictFirstFrameDefaultsOn(t *testing.T) {
	if !(VideoRunConfig{}).strictFirstFrameEnabled() {
		t.Fatal("严格首帧应默认开启")
	}
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "严格首帧默认", FirstFrame: "https://example.com/first.png"})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if cfg.StrictFirstFrame == nil || !*cfg.StrictFirstFrame {
		t.Fatalf("归一化应写入默认的严格首帧: %v", cfg.StrictFirstFrame)
	}

	off := false
	loose, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "换角色参考图", StrictFirstFrame: &off})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if loose.strictFirstFrameEnabled() {
		t.Fatal("显式关闭后应保持关闭")
	}
}
