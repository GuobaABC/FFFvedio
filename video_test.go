package main

import (
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
	prompts, _ := (&App{}).generatePrompts(t.Context(), cfg, script, defaultVideoCapabilities())
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
	prompts, _ := (&App{}).generatePrompts(t.Context(), cfg, script, defaultVideoCapabilities())
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
