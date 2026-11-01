package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	configureHiddenProcess(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		message := commandErrorText(stderr.String())
		if message == "" {
			return fmt.Errorf("%s 执行失败: %w", filepath.Base(name), err)
		}
		return fmt.Errorf("%s 执行失败: %v；命令行：%s %s；输出尾部：%s",
			filepath.Base(name), err, filepath.Base(name), strings.Join(args, " "), message)
	}
	return nil
}

// commandErrorText 取子进程输出的尾部：ffmpeg 这类工具开头是版本与参数回显，
// 真正的报错在最后一两行，只留开头等于没留。
func commandErrorText(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	const limit = 1200
	if len(runes) > limit {
		return "…" + string(runes[len(runes)-limit:])
	}
	return text
}

// extractLastFrame 抽取上一段尾帧，作为下一段的首帧条件输入（§5.3 last_frame）。
func extractLastFrame(ctx context.Context, clipPath, framePath string) error {
	ffmpeg, ok := ffmpegPath()
	if !ok {
		return errors.New("ffmpeg 不可用，无法抽取尾帧")
	}
	if err := os.MkdirAll(filepath.Dir(framePath), 0o755); err != nil {
		return err
	}
	return runCommand(ctx, ffmpeg,
		"-y", "-sseof", "-0.3", "-i", clipPath,
		"-frames:v", "1", "-q:v", "2", framePath,
	)
}

/* ---------------- 接缝冻帧裁剪：逐帧 SSIM 比较下一段开头帧与上一段尾帧 ---------------- */

// ssimFreezeThreshold：开头帧与上一段尾帧的 SSIM 高于此值＝视觉几乎相同（模型还在重放条件尾帧），
// 标记为应删帧；一旦降到阈值以下说明真实运动恢复，从该帧起保留。
const ssimFreezeThreshold = 0.995

// freezeScanMaxFrames：每个接缝最多比对这么多帧。冻帧通常远小于 1 秒，
// 超出窗口仍“冻”的按创作内容处理，不再多删，避免误删整段静画面。
const freezeScanMaxFrames = 30

// 不同 ffmpeg 版本回显大小写不一（all:/All:），统一不敏感匹配
var ssimAllPattern = regexp.MustCompile(`(?i)all:([0-9]+(?:\.[0-9]+)?)`)

// frameSSIM 用 ffmpeg ssim 滤镜算两张图的灰度相似度，从 stderr 回显里读 all 值。
// 两图尺寸不一致时 ssim 会直接报错，由调用方视为“无法判定”跳过裁剪。
func frameSSIM(ctx context.Context, ffmpeg, imageA, imageB string) (float64, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-y",
		"-i", imageA, "-i", imageB,
		"-filter_complex", "[0:v]format=gray[g0];[1:v]format=gray[g1];[g0][g1]ssim",
		"-f", "null", "-",
	)
	configureHiddenProcess(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("SSIM 比对失败: %s", commandErrorText(stderr.String()))
	}
	matched := ssimAllPattern.FindStringSubmatch(stderr.String())
	if matched == nil {
		return 0, fmt.Errorf("没能从 ssim 输出里读到相似度: %s", commandErrorText(stderr.String()))
	}
	value, err := strconv.ParseFloat(matched[1], 64)
	if err != nil {
		return 0, fmt.Errorf("解析 SSIM 值失败: %w", err)
	}
	return value, nil
}

// countFrozenHeadFrames 逐帧比较待接片段开头与上一段尾帧，返回应删的开头帧数。
// prebuiltTail 非空且存在时直接用它（渲染阶段存下的条件图，就是模型重放的那一帧），没有才从上一段抽取。
func countFrozenHeadFrames(ctx context.Context, ffmpeg, prevClip, headClip, scanDir, prebuiltTail string) (int, error) {
	if err := os.RemoveAll(scanDir); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		return 0, err
	}
	tailFrame := filepath.Join(scanDir, "tail.png")
	if prebuiltTail != "" && fileExists(prebuiltTail) {
		tailFrame = prebuiltTail
	} else if err := extractLastFrame(ctx, prevClip, tailFrame); err != nil {
		return 0, err
	}
	if err := runCommand(ctx, ffmpeg,
		"-y", "-i", headClip, "-vsync", "0", "-frames:v", strconv.Itoa(freezeScanMaxFrames),
		"-q:v", "2", filepath.Join(scanDir, "f%03d.png"),
	); err != nil {
		return 0, fmt.Errorf("抽取开头帧失败: %w", err)
	}
	frames, err := filepath.Glob(filepath.Join(scanDir, "f*.png"))
	if err != nil || len(frames) == 0 {
		return 0, errors.New("没有可比的开头帧")
	}
	discard := 0
	for _, frame := range frames {
		value, err := frameSSIM(ctx, ffmpeg, frame, tailFrame)
		if err != nil {
			return 0, err
		}
		if value <= ssimFreezeThreshold {
			break // 真实运动恢复，这一帧起全部保留
		}
		discard++
	}
	return discard, nil
}

// trimHeadFrames 去掉片段开头 count 帧。重编码音视频同步裁掉同一时间偏移，
// 保证字幕测时与对白不失同步；输入侧 -ss 配合重编码在 ffmpeg 里是帧精确的。
func trimHeadFrames(ctx context.Context, ffmpeg, input, output string, count int, fps float64) error {
	if fps <= 0 {
		fps = 24
	}
	offset := float64(count) / fps
	return runCommand(ctx, ffmpeg,
		"-y", "-ss", fmt.Sprintf("%.5f", offset), "-i", input,
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", output,
	)
}

// composeClip 合成阶段参与拼接的一个片段。
type composeClip struct {
	SegmentID string
	Path      string
}

// trimFrozenClipHeads 从第二段起逐接缝检测并裁掉冻帧重复帧，返回拼接清单、
// 段 ID → 实际入片文件（字幕按裁剪后文件实测时长）与给用户看的说明。
// 任一接缝判定失败只跳过该接缝（保持原有硬切行为），不阻断合成。
func trimFrozenClipHeads(ctx context.Context, ffmpeg, workDir, outputDir string, clips []composeClip, defaultFps int) ([]string, map[string]string, []string) {
	paths := make([]string, len(clips))
	composed := make(map[string]string, len(clips))
	for index, clip := range clips {
		paths[index] = clip.Path
		composed[clip.SegmentID] = clip.Path
	}
	warnings := make([]string, 0, 2)
	if len(clips) < 2 {
		return paths, composed, warnings
	}

	scanBase := filepath.Join(outputDir, "freeze-scan")
	_ = os.RemoveAll(scanBase)
	for index := 1; index < len(clips); index++ {
		clip := clips[index]
		select {
		case <-ctx.Done():
			warnings = append(warnings, "冻帧裁剪超时，剩余接缝按硬切拼接")
			return paths, composed, warnings
		default:
		}

		// 渲染阶段给本段抽过条件尾帧就直接用它比对，比“上一段的最后一帧”更贴合模型重放的源
		prebuiltTail := filepath.Join(workDir, "clips", clip.SegmentID+"-tail.png")
		discard, err := countFrozenHeadFrames(ctx, ffmpeg, clips[index-1].Path, clip.Path,
			filepath.Join(scanBase, clip.SegmentID), prebuiltTail)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("第 %d 段冻帧检测失败，该接缝按硬切拼接: %v", index+1, err))
			continue
		}
		if discard <= 0 {
			continue
		}

		// 防止把整段都删空：比对的只是开头窗口，若窗口内全冻且快抵到全段时长，只保留尾部安全帧
		info, probeErr := mediaProbe(ctx, clip.Path)
		fps := info.FPS
		if fps <= 0 {
			if defaultFps > 0 {
				fps = float64(defaultFps)
			} else {
				fps = 24
			}
		}
		if probeErr == nil && info.Duration > 0 {
			totalFrames := int(math.Round(info.Duration * fps))
			if discard >= totalFrames-1 {
				warnings = append(warnings, fmt.Sprintf("第 %d 段开头整段与上一段尾帧相同，为避免删空已按原样硬切", index+1))
				continue
			}
		}

		trimmed := filepath.Join(outputDir, fmt.Sprintf("trim-%s.mp4", clip.SegmentID))
		if err := trimHeadFrames(ctx, ffmpeg, clip.Path, trimmed, discard, fps); err != nil {
			warnings = append(warnings, fmt.Sprintf("第 %d 段冻帧裁剪失败，该接缝按硬切拼接: %v", index+1, err))
			continue
		}
		paths[index] = trimmed
		composed[clip.SegmentID] = trimmed
		warnings = append(warnings, fmt.Sprintf(
			"第 %d 段开头 %d 帧与上一段尾帧 SSIM>%.3f（还在冻帧），已删除，运动恢复后保留",
			index+1, discard, ssimFreezeThreshold,
		))
	}
	_ = os.RemoveAll(scanBase)
	return paths, composed, warnings
}

// concatVideoClips 多段拼接：优先流拷贝，失败再统一重编码（避免分辨率/编码不一致）。
func concatVideoClips(ctx context.Context, clips []string, output string) error {
	ffmpeg, ok := ffmpegPath()
	if !ok {
		return errors.New("ffmpeg 不可用，无法拼接多段视频")
	}
	if len(clips) == 0 {
		return errors.New("没有可拼接的片段")
	}
	if len(clips) == 1 {
		return runCommand(ctx, ffmpeg, "-y", "-i", clips[0], "-c", "copy", output)
	}

	listPath := filepath.Join(filepath.Dir(output), "clips.txt")
	var list strings.Builder
	for _, clip := range clips {
		list.WriteString("file '" + strings.ReplaceAll(filepath.ToSlash(clip), "'", `'\''`) + "'\n")
	}
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return fmt.Errorf("写入拼接清单失败: %w", err)
	}

	copyErr := runCommand(ctx, ffmpeg, "-y", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", output)
	if copyErr == nil {
		return nil
	}
	return runCommand(ctx, ffmpeg,
		"-y", "-f", "concat", "-safe", "0", "-i", listPath,
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast",
		"-pix_fmt", "yuv420p", "-c:a", "aac",
		output,
	)
}

// mediaInfo 片段实测到的画面/音频元数据。
type mediaInfo struct {
	Width    int
	Height   int
	Duration float64
	HasAudio bool
	FPS      float64
}

// mediaProbe 读视频片段的画面尺寸、时长与有无音轨。字幕对齐和画幅兜底都依赖实测，
// 不能信服务商回报的标称时长（实际产物常常多 0.0x～0.x 秒）。
// 优先用 ffprobe；但安装包内置的是 imageio-ffmpeg 静态构建（只有 ffmpeg），
// 所以必须有 ffmpeg 兜底，否则归一画幅与字幕对齐在用户机器上会默默不生效。
func mediaProbe(ctx context.Context, path string) (mediaInfo, error) {
	if ffprobe, ok := ffprobePath(); ok {
		info, err := probeWithFFprobe(ctx, ffprobe, path)
		if err == nil {
			return info, nil
		} else if fallback, fbErr := probeWithFFmpeg(ctx, path); fbErr == nil {
			return fallback, nil
		} else {
			return info, err
		}
	}
	return probeWithFFmpeg(ctx, path)
}

func probeWithFFprobe(ctx context.Context, ffprobe, path string) (mediaInfo, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-show_entries", "stream=codec_type,width,height,duration,r_frame_rate:format=duration",
		"-of", "json", path,
	)
	configureHiddenProcess(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return mediaInfo{}, fmt.Errorf("ffprobe 读取 %s 失败: %v；输出尾部：%s", filepath.Base(path), err, commandErrorText(stderr.String()))
	}
	var payload struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			// ffprobe 的 JSON 把数值都输出为字符串，声明成 float64 会直接解析失败
			Duration string `json:"duration"`
			// 帧率是 "24/1" 这样的分数，删帧后算时间偏移要用
			FrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		return mediaInfo{}, fmt.Errorf("解析 ffprobe 输出失败: %w；输出尾部：%s", err, commandErrorText(stdout.String()))
	}
	info := mediaInfo{}
	for _, stream := range payload.Streams {
		switch stream.CodecType {
		case "video":
			if info.Width == 0 && stream.Width > 0 && stream.Height > 0 {
				info.Width, info.Height = stream.Width, stream.Height
				if info.Duration <= 0 {
					info.Duration, _ = strconv.ParseFloat(strings.TrimSpace(stream.Duration), 64)
				}
				info.FPS = parseFrameRate(stream.FrameRate)
			}
		case "audio":
			info.HasAudio = true
		}
	}
	if info.Width <= 0 || info.Height <= 0 {
		return info, fmt.Errorf("%s 里没有可用的视频流：%s", filepath.Base(path), commandErrorText(stdout.String()))
	}
	if duration, err := strconv.ParseFloat(strings.TrimSpace(payload.Format.Duration), 64); err == nil && duration > 0 {
		info.Duration = duration
	}
	return info, nil
}

var (
	probeDurationPattern = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)`)
	probeVideoPattern    = regexp.MustCompile(`Stream #\d+:\d+[^:]*:\s*Video:[^\n]*?(\d{2,5})x(\d{2,5})`)
	probeAudioPattern    = regexp.MustCompile(`Stream #\d+:\d+[^:]*:\s*Audio:`)
	probeFPSPattern      = regexp.MustCompile(`Video:[^\n]*?,\s*(\d+(?:\.\d+)?)\s*fps`)
)

// parseFrameRate 解析 ffprobe 的 r_frame_rate 分数（"24/1"）；异常时返回 0 交给上层兜底。
func parseFrameRate(raw string) float64 {
	parts := strings.SplitN(strings.TrimSpace(raw), "/", 2)
	numerator, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || numerator <= 0 {
		return 0
	}
	if len(parts) == 1 {
		return numerator
	}
	denominator, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || denominator <= 0 {
		return 0
	}
	return numerator / denominator
}

// probeWithFFmpeg 从 `ffmpeg -i` 的 stderr 回显里取元数据（没有流时 ffmpeg 也会先打完信息再报错）。
func probeWithFFmpeg(ctx context.Context, path string) (mediaInfo, error) {
	ffmpeg, ok := ffmpegPath()
	if !ok {
		return mediaInfo{}, errors.New("ffmpeg / ffprobe 均不可用，无法读取片段的时长与画幅")
	}
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-i", path)
	configureHiddenProcess(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()
	info := parseFFmpegMetadata(stderr.String())
	if info.Width <= 0 || info.Height <= 0 {
		return info, fmt.Errorf("没能从 %s 的输出里读出画面尺寸：%s", filepath.Base(path), commandErrorText(stderr.String()))
	}
	return info, nil
}

// parseFFmpegMetadata 解析 `ffmpeg -i` 的回显文本。
func parseFFmpegMetadata(text string) mediaInfo {
	info := mediaInfo{}
	if matched := probeVideoPattern.FindStringSubmatch(text); matched != nil {
		info.Width, _ = strconv.Atoi(matched[1])
		info.Height, _ = strconv.Atoi(matched[2])
	}
	if matched := probeDurationPattern.FindStringSubmatch(text); matched != nil {
		hour, _ := strconv.ParseFloat(matched[1], 64)
		minute, _ := strconv.ParseFloat(matched[2], 64)
		second, _ := strconv.ParseFloat(matched[3], 64)
		info.Duration = hour*3600 + minute*60 + second
	}
	if matched := probeFPSPattern.FindStringSubmatch(text); matched != nil {
		info.FPS, _ = strconv.ParseFloat(matched[1], 64)
	}
	info.HasAudio = probeAudioPattern.MatchString(text)
	return info
}

// measuredSubtitleSpans 用片段真实时长重排字幕时间轴。
// 拼接是硬切，第 N 段的起点就是它之前所有已入选片段实际时长之和；
// 沿用计划时长会逐段累计偏移，到后半段字幕就和对白错开了。
// composedBySegment 是冻帧裁剪后实际入片的文件映射，测时必须跟着裁剪后的时长走。
func measuredSubtitleSpans(ctx context.Context, plan *VideoPlan, renders []VideoRender, composedBySegment map[string]string) ([]SubtitleSpan, error) {
	if plan == nil || len(plan.Segments) == 0 {
		return nil, errors.New("缺少渲染计划，无法建字幕时间轴")
	}
	bySegment := make(map[string]VideoRender, len(renders))
	for _, render := range renders {
		if render.Status == "succeeded" && fileExists(render.VideoURI) {
			bySegment[render.SegmentID] = render
		}
	}
	spans := make([]SubtitleSpan, 0, len(plan.Segments))
	offset := 0.0
	for _, segment := range plan.Segments {
		render, ok := bySegment[segment.ID]
		if !ok {
			continue // 失败/跳过的段不进成片，时间轴也不给它留位
		}
		probePath := render.VideoURI
		if composed, ok := composedBySegment[segment.ID]; ok && fileExists(composed) {
			probePath = composed
		}
		info, err := mediaProbe(ctx, probePath)
		if err != nil {
			return nil, err
		}
		duration := info.Duration
		if duration <= 0 {
			duration = segment.End - segment.Start
		}
		// 画面自带音轨时，语速由模型自己掌控，台词铺满整段比按字速估算更贴
		spans = append(spans, SubtitleSpan{Start: offset, Duration: duration, Text: segment.Dialogue, FillWindow: info.HasAudio})
		offset += duration
	}
	if len(spans) == 0 {
		return nil, errors.New("没有成功片段可供测时")
	}
	return spans, nil
}

// ---------- 字幕声学对齐：成片画布 → 提取音频 → 说话区间检测 → 台词铺进说话时钟 ----------

const (
	silenceNoiseDB    = "-35dB" // silencedetect 噪声门：低于它算静音
	silenceMinDurS    = 0.2     // 短于此不算停顿（换气 / 句间微停）
	speechWindowMinS  = 0.15    // 太短的活动窗口多半是杂音，不算说话
	speechCoverageMin = 0.20    // 台词段覆盖率低于此：检测不可信（全程 BGM 等），退回测时
	alignedLeadS      = 0.1     // 段首留白（说话时钟上）：起音瞬间不上字幕
	alignedGapS       = 0.08    // 句间空隙，与估算路径一致
)

// speechWindow 一段「有声音活动」的绝对时间区间。
type speechWindow struct {
	Start float64
	End   float64
}

var (
	silenceStartPattern = regexp.MustCompile(`silence_start:\s*([0-9]+(?:\.[0-9]+)?)`)
	silenceEndPattern   = regexp.MustCompile(`silence_end:\s*([0-9]+(?:\.[0-9]+)?)`)
)

// parseSilenceEvents 把 silencedetect 回显取反成说话窗口：静音区间的补集就是「有人在说话」。
// 开头直接说话（0 到第一个 silence_start）与结尾非静音（最后一个 silence_end 到 total）都算说话。
func parseSilenceEvents(text string, total float64) []speechWindow {
	if total <= 0 {
		return nil
	}
	type silenceEvent struct {
		at      float64
		isStart bool
	}
	events := make([]silenceEvent, 0, 16)
	for _, match := range silenceStartPattern.FindAllStringSubmatch(text, -1) {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			events = append(events, silenceEvent{at: value, isStart: true})
		}
	}
	for _, match := range silenceEndPattern.FindAllStringSubmatch(text, -1) {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			events = append(events, silenceEvent{at: value, isStart: false})
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].at == events[j].at {
			return !events[i].isStart // 同一时刻先处理结束再处理开始，避免凭空造出静音段
		}
		return events[i].at < events[j].at
	})

	windows := make([]speechWindow, 0, len(events)/2+1)
	appendWindow := func(start, end float64) {
		if start < 0 {
			start = 0
		}
		if end > total {
			end = total
		}
		if end-start >= speechWindowMinS {
			windows = append(windows, speechWindow{Start: round2(start), End: round2(end)})
		}
	}
	cursor := 0.0
	inSilence := false
	for _, event := range events {
		if event.isStart {
			if inSilence {
				continue // 连续两个 silence_start（异常回显），后一个作废
			}
			inSilence = true
			appendWindow(cursor, event.at)
			cursor = event.at
		} else {
			if !inSilence {
				continue // 孤立的 silence_end，忽略
			}
			inSilence = false
			cursor = event.at
		}
	}
	if !inSilence {
		appendWindow(cursor, total) // 结尾非静音：说话一直铺到成片末尾
	}
	return windows
}

// extractSpeechWav 从源媒体提取单声道 16k wav：声学分析只要人声频段，降采样还更快。
func extractSpeechWav(ctx context.Context, ffmpeg, source, wavPath string) error {
	return runCommand(ctx, ffmpeg, "-y", "-i", source, "-vn", "-ac", "1", "-ar", "16000", wavPath)
}

// detectSpeechWindows 对 wav 跑一遍 silencedetect，把 stderr 回显取反成说话窗口。
// 全程无事件时窗口就是一整段（全程有声），由上层覆盖率护栏判断是否采信。
func detectSpeechWindows(ctx context.Context, ffmpeg, wavPath string, total float64) ([]speechWindow, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-i", wavPath,
		"-af", fmt.Sprintf("silencedetect=noise=%s:d=%g", silenceNoiseDB, silenceMinDurS),
		"-f", "null", "-")
	configureHiddenProcess(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("音频活动分析失败: %w", err)
	}
	return parseSilenceEvents(stderr.String(), total), nil
}

// speechWindowsIn 把窗口裁剪到 [start, end) 并平移成相对 start 的局部坐标。
func speechWindowsIn(windows []speechWindow, start, end float64) []speechWindow {
	local := make([]speechWindow, 0, len(windows))
	for _, window := range windows {
		left, right := math.Max(window.Start, start), math.Min(window.End, end)
		if right-left > 0.05 {
			local = append(local, speechWindow{Start: left - start, End: right - start})
		}
	}
	return local
}

// speechClockMapper 返回把「说话时钟」位置映回段局部时间的函数：
// 窗口首尾相接排成一条只有说话的轴，句子只消耗真实说话时长，永远不会挂进静音区。
func speechClockMapper(windows []speechWindow) func(float64) float64 {
	if len(windows) == 0 {
		return func(float64) float64 { return 0 }
	}
	total := 0.0
	for _, window := range windows {
		total += window.End - window.Start
	}
	return func(t float64) float64 {
		if t <= 0 {
			return 0
		}
		if t >= total {
			return windows[len(windows)-1].End
		}
		acc := 0.0
		for _, window := range windows {
			duration := window.End - window.Start
			if t <= acc+duration {
				return window.Start + (t - acc)
			}
			acc += duration
		}
		return windows[len(windows)-1].End
	}
}

// alignSubtitleCues 把台词按字数权重铺进说话窗口（说话时钟）；
// 没检出声音活动的段退回字速估算，保证时间轴仍单调、不重叠、不越界。
func alignSubtitleCues(cfg VideoRunConfig, spans []SubtitleSpan, windows []speechWindow) []VideoSubtitleCue {
	cues := make([]VideoSubtitleCue, 0, len(spans))
	for _, span := range spans {
		text := strings.TrimSpace(span.Text)
		if text == "" || span.Duration <= 0 {
			continue
		}
		local := speechWindowsIn(windows, span.Start, span.Start+span.Duration)
		if len(local) == 0 {
			cues = appendEstimatedCues(cues, cfg, span)
			continue
		}
		parts := splitDialogueSentences(text)
		if len(parts) == 0 {
			continue
		}
		weights := make([]float64, len(parts))
		weightSum, speechTotal := 0.0, 0.0
		for index, part := range parts {
			weights[index] = math.Max(1, float64(len([]rune(part))))
			weightSum += weights[index]
		}
		for _, window := range local {
			speechTotal += window.End - window.Start
		}
		// 与估算路径同一策略：每条至少停留 0.9s，再等比缩回总说话时长内
		slots := make([]float64, len(parts))
		slotSum := 0.0
		for index := range parts {
			slots[index] = math.Max(0.9, speechTotal*weights[index]/weightSum)
			slotSum += slots[index]
		}
		if scale := speechTotal / slotSum; scale < 1 {
			for index := range slots {
				slots[index] *= scale
			}
		}

		mapClock := speechClockMapper(local)
		cursor := alignedLeadS
		for index, part := range parts {
			startLocal := math.Min(mapClock(cursor), span.Duration)
			endLocal := math.Min(mapClock(cursor+slots[index]), span.Duration)
			display := endLocal - alignedGapS
			if index == len(parts)-1 {
				display = endLocal // 末句吃掉取整误差，避免字幕提前消失
			}
			if display > startLocal {
				cues = append(cues, VideoSubtitleCue{
					Index: len(cues) + 1,
					Start: round2(span.Start + startLocal),
					End:   round2(span.Start + display),
					Text:  wrapSubtitleText(part, cfg.Language),
				})
			}
			cursor += slots[index]
		}
	}
	return cues
}

// alignCuesToSpeechActivity 声学对齐整条流水线：探音轨 → 提取音频 → 检测说话区间 → 对齐。
// 任何一步不可信都返回 ok=false，由调用方退回片段测时轴；message 说明原因或对齐概况。
func alignCuesToSpeechActivity(ctx context.Context, ffmpeg, workDir, canvas string, cfg VideoRunConfig, spans []SubtitleSpan) ([]VideoSubtitleCue, bool, string) {
	dialogueTotal := 0.0
	fallbackTotal := 0.0
	for _, span := range spans {
		if span.Duration > 0 {
			fallbackTotal += span.Duration
		}
		if strings.TrimSpace(span.Text) != "" {
			dialogueTotal += span.Duration
		}
	}
	if dialogueTotal <= 0 {
		return nil, false, "没有台词段可供对齐"
	}
	info, err := mediaProbe(ctx, canvas)
	if err != nil {
		return nil, false, "读取成片音轨失败: " + err.Error()
	}
	if !info.HasAudio {
		return nil, false, "成片没有音轨，字幕仍按片段测时轴"
	}
	total := info.Duration
	if total <= 0 {
		total = fallbackTotal
	}
	wavPath := filepath.Join(workDir, "output", "speech.wav")
	analysisCtx, cancelAnalysis := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelAnalysis()
	if err := extractSpeechWav(analysisCtx, ffmpeg, canvas, wavPath); err != nil {
		return nil, false, "提取音频失败: " + err.Error()
	}
	windows, err := detectSpeechWindows(analysisCtx, ffmpeg, wavPath, total)
	if err != nil {
		return nil, false, "音频活动分析失败: " + err.Error()
	}
	covered := 0.0
	for _, span := range spans {
		if strings.TrimSpace(span.Text) == "" || span.Duration <= 0 {
			continue
		}
		for _, window := range speechWindowsIn(windows, span.Start, span.Start+span.Duration) {
			covered += window.End - window.Start
		}
	}
	coverage := covered / dialogueTotal
	if coverage < speechCoverageMin {
		return nil, false, fmt.Sprintf("说话活动仅覆盖台词段的 %.0f%%，检测不可信", coverage*100)
	}
	cues := alignSubtitleCues(cfg, spans, windows)
	if len(cues) == 0 {
		return nil, false, "声学对齐没有产出字幕行"
	}
	if cues[len(cues)-1].End > total+0.5 {
		return nil, false, "对齐后的时间轴超出成片时长"
	}
	return cues, true, fmt.Sprintf("检出 %d 个说话窗口，覆盖台词段 %.0f%%", len(windows), coverage*100)
}

// normalizeAspectCanvas 把成片统一到任务选定的画幅。
// 服务商并不总按请求执行：方舟首帧/首尾帧任务的输出宽高比强制跟随首帧图（ratio 只能为 adaptive），
// 选了 9:16 也可能拿回 16:9 的素材，所以在合成阶段兜底。
// 用画面自身放大模糊做底、原画面居中，比直接裁切保住内容，也不像黑边那样难看。
func normalizeAspectCanvas(ctx context.Context, input, output, aspect string) (int, int, bool, error) {
	info, err := mediaProbe(ctx, input)
	if err != nil {
		return info.Width, info.Height, false, err
	}
	srcWidth, srcHeight := info.Width, info.Height
	width, height, needed := targetCanvas(aspect, srcWidth, srcHeight)
	if !needed {
		return srcWidth, srcHeight, false, nil
	}
	ffmpeg, ok := ffmpegPath()
	if !ok {
		return width, height, false, errors.New("ffmpeg 不可用，无法统一画幅")
	}
	filter := fmt.Sprintf(
		"[0:v]split=2[bg][fg];"+
			"[bg]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=20:2[bgb];"+
			"[fg]scale=%d:%d:force_original_aspect_ratio=decrease[fgb];"+
			"[bgb][fgb]overlay=(W-w)/2:(H-h)/2,setsar=1[vout]",
		width, height, width, height, width, height,
	)
	if err := runCommand(ctx, ffmpeg,
		"-y", "-i", input,
		"-filter_complex", filter, "-map", "[vout]", "-map", "0:a?",
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", output,
	); err != nil {
		// 退一步：直接黑边填充，至少保证画幅对
		padFilter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1", width, height, width, height)
		if padErr := runCommand(ctx, ffmpeg,
			"-y", "-i", input, "-vf", padFilter,
			"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
			"-c:a", "copy", "-movflags", "+faststart", output,
		); padErr != nil {
			return width, height, false, fmt.Errorf("画幅归一失败: %v（黑边兜底也失败: %v）", err, padErr)
		}
	}
	return width, height, true, nil
}

// escapeFilterPath 处理 Windows 路径在 ffmpeg 滤镜参数中的转义：反斜杠、盘符冒号、单引号。
func escapeFilterPath(path string) string {
	escaped := strings.ReplaceAll(path, "\\", "/")
	escaped = strings.ReplaceAll(escaped, ":", `\:`)
	escaped = strings.ReplaceAll(escaped, "'", `\'`)
	return "'" + escaped + "'"
}

// burnSubtitles 烧录字幕必须重编码（不能 -c:v copy），音频原样保留。
func burnSubtitles(ctx context.Context, ffmpeg, input, assPath, output string) error {
	if assPath == "" || !fileExists(assPath) {
		return errors.New("缺少 ASS 字幕文件")
	}
	return runCommand(ctx, ffmpeg,
		"-y", "-i", input,
		"-vf", "ass="+escapeFilterPath(assPath),
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "copy",
		"-movflags", "+faststart",
		output,
	)
}

// hexToASSColor ASS 颜色是 &HAABBGGRR（不是 #RRGGBB），AA=00 表示不透明。
func hexToASSColor(hex string) string {
	value := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(value) != 6 {
		value = "FFFFFF"
	}
	r := value[0:2]
	g := value[2:4]
	b := value[4:6]
	return "&H00" + strings.ToUpper(b+g+r)
}

func srtTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(math.Round(seconds * 1000))
	ms := total % 1000
	total /= 1000
	sec := total % 60
	total /= 60
	min := total % 60
	hour := total / 60
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hour, min, sec, ms)
}

func assTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(math.Round(seconds * 100))
	cs := total % 100
	total /= 100
	sec := total % 60
	total /= 60
	min := total % 60
	hour := total / 60
	return fmt.Sprintf("%d:%02d:%02d.%02d", hour, min, sec, cs)
}

func renderSRT(cues []VideoSubtitleCue) string {
	var builder strings.Builder
	for index, cue := range cues {
		builder.WriteString(fmt.Sprintf("%d\n", index+1))
		builder.WriteString(fmt.Sprintf("%s --> %s\n", srtTime(cue.Start), srtTime(cue.End)))
		builder.WriteString(cue.Text)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

// renderASS PlayRes 与视频分辨率一致，字号随分辨率线性缩放（§6.3）。
func renderASS(cues []VideoSubtitleCue, cfg VideoRunConfig, width, height int) string {
	style := cfg.SubtitleStyle
	fontSize := math.Max(10, style.FontSizePct/100*float64(height))
	marginV := math.Max(0, style.MarginVPct/100*float64(height))
	marginH := math.Max(10, float64(width)*0.04)

	var builder strings.Builder
	builder.WriteString("[Script Info]\n")
	builder.WriteString("ScriptType: v4.00+\n")
	builder.WriteString(fmt.Sprintf("PlayResX: %d\n", width))
	builder.WriteString(fmt.Sprintf("PlayResY: %d\n", height))
	builder.WriteString("WrapStyle: 2\n")
	builder.WriteString("ScaledBorderAndShadow: yes\n\n")
	builder.WriteString("[V4+ Styles]\n")
	builder.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	builder.WriteString(fmt.Sprintf(
		"Style: Default,%s,%.0f,%s,&H000000FF,%s,&H64000000,%d,0,0,0,100,100,0,0,1,%.1f,1,%d,%.0f,%.0f,%.0f,1\n\n",
		style.FontName, fontSize,
		hexToASSColor(style.PrimaryColor), hexToASSColor(style.OutlineColor),
		boolToInt(style.Bold), style.OutlineWidth, style.Alignment,
		marginH, marginH, marginV,
	))
	builder.WriteString("[Events]\n")
	builder.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	for _, cue := range cues {
		text := strings.ReplaceAll(cue.Text, "\n", `\N`)
		builder.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Default,,0,0,0,,%s\n", assTime(cue.Start), assTime(cue.End), text))
	}
	return builder.String()
}

// prepareSubtitleFiles 渲染前产出 SRT 与 ASS（§4.3），时间轴基于预算的分镜时长。
func prepareSubtitleFiles(workDir, taskID string, cfg VideoRunConfig, plan *VideoPlan) (*VideoSubtitles, error) {
	if plan == nil {
		return nil, errors.New("缺少渲染计划，无法生成字幕时间轴")
	}
	width, height := aspectResolution(cfg.AspectRatio)
	return writeSubtitleFiles(workDir, taskID, cfg, buildSubtitleCues(*plan, cfg), "estimate", "", width, height)
}

// subtitleTimingTable 字幕/时间对应表：cue 与对齐依据留档，供下载、复查与重渲染。
type subtitleTimingTable struct {
	TimingSource  string             `json:"timingSource"`
	GeneratedAt   string             `json:"generatedAt"`
	AudioSource   string             `json:"audioSource,omitempty"`
	SilenceDetect string             `json:"silenceDetect,omitempty"`
	Cues          []VideoSubtitleCue `json:"cues"`
}

// writeSubtitleFiles 脚本合成：按给定的 cue 时间轴生成 SRT/ASS 与字幕/时间对应表。
// cue 由调用方决定来源（estimate/measured/aligned），合成阶段对齐后保证烧进成片的字幕与真实画面、对白同步。
// audioSource 是被分析的媒体文件（声学对齐路径下非空），写进时间表供追溯。
func writeSubtitleFiles(workDir, taskID string, cfg VideoRunConfig, cues []VideoSubtitleCue, timingSource, audioSource string, width, height int) (*VideoSubtitles, error) {
	outputDir := filepath.Join(workDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}

	srtPath := filepath.Join(outputDir, "subtitles.srt")
	assPath := filepath.Join(outputDir, "subtitles.ass")
	timingPath := filepath.Join(outputDir, "subtitle-timing.json")

	if err := os.WriteFile(srtPath, []byte(renderSRT(cues)), 0o644); err != nil {
		return nil, fmt.Errorf("写入 SRT 字幕失败: %w", err)
	}
	if err := os.WriteFile(assPath, []byte(renderASS(cues, cfg, width, height)), 0o644); err != nil {
		return nil, fmt.Errorf("写入 ASS 字幕失败: %w", err)
	}
	table := subtitleTimingTable{
		TimingSource: timingSource,
		GeneratedAt:  time.Now().Format(time.RFC3339),
		AudioSource:  audioSource,
		Cues:         cues,
	}
	if table.Cues == nil {
		table.Cues = []VideoSubtitleCue{}
	}
	if audioSource != "" {
		table.SilenceDetect = fmt.Sprintf("noise=%s:d=%g", silenceNoiseDB, silenceMinDurS)
	}
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化字幕时间对应表失败: %w", err)
	}
	if err := os.WriteFile(timingPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("写入字幕时间对应表失败: %w", err)
	}

	return &VideoSubtitles{
		SRTPath:         srtPath,
		ASSPath:         assPath,
		SRTUrl:          videoMediaURL(taskID, "output/subtitles.srt"),
		ASSUrl:          videoMediaURL(taskID, "output/subtitles.ass"),
		TimingTablePath: timingPath,
		TimingTableURL:  videoMediaURL(taskID, "output/subtitle-timing.json"),
		TimingSource:    timingSource,
		Cues:            cues,
	}, nil
}

// finalizeVideo 输出成片：字幕烧录 + 模板资产（水印 / logo / BGM）。逐项降级，不阻断成片。
// 第一个返回值表示 final 文件是否已写出，第二个表示字幕是否烧录成功。
func finalizeVideo(ctx context.Context, ffmpeg, input, output string, run *VideoRunContext) (bool, bool, []string) {
	warnings := make([]string, 0, 3)
	assPath := ""
	if run != nil && run.Subtitles != nil && len(run.Subtitles.Cues) > 0 && fileExists(run.Subtitles.ASSPath) {
		assPath = run.Subtitles.ASSPath
	}
	overlayPath, bgmPath := "", ""
	if run != nil && run.Template != nil {
		overlayPath, bgmPath = templateAssetFiles(run.Template, &warnings)
	}
	if assPath == "" && overlayPath == "" && bgmPath == "" {
		return false, false, warnings
	}

	if err := renderFinalWithAssets(ctx, ffmpeg, input, output, assPath, overlayPath, bgmPath); err == nil {
		return true, assPath != "", warnings
	} else {
		warnings = append(warnings, "字幕/模板资产一次渲染失败，已降级为逐项处理: "+err.Error())
	}

	// 降级一：只烧字幕（模板资产放弃，但成片仍可交付）
	if assPath != "" {
		if err := burnSubtitles(ctx, ffmpeg, input, assPath, output); err == nil {
			return true, true, warnings
		}
		warnings = append(warnings, "字幕烧录失败，将输出无字幕成片")
	}
	return false, false, warnings
}

// templateAssetFiles 取模板资产里真实可用的本地文件；远程 / 不存在的路径只提示不阻断。
func templateAssetFiles(tpl *VideoTemplateRun, warnings *[]string) (overlayPath, bgmPath string) {
	overlayPath = firstNonEmpty(tpl.Assets.Watermark, tpl.Assets.Logo)
	if overlayPath != "" && !fileExists(overlayPath) {
		*warnings = append(*warnings, "模板水印/logo 不可读，已跳过叠加："+overlayPath)
		overlayPath = ""
	}
	bgmPath = tpl.Assets.Bgm
	if bgmPath != "" && !fileExists(bgmPath) {
		*warnings = append(*warnings, "模板 BGM 不可读，已跳过混音："+bgmPath)
		bgmPath = ""
	}
	return overlayPath, bgmPath
}

// renderFinalWithAssets 单次 ffmpeg 完成：烧字幕 + 右下角水印 + BGM 混音（BGM 循环至成片时长）。
func renderFinalWithAssets(ctx context.Context, ffmpeg, input, output, assPath, overlayPath, bgmPath string) error {
	args := []string{"-y", "-i", input}
	nextInput := 1
	overlayIndex := -1
	bgmIndex := -1
	if overlayPath != "" {
		args = append(args, "-i", overlayPath)
		overlayIndex = nextInput
		nextInput++
	}
	if bgmPath != "" {
		args = append(args, "-stream_loop", "-1", "-i", bgmPath)
		bgmIndex = nextInput
		nextInput++
	}

	filters := make([]string, 0, 6)
	last := "[0:v]"
	if assPath != "" {
		filters = append(filters, last+"ass="+escapeFilterPath(assPath)+"[vsub]")
		last = "[vsub]"
	}
	if overlayIndex >= 0 {
		filters = append(filters, fmt.Sprintf("[%d:v]scale=iw*0.16:-1[wm]", overlayIndex))
		filters = append(filters, last+"[wm]overlay=W-w-24:H-h-24[vout]")
		last = "[vout]"
	}
	if len(filters) == 0 {
		return errors.New("没有可应用的滤镜（字幕与模板资产均未就绪）")
	}
	if last != "[vout]" {
		filters = append(filters, last+"null[vout]")
	}
	if bgmIndex >= 0 {
		filters = append(filters, fmt.Sprintf("[%d:a]volume=0.22[bg]", bgmIndex))
		filters = append(filters, "[0:a][bg]amix=inputs=2:duration=first:dropout_transition=0[aout]")
	}

	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[vout]")
	if bgmIndex >= 0 {
		args = append(args, "-map", "[aout]", "-shortest")
	} else {
		args = append(args, "-map", "0:a?")
	}
	args = append(args,
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart",
		output,
	)
	return runCommand(ctx, ffmpeg, args...)
}

// composeVideo 合成阶段（§6）：拼接 → 混流 → 烧录字幕 → 成片。
func composeVideo(ctx context.Context, workDir, taskID string, cfg VideoRunConfig, run *VideoRunContext) (VideoOutput, []string, error) {
	warnings := make([]string, 0, 3)
	output := VideoOutput{FinishedAt: time.Now().Format(time.RFC3339)}

	outputDir := filepath.Join(workDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return output, warnings, fmt.Errorf("创建输出目录失败: %w", err)
	}

	// 字幕默认关闭：未开启时不准备字幕文件（时间轴要等片段就位才能实测，放到后面处理）

	selected := make([]composeClip, 0, len(run.Renders))
	for _, render := range run.Renders {
		if render.Status == "succeeded" && fileExists(render.VideoURI) {
			selected = append(selected, composeClip{SegmentID: render.SegmentID, Path: render.VideoURI})
		}
	}
	if len(selected) == 0 {
		return output, warnings, errors.New("没有可合成的视频片段")
	}
	clips := make([]string, 0, len(selected))
	for _, clip := range selected {
		clips = append(clips, clip.Path)
	}
	// 段 ID → 实际入片文件；冻帧裁剪会替换开头冻帧较多的段，字幕测时要用裁剪后的文件
	composedBySegment := make(map[string]string, len(selected))
	for _, clip := range selected {
		composedBySegment[clip.SegmentID] = clip.Path
	}

	ffmpeg, hasFFmpeg := ffmpegPath()
	if !hasFFmpeg {
		output.MergedPath = clips[0]
		output.Note = "未检测到 ffmpeg：已保留分段视频，跳过拼接与字幕烧录"
		warnings = append(warnings, output.Note)
		return output, warnings, nil
	}

	merged := clips[0]
	if len(clips) > 1 {
		// 尾帧延续下服务商常在下一段开头重放条件尾帧并冻住几帧；逐帧 SSIM 比对、删冻帧后再拼，接缝才不顿
		if cfg.ContinuationMode == "last_frame" {
			freezeCtx, cancelFreeze := context.WithTimeout(ctx, 20*time.Minute)
			var freezeWarnings []string
			clips, composedBySegment, freezeWarnings = trimFrozenClipHeads(freezeCtx, ffmpeg, workDir, outputDir, selected, cfg.Fps)
			cancelFreeze()
			warnings = append(warnings, freezeWarnings...)
		}
		merged = filepath.Join(outputDir, "merged.mp4")
		concatCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		err := concatVideoClips(concatCtx, clips, merged)
		cancel()
		if err != nil {
			return output, warnings, fmt.Errorf("拼接视频失败: %w", err)
		}
		warnings = append(warnings, "多段拼接使用硬切（concat）；尾帧延续已在生成阶段完成")
	}
	output.MergedPath = merged

	// 画幅兜底：服务商可能按素材自身比例出片（方舟首帧任务尤其如此），
	// 不在这里归一，用户选 9:16 拿到的就永远是 16:9。
	canvas := merged
	canvasWidth, canvasHeight := 0, 0
	if info, probeErr := mediaProbe(ctx, merged); probeErr != nil {
		warnings = append(warnings, "读取成片画幅失败，已保留素材原始尺寸: "+probeErr.Error())
	} else {
		canvasWidth, canvasHeight = info.Width, info.Height
	}
	if canvasWidth > 0 && canvasHeight > 0 {
		sourceWidth, sourceHeight := canvasWidth, canvasHeight
		scaled := filepath.Join(outputDir, "scaled.mp4")
		width, height, applied, err := normalizeAspectCanvas(ctx, merged, scaled, cfg.AspectRatio)
		if err != nil {
			warnings = append(warnings, err.Error())
		} else if applied {
			canvas = scaled
			canvasWidth, canvasHeight = width, height
			warnings = append(warnings, fmt.Sprintf(
				"素材画幅 %dx%d 与任务画幅 %s 不一致，已在合成阶段统一为 %dx%d（模糊填充）",
				sourceWidth, sourceHeight, cfg.AspectRatio, width, height,
			))
		} else {
			warnings = append(warnings, fmt.Sprintf("画幅已符合任务设定：%s（%dx%d）", cfg.AspectRatio, canvasWidth, canvasHeight))
		}
	}

	if cfg.Subtitles {
		spans, spanErr := measuredSubtitleSpans(ctx, run.Plan, run.Renders, composedBySegment)
		timingSource := "measured"
		var cues []VideoSubtitleCue
		audioSource := ""
		if spanErr != nil {
			if run.Plan == nil {
				warnings = append(warnings, "生成字幕文件失败: "+spanErr.Error())
			} else {
				cues = buildSubtitleCuesFromSpans(cfg, plannedSubtitleSpans(*run.Plan))
				timingSource = "estimate"
				warnings = append(warnings, "无法按片段实测时长对齐字幕，已沿用预算时间轴: "+spanErr.Error())
			}
		} else {
			cues = buildSubtitleCuesFromSpans(cfg, spans)
			// 声学对齐：成片画布提取音频 → 说话区间检测 → 台词只铺在真有声音的地方
			if aligned, ok, message := alignCuesToSpeechActivity(ctx, ffmpeg, workDir, canvas, cfg, spans); ok {
				cues = aligned
				timingSource = "aligned"
				audioSource = canvas
				warnings = append(warnings, fmt.Sprintf("字幕已按音频活动对齐：%s，共 %d 条", message, len(cues)))
			} else {
				warnings = append(warnings, "字幕声学对齐未生效，按片段测时轴: "+message)
			}
		}
		if len(cues) > 0 {
			subtitleWidth, subtitleHeight := canvasWidth, canvasHeight
			if subtitleWidth <= 0 || subtitleHeight <= 0 {
				subtitleWidth, subtitleHeight = aspectResolution(cfg.AspectRatio)
			}
			subtitles, err := writeSubtitleFiles(workDir, taskID, cfg, cues, timingSource, audioSource, subtitleWidth, subtitleHeight)
			if err != nil {
				warnings = append(warnings, "生成字幕文件失败: "+err.Error())
			} else {
				run.Subtitles = subtitles
				if timingSource == "measured" {
					warnings = append(warnings, fmt.Sprintf(
						"字幕时间轴已按 %d 个片段的实际时长重排（共 %d 条）",
						len(spans), len(subtitles.Cues),
					))
				}
			}
		}
	}

	final := filepath.Join(outputDir, "final.mp4")
	finalCtx, cancelFinal := context.WithTimeout(ctx, 45*time.Minute)
	rendered, burned, finalWarnings := finalizeVideo(finalCtx, ffmpeg, canvas, final, run)
	cancelFinal()
	warnings = append(warnings, finalWarnings...)

	if !rendered {
		copyErr := runCommand(ctx, ffmpeg, "-y", "-i", canvas, "-c", "copy", "-movflags", "+faststart", final)
		if copyErr != nil {
			reencodeErr := runCommand(ctx, ffmpeg,
				"-y", "-i", canvas,
				"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
				"-c:a", "aac", "-movflags", "+faststart",
				final,
			)
			if reencodeErr != nil {
				return output, warnings, fmt.Errorf("输出成片失败: %w", reencodeErr)
			}
		}
	}

	output.FinalPath = final
	output.BurnSubtitles = burned
	output.MediaURL = videoMediaURL(taskID, "output/final.mp4")
	if !burned {
		if cfg.Subtitles {
			output.Note = "成片未烧录字幕（字幕文件已生成）"
		} else {
			output.Note = "字幕已关闭：成片不含字幕，需要时可在新建任务时勾选「生成字幕」"
		}
	}
	return output, warnings, nil
}
