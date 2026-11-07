package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 合成阶段的两个兜底：画幅统一到任务设定、字幕时间轴跟随片段实测时长。
// 依赖仓库内置的 ffmpeg（scripts/fetch-deps-windows.ps1 下载），没有就跳过。
func bundledFFmpeg(t *testing.T) string {
	t.Helper()
	candidate, err := filepath.Abs(filepath.Join("build", "deps", "bin", "ffmpeg"+exeSuffix()))
	if err != nil || !fileExists(candidate) {
		t.Skip("仓库内没有内置 ffmpeg，跳过合成实测")
	}
	t.Setenv("FFFVedio_FFMPEG", candidate)
	return candidate
}

// makeTestClip 生成指定尺寸、带音轨的测试片段。
func makeTestClip(t *testing.T, ffmpeg, path, size string, seconds int) {
	t.Helper()
	args := []string{
		"-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%s:rate=24:duration=%d", size, seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", path,
	}
	if err := runCommand(context.Background(), ffmpeg, args...); err != nil {
		t.Fatalf("生成测试片段失败: %v", err)
	}
}

func TestComposeAlignsSubtitlesToMeasuredClips(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	dir := t.TempDir()
	// 计划每段 3s，实际渲染各 2s：字幕第二段必须落在 2s 附近，而不是计划里的 3s
	clipA := filepath.Join(dir, "01-a.mp4")
	clipB := filepath.Join(dir, "02-b.mp4")
	makeTestClip(t, ffmpeg, clipA, "1920x1080", 2)
	makeTestClip(t, ffmpeg, clipB, "1920x1080", 2)

	plan := &VideoPlan{Segments: []VideoSegment{
		{ID: "a", Start: 0, End: 3, Dialogue: "第一句很短。第二句也在这段。"},
		{ID: "b", Start: 3, End: 6, Dialogue: "第三句在下一段"},
		{ID: "c", Start: 6, End: 9, Dialogue: "这段渲染失败不该占时间轴"},
	}}
	renders := []VideoRender{
		{SegmentID: "a", VideoURI: clipA, Status: "succeeded"},
		{SegmentID: "b", VideoURI: clipB, Status: "succeeded"},
		{SegmentID: "c", VideoURI: filepath.Join(dir, "missing.mp4"), Status: "failed"},
	}

	spans, err := measuredSubtitleSpans(context.Background(), plan, renders, nil)
	if err != nil {
		t.Fatalf("按实测时长建时间轴失败: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("失败段不该进时间轴: %+v", spans)
	}
	if spans[1].Start < 1.6 || spans[1].Start > 2.4 {
		t.Fatalf("第二段起点应跟随第一段实际时长（约 2s），实际 %.2f", spans[1].Start)
	}
	if !spans[0].FillWindow {
		t.Fatal("带音轨的片段应标记为按窗口铺开字幕")
	}

	// 冻帧裁剪后实际入片的是更短的文件：字幕测时必须跟着裁剪后的时长走
	clipShort := filepath.Join(dir, "01-short.mp4")
	makeTestClip(t, ffmpeg, clipShort, "1920x1080", 1)
	trimmedSpans, err := measuredSubtitleSpans(context.Background(), plan, renders, map[string]string{"a": clipShort})
	if err != nil {
		t.Fatalf("按裁剪后文件建时间轴失败: %v", err)
	}
	if trimmedSpans[1].Start < 0.6 || trimmedSpans[1].Start > 1.4 {
		t.Fatalf("第一段被裁短后，第二段起点应跟着变成约 1s，实际 %.2f", trimmedSpans[1].Start)
	}

	cfg, _, _ := normalizeVideoConfig(VideoRunConfig{Topic: "测试", Subtitles: true, AspectRatio: "9:16"})
	cues := buildSubtitleCuesFromSpans(cfg, spans)
	if len(cues) < 3 {
		t.Fatalf("台词应逐句铺开，实际 %d 条: %+v", len(cues), cues)
	}
	if cues[len(cues)-1].End > 4.1 {
		t.Fatalf("字幕越过了成片总时长: %+v", cues[len(cues)-1])
	}
}

func TestComposeNormalizesAspectToTargetCanvas(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "src.mp4")
	makeTestClip(t, ffmpeg, source, "1920x1080", 2)

	scaled := filepath.Join(dir, "scaled.mp4")
	width, height, applied, err := normalizeAspectCanvas(context.Background(), source, scaled, "9:16")
	if err != nil {
		t.Fatalf("画幅归一失败: %v", err)
	}
	if !applied || width != 1080 || height != 1920 {
		t.Fatalf("横屏素材应归一为 1080x1920，实际 %dx%d applied=%v", width, height, applied)
	}
	info, err := mediaProbe(context.Background(), scaled)
	if err != nil {
		t.Fatalf("回读归一结果失败: %v", err)
	}
	if info.Width != 1080 || info.Height != 1920 {
		t.Fatalf("成片仍是 %dx%d，没有按任务画幅出片", info.Width, info.Height)
	}
	if !info.HasAudio {
		t.Fatal("归一后音轨丢了，字幕同步前提就没了")
	}

	// 已经符合画幅时不该再重编码一次
	if _, _, stillNeeded, _ := normalizeAspectCanvas(context.Background(), scaled, filepath.Join(dir, "again.mp4"), "9:16"); stillNeeded {
		t.Fatal("画幅一致时不应重复归一")
	}
}

// makeColorClip 生成一段纯色测试片段（无音轨）。
func makeColorClip(t *testing.T, ffmpeg, path, color string, seconds float64) {
	t.Helper()
	args := []string{
		"-y", "-f", "lavfi", "-i", fmt.Sprintf("color=c=%s:s=320x240:r=24:d=%g", color, seconds),
		"-c:v", "libx264", "-crf", "12", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path,
	}
	if err := runCommand(context.Background(), ffmpeg, args...); err != nil {
		t.Fatalf("生成纯色片段失败: %v", err)
	}
}

// 接缝冻帧裁剪：尾帧延续后下一段开头重放上一段尾帧（SSIM>0.995 视为还在冻），
// 这些帧应删掉；一旦降到阈值以下说明运动恢复，从该帧起保留。
func TestTrimFrozenClipHeadsDropsReplayedTailFrames(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	workDir := filepath.Join(dir, "task")
	outputDir := filepath.Join(workDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}

	prev := filepath.Join(workDir, "s1-1.mp4")
	makeColorClip(t, ffmpeg, prev, "red", 1)

	// 模拟服务商行为：段 2 开头 5 帧纯红（重放上一段尾帧），后 3 帧纯蓝（真实运动恢复）
	framesDir := filepath.Join(dir, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		t.Fatalf("创建帧目录失败: %v", err)
	}
	for _, spec := range []struct{ name, color string }{{"red.png", "red"}, {"blue.png", "blue"}} {
		path := filepath.Join(framesDir, spec.name)
		if err := runCommand(ctx, ffmpeg, "-y", "-f", "lavfi", "-i",
			fmt.Sprintf("color=c=%s:s=320x240:d=0.05", spec.color), "-frames:v", "1", path); err != nil {
			t.Fatalf("生成纯色帧失败: %v", err)
		}
	}
	redRaw, err := os.ReadFile(filepath.Join(framesDir, "red.png"))
	if err != nil {
		t.Fatal(err)
	}
	blueRaw, err := os.ReadFile(filepath.Join(framesDir, "blue.png"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 8; index++ {
		content := blueRaw
		if index <= 5 {
			content = redRaw
		}
		if err := os.WriteFile(filepath.Join(framesDir, fmt.Sprintf("f%03d.png", index)), content, 0o644); err != nil {
			t.Fatalf("写入帧序列失败: %v", err)
		}
	}
	head := filepath.Join(workDir, "s1-2.mp4")
	if err := runCommand(ctx, ffmpeg, "-y", "-framerate", "24", "-i", filepath.Join(framesDir, "f%03d.png"),
		"-c:v", "libx264", "-crf", "12", "-preset", "ultrafast", "-pix_fmt", "yuv420p", head); err != nil {
		t.Fatalf("合成冻帧片段失败: %v", err)
	}

	clips := []composeClip{{SegmentID: "s1-1", Path: prev}, {SegmentID: "s1-2", Path: head}}
	paths, composed, warnings := trimFrozenClipHeads(ctx, ffmpeg, workDir, outputDir, clips, 24)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "开头 5 帧") || !strings.Contains(joined, "冻帧") {
		t.Fatalf("应报告删了 5 帧冻帧，实际警告: %v", warnings)
	}
	if paths[1] == head || composed["s1-2"] != paths[1] {
		t.Fatalf("第二段应被替换为裁剪后文件: %v %+v", paths, composed)
	}

	// 裁剪后只剩 3 帧蓝（≈0.125s），不该把运动恢复后的内容也删掉
	info, err := mediaProbe(ctx, paths[1])
	if err != nil {
		t.Fatalf("回读裁剪结果失败: %v", err)
	}
	if info.Duration < 0.04 || info.Duration > 0.25 {
		t.Fatalf("裁剪后应只剩约 0.125s，实际 %.3fs", info.Duration)
	}

	// 首帧就不冻（上一段蓝、本段红）时不应产生裁剪
	prevBlue := filepath.Join(workDir, "s2-1.mp4")
	makeColorClip(t, ffmpeg, prevBlue, "blue", 1)
	headRed := filepath.Join(workDir, "s2-2.mp4")
	makeColorClip(t, ffmpeg, headRed, "red", 1)
	cleanPaths, cleanComposed, cleanWarnings := trimFrozenClipHeads(ctx, ffmpeg, workDir, outputDir,
		[]composeClip{{SegmentID: "s2-1", Path: prevBlue}, {SegmentID: "s2-2", Path: headRed}}, 24)
	if cleanPaths[1] != headRed || cleanComposed["s2-2"] != headRed {
		t.Fatalf("无冻帧时不应裁剪: %v %v", cleanPaths, cleanWarnings)
	}
}

// 段尾冻帧裁剪：分镜被 nearestAllowedDuration 夹到模型档位后，多出的空窗常被静止帧收尾填充，
// 这些帧应删（末帧保留收尾）；整段静止的（定格画面属创作内容）绝不删；
// 计划时间轴要按实际入片时长修正，但档位值保留给重渲提交。
func TestTrimFrozenClipTailsAndPlanSync(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("创建输出目录失败: %v", err)
	}

	// 前 1s 红（真实内容）+ 后 0.5s 蓝（段尾静止填充：档位多出的时长）
	frozen := filepath.Join(dir, "tail-frozen.mp4")
	if err := runCommand(ctx, ffmpeg, "-y",
		"-f", "lavfi", "-i", "color=c=red:s=320x240:r=24:d=1",
		"-f", "lavfi", "-i", "color=c=blue:s=320x240:r=24:d=0.5",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0",
		"-c:v", "libx264", "-crf", "12", "-preset", "ultrafast", "-pix_fmt", "yuv420p", frozen); err != nil {
		t.Fatalf("生成尾部冻帧片段失败: %v", err)
	}
	// 整段静止：不该被当成“可裁的填充”删空
	still := filepath.Join(dir, "all-still.mp4")
	makeColorClip(t, ffmpeg, still, "blue", 1)

	clips := []composeClip{{SegmentID: "t1-1", Path: frozen}, {SegmentID: "t1-2", Path: still}}
	paths, composed, warnings := trimFrozenClipTails(ctx, ffmpeg, outputDir, clips, 24)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "第 1 段") || !strings.Contains(joined, "已裁掉") {
		t.Fatalf("应报告裁掉第一段段尾冻帧，实际警告: %v", warnings)
	}
	if !strings.Contains(joined, "整段静止") {
		t.Fatalf("整段静止应提示按创作内容保留: %v", warnings)
	}
	if paths[0] == frozen || composed["t1-1"] != paths[0] {
		t.Fatalf("第一段应被替换为裁剪后文件: %v %+v", paths, composed)
	}
	if paths[1] != still || composed["t1-2"] != still {
		t.Fatalf("整段静止的不应被裁剪: %v", paths)
	}

	// 尾部 12 帧蓝里裁掉与末帧相同的部分、留着末帧：应剩约 1.0~1.1s
	info, err := mediaProbe(ctx, paths[0])
	if err != nil {
		t.Fatalf("回读裁剪结果失败: %v", err)
	}
	if info.Duration < 0.9 || info.Duration > 1.2 {
		t.Fatalf("裁剪后应剩约 1.04s，实际 %.3fs", info.Duration)
	}

	// 档位夹取的计划段长（10s）按实测入片时长修正，DurationS 保留档位值供重渲提交
	plan := &VideoPlan{Segments: []VideoSegment{
		{ID: "t1-1", Start: 0, End: 10, DurationS: 10},
		{ID: "t1-2", Start: 10, End: 15, DurationS: 5},
	}}
	run := &VideoRunContext{Plan: plan}
	if msgs := syncPlanWithComposedDurations(ctx, run, composed); len(msgs) == 0 {
		t.Fatal("计划时间轴与实际时长不一致时应给出修正提示")
	}
	if plan.Segments[0].End < 0.9 || plan.Segments[0].End > 1.2 {
		t.Fatalf("第一段终点应跟实测入片时长（约 1.04s），实际 %.2f", plan.Segments[0].End)
	}
	if math.Abs(plan.Segments[1].Start-plan.Segments[0].End) > 0.01 {
		t.Fatalf("第二段起点应紧跟上一段终点: %.2f / %.2f", plan.Segments[1].Start, plan.Segments[0].End)
	}
	if plan.Segments[0].DurationS != 10 || plan.Segments[1].DurationS != 5 {
		t.Fatalf("档位值是重渲提交依据，不得改写: %+v", plan.Segments)
	}
	if plan.TotalS < 1.9 || plan.TotalS > 2.3 {
		t.Fatalf("总时长应为实测两段之和（约 2.0s），实际 %.2f", plan.TotalS)
	}
}

// 拼接前的参数归一：方舟首帧任务按素材自身比例出段，尺寸不齐的段直接 -c copy 硬拼
// 不会报错，产物却会在接缝处定格——必须先统一到同一画布再拼。
func TestNormalizeClipsBeforeConcatUnifiesMismatchedCanvas(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("创建输出目录失败: %v", err)
	}

	small := filepath.Join(dir, "a.mp4")
	makeColorClip(t, ffmpeg, small, "red", 1) // 320x240
	big := filepath.Join(dir, "b.mp4")
	if err := runCommand(ctx, ffmpeg, "-y", "-f", "lavfi", "-i", "color=c=blue:s=640x480:r=24:d=1",
		"-c:v", "libx264", "-crf", "12", "-preset", "ultrafast", "-pix_fmt", "yuv420p", big); err != nil {
		t.Fatalf("生成大尺寸片段失败: %v", err)
	}

	cfg := VideoRunConfig{AspectRatio: "4:3", Fps: 24}
	clips := []composeClip{{SegmentID: "m1", Path: small}, {SegmentID: "m2", Path: big}}
	paths, composed, warnings := normalizeClipsBeforeConcat(ctx, ffmpeg, outputDir, clips, cfg)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "统一") {
		t.Fatalf("尺寸不一致时应报告已统一画幅: %v", warnings)
	}
	if paths[0] == small || paths[1] == big || composed["m1"] != paths[0] || composed["m2"] != paths[1] {
		t.Fatalf("两段都应被替换为归一后文件: %v %+v", paths, composed)
	}
	first, err := mediaProbe(ctx, paths[0])
	if err != nil {
		t.Fatalf("回读归一结果失败: %v", err)
	}
	second, err := mediaProbe(ctx, paths[1])
	if err != nil {
		t.Fatalf("回读归一结果失败: %v", err)
	}
	if first.Width != second.Width || first.Height != second.Height {
		t.Fatalf("归一后尺寸仍不一致: %dx%d vs %dx%d", first.Width, first.Height, second.Width, second.Height)
	}

	// 参数完全一致时走快路径：不重编码、不产生警告
	same1 := filepath.Join(dir, "s1.mp4")
	same2 := filepath.Join(dir, "s2.mp4")
	makeColorClip(t, ffmpeg, same1, "red", 1)
	makeColorClip(t, ffmpeg, same2, "blue", 1)
	fastPaths, fastComposed, fastWarnings := normalizeClipsBeforeConcat(ctx, ffmpeg, outputDir,
		[]composeClip{{SegmentID: "f1", Path: same1}, {SegmentID: "f2", Path: same2}}, cfg)
	if fastPaths[0] != same1 || fastPaths[1] != same2 || fastComposed["f2"] != same2 || len(fastWarnings) != 0 {
		t.Fatalf("参数一致时不应重编码: %v %v", fastPaths, fastWarnings)
	}
}

// 拼接实测对账：参数齐的段走 -c copy 快路径，成片时长必须等于各段实测之和。
func TestConcatVideoClipsKeepsDurationAccounting(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "output"), 0o755); err != nil {
		t.Fatalf("创建输出目录失败: %v", err)
	}
	first := filepath.Join(dir, "c1.mp4")
	second := filepath.Join(dir, "c2.mp4")
	makeColorClip(t, ffmpeg, first, "red", 1)
	makeColorClip(t, ffmpeg, second, "blue", 1)

	merged := filepath.Join(dir, "output", "merged.mp4")
	if err := concatVideoClips(ctx, []string{first, second}, merged); err != nil {
		t.Fatalf("拼接失败: %v", err)
	}
	info, err := mediaProbe(ctx, merged)
	if err != nil {
		t.Fatalf("回读拼接结果失败: %v", err)
	}
	if info.Duration < 1.7 || info.Duration > 2.4 {
		t.Fatalf("成片应约 2s（两段之和），实际 %.3fs", info.Duration)
	}
}

// ---------- 字幕声学对齐：提取音频 → 说话区间 → 台词对齐 → 时间对应表 ----------

func TestParseSilenceEvents(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		total float64
		want  [][2]float64
	}{
		{
			name:  "正常交错：静音取反就是说话",
			text:  "silence_start: 0.98\nsilence_end: 1.02\nsilence_start: 2.5\nsilence_end: 3.1\n",
			total: 4,
			want:  [][2]float64{{0, 0.98}, {1.02, 2.5}, {3.1, 4}},
		},
		{
			name:  "开头短静音被丢弃，不够长的活动窗口不留",
			text:  "silence_start: 0.05\nsilence_end: 1.0\n",
			total: 3,
			want:  [][2]float64{{1.0, 3}},
		},
		{
			name:  "全程静音：没有说话窗口",
			text:  "silence_start: 0.0\n",
			total: 2,
			want:  nil,
		},
		{
			name:  "全程有声（无事件）：整段都是说话区",
			text:  "frame= 90 fps=512 speed=17x",
			total: 5,
			want:  [][2]float64{{0, 5}},
		},
		{
			name:  "孤立 silence_end 与重复 silence_start 都不作乱",
			text:  "silence_end: 9\nsilence_start: 1\nsilence_start: 2\nsilence_end: 3\n",
			total: 5,
			want:  [][2]float64{{0, 1}, {3, 5}},
		},
		{name: "total 非法时不产出窗口", text: "silence_start: 1\n", total: 0, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSilenceEvents(tc.text, tc.total)
			if len(got) != len(tc.want) {
				t.Fatalf("窗口数不符: 期望 %d 实际 %d %+v", len(tc.want), len(got), got)
			}
			for index, want := range tc.want {
				if got[index].Start != want[0] || got[index].End != want[1] {
					t.Fatalf("第 %d 个窗口不符: 期望 %v 实际 %v", index+1, want, got[index])
				}
			}
		})
	}
}

func TestAlignSubtitleCuesSpreadDialogueOverSpeechWindows(t *testing.T) {
	cfg, _, _ := normalizeVideoConfig(VideoRunConfig{Topic: "测试", Subtitles: true})
	spans := []SubtitleSpan{
		{Start: 0, Duration: 3, Text: "第一句话开始说了。第二句话在后面说。"},
		{Start: 3, Duration: 2, Text: "这段没有声音活动"},
	}
	// 只在 0.5~1.1s 有声音活动：对齐后两句都必须挤进这 0.6s，绝不挂进静音区
	windows := []speechWindow{{Start: 0.5, End: 1.1}}
	cues := alignSubtitleCues(cfg, spans, windows)
	if len(cues) < 3 {
		t.Fatalf("两句对齐 + 一句估算退回，至少 3 条: %+v", cues)
	}
	// 单调、不重叠
	for index := 1; index < len(cues); index++ {
		if cues[index].Start < cues[index-1].End-0.011 {
			t.Fatalf("字幕重叠: %+v", cues)
		}
	}
	// 对齐的两条：起止都落在说话窗口内（带留白与取整容差）
	if cues[0].Start < 0.5 || cues[0].Start > 0.62 {
		t.Fatalf("首句应贴着说话窗口开头: %+v", cues[0])
	}
	if cues[1].End > 1.15 {
		t.Fatalf("次句结束越过了静音区边界: %+v", cues[1])
	}
	// 无活动区间的段退回估算，仍在本段时间窗内
	last := cues[len(cues)-1]
	if last.Start < 3 || last.End > 5 {
		t.Fatalf("无窗口段的字幕应留在自己的时间窗内: %+v", last)
	}

	// 跨段边界的窗口局部化到各自段内
	local := speechWindowsIn([]speechWindow{{Start: 2.5, End: 4.0}}, 3, 5)
	if len(local) != 1 || local[0].Start != 0 || local[0].End != 1.0 {
		t.Fatalf("窗口应被裁到 [3,5) 并平移: %+v", local)
	}
}

func makeToneSilenceClip(t *testing.T, ffmpeg, path string) {
	t.Helper()
	args := []string{
		"-y",
		"-f", "lavfi", "-i", "color=c=black:s=320x240:r=24:d=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=44100",
		"-filter_complex", "[1:a]asplit=2[t1][t2];[2:a]atrim=duration=1[sil];[t1][sil][t2]concat=n=3:v=0:a=1[a]",
		"-map", "0:v", "-map", "[a]",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", path,
	}
	if err := runCommand(context.Background(), ffmpeg, args...); err != nil {
		t.Fatalf("生成有声/静音片段失败: %v", err)
	}
}

func TestSubtitleAcousticAlignmentPipeline(t *testing.T) {
	ffmpeg := bundledFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	workDir := filepath.Join(dir, "task")
	if err := os.MkdirAll(filepath.Join(workDir, "output"), 0o755); err != nil {
		t.Fatalf("创建工作目录失败: %v", err)
	}
	source := filepath.Join(dir, "src.mp4")
	makeToneSilenceClip(t, ffmpeg, source)

	// 提取音频 → 检测说话窗口：应命中 [0,1] 与 [2,3] 两段，中间 1s 静音不算
	wavPath := filepath.Join(workDir, "output", "speech.wav")
	if err := extractSpeechWav(ctx, ffmpeg, source, wavPath); err != nil {
		t.Fatalf("提取音频失败: %v", err)
	}
	if !fileExists(wavPath) {
		t.Fatal("speech.wav 没有落盘")
	}
	windows, err := detectSpeechWindows(ctx, ffmpeg, wavPath, 3.0)
	if err != nil {
		t.Fatalf("音频活动分析失败: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("应检出两个说话窗口，实际 %+v", windows)
	}
	if windows[0].Start > 0.3 || windows[0].End < 0.6 {
		t.Fatalf("首窗应覆盖 0~1s 发声区: %+v", windows[0])
	}
	if windows[1].Start < 1.7 || windows[1].Start > 2.4 || windows[1].End < 2.6 {
		t.Fatalf("次窗应落在 2s 后的发声区: %+v", windows[1])
	}

	// 整条对齐链路：第二句必须跨过静音区，不能挂在 1~2s 的无声段
	cfg, _, _ := normalizeVideoConfig(VideoRunConfig{Topic: "测试", Subtitles: true})
	spans := []SubtitleSpan{{Start: 0, Duration: 3, Text: "第一句话正在说话。第二句话歇了一会儿又说。"}}
	cues, ok, message := alignCuesToSpeechActivity(ctx, ffmpeg, workDir, source, cfg, spans)
	if !ok {
		t.Fatalf("声学对齐应生效: %s", message)
	}
	if len(cues) < 2 {
		t.Fatalf("两句应拆成两条字幕: %+v", cues)
	}
	// 每条字幕的起点都必须落在真实发声窗口内（±0.1s 容差），绝不挂在静音中段
	for _, cue := range cues {
		hit := false
		for _, window := range windows {
			if cue.Start >= window.Start-0.1 && cue.Start <= window.End+0.1 {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("字幕起点挂在发声区外: %+v windows=%+v", cue, windows)
		}
	}
	// 第二句必须铺到第二个发声 burst 里（跨过 1~2s 的无声段）
	if cues[len(cues)-1].End < 2.0 {
		t.Fatalf("末句应延伸到第二发声窗口: %+v", cues[len(cues)-1])
	}

	// 全程静音：覆盖率为 0，护栏必须拦下（检测不可信，退回测时轴）
	silent := filepath.Join(dir, "silent.mp4")
	if err := runCommand(ctx, ffmpeg, "-y",
		"-f", "lavfi", "-i", "color=c=black:s=320x240:r=24:d=2",
		"-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=44100",
		"-map", "0:v", "-map", "1:a", "-t", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", silent); err != nil {
		t.Fatalf("生成全静音片段失败: %v", err)
	}
	if _, ok, message := alignCuesToSpeechActivity(ctx, ffmpeg, workDir, silent, cfg, spans); ok ||
		!strings.Contains(message, "覆盖") {
		t.Fatalf("全静音应被覆盖率护栏拦下: ok=%v message=%s", ok, message)
	}

	// 脚本合成：SRT/ASS 与字幕/时间对应表一起产出，表里带回溯信息
	subtitles, err := writeSubtitleFiles(workDir, "vt-align", cfg, cues, "aligned", source, 720, 1280)
	if err != nil {
		t.Fatalf("写字幕产物失败: %v", err)
	}
	if subtitles.TimingTablePath == "" || !fileExists(subtitles.TimingTablePath) {
		t.Fatalf("字幕/时间对应表没落盘: %+v", subtitles)
	}
	if subtitles.TimingTableURL == "" || subtitles.TimingSource != "aligned" {
		t.Fatalf("返回值不完整: %+v", subtitles)
	}
	data, err := os.ReadFile(subtitles.TimingTablePath)
	if err != nil {
		t.Fatalf("读时间表失败: %v", err)
	}
	var table subtitleTimingTable
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("时间表不是合法 JSON: %v", err)
	}
	if table.TimingSource != "aligned" || table.AudioSource != source ||
		!strings.Contains(table.SilenceDetect, "-35dB") || len(table.Cues) != len(cues) {
		t.Fatalf("时间表内容不符: %+v", table)
	}
}
