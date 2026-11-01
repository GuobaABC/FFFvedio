package main

import (
	"context"
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

func withTempVideoRoot(t *testing.T) {
	t.Helper()
	previous := videoWorkRootOverride
	videoWorkRootOverride = t.TempDir()
	t.Cleanup(func() { videoWorkRootOverride = previous })
}

func TestNormalizeVideoConfigDefaults(t *testing.T) {
	enabled := true
	cfg, warnings, err := normalizeVideoConfig(VideoRunConfig{
		Topic:         "  一杯咖啡唤醒清晨  ",
		DurationS:     0,
		Fps:           0,
		AspectRatio:   "21:9",
		SpeechRate:    0,
		Language:      "",
		LocalFallback: &enabled,
		SubtitleStyle: VideoSubtitleStyle{PrimaryColor: "white", OutlineColor: "#000"},
	})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if cfg.Topic != "一杯咖啡唤醒清晨" {
		t.Fatalf("主题未去除首尾空格: %q", cfg.Topic)
	}
	if cfg.DurationS != 10 || cfg.Fps != 24 {
		t.Fatalf("默认时长/帧率错误: %d / %d", cfg.DurationS, cfg.Fps)
	}
	if cfg.AspectRatio != "16:9" || cfg.Language != "zh-CN" {
		t.Fatalf("默认画幅/语言错误: %s / %s", cfg.AspectRatio, cfg.Language)
	}
	if cfg.SpeechRate != 4 {
		t.Fatalf("中文默认语速应为 4 字/秒，实际 %.2f", cfg.SpeechRate)
	}
	if cfg.SubtitleStyle.PrimaryColor != "#FFFFFF" || cfg.SubtitleStyle.OutlineColor != "#000000" {
		t.Fatalf("字幕颜色未归一化: %+v", cfg.SubtitleStyle)
	}
	if cfg.SubtitleStyle.FontName == "" || cfg.SubtitleStyle.Alignment != 2 {
		t.Fatalf("字幕样式默认值缺失: %+v", cfg.SubtitleStyle)
	}
	if len(warnings) != 0 {
		t.Fatalf("不应产生警告: %v", warnings)
	}

	if _, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "  "}); err == nil {
		t.Fatal("空主题应返回错误")
	}
	if _, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "主题", FirstFrame: "不存在.png"}); err == nil {
		t.Fatal("不可读的首帧应返回错误")
	}
}

func TestSpeechTimingModel(t *testing.T) {
	if got := speechUnitCount("这杯咖啡，唤醒整座城市", "zh-CN"); got != 10 {
		t.Fatalf("中文字数统计错误: %d", got)
	}
	if got := speechUnitCount("good morning city", "en-US"); got != 3 {
		t.Fatalf("英文词数统计错误: %d", got)
	}
	if got := estimateSpeechSeconds("这杯咖啡，唤醒整座城市", "zh-CN", 4); got != 2.8 {
		t.Fatalf("台词时长估算错误: %.2f", got)
	}
	if got := estimateSpeechSeconds("", "zh-CN", 4); got != 0 {
		t.Fatalf("空台词应为 0，实际 %.2f", got)
	}
}

func TestNearestAllowedDuration(t *testing.T) {
	allowed := []int{5, 10, 15}
	cases := map[float64]int{4: 5, 7: 5, 8: 10, 13: 15, 30: 15}
	for input, expected := range cases {
		if got := nearestAllowedDuration(input, allowed); got != expected {
			t.Fatalf("时长 %.1f 夹取错误: 期望 %d，实际 %d", input, expected, got)
		}
	}
}

func TestBuildVideoPlanSplitsLongShot(t *testing.T) {
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "测试主题", DurationS: 18, SpeechRate: 4})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	prompts := []VideoPromptItem{{
		ShotID: "s1", VisualPrompt: "缓慢推近", Dialogue: "一二三四五六七八",
		Start: 0, End: 18,
	}}
	caps := VideoCapabilities{AllowedDurations: []int{5, 10}, MaxClipS: 10, LastFrame: false}
	plan := buildVideoPlan(cfg, prompts, caps, "generic")

	if len(plan.Segments) != 2 {
		t.Fatalf("18s 应被切成 2 段，实际 %d 段", len(plan.Segments))
	}
	if plan.Segments[0].Dialogue == "" || plan.Segments[1].Dialogue != "" {
		t.Fatal("台词只应落在切分后的第一段")
	}
	if plan.TotalS != 20 {
		t.Fatalf("按档位生成后总时长应为 20s，实际 %.1f", plan.TotalS)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("时长档位导致偏差时应给出警告")
	}
}

func TestSubtitleCuesAlignedWithSegments(t *testing.T) {
	cfg, _, _ := normalizeVideoConfig(VideoRunConfig{Topic: "测试", DurationS: 10, SpeechRate: 4})
	plan := VideoPlan{Segments: []VideoSegment{
		{ID: "s1-1", Start: 0, End: 5, Dialogue: "一二三四五六七八"},
		{ID: "s1-2", Start: 5, End: 10, Dialogue: ""},
	}}
	cues := buildSubtitleCues(plan, cfg)
	if len(cues) != 1 {
		t.Fatalf("应只生成 1 条字幕，实际 %d", len(cues))
	}
	if cues[0].Start < 0 || cues[0].End > 5 || cues[0].End <= cues[0].Start {
		t.Fatalf("字幕时间轴越界: %+v", cues[0])
	}
}

// 合成阶段拿到的是片段实测时长：一句一条，后面的段要跟着新起点，不能沿计划坐标漂着。
func TestSubtitleCuesFollowMeasuredSpans(t *testing.T) {
	cfg, _, _ := normalizeVideoConfig(VideoRunConfig{Topic: "测试", DurationS: 10, SpeechRate: 4, Subtitles: true})
	// 计划是 5+5，实渲成 4.1+5.9，第二段起点应跟着变成 4.1
	spans := []SubtitleSpan{
		{Start: 0, Duration: 4.1, Text: "第一句很短。第二句也在这段里说。"},
		{Start: 5, Duration: 5.9, Text: "第二段才开始的话"},
	}
	cues := buildSubtitleCuesFromSpans(cfg, spans)
	if len(cues) < 3 {
		t.Fatalf("同一段的两句台词应拆成两条字幕，实际 %d 条: %+v", len(cues), cues)
	}
	if cues[0].End > 4.1 || cues[len(cues)-1].End > 10 || cues[len(cues)-1].Start < 4.1 {
		t.Fatalf("字幕越界或未跟随实测起点: %+v", cues)
	}
	for index := 1; index < len(cues); index++ {
		if cues[index].Start < cues[index-1].End {
			t.Fatalf("字幕重叠: %+v", cues)
		}
	}
}

func TestTargetCanvasMatchesAspect(t *testing.T) {
	// 16:9 素材转竖屏：以源短边为基准，不把体积拉大
	if width, height, ok := targetCanvas("9:16", 1920, 1080); !ok || width != 1080 || height != 1920 {
		t.Fatalf("横转竖换算错误: %dx%d %v", width, height, ok)
	}
	if _, _, ok := targetCanvas("16:9", 1920, 1080); ok {
		t.Fatal("画幅已一致时不该再重编码")
	}
	if width, height, ok := targetCanvas("1:1", 1080, 1920); !ok || width != 1080 || height != 1080 {
		t.Fatalf("方画幅换算错误: %dx%d %v", width, height, ok)
	}
}

// 安装包内置的 imageio-ffmpeg 只有 ffmpeg，探测必须能从它的回显里读出元数据。
func TestParseFFmpegMetadataFromBundledFFmpeg(t *testing.T) {
	verbose := `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'src.mp4':
  Metadata:
    major_brand     : isom
  Duration: 00:00:05.04, start: 0.000000, bitrate: 241 kb/s
  Stream #0:0[0x1](und): Video: h264 (High) (avc1 / 0x31637661), yuv420p(progressive), 1920x1080 [SAR 1:1 DAR 16:9], 161 kb/s, 24 fps, 24 tbr, 12288 tbn (default)
    Metadata:
      handler_name    : VideoHandler
  Stream #0:1[0x2](und): Audio: aac (LC) (mp4a / 0x6134706D), 44100 Hz, mono, fltp, 69 kb/s (default)`

	info := parseFFmpegMetadata(verbose)
	if info.Width != 1920 || info.Height != 1080 {
		t.Fatalf("尺寸解析错误: %+v", info)
	}
	if info.Duration < 5.03 || info.Duration > 5.05 {
		t.Fatalf("时长解析错误: %+v", info)
	}
	if !info.HasAudio {
		t.Fatal("带音轨的回显应被识别为有声")
	}

	silent := `Duration: 00:01:12.50, start: 0.000000, bitrate: 900 kb/s
  Stream #0:0[0x1](und): Video: h264 (High), yuv420p(progressive), 1080x1920 [SAR 1:1 DAR 9:16], 24 fps`
	info = parseFFmpegMetadata(silent)
	if info.Width != 1080 || info.Height != 1920 || info.HasAudio {
		t.Fatalf("无声竖屏解析错误: %+v", info)
	}
	if info.Duration != 72.5 {
		t.Fatalf("分秒换算错误: %v", info.Duration)
	}
}

func TestSubtitleRenderUsesASSColorAndPlayRes(t *testing.T) {
	if got := hexToASSColor("#FFB43A"); got != "&H003AB4FF" {
		t.Fatalf("ASS 颜色转换错误: %s", got)
	}
	cfg, _, _ := normalizeVideoConfig(VideoRunConfig{Topic: "测试"})
	cues := []VideoSubtitleCue{{Index: 1, Start: 0, End: 2.5, Text: "第一行\n第二行"}}
	ass := renderASS(cues, cfg, 1280, 720)
	if !strings.Contains(ass, "PlayResX: 1280") || !strings.Contains(ass, "PlayResY: 720") {
		t.Fatalf("ASS 缺少 PlayRes: %s", ass)
	}
	if !strings.Contains(ass, `0:00:00.00,0:00:02.50`) {
		t.Fatalf("ASS 时间轴格式错误: %s", ass)
	}
	if !strings.Contains(ass, `第一行\N第二行`) {
		t.Fatalf("ASS 断行未转换: %s", ass)
	}
	srt := renderSRT(cues)
	if !strings.Contains(srt, "00:00:00,000 --> 00:00:02,500") {
		t.Fatalf("SRT 时间轴格式错误: %s", srt)
	}
}

func TestVideoTaskLifecycleAndInvalidation(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "一杯咖啡唤醒清晨的城市", DurationS: 8, Adapter: "mock",
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if created.Title == "" || created.Stage != VideoStageDraftScript || created.Status != VideoStatusDraft {
		t.Fatalf("任务初始状态不符合预期: %+v", created)
	}

	list, err := app.ListVideoTasks()
	if err != nil || len(list) != 1 {
		t.Fatalf("任务列表异常: %v %+v", err, list)
	}

	// 模拟已产出剧本与下游产物，验证确认门①改剧本后的最小重算闭包
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageConfirmScript
	task.Status = VideoStatusWaiting
	task.Run.Script = &VideoScript{Shots: []VideoShot{{ID: "s1", DurationS: 8, SceneIntent: "原始"}}}
	task.Run.Prompts = []VideoPromptItem{{ShotID: "s1", VisualPrompt: "旧提示词"}}
	task.Run.Renders = []VideoRender{{SegmentID: "s1-1", Status: "succeeded"}}
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	edited, err := app.SaveVideoStageEdits(task.ID, VideoGateEdits{
		Shots: []VideoShot{{ID: "s1", DurationS: 8, SceneIntent: "修改后的画面意图", Dialogue: "新台词"}},
	})
	if err != nil {
		t.Fatalf("保存分镜修改失败: %v", err)
	}
	if edited.Run.Prompts != nil || edited.Run.Renders != nil || edited.Run.Output != nil {
		t.Fatalf("剧本修改后下游产物应失效: %+v", edited.Run)
	}
	if edited.Run.Script == nil || edited.Run.Script.Shots[0].SceneIntent != "修改后的画面意图" {
		t.Fatalf("剧本修改未生效: %+v", edited.Run.Script)
	}
	if edited.Run.Gates[VideoGateScript] != videoGatePending {
		t.Fatalf("确认门①应回到 pending: %v", edited.Run.Gates)
	}
	if edited.Status != VideoStatusWaiting {
		t.Fatalf("修改后应停留在确认关卡: %s", edited.Status)
	}

	if err := app.DeleteVideoTask(task.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	if list, err := app.ListVideoTasks(); err != nil || len(list) != 0 {
		t.Fatalf("删除后任务列表应为空: %v %+v", err, list)
	}
}

func TestPlatformProfilesNormalize(t *testing.T) {
	ids := normalizePlatforms([]string{"Douyin", " douyin ", "unknown", "bilibili", "xiaohongshu", "kuaishou"})
	if len(ids) != 4 {
		t.Fatalf("平台多选应去重并限制为 4 个: %v", ids)
	}
	if ids[0] != "douyin" || ids[1] != "bilibili" {
		t.Fatalf("平台归一化顺序错误: %v", ids)
	}
	for _, profile := range platformProfiles() {
		if profile.TitleMaxLen <= 0 || profile.HashtagMax <= 0 || len(profile.PeakHours) == 0 {
			t.Fatalf("平台档位不完整: %+v", profile)
		}
	}
	if platformProfile("not-exist").ID != "generic" {
		t.Fatal("未知平台应回落到通用默认档")
	}
	if normalizeRegion("Mars/Base") != "Asia/Shanghai" {
		t.Fatal("非法时区应回落到 Asia/Shanghai")
	}
}

func TestLocalPublishMetaRespectsPlatformProfile(t *testing.T) {
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{
		Topic:     "3步做出焦糖布丁，第2步最关键",
		DurationS: 12,
		Platforms: []string{"douyin"},
		Vertical:  "美食",
	})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	script := normalizeScript(localVideoScript(cfg), cfg)
	prompts, _ := (&App{}).generatePrompts(t.Context(), cfg, script, defaultVideoCapabilities(), "")
	profile := platformProfile(primaryPlatform(cfg))

	meta := localPublishMeta(cfg, &script, prompts, profile)
	finalizePublishMeta(meta, cfg, profile)

	if meta.Platform != "douyin" || meta.PlatformLabel != "抖音" {
		t.Fatalf("平台档位未生效: %+v", meta)
	}
	if meta.Title.Limit != profile.TitleMaxLen {
		t.Fatalf("标题上限应来自平台档位: %d", meta.Title.Limit)
	}
	if meta.Title.Primary == "" || meta.Title.CharCount != runeLen(meta.Title.Primary) {
		t.Fatalf("标题字数统计错误: %q (%d)", meta.Title.Primary, meta.Title.CharCount)
	}
	if meta.Title.CharCount > meta.Title.Limit {
		t.Fatalf("标题超过平台上限: %d > %d", meta.Title.CharCount, meta.Title.Limit)
	}
	if len(meta.Tags.Hashtags) == 0 || len(meta.Tags.Hashtags) > profile.HashtagMax {
		t.Fatalf("话题数量超出档位: %d", len(meta.Tags.Hashtags))
	}
	for _, tag := range meta.Tags.Hashtags {
		if !strings.HasPrefix(tag, "#") {
			t.Fatalf("话题应带 # 前缀: %q", tag)
		}
	}
	if meta.Tags.Counts.Hashtag != len(meta.Tags.Hashtags) || meta.Tags.Counts.Keyword != len(meta.Tags.Keywords) {
		t.Fatalf("标签计数不一致: %+v", meta.Tags.Counts)
	}
	if meta.PublishTime.Timezone != "Asia/Shanghai" || len(meta.PublishTime.Candidates) == 0 {
		t.Fatalf("发布时段异常: %+v", meta.PublishTime)
	}
	if meta.Cover.Ratio != cfg.AspectRatio || meta.Cover.FrameTS <= 0 {
		t.Fatalf("封面契约异常: %+v", meta.Cover)
	}
	if meta.Description == "" {
		t.Fatal("发布描述不应为空")
	}
}

func TestPublishTimeUsesAudienceTimezoneAndVertical(t *testing.T) {
	profile := platformProfile("douyin")
	cfg := VideoRunConfig{Platforms: []string{"douyin"}, Region: "Asia/Shanghai", Vertical: "美食", DurationS: 10}
	now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))

	pt := buildPublishTime(profile, cfg, now)
	if len(pt.Candidates) == 0 || len(pt.Candidates) > 3 {
		t.Fatalf("时段候选应为 1-3 个: %+v", pt.Candidates)
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	first, err := time.ParseInLocation("2006-01-02 15:04", pt.Candidates[0].Slot, loc)
	if err != nil {
		t.Fatalf("时段格式错误: %q", pt.Candidates[0].Slot)
	}
	if !first.After(now) {
		t.Fatalf("不应给出已过去的时段: %s", pt.Candidates[0].Slot)
	}
	verticalHit := false
	for _, candidate := range pt.Candidates {
		if strings.Contains(candidate.Reason, "饭点") {
			verticalHit = true
		}
	}
	if !verticalHit {
		t.Fatalf("美食垂类应修正出饭点时段: %+v", pt.Candidates)
	}
	if !strings.Contains(pt.BufferNote, "审核") {
		t.Fatalf("应提示审核缓冲: %q", pt.BufferNote)
	}
}

func TestPublishEditsClampAndSelectSlot(t *testing.T) {
	meta := &PublishMeta{
		Platform: "douyin",
		PublishTime: PublishTime{Candidates: []PublishTimeCandidate{
			{Slot: "2026-11-02 12:00", Reason: "午休", Score: 0.8},
			{Slot: "2026-11-02 19:30", Reason: "晚高峰", Score: 0.9},
		}},
	}
	coverTS := 2.5
	applyPublishEdits(meta, VideoPublishEdit{
		Title:    strings.Repeat("长", 60),
		Hashtags: []string{"#测试", "测试", "咖啡", "城市", "清晨", "光影", "多余"},
		Keywords: []string{"k1", "k1", "k2", "k3", "k4", "k5", "k6"},
		Slot:     "2026-11-02 19:30",
		CoverTS:  &coverTS,
	}, VideoRunConfig{DurationS: 10})

	if got := runeLen(meta.Title.Primary); got > 30 {
		t.Fatalf("标题应被平台档位截断: %d", got)
	}
	if len(meta.Tags.Hashtags) != 5 || meta.Tags.Hashtags[0] != "#测试" || meta.Tags.Hashtags[1] != "#咖啡" {
		t.Fatalf("话题应去重并按档位截断: %v", meta.Tags.Hashtags)
	}
	if meta.PublishTime.Candidates[0].Slot != "2026-11-02 19:30" || meta.PublishTime.Candidates[0].Score != 1 {
		t.Fatalf("选定时段应置顶: %+v", meta.PublishTime.Candidates)
	}
	if meta.Cover.FrameTS != 2.5 {
		t.Fatalf("封面帧时间未生效: %.2f", meta.Cover.FrameTS)
	}
	if meta.Tags.Counts.Hashtag != 5 || meta.Tags.Counts.Keyword != 5 {
		t.Fatalf("标签计数应重算: %+v", meta.Tags.Counts)
	}
}

func TestConfirmMetaGateCompletesTask(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "发布元数据确认关卡", DurationS: 8, Adapter: "mock", Platforms: []string{"xiaohongshu"},
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageConfirmMeta
	task.Status = VideoStatusWaiting
	task.Run.Script = &VideoScript{Shots: []VideoShot{{ID: "s1", DurationS: 8, SceneIntent: "画面"}}}
	task.Run.PublishMeta = &PublishMeta{Platform: "xiaohongshu", PlatformLabel: "小红书"}
	ensureVideoGates(&task.Run)
	task.Run.Gates[VideoGateMeta] = videoGatePending
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	if err := app.ApproveVideoGate(created.ID); err != nil {
		t.Fatalf("确认 gate③ 失败: %v", err)
	}
	final, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if final.Stage != VideoStageDone || final.Status != VideoStatusDone {
		t.Fatalf("确认后任务应完成: %s / %s", final.Stage, final.Status)
	}
	if final.Run.Gates[VideoGateMeta] != videoGateApproved {
		t.Fatalf("gate③ 应为 approved: %v", final.Run.Gates)
	}
}

func TestPublishMetaInvalidatedByScriptEdit(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "分镜改动会让发布元数据失效", DurationS: 8, Adapter: "mock", Platforms: []string{"douyin"},
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageConfirmScript
	task.Status = VideoStatusWaiting
	task.Run.Script = &VideoScript{Shots: []VideoShot{{ID: "s1", DurationS: 8, SceneIntent: "原始"}}}
	task.Run.PublishMeta = &PublishMeta{Platform: "douyin", PlatformLabel: "抖音"}
	ensureVideoGates(&task.Run)
	task.Run.Gates[VideoGateMeta] = videoGateApproved
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	edited, err := app.SaveVideoStageEdits(task.ID, VideoGateEdits{
		Shots: []VideoShot{{ID: "s1", DurationS: 8, SceneIntent: "改过的画面", Dialogue: "新台词"}},
	})
	if err != nil {
		t.Fatalf("保存分镜失败: %v", err)
	}
	if edited.Run.PublishMeta != nil {
		t.Fatalf("分镜改动后发布元数据应失效: %+v", edited.Run.PublishMeta)
	}
	if edited.Run.Gates[VideoGateMeta] != videoGatePending {
		t.Fatalf("gate③ 应回到 pending: %v", edited.Run.Gates)
	}
}

func TestLocalScriptCoversTotalDuration(t *testing.T) {
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "海边日出，慢镜头，旅人剪影", DurationS: 16})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	script := normalizeScript(localVideoScript(cfg), cfg)
	total := 0.0
	for _, shot := range script.Shots {
		total += shot.DurationS
	}
	if len(script.Shots) == 0 {
		t.Fatal("本地规则应至少生成一个分镜")
	}
	if total < float64(cfg.DurationS)-0.2 || total > float64(cfg.DurationS)+0.2 {
		t.Fatalf("分镜时长之和应等于目标时长，实际 %.2f / %d", total, cfg.DurationS)
	}
	prompts, _ := (&App{}).generatePrompts(t.Context(), cfg, script, defaultVideoCapabilities(), "")
	if len(prompts) != len(script.Shots) {
		t.Fatalf("提示词数量应与分镜一致: %d / %d", len(prompts), len(script.Shots))
	}
}

// 字幕默认关闭：未显式开启时归一化结果仍为关闭。
func TestSubtitlesDefaultOff(t *testing.T) {
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "字幕默认关闭", DurationS: 8})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if cfg.Subtitles {
		t.Fatal("字幕应默认关闭")
	}

	opened, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "显式开启字幕", DurationS: 8, Subtitles: true})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if !opened.Subtitles {
		t.Fatal("显式开启后应保留字幕开关")
	}
}

// 出镜主体统一约束：三段 Agent 提示词、落到视频模型的提示词、以及用户改写后的提示词都要带上动物主体要求。
func TestAnimalCastDirectiveAppliedEverywhere(t *testing.T) {
	for name, prompt := range map[string]string{
		"分镜":   scriptSystemPrompt,
		"提示词":  promptSystemPrompt,
		"单段重写": segmentPromptSuggestSystemPrompt,
	} {
		if !strings.Contains(prompt, animalCastDirective) {
			t.Fatalf("%s 的 system 提示词缺少动物主体约束", name)
		}
	}

	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "橘猫开咖啡馆", DurationS: 12})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	script := normalizeScript(localVideoScript(cfg), cfg)
	prompts, _ := (&App{}).generatePrompts(t.Context(), cfg, script, defaultVideoCapabilities(), "")
	if len(prompts) == 0 {
		t.Fatal("提示词不应为空")
	}
	for _, prompt := range prompts {
		if !strings.Contains(prompt.VisualPrompt, animalCastDirective) {
			t.Fatalf("本地兜底提示词缺少动物主体约束: %q", prompt.VisualPrompt)
		}
	}

	// 用户在确认关卡改写提示词后仍要补齐，且重复提交不叠加
	edited := []VideoPromptItem{{
		ShotID:       prompts[0].ShotID,
		VisualPrompt: "固定机位，柴犬推开店门",
		Start:        prompts[0].Start,
		End:          prompts[0].End,
	}}
	submission := []VideoPromptEdit{{ShotID: edited[0].ShotID, VisualPrompt: "固定机位，柴犬推开店门"}}
	applyPromptEdits(edited, submission, cfg)
	applyPromptEdits(edited, []VideoPromptEdit{{ShotID: edited[0].ShotID, VisualPrompt: edited[0].VisualPrompt}}, cfg)
	if count := strings.Count(edited[0].VisualPrompt, animalCastDirective); count != 1 {
		t.Fatalf("动物主体约束应只出现一次，实际 %d 次: %q", count, edited[0].VisualPrompt)
	}
}

// 角色参考图不能在分镜没标角色时静默消失：那是“角色图没被视频模型参考”的头号成因。
func TestCharacterImagesForSegmentNeverSilentlyEmpty(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "host", Name: "主理人（橘猫）", Images: []string{"host.png", ""}},
		{RoleID: "guest", Name: "嘉宾（边牧）", Images: []string{"guest.png"}},
	}}
	script := &VideoScript{Shots: []VideoShot{
		{ID: "s1", Roles: nil},
		{ID: "s2", Roles: []string{"主理人"}},
		{ID: "s3", Roles: []string{"unknown_role"}},
	}}

	// 分镜没写 roles → 全部带上 + 可读警告
	images, warning := characterImagesForSegment(cfg, script, VideoSegment{ShotID: "s1", Index: 0})
	if len(images) != 2 || warning == "" {
		t.Fatalf("无 roles 时应附全部参考图并告警: %d 张 / %q", len(images), warning)
	}
	if images[0].RoleID != "host" || images[0].Name != "主理人（橘猫）" || images[0].URL != "host.png" {
		t.Fatalf("参考图必须带角色身份，否则无法按「图片N」绑定: %+v", images[0])
	}

	// 模型只写了角色名（不是 role_id）→ 仍要匹配上且不告警
	if images, warning = characterImagesForSegment(cfg, script, VideoSegment{ShotID: "s2", Index: 1}); len(images) != 1 || warning != "" {
		t.Fatalf("按角色名应匹配到单一角色: %d 张 / %q", len(images), warning)
	}

	// role_id 完全对不上 → 退化带上全部，不让一致性静默失效
	if images, warning = characterImagesForSegment(cfg, script, VideoSegment{ShotID: "s3", Index: 2}); len(images) != 2 || warning == "" {
		t.Fatalf("角色表对不上时应兜底: %d 张 / %q", len(images), warning)
	}

	// 没配角色参考时依旧为空（不能凭空编造图）
	if images, warning := characterImagesForSegment(VideoRunConfig{}, script, VideoSegment{ShotID: "s1"}); images != nil || warning != "" {
		t.Fatalf("无参考图配置时应返回空: %+v / %q", images, warning)
	}
}

// 用户上传的定妆图排在模板生成图前面：方舟参考图有张数上限，靠后的会被截掉，
// 用户亲自认定的形象不能死于顺序。
func TestCharacterImagesForSegmentPutsUserUploadsFirst(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "guest", Name: "嘉宾（边牧）", Images: []string{"tpl-guest.png"}, ImageSource: CharacterImageSourceTemplate},
		{RoleID: "host", Name: "主理人（橘猫）", Images: []string{"user-host.png"}, ImageSource: CharacterImageSourceUser},
	}}
	script := &VideoScript{Shots: []VideoShot{
		{ID: "s1", Roles: []string{"guest", "host"}},
		{ID: "s2", Roles: nil},
	}}

	images, warning := characterImagesForSegment(cfg, script, VideoSegment{ShotID: "s1", Index: 0})
	if warning != "" || len(images) != 2 {
		t.Fatalf("两个角色都应匹配上: %d 张 / %q", len(images), warning)
	}
	if images[0].Source != CharacterImageSourceUser || images[0].URL != "user-host.png" {
		t.Fatalf("用户上传的图应排在最前: %+v", images)
	}

	// 分镜没标角色的兜底路径同样要按优先级排序
	if images, _ = characterImagesForSegment(cfg, script, VideoSegment{ShotID: "s2", Index: 1}); len(images) != 2 || images[0].Source != CharacterImageSourceUser {
		t.Fatalf("兜底附全部时也要优先上传图: %+v", images)
	}
}

// 文字外观锚点要让位给上传图：模板写“成年橘猫”而用户传的是黑猫时，必须以图为准。
func TestRoleUsesUserImages(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "host", Name: "主理人（橘猫）", Images: []string{"user-host.png"}, ImageSource: CharacterImageSourceUser},
		{RoleID: "guest", Name: "嘉宾（边牧）", Images: []string{"tpl-guest.png"}, ImageSource: CharacterImageSourceTemplate},
		{RoleID: "empty", Name: "没图的角色", ImageSource: CharacterImageSourceUser},
	}}
	if !roleUsesUserImages(cfg, "主理人") || !roleUsesUserImages(cfg, "host") {
		t.Fatal("用户上传了图的角色应识别为上传图优先")
	}
	if roleUsesUserImages(cfg, "guest") || roleUsesUserImages(cfg, "empty") || roleUsesUserImages(cfg, "unknown") {
		t.Fatal("模板图 / 没图 / 不存在的角色不应识别为上传图")
	}
}

// 没标来源的历史配置（手填、老任务）一律按用户上传处理；已标 template 的要保留，否则优先权会被误给。
func TestNormalizeVideoConfigKeepsCharacterImageSource(t *testing.T) {
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "周末手冲", CharacterRefs: []VideoCharacterRef{
		{RoleID: "host", Name: "主理人", Images: []string{"https://example.com/host.png"}},
		{RoleID: "guest", Name: "嘉宾", Images: []string{"https://example.com/guest.png"}, ImageSource: "TEMPLATE"},
	}})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if len(cfg.CharacterRefs) != 2 {
		t.Fatalf("应有两个角色: %+v", cfg.CharacterRefs)
	}
	if cfg.CharacterRefs[0].ImageSource != CharacterImageSourceUser {
		t.Fatalf("未标来源应视为用户上传: %+v", cfg.CharacterRefs[0])
	}
	if cfg.CharacterRefs[1].ImageSource != CharacterImageSourceTemplate {
		t.Fatalf("模板来源应原样保留: %+v", cfg.CharacterRefs[1])
	}
}

/* ------------------------------------------------------------------
   人工返工意见：关卡上敲的话必须真的进模型输入
   ------------------------------------------------------------------ */

// feedbackStub 起一个假的对话模型：记下每次收到的 user 文本，返回指定的模型输出。
func feedbackStub(t *testing.T, modelContent string) (*httptest.Server, *[]string) {
	t.Helper()

	users := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		for _, message := range body.Messages {
			if message.Role != "user" {
				continue
			}
			switch text := message.Content.(type) {
			case string:
				*users = append(*users, text)
			default:
				// 带图片的多模态请求：content 是数组，把所有 text 块拼起来
				var builder strings.Builder
				if parts, ok := message.Content.([]any); ok {
					for _, part := range parts {
						if block, ok := part.(map[string]any); ok {
							if piece, ok := block["text"].(string); ok {
								builder.WriteString(piece)
							}
						}
					}
				}
				*users = append(*users, builder.String())
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": modelContent}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, string(payload))
	}))
	t.Cleanup(server.Close)

	return server, users
}

func feedbackTestConfig() VideoRunConfig {
	return VideoRunConfig{Topic: "小猫打哈欠", DurationS: 6, Language: "zh", Adapter: "mock", Platforms: []string{"douyin"}}
}

func feedbackTestScript() VideoScript {
	return VideoScript{Logline: "小猫打哈欠", Language: "zh", Shots: []VideoShot{{
		ID: "s1-1", TimeRange: [2]float64{0, 6}, DurationS: 6, SceneIntent: "开场", Dialogue: "喵", Camera: "固定", Mood: "好奇",
	}}}
}

func feedbackTestPrompts() []VideoPromptItem {
	return []VideoPromptItem{{ShotID: "s1-1", VisualPrompt: "小猫对着镜头缓慢转头", Dialogue: "喵", RenderDurationS: 6}}
}

// lastUserPrompt 取最近一次发给模型的 user 文本。
func lastUserPrompt(t *testing.T, users *[]string) string {
	t.Helper()
	if len(*users) == 0 {
		t.Fatal("没有发出任何模型请求")
	}
	return (*users)[len(*users)-1]
}

// 三个关卡上的「重新生成意见」必须拼进对应 Agent 的输入：只写进 task.Run.Feedback 而没人读，
// 就等于让用户对着空气提意见。
func TestFeedbackReachesEveryAgent(t *testing.T) {
	const opinion = "第 2 个镜头推进太快，改成缓慢横移"
	cfg := feedbackTestConfig()
	script := feedbackTestScript()
	prompts := feedbackTestPrompts()

	agents := []struct {
		name    string
		content string // 模型返回的 JSON，形状按各 Agent 的输出契约
		run     func(app *App, feedback string) []string
	}{
		{
			name:    "Agent1 编剧",
			content: `{"logline":"小猫打哈欠","language":"zh","shots":[{"id":"s1-1","timeRange":[0,6],"durationS":6,"sceneIntent":"开场","dialogue":"喵","camera":"固定","mood":"好奇"}]}`,
			run: func(app *App, feedback string) []string {
				_, warnings := app.generateScript(context.Background(), cfg, feedback)
				return warnings
			},
		},
		{
			name:    "Agent2 提示词",
			content: `{"prompts":[{"shotId":"s1-1","visualPrompt":"缓慢横移镜头，小猫对着镜头打哈欠","dialogue":"喵"}]}`,
			run: func(app *App, feedback string) []string {
				_, warnings := app.generatePrompts(context.Background(), cfg, script, defaultVideoCapabilities(), feedback)
				return warnings
			},
		},
		{
			name:    "Agent4 发布元数据",
			content: `{"title":{"primary":"小猫打哈欠的 3 个瞬间","alternatives":[],"hook_type":"数字清单"},"tags":{"hashtags":["小猫"],"keywords":["猫咪日常"]},"description":"小猫打哈欠","category":"宠物","cover":{"text_overlay":"打哈欠"}}`,
			run: func(app *App, feedback string) []string {
				_, warnings := app.generatePublishMeta(context.Background(), cfg, &script, prompts, feedback)
				return warnings
			},
		},
	}

	for _, agent := range agents {
		t.Run(agent.name, func(t *testing.T) {
			server, users := feedbackStub(t, agent.content)
			app := newTestApp(t)
			if _, err := app.SaveProvider(Provider{
				Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model",
			}); err != nil {
				t.Fatalf("登记提供商失败: %v", err)
			}

			warnings := strings.Join(agent.run(app, opinion), "\n")
			// 模型正常返回了可用结果，就不该冒出「没生效」：这条能拦住“失败路径默默回退”
			if strings.Contains(warnings, "未生效") {
				t.Fatalf("模型已返回可用结果，不应提示意见没生效：%s", warnings)
			}
			sent := lastUserPrompt(t, users)
			if !strings.Contains(sent, "用户返工意见") || !strings.Contains(sent, opinion) {
				t.Fatalf("意见没拼进模型输入：\n%s", sent)
			}

			// 没填意见时不该凭空多出这一段，免得模型把空意见当成指令
			agent.run(app, "   ")
			if strings.Contains(lastUserPrompt(t, users), "用户返工意见") {
				t.Fatal("空意见不应拼进提示词")
			}
		})
	}
}

// 本地规则读不懂自然语言意见：回退时必须明确说“没生效”，
// 不能让用户看到“已重新生成”就以为 AI 听了话。
func TestFeedbackDroppedWarningOnLocalFallback(t *testing.T) {
	app := newTestApp(t) // 没登记任何提供商：三个 Agent 都会走本地规则
	cfg := feedbackTestConfig()
	script := feedbackTestScript()

	fallbacks := []struct {
		name string
		run  func(feedback string) []string
	}{
		{"Agent1 编剧", func(feedback string) []string {
			_, warnings := app.generateScript(context.Background(), cfg, feedback)
			return warnings
		}},
		{"Agent2 提示词", func(feedback string) []string {
			_, warnings := app.generatePrompts(context.Background(), cfg, script, defaultVideoCapabilities(), feedback)
			return warnings
		}},
		{"Agent4 发布元数据", func(feedback string) []string {
			_, warnings := app.generatePublishMeta(context.Background(), cfg, &script, feedbackTestPrompts(), feedback)
			return warnings
		}},
	}

	for _, fallback := range fallbacks {
		if warnings := strings.Join(fallback.run("镜头2 太赶"), "\n"); !strings.Contains(warnings, "返工意见未生效") {
			t.Fatalf("%s 回退本地规则时应告知意见没生效：%s", fallback.name, warnings)
		}
		if warnings := strings.Join(fallback.run(""), "\n"); strings.Contains(warnings, "未生效") {
			t.Fatalf("%s 没提意见时不该冒出这句：%s", fallback.name, warnings)
		}
	}
}

// 意见只能被按下「重生成」的那个阶段消费：渲染阶段会顺带预生成发布元数据，
// 针对画面的意见不该被 Agent4 拿去改标题。
func TestConsumeFeedbackScopedToOwnerStage(t *testing.T) {
	task := &VideoTask{ID: "t-feedback", Run: VideoRunContext{Feedback: "整体偏冷调"}}
	runner := &videoRunner{task: task, feedbackStage: VideoStageBuildPrompt}

	if got := runner.consumeFeedback(VideoStageBuildMeta); got != "" {
		t.Fatal("别的阶段不该读到意见")
	}
	if task.Run.Feedback != "整体偏冷调" {
		t.Fatalf("没消费时不该清空：%q", task.Run.Feedback)
	}
	if got := runner.consumeFeedback(VideoStageBuildPrompt); got != "整体偏冷调" {
		t.Fatalf("归属阶段应拿到意见，实际 %q", got)
	}
	if task.Run.Feedback != "" {
		t.Fatalf("消费后应清空，免得下一轮莫名继承：%q", task.Run.Feedback)
	}
	if got := runner.consumeFeedback(VideoStageBuildPrompt); got != "" {
		t.Fatal("同一份意见不该被消费两次")
	}
	if !strings.Contains(strings.Join(task.Run.Logs, "\n"), "本轮按人工返工意见重出：整体偏冷调") {
		t.Fatalf("日志要留下意见原文便于核对：%v", task.Run.Logs)
	}
}

// 端到端：提示词关卡点「重新生成」时敲的意见，要出现在真正发出的模型请求里，
// 并从任务上消费掉，同时不影响提示词落库。
func TestRegeneratePromptStageSendsFeedbackToModel(t *testing.T) {
	withTempVideoRoot(t)

	const opinion = "整体色调改暖，主体动作更克制"
	server, users := feedbackStub(t, `{"prompts":[{"shotId":"s1-1","visualPrompt":"暖色逆光，小猫缓慢抬头","dialogue":"喵"}]}`)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{
		Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model",
	}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}

	created, err := app.CreateVideoTask(VideoTaskInput{Config: feedbackTestConfig()})
	if err != nil {
		t.Fatalf("新建任务失败: %v", err)
	}
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageConfirmPrompt
	script := feedbackTestScript()
	task.Run.Script = &script
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	app.runVideoPipeline(created.ID, VideoStageBuildPrompt, opinion)

	if sent := lastUserPrompt(t, users); !strings.Contains(sent, opinion) {
		t.Fatalf("意见没进模型输入：\n%s", sent)
	}

	final, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if final.Status == VideoStatusFailed {
		t.Fatalf("流水线失败：%s", final.Error)
	}
	if final.Run.Feedback != "" {
		t.Fatalf("意见应被消费掉：%q", final.Run.Feedback)
	}
	if final.Stage != VideoStageConfirmPrompt {
		t.Fatalf("提示词阶段完成后应停在关卡②，实际 %s", final.Stage)
	}
	if len(final.Run.Prompts) == 0 || !strings.Contains(final.Run.Prompts[0].VisualPrompt, "暖色逆光") {
		t.Fatalf("模型给的提示词应落到任务上：%+v", final.Run.Prompts)
	}
}

/* ------------------------------------------------------------------
   用户参数契约：表单上能填的每一项都必须真的进模型输入
   ------------------------------------------------------------------ */

// fullParamConfig 把新建任务表单能填的参数全填上（含本地首帧路径）。
func fullParamConfig(firstFrame string) VideoRunConfig {
	return VideoRunConfig{
		Topic:            "橘猫开咖啡馆",
		DurationS:        15,
		Fps:              30,
		AspectRatio:      "9:16",
		ContinuationMode: "last_frame",
		SpeechRate:       5.2,
		Language:         "zh-CN",
		Subtitles:        true,
		FirstFrame:       firstFrame,
		CharacterRefs: []VideoCharacterRef{{
			RoleID: "host", Name: "主理人（橘猫）", Images: []string{"host.png"},
			ImageSource: CharacterImageSourceUser, VoiceHint: "低沉男声",
		}},
		VideoParams: VideoParams{
			Style: "暖色逆光实拍", Camera: "缓慢推近", Mood: "克制期待",
			ExtraPrompt: "桌面永远有一只搪瓷杯", NegativePrompt: "真人面孔、文字水印",
		},
		Platforms: []string{"douyin", "xiaohongshu"},
		Region:    "Asia/Shanghai",
		Vertical:  "美食",
		Adapter:   "mock",
	}
}

// multimodalStub 同时记录 user 文本与“本次请求带了几张图”：用来验证首帧图真的作为附图交给了 Agent。
func multimodalStub(t *testing.T, modelContent string) (*httptest.Server, *[]string, *[]bool) {
	t.Helper()

	users := &[]string{}
	withImage := &[]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		hasImage := false
		for _, message := range body.Messages {
			parts, ok := message.Content.([]any)
			if !ok {
				continue
			}
			var builder strings.Builder
			for _, part := range parts {
				block, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] == "image_url" {
					hasImage = true
				}
				if piece, ok := block["text"].(string); ok {
					builder.WriteString(piece)
				}
			}
			*users = append(*users, builder.String())
		}
		*withImage = append(*withImage, hasImage)
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": modelContent}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, string(payload))
	}))
	t.Cleanup(server.Close)

	return server, users, withImage
}

// 表单上填的每一个参数都要出现在对应 Agent 的输入里：只存在于 cfg 结构体的参数，等于没填。
func TestUserParamsReachBothAgents(t *testing.T) {
	firstFrame := filepath.Join(t.TempDir(), "first.png")
	if err := os.WriteFile(firstFrame, []byte("fake-png"), 0o644); err != nil {
		t.Fatalf("写首帧失败: %v", err)
	}
	cfg := fullParamConfig(firstFrame)

	// 两段 Agent 必须看到同一套参数
	expect := []string{
		"主题：橘猫开咖啡馆",
		"目标总时长：15 秒",
		"画幅：9:16",
		"语言：zh-CN；语速预算：5.2 字/秒",
		"主平台 抖音",
		"垂类 美食",
		"受众时区 Asia/Shanghai",
		"风格偏好：暖色逆光实拍",
		"运镜偏好：缓慢推近",
		"情绪偏好：克制期待",
		"必须落实的额外要求：桌面永远有一只搪瓷杯",
		"必须全片避开的元素（用正面表述达成）：真人面孔、文字水印",
		"续接方式：尾帧续接",
		"已提供首帧图",
		"host（主理人（橘猫））",
		"形象：用户上传的定妆图",
		"声音：低沉男声",
		"字幕：后期烧录",
	}

	t.Run("Agent1 编剧", func(t *testing.T) {
		server, users := feedbackStub(t, `{"logline":"橘猫开咖啡馆","shots":[{"id":"s1","durationS":15,"roles":["host"],"sceneIntent":"推门","dialogue":"开业"}]}`)
		app := newTestApp(t)
		if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
			t.Fatalf("登记提供商失败: %v", err)
		}
		app.generateScript(context.Background(), cfg, "")

		sent := lastUserPrompt(t, users)
		for _, want := range expect {
			if !strings.Contains(sent, want) {
				t.Fatalf("Agent1 输入缺参数 %q：\n%s", want, sent)
			}
		}
		// 本地绝对路径不进模型输入：模型读不懂，也没必要发给第三方
		if strings.Contains(sent, firstFrame) || strings.Contains(sent, ".png") {
			t.Fatalf("首帧路径不应泄漏进提示词：\n%s", sent)
		}
	})

	t.Run("Agent2 提示词", func(t *testing.T) {
		server, users, withImage := multimodalStub(t, `{"prompts":[{"shotId":"s1-1","visualPrompt":"推镜，橘猫推门","dialogue":"喵"}]}`)
		app := newTestApp(t)
		if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
			t.Fatalf("登记提供商失败: %v", err)
		}
		app.generatePrompts(context.Background(), cfg, feedbackTestScript(), defaultVideoCapabilities(), "")

		sent := lastUserPrompt(t, users)
		for _, want := range expect {
			if !strings.Contains(sent, want) {
				t.Fatalf("Agent2 输入缺参数 %q：\n%s", want, sent)
			}
		}
		// 两张图的信息也要给到：模型能力与角色表
		for _, want := range []string{"本段模型能力：", "角色表：", "分镜（JSON，shotId 必须逐字复用）"} {
			if !strings.Contains(sent, want) {
				t.Fatalf("Agent2 输入缺 %q：\n%s", want, sent)
			}
		}
		if strings.Contains(sent, firstFrame) {
			t.Fatalf("首帧路径不应泄漏进提示词：\n%s", sent)
		}
		if len(*withImage) == 0 || !(*withImage)[len(*withImage)-1] {
			t.Fatalf("首帧图应作为附图随 Agent2 请求发出：%v", *withImage)
		}
	})
}

// 用户关掉本地兜底后不许偷偷降级：没模型就失败，别拿本地模板顶上假装是 AI 写的。
func TestLocalFallbackSwitchRespected(t *testing.T) {
	disabled := false
	cfg := feedbackTestConfig()
	cfg.LocalFallback = &disabled

	app := newTestApp(t) // 没登记任何文本模型
	script, warnings := app.generateScript(context.Background(), cfg, "")
	if len(script.Shots) != 0 {
		t.Fatalf("关闭本地兜底后不该产出本地分镜：%+v", script.Shots)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "已关闭本地兜底") {
		t.Fatalf("关闭本地兜底应明确告知原因：%v", warnings)
	}

	prompts, promptWarnings := app.generatePrompts(context.Background(), cfg, feedbackTestScript(), defaultVideoCapabilities(), "")
	if len(prompts) != 0 || !strings.Contains(strings.Join(promptWarnings, "\n"), "已关闭本地兜底") {
		t.Fatalf("关闭本地兜底后不应产出本地提示词：%v / %v", prompts, promptWarnings)
	}

	// 默认值（nil）仍然兜底，旧行为不变
	enabled, _ := app.generateScript(context.Background(), feedbackTestConfig(), "")
	if len(enabled.Shots) == 0 {
		t.Fatal("默认设置下仍应使用本地规则兜底")
	}
}

// 模型漏写段落（或 shotId 写错）时，那几段会静默退回本地模板：必须打出告警。
func TestMissingShotWarningWhenModelSkipsSegment(t *testing.T) {
	cfg := feedbackTestConfig()
	script := feedbackTestScript()
	script.Shots = append(script.Shots, VideoShot{ID: "s1-2", TimeRange: [2]float64{6, 12}, DurationS: 6, SceneIntent: "收尾", Dialogue: ""})

	server, _ := feedbackStub(t, `{"prompts":[{"shotId":"s1-1","visualPrompt":"推镜，小猫回头","dialogue":"喵"}]}`)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}

	_, warnings := app.generatePrompts(context.Background(), cfg, script, defaultVideoCapabilities(), "")
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "只覆盖 1/2 段") || !strings.Contains(joined, "s1-2") {
		t.Fatalf("漏段必须告警：%v", warnings)
	}
}

/* ------------------------------------------------------------------
   台词强绑定：每句台词必须有确定的说话人（角色或旁白）
   ------------------------------------------------------------------ */

func speakerTestConfig() VideoRunConfig {
	return VideoRunConfig{
		Topic: "橘猫开咖啡馆", DurationS: 12, AspectRatio: "9:16", Language: "zh-CN", SpeechRate: 4,
		CharacterRefs: []VideoCharacterRef{
			{RoleID: "host", Name: "主理人（橘猫）", Images: []string{"host.png"}},
			{RoleID: "guest", Name: "客人（边牧）", Images: []string{"guest.png"}},
		},
		Adapter: "mock",
	}
}

// 说话人写法要归一：role_id、角色名、名字片段都该落到 role_id；猜不准就落到旁白并说清楚。
func TestSpeakerBindingNormalization(t *testing.T) {
	cfg := speakerTestConfig()
	script := VideoScript{Shots: []VideoShot{
		{ID: "s1", Roles: []string{"host"}, Dialogue: "今日开业", Speaker: "主理人"},
		{ID: "s2", Roles: []string{"host", "guest"}, Dialogue: "欢迎光临"},
		{ID: "s3", Roles: []string{"guest"}, Dialogue: "", Speaker: "guest"},
		{ID: "s4", Roles: []string{"guest"}, Dialogue: "谢谢，下次再来", Speaker: "隔壁老王"},
		{ID: "s5", Roles: nil, Dialogue: "三个月后…", Speaker: "旁白"},
	}}

	bound, warnings := bindSpeakers(script, cfg)
	joined := strings.Join(warnings, "\n")

	if got := bound.Shots[0].Speaker; got != "host" {
		t.Fatalf("角色名应归一成 role_id，得到 %q", got)
	}
	if got := bound.Shots[1].Speaker; got != VideoSpeakerNarration {
		t.Fatalf("多角色且未指定说话人，应落到旁白，得到 %q", got)
	}
	if !strings.Contains(joined, "s2") || !strings.Contains(joined, "旁白") {
		t.Fatalf("多角色猜不准必须告警：%v", warnings)
	}
	if got := bound.Shots[2].Speaker; got != "" {
		t.Fatalf("没台词的段落不该有说话人，得到 %q", got)
	}
	if got := bound.Shots[3].Speaker; got != "guest" {
		t.Fatalf("说话人不在角色表但本段只有一个角色，应归他，得到 %q", got)
	}
	if !strings.Contains(joined, "隔壁老王") {
		t.Fatalf("降级要给出来源：%v", warnings)
	}
	if got := bound.Shots[4].Speaker; got != VideoSpeakerNarration {
		t.Fatalf("“旁白”应归一成 narration，得到 %q", got)
	}
}

// 归属要一路带到渲染提示词：角色说话就把口型给到他，旁白就让画面别安排人对口型。
func TestDialogueSpeakerReachesRenderPrompt(t *testing.T) {
	cfg := speakerTestConfig()
	script := VideoScript{Shots: []VideoShot{
		{ID: "s1", TimeRange: [2]float64{0, 6}, DurationS: 6, Roles: []string{"host"}, SceneIntent: "橘猫推开店门", Dialogue: "今日开业"},
		{ID: "s2", TimeRange: [2]float64{6, 12}, DurationS: 6, Roles: nil, SceneIntent: "空镜收尾", Dialogue: "三个月过去了"},
	}}

	caps := defaultVideoCapabilities()
	caps.NativeAudio = true
	prompts, warnings := (&App{}).generatePrompts(context.Background(), cfg, script, caps, "")
	if len(prompts) != 2 {
		t.Fatalf("应有两条提示词：%d / %v", len(prompts), warnings)
	}
	if prompts[0].Speaker != "host" || prompts[1].Speaker != VideoSpeakerNarration {
		t.Fatalf("台词归属没落到提示词上：%+v", prompts)
	}

	plan := buildVideoPlan(cfg, prompts, caps, "mock")
	if len(plan.Segments) < 2 {
		t.Fatalf("渲染计划段数不够：%+v", plan.Segments)
	}
	if plan.Segments[0].SpeakerName != "主理人（橘猫）" {
		t.Fatalf("角色台词应带显示名，得到 %q", plan.Segments[0].SpeakerName)
	}

	first := composeRenderPrompt(plan.Segments[0], caps)
	if !strings.Contains(first, "主理人（橘猫） 说") || !strings.Contains(first, "今日开业") {
		t.Fatalf("角色台词没绑定到说话人：\n%s", first)
	}

	narration := composeRenderPrompt(plan.Segments[1], caps)
	if !strings.Contains(narration, "旁白（画外音") || strings.Contains(narration, "说（只允许") {
		t.Fatalf("旁白不该要求主体对口型：\n%s", narration)
	}
}

// 说话人即使没写进 roles，也要带上他的定妆图：台词归他、画面却没参考过他，脸一定翻。
func TestSpeakerRoleBringsCharacterImage(t *testing.T) {
	cfg := speakerTestConfig()
	script := &VideoScript{Shots: []VideoShot{{ID: "s1", Roles: nil}}}
	images, warning := characterImagesForSegment(cfg, script, VideoSegment{ShotID: "s1", Speaker: "guest"})
	if len(images) != 1 || images[0].RoleID != "guest" || warning != "" {
		t.Fatalf("说话人的定妆图应被带上：%+v / %q", images, warning)
	}
}

// 关卡②里改台词归属要真的生效，清空台词则归属一并清空。
func TestPromptEditorCanRebindSpeaker(t *testing.T) {
	cfg := speakerTestConfig()
	prompts := []VideoPromptItem{{ShotID: "s1", VisualPrompt: "推镜", Dialogue: "谢谢惠顾", Speaker: "host"}}

	applyPromptEdits(prompts, []VideoPromptEdit{{ShotID: "s1", VisualPrompt: "推镜", Dialogue: "谢谢惠顾", Speaker: "画外音"}}, cfg)
	if prompts[0].Speaker != VideoSpeakerNarration {
		t.Fatalf("改成旁白应归一成 narration，得到 %q", prompts[0].Speaker)
	}

	applyPromptEdits(prompts, []VideoPromptEdit{{ShotID: "s1", VisualPrompt: "推镜", Dialogue: "谢谢惠顾", Speaker: "客人"}}, cfg)
	if prompts[0].Speaker != "guest" {
		t.Fatalf("按角色名改归属应归一成 role_id，得到 %q", prompts[0].Speaker)
	}

	applyPromptEdits(prompts, []VideoPromptEdit{{ShotID: "s1", VisualPrompt: "推镜", Dialogue: ""}}, cfg)
	if prompts[0].Speaker != "" {
		t.Fatalf("台词清空后归属应跟着清空，得到 %q", prompts[0].Speaker)
	}
}
