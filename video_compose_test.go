package main

import (
	"context"
	"fmt"
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
