package main

import (
	"context"
	"fmt"
	"path/filepath"
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

	spans, err := measuredSubtitleSpans(context.Background(), plan, renders)
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
