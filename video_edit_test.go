package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v0.1.1 的任务里 gate_subs 已批准，改名后不能又变回待确认。
func TestEnsureVideoGatesMigratesLegacySubsGate(t *testing.T) {
	run := VideoRunContext{Gates: map[string]string{"gate_subs": videoGateApproved}}
	ensureVideoGates(&run)
	if run.Gates[VideoGateEdit] != videoGateApproved {
		t.Fatalf("旧 gate_subs 状态应迁移到 gate_edit：%v", run.Gates)
	}
	if _, ok := run.Gates[VideoGateEdit]; !ok {
		t.Fatalf("迁移后必须存在 gate_edit：%v", run.Gates)
	}
}

// 尾帧延续模式下片段顺序由生成链路决定，用户只能裁剪与启停。
func TestNormalizeVideoEditClipsRejectsReorderInLastFrameMode(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{}
	for _, id := range []string{"a", "b"} {
		path := filepath.Join(dir, id+".mp4")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("写入测试文件失败: %v", err)
		}
		paths[id] = path
	}
	run := &VideoRunContext{
		Renders: []VideoRender{
			{SegmentID: "a", VideoURI: paths["a"], Status: "succeeded"},
			{SegmentID: "b", VideoURI: paths["b"], Status: "succeeded"},
		},
	}
	cfg := VideoRunConfig{ContinuationMode: "last_frame"}
	clips := []VideoEditClip{
		{SegmentID: "b", Order: 0, Enabled: true, InPoint: 0, OutPoint: 2, SourceDurationS: 2},
		{SegmentID: "a", Order: 1, Enabled: true, InPoint: 0, OutPoint: 2, SourceDurationS: 2},
	}
	if _, _, err := normalizeVideoEditClips(context.Background(), cfg, run, clips, cfg.ContinuationMode == "last_frame"); err == nil {
		t.Fatal("last_frame 模式下改变顺序应被拒绝")
	}

	clips = []VideoEditClip{
		{SegmentID: "a", Order: 0, Enabled: true, InPoint: 0.5, OutPoint: 1.5, SourceDurationS: 2},
		{SegmentID: "b", Order: 1, Enabled: false, InPoint: 0, OutPoint: 2, SourceDurationS: 2},
	}
	normalized, _, err := normalizeVideoEditClips(context.Background(), cfg, run, clips, true)
	if err != nil {
		t.Fatalf("裁剪与禁用应被允许: %v", err)
	}
	if normalized[0].InPoint != 0.5 || normalized[0].OutPoint != 1.5 || normalized[1].Enabled {
		t.Fatalf("裁剪/启停结果被改写：%+v", normalized)
	}
	if !videoEditChanged(&VideoEditPlan{Clips: []VideoEditClip{{SegmentID: "a", Enabled: true}}}, &VideoEditPlan{Clips: []VideoEditClip{{SegmentID: "b", Enabled: true}}}) {
		t.Fatal("片段顺序变化应判定为剪辑版本变化")
	}
	if videoEditChanged(&VideoEditPlan{Clips: clips}, &VideoEditPlan{Clips: clips}) {
		t.Fatal("完全相同的剪辑计划不应变化")
	}
}

// 入出点规范化：负入点归零、0 出点视为片尾、过短窗口拒绝。
func TestClipWindowNormalizesPoints(t *testing.T) {
	in, out, ok := clipWindow(VideoEditClip{InPoint: -2, OutPoint: 0}, 5)
	if !ok || in != 0 || out != 5 {
		t.Fatalf("负入点/零出点应规范为整段：%.2f-%.2f ok=%v", in, out, ok)
	}
	in, out, ok = clipWindow(VideoEditClip{InPoint: 4.9, OutPoint: 5}, 5)
	if !ok || out-in < minEditClipS-1e-9 {
		t.Fatalf("入点应夹到至少保留 %.2fs：%.2f-%.2f", minEditClipS, in, out)
	}
	if _, _, ok := clipWindow(VideoEditClip{InPoint: 4, OutPoint: 3}, 5); ok {
		t.Fatal("倒流入出点应判非法")
	}
}

// 片段顺序变化后，字幕按段内偏移跟随移动；被禁用片段的字幕一并移除。
func TestRelayoutSubtitleCuesFollowsSegmentOrder(t *testing.T) {
	cues := []VideoSubtitleCue{
		{Index: 1, Start: 1, End: 2, Text: "A1", SegmentID: "a", OffsetS: 1},
		{Index: 2, Start: 5.5, End: 6.5, Text: "B1", SegmentID: "b", OffsetS: 0.5},
		{Index: 3, Start: 0, End: 1, Text: "C1", SegmentID: "c", OffsetS: 0},
	}
	newSpans := map[string]SubtitleSpan{
		"b": {SegmentID: "b", Start: 0, Duration: 3},
		"a": {SegmentID: "a", Start: 3, Duration: 5},
	}
	out, warnings := relayoutSubtitleCues(cues, nil, newSpans, 8, VideoRunConfig{
		SubtitleStyle: defaultVideoSubtitleStyle(),
	})
	if len(out) != 2 {
		t.Fatalf("禁用片段的字幕应被移除：%+v（warnings=%v）", out, warnings)
	}
	if out[0].SegmentID != "b" || out[0].Start != 0.5 {
		t.Fatalf("B 段字幕应按段内偏移移动：%+v", out[0])
	}
	if out[1].SegmentID != "a" || out[1].Start != 4 {
		t.Fatalf("A 段字幕应跟随新起点：%+v", out[1])
	}
	if len(warnings) == 0 {
		t.Fatal("移除字幕要给用户明确提示")
	}
}

// 剪辑版本变化必须让预处理缓存失效，否则会烧旧画布。
func TestComposePrepUsableChecksEditRevision(t *testing.T) {
	dir := t.TempDir()
	canvas := filepath.Join(dir, "canvas.mp4")
	clip := filepath.Join(dir, "a.mp4")
	for _, path := range []string{canvas, clip} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("写入测试文件失败: %v", err)
		}
	}
	run := &VideoRunContext{
		Edit: &VideoEditPlan{Revision: 3, Clips: []VideoEditClip{{SegmentID: "a", Enabled: true, SourceDurationS: 2}}},
		Renders: []VideoRender{
			{SegmentID: "a", VideoURI: clip, Status: "succeeded"},
		},
	}
	prep := &VideoComposePrep{
		CanvasPath:        canvas,
		ComposedBySegment: map[string]string{"a": clip},
		SegmentIDs:        []string{"a"},
		EditRevision:      2,
		DurationS:         2,
		HasFFmpeg:         true,
	}
	if composePrepUsable(prep, run) {
		t.Fatal("剪辑版本不一致时应判定缓存不可用")
	}
	prep.EditRevision = 3
	if !composePrepUsable(prep, run) {
		t.Fatal("版本一致且文件齐全时应复用缓存")
	}
}

// 用户文本里的 {} 不能被 ASS 当成控制标签吞掉。
func TestASSEscapeTextKeepsBraces(t *testing.T) {
	escaped := assEscapeText("{测试}ABC \\ raw\n第二行")
	if strings.ContainsAny(escaped, "{}") {
		t.Fatalf("花括号应转成全角：%q", escaped)
	}
	if !strings.Contains(escaped, "｛测试｝ABC") || !strings.Contains(escaped, `\N`) {
		t.Fatalf("文本内容或换行标记丢失：%q", escaped)
	}
	if strings.Contains(escaped, `\ raw`) {
		t.Fatalf("反斜杠应转义，避免被 ASS 解释：%q", escaped)
	}
}

// 长句换行必须保留所有字符，不能再像旧实现那样只留前两行。
func TestWrapSubtitleTextKeepsEveryRune(t *testing.T) {
	text := strings.Repeat("这是一句很长的中文字幕", 10)
	wrapped := wrapSubtitleText(text, "zh-CN")
	if strings.ReplaceAll(wrapped, "\n", "") != text {
		t.Fatalf("换行后字符丢失：%q", wrapped)
	}
	if lines := strings.Split(wrapped, "\n"); len(lines) < 3 {
		t.Fatalf("长句应拆成多行：%d 行", len(lines))
	}
}

// 校验口径：空文本与越界是 error，语速/行宽只是 warning。
func TestValidateVideoSubtitlesLevels(t *testing.T) {
	cfg := VideoRunConfig{Language: "zh-CN", SubtitleStyle: defaultVideoSubtitleStyle()}
	cues := []VideoSubtitleCue{
		{Index: 1, Start: 0, End: 2, Text: "正常"},
		{Index: 2, Start: 3, End: 4, Text: "  "},
		{Index: 3, Start: 11, End: 12, Text: "越界"},
	}
	issues := ValidateVideoSubtitles(cues, 10, cfg)
	if !hasSubtitleErrors(issues) {
		t.Fatalf("空文本与越界应产生 error：%+v", issues)
	}
	if hasSubtitleErrors(nil) {
		t.Fatal("没有问题时 hasSubtitleErrors 应为 false")
	}
}

// 内置字体必须能解压出来，跨机器不依赖系统安装。
func TestEnsureSubtitleFontDirExtractsBundledFont(t *testing.T) {
	withTempVideoRoot(t)
	dir, err := ensureSubtitleFontDir()
	if err != nil {
		t.Fatalf("解压内置字体失败: %v", err)
	}
	fontPath := filepath.Join(dir, subtitleFontFileName)
	info, err := os.Stat(fontPath)
	if err != nil {
		t.Fatalf("字体文件不存在: %v", err)
	}
	if info.Size() < 1_000_000 {
		t.Fatalf("字体文件异常小：%d 字节", info.Size())
	}
	if !subtitleFontAvailable("Noto Sans SC") {
		t.Fatal("内置 Noto Sans SC 应始终判定可用")
	}
}

// 音色只写给支持原生音频的模型；不支持时不能凭空承诺音色。
func TestVoiceProfileOnlyInNativeAudioPrompt(t *testing.T) {
	rate, pitch := 1.2, -1.5
	cfg := VideoRunConfig{
		Language: "zh-CN",
		CharacterRefs: []VideoCharacterRef{
			{RoleID: "hero", Name: "主角", VoiceHint: "低沉男声",
				VoiceProfile: &VideoVoiceProfile{VoiceID: "deep-male", Rate: rate, Pitch: pitch, Volume: 1, Emotion: "克制"}},
		},
	}
	lines := []VideoDialogueLine{{Speaker: "hero", Text: "我回来了。"}}
	native := dialoguePromptBlock(cfg, lines, VideoCapabilities{NativeAudio: true})
	if !strings.Contains(native, "音色") || !strings.Contains(native, "deep-male") || !strings.Contains(native, "低沉男声") {
		t.Fatalf("原生音频提示词应包含音色线索：%s", native)
	}
	nonNative := dialoguePromptBlock(cfg, lines, VideoCapabilities{NativeAudio: false})
	if strings.Contains(nonNative, "的音色：") {
		t.Fatalf("非原生音频模型不应写音色：%s", nonNative)
	}
}

// 全流程冒烟：视频生成后必须经过 BUILD_EDIT / CONFIRM_EDIT 才能合成，最终产出成片。
func TestFullPipelineRunsThroughEditStage(t *testing.T) {
	bundledFFmpeg(t)
	withTempVideoRoot(t)
	app := newTestApp(t)

	reviewOff := false
	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "小猫的一天", DurationS: 4, Adapter: "mock", AspectRatio: "16:9", SegmentReview: &reviewOff,
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := app.RunVideoTask(created.ID); err != nil {
		t.Fatalf("启动任务失败: %v", err)
	}

	waitForVideoTask(t, app, created.ID, func(task VideoTask) bool {
		return task.Stage == VideoStageConfirmScript
	}, "应进入分镜确认关卡")
	waitForVideoTaskIdle(t, app, created.ID, "分镜确认关卡")
	if err := app.ApproveVideoGate(created.ID); err != nil {
		t.Fatalf("确认分镜失败: %v", err)
	}

	waitForVideoTask(t, app, created.ID, func(task VideoTask) bool {
		return task.Stage == VideoStageConfirmPrompt
	}, "应进入提示词确认关卡")
	waitForVideoTaskIdle(t, app, created.ID, "提示词确认关卡")
	if err := app.ApproveVideoGate(created.ID); err != nil {
		t.Fatalf("确认提示词失败: %v", err)
	}

	edited := waitForVideoTask(t, app, created.ID, func(task VideoTask) bool {
		return task.Stage == VideoStageConfirmEdit || task.Stage == VideoStageDone || task.Status == VideoStatusFailed
	}, "生成视频后应进入剪辑确认关卡")
	if edited.Status == VideoStatusFailed {
		t.Fatalf("流水线失败: %s", edited.Error)
	}
	if edited.Stage != VideoStageConfirmEdit {
		t.Fatalf("应停在 CONFIRM_EDIT，实际 %s/%s", edited.Stage, edited.Status)
	}
	if edited.Run.Edit == nil || len(edited.Run.Edit.Clips) == 0 {
		t.Fatalf("缺少剪辑计划：%+v", edited.Run.Edit)
	}
	if edited.Run.Compose == nil || edited.Run.Compose.CanvasUrl == "" || edited.Run.Compose.DurationS <= 0 {
		t.Fatalf("缺少可预览画布：%+v", edited.Run.Compose)
	}
	if edited.Run.Subtitles == nil || len(edited.Run.Subtitles.Cues) == 0 {
		t.Fatalf("默认开启字幕时应产出 cue：%+v", edited.Run.Subtitles)
	}
	cue := edited.Run.Subtitles.Cues[0]
	if cue.SegmentID == "" {
		t.Fatalf("自动字幕应带上归属片段，剪辑重排才能跟随：%+v", cue)
	}

	waitForVideoTaskIdle(t, app, created.ID, "剪辑确认关卡")
	if err := app.ApproveVideoGate(created.ID); err != nil {
		t.Fatalf("确认剪辑失败: %v", err)
	}
	meta := waitForVideoTask(t, app, created.ID, func(task VideoTask) bool {
		return task.Stage == VideoStageConfirmMeta || task.Status == VideoStatusFailed
	}, "合成后应进入发布确认关卡")
	if meta.Status == VideoStatusFailed {
		t.Fatalf("合成失败: %s", meta.Error)
	}
	if meta.Run.Output == nil || !fileExists(meta.Run.Output.FinalPath) {
		t.Fatalf("合成后应产出成片：%+v", meta.Run.Output)
	}
	if !meta.Run.Output.BurnSubtitles {
		t.Fatalf("默认字幕应烧进成片：%+v", meta.Run.Output)
	}

	waitForVideoTaskIdle(t, app, created.ID, "发布确认关卡")
	if err := app.ApproveVideoGate(created.ID); err != nil {
		t.Fatalf("确认发布失败: %v", err)
	}
	done := waitForVideoTask(t, app, created.ID, func(task VideoTask) bool {
		return task.Stage == VideoStageDone
	}, "确认发布后应完成")
	if done.Status != VideoStatusDone {
		t.Fatalf("任务应以 done 收尾：%s", done.Status)
	}
	time.Sleep(50 * time.Millisecond)
}

// 「丢弃草稿并重新生成」必须真的清掉剪辑/画布/字幕，而不是只把阶段切回 BUILD_EDIT 后沿用原草稿。
func TestRegenerateEditDiscardsDraft(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	withTempVideoRoot(t)
	app := newTestApp(t)

	created, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "丢弃剪辑草稿", DurationS: 2, Adapter: "mock", Subtitles: true, AspectRatio: "16:9",
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	task.Stage = VideoStageBuildEdit
	task.Run.Script = &VideoScript{Shots: []VideoShot{{ID: "s1", DurationS: 2, Dialogue: "重来一次。"}}}
	task.Run.Prompts = []VideoPromptItem{{ShotID: "s1", VisualPrompt: "推镜", Dialogue: "重来一次。", Start: 0, End: 2}}
	plan := buildVideoPlan(task.Config, task.Run.Prompts, defaultVideoCapabilities(), "mock")
	task.Run.Plan = &plan
	workDir, err := app.ensureTaskWorkDir(created.ID)
	if err != nil {
		t.Fatalf("创建任务目录失败: %v", err)
	}
	segmentID := plan.Segments[0].ID
	clip := filepath.Join(workDir, "clips", "01-"+segmentID+".mp4")
	if err := os.MkdirAll(filepath.Dir(clip), 0o755); err != nil {
		t.Fatalf("创建片段目录失败: %v", err)
	}
	makeTestClip(t, ffmpeg, clip, "1280x720", 2)
	task.Run.Renders = []VideoRender{{SegmentID: segmentID, ShotID: "s1", VideoURI: clip, Status: "succeeded", DurationS: 2}}
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	app.runVideoPipeline(created.ID, VideoStageBuildEdit, "")
	ready := waitForVideoTask(t, app, created.ID, func(item VideoTask) bool {
		return item.Stage == VideoStageConfirmEdit && item.Status == VideoStatusWaiting
	}, "应先进入剪辑确认关卡")
	waitForVideoTaskIdle(t, app, created.ID, "剪辑确认关卡")
	if ready.Run.Edit == nil || ready.Run.Compose == nil || ready.Run.Subtitles == nil {
		t.Fatalf("剪辑关卡产物不完整：%+v", ready.Run)
	}

	// 模拟用户草稿：裁剪片段、改字幕文本并抬高版本号
	ready.Run.Edit.Clips[0].InPoint = 0.5
	ready.Run.Edit.Clips[0].OutPoint = 1.5
	ready.Run.Edit.Revision = 9
	ready.Run.Subtitles.Cues = []VideoSubtitleCue{{Index: 1, Start: 0, End: 1, Text: "用户手工字幕"}}
	if err := app.updateVideoTaskRecord(ready); err != nil {
		t.Fatalf("写入用户草稿失败: %v", err)
	}

	if err := app.RegenerateVideoStage(created.ID, ""); err != nil {
		t.Fatalf("重新生成失败: %v", err)
	}
	fresh := waitForVideoTask(t, app, created.ID, func(item VideoTask) bool {
		return item.Stage == VideoStageConfirmEdit && item.Status == VideoStatusWaiting &&
			item.Run.Edit != nil && item.Run.Edit.Revision != 9
	}, "重生成后应回到剪辑关卡并重置计划")
	if fresh.Run.Edit.Clips[0].InPoint != 0 || fresh.Run.Edit.Clips[0].OutPoint != fresh.Run.Edit.Clips[0].SourceDurationS {
		t.Fatalf("重生成应回到整段保留，实际入出点 %.2f/%.2f（源 %.2f）",
			fresh.Run.Edit.Clips[0].InPoint, fresh.Run.Edit.Clips[0].OutPoint, fresh.Run.Edit.Clips[0].SourceDurationS)
	}
	if fresh.Run.Compose == nil || fresh.Run.Compose.CanvasUrl == "" {
		t.Fatalf("重生成后应重建预览画布：%+v", fresh.Run.Compose)
	}
	for _, cue := range fresh.Run.Subtitles.Cues {
		if strings.Contains(cue.Text, "用户手工字幕") {
			t.Fatalf("重生成后不应保留旧字幕草稿：%+v", fresh.Run.Subtitles.Cues)
		}
	}
	if logs := strings.Join(fresh.Run.Logs, "\n"); !strings.Contains(logs, "已丢弃剪辑草稿") {
		t.Fatalf("日志要说明草稿已丢弃：%v", fresh.Run.Logs)
	}
}
