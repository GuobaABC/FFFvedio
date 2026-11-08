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

/* ---------------- 冻帧检测与裁剪：段头重放帧 + 段尾档位填充帧 ---------------- */

// ssimFreezeThreshold：开头帧与上一段尾帧的 SSIM 高于此值＝视觉几乎相同（模型还在重放条件尾帧），
// 标记为应删帧；一旦降到阈值以下说明真实运动恢复，从该帧起保留。
const ssimFreezeThreshold = 0.995

// freezeScanMaxFrames：每个接缝/每段尾部最多比对这么多帧。冻帧通常在 2 秒上下，
// 30 帧（约 1.25s）的旧窗口会把超窗的重放与档位填充当成创作内容原样进成片，
// 放宽到 60 帧（约 2.5s）；超出窗口仍“冻”的按创作内容（如定格收尾）处理，不再多删。
const freezeScanMaxFrames = 60

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
	// SkipHeadFreeze 用户显式设了入点时，起点由用户定，不再自动删段头冻帧
	SkipHeadFreeze bool
	// SkipTailFreeze 用户显式设了出点时，终点由用户定，不再自动删段尾冻帧
	SkipTailFreeze bool
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
		if clip.SkipHeadFreeze {
			continue
		}
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

// trimClipRange 按用户入/出点裁剪源片段：输入侧 -ss 保证帧精确，音视频同步重编码。
// 裁剪后文件再参与冻帧检测/拼接，字幕测时也以它为准。
func trimClipRange(ctx context.Context, ffmpeg, input, output string, inPoint, outPoint float64) error {
	if inPoint < 0 {
		inPoint = 0
	}
	duration := outPoint - inPoint
	if duration <= 0 {
		return errors.New("裁剪区间无效")
	}
	return runCommand(ctx, ffmpeg,
		"-y", "-ss", fmt.Sprintf("%.5f", inPoint), "-i", input,
		"-t", fmt.Sprintf("%.5f", duration),
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart",
		output,
	)
}

/* ---------------- 段尾冻帧裁剪：档位夹取多出的空窗常被模型用静止帧收尾填充 ---------------- */

// minTailFreezeFrames：段尾连续静止至少这么多帧才动手裁——裁一次就多一次重编码，
// 肉眼几乎不可见的一两帧不值得。
const minTailFreezeFrames = 6

// countFrozenTailFrames 从片段尾部窗口倒数：逐帧与末帧比 SSIM，返回与末帧「视觉相同」
// 的连续帧数（不含末帧本身——末帧留着当画面收尾）。fps 是按素材实测帧率归一后的值，
// 调用方拿它换算保留时长。无法判定（抽帧失败/没有可比帧）以错误返回，由调用方跳过。
func countFrozenTailFrames(ctx context.Context, ffmpeg, clipPath, scanDir string, defaultFps int) (int, mediaInfo, float64, error) {
	info, err := mediaProbe(ctx, clipPath)
	if err != nil {
		return 0, info, 0, err
	}
	fps := info.FPS
	if fps <= 0 {
		if defaultFps > 0 {
			fps = float64(defaultFps)
		} else {
			fps = 24
		}
	}
	if err := os.RemoveAll(scanDir); err != nil {
		return 0, info, fps, err
	}
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		return 0, info, fps, err
	}
	// 只取尾部窗口抽帧：-sseof 从片尾倒数，窗口按实测帧率折算，短素材夹到整段
	window := float64(freezeScanMaxFrames)/fps + 1
	if info.Duration > 0 && window > info.Duration {
		window = info.Duration
	}
	if err := runCommand(ctx, ffmpeg,
		"-y", "-sseof", fmt.Sprintf("%.3f", -window), "-i", clipPath,
		"-vsync", "0", "-q:v", "2", filepath.Join(scanDir, "f%03d.png"),
	); err != nil {
		return 0, info, fps, fmt.Errorf("抽取尾部帧失败: %w", err)
	}
	frames, err := filepath.Glob(filepath.Join(scanDir, "f*.png"))
	if err != nil || len(frames) < 2 {
		return 0, info, fps, errors.New("没有可比的尾部帧")
	}
	tailFrame := frames[len(frames)-1]
	discard := 0
	for index := len(frames) - 2; index >= 0 && discard < freezeScanMaxFrames; index-- {
		value, err := frameSSIM(ctx, ffmpeg, frames[index], tailFrame)
		if err != nil {
			return 0, info, fps, err
		}
		if value <= ssimFreezeThreshold {
			break // 静止起点找到了：这一帧往后是真实运动
		}
		discard++
	}
	return discard, info, fps, nil
}

// trimTailFrames 去掉片段末尾 count 帧：-t 截到保留时长并重编码，音视频同步截断，
// 字幕测时按裁剪后文件实测时长走，不会和画面对不上。
func trimTailFrames(ctx context.Context, ffmpeg, input, output string, count int, fps, duration float64) error {
	if fps <= 0 {
		fps = 24
	}
	keep := duration - float64(count)/fps
	if keep <= 0 {
		return errors.New("裁剪后没有剩余画面")
	}
	return runCommand(ctx, ffmpeg,
		"-y", "-i", input, "-t", fmt.Sprintf("%.5f", keep),
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", output,
	)
}

// trimFrozenClipTails 逐段检测并裁掉尾部冻帧，返回拼接清单、段 ID → 实际入片文件与说明。
// 与段头检测不同，这里不限衔接模式也不限段数：nearestAllowedDuration 把段长夹到模型档位
// 后多出来的空窗跟衔接方式无关，单段任务一样会被静止帧填充。任一判定失败只跳过该段
// （保持原样入片），不阻断合成。
func trimFrozenClipTails(ctx context.Context, ffmpeg, outputDir string, clips []composeClip, defaultFps int) ([]string, map[string]string, []string) {
	paths := make([]string, len(clips))
	composed := make(map[string]string, len(clips))
	for index, clip := range clips {
		paths[index] = clip.Path
		composed[clip.SegmentID] = clip.Path
	}
	warnings := make([]string, 0, len(clips))
	scanBase := filepath.Join(outputDir, "tail-scan")
	_ = os.RemoveAll(scanBase)
	for index, clip := range clips {
		if clip.SkipTailFreeze {
			continue
		}
		select {
		case <-ctx.Done():
			warnings = append(warnings, "段尾冻帧裁剪超时，剩余片段按原样入片")
			return paths, composed, warnings
		default:
		}
		discard, info, fps, err := countFrozenTailFrames(ctx, ffmpeg, clip.Path,
			filepath.Join(scanBase, clip.SegmentID), defaultFps)
		if err != nil || discard < minTailFreezeFrames || info.Duration <= 0 {
			continue // 无法判定或静止太短：保持原样
		}
		// 窗口内整段都冻＝创作者要的画面（如定格收尾），绝不删空
		totalFrames := int(math.Round(info.Duration * fps))
		if discard >= totalFrames-1 {
			warnings = append(warnings, fmt.Sprintf("第 %d 段整段静止，按创作内容保留原样", index+1))
			continue
		}
		if info.Duration-float64(discard)/fps < 1 {
			continue // 裁完不足 1 秒，宁可留着冻帧
		}
		trimmed := filepath.Join(outputDir, fmt.Sprintf("trim-tail-%s.mp4", clip.SegmentID))
		if err := trimTailFrames(ctx, ffmpeg, clip.Path, trimmed, discard, fps, info.Duration); err != nil {
			warnings = append(warnings, fmt.Sprintf("第 %d 段段尾冻帧裁剪失败，按原样入片: %v", index+1, err))
			continue
		}
		paths[index] = trimmed
		composed[clip.SegmentID] = trimmed
		warnings = append(warnings, fmt.Sprintf(
			"第 %d 段末尾 %d 帧与末帧 SSIM>%.3f（档位多出的时长被静止帧填充），已裁掉，末帧保留收尾",
			index+1, discard, ssimFreezeThreshold,
		))
	}
	_ = os.RemoveAll(scanBase)
	return paths, composed, warnings
}

// syncPlanWithComposedDurations 按实际入片文件时长重写计划时间轴：
// nearestAllowedDuration 的档位夹取让计划段长虚高，冻帧裁剪又让实际入片更短，
// 预算时间轴（plannedSubtitleSpans 兜底）与任务详情都读这份计划，不跟着修正就一直偏高。
// 每段 DurationS 保留档位值不动：重试重渲仍按档位提交，厂商只认档位。
func syncPlanWithComposedDurations(ctx context.Context, run *VideoRunContext, composedBySegment map[string]string) []string {
	if run == nil || run.Plan == nil || len(run.Plan.Segments) == 0 || len(composedBySegment) == 0 {
		return nil
	}
	// 时间轴顺序以剪辑计划为准；Plan.Segments 数组顺序保持不动，断点续跑与单段重渲仍按原顺序
	order := make([]string, 0, len(run.Plan.Segments))
	if run.Edit != nil && len(run.Edit.Clips) > 0 {
		for _, clip := range orderedEditClips(run.Edit.Clips) {
			if clip.Enabled {
				order = append(order, clip.SegmentID)
			}
		}
	} else {
		for _, segment := range run.Plan.Segments {
			order = append(order, segment.ID)
		}
	}
	byID := make(map[string]*VideoSegment, len(run.Plan.Segments))
	for index := range run.Plan.Segments {
		byID[run.Plan.Segments[index].ID] = &run.Plan.Segments[index]
	}
	cursor, changed := 0.0, false
	for _, segmentID := range order {
		segment, ok := byID[segmentID]
		if !ok {
			continue
		}
		duration := segment.End - segment.Start
		if path, ok := composedBySegment[segmentID]; ok && fileExists(path) {
			if info, err := mediaProbe(ctx, path); err == nil && info.Duration > 0 {
				if math.Abs(info.Duration-duration) >= 0.05 {
					changed = true
				}
				duration = info.Duration
			}
		}
		if math.Abs(segment.Start-cursor) >= 0.01 {
			changed = true
		}
		segment.Start = round2(cursor)
		segment.End = round2(cursor + duration)
		cursor += duration
	}
	if math.Abs(run.Plan.TotalS-cursor) >= 0.01 {
		changed = true
	}
	if !changed {
		return nil
	}
	run.Plan.TotalS = round2(cursor)
	return []string{fmt.Sprintf(
		"已按实际入片时长修正计划时间轴：总时长 %.1fs（段时长档位值保留，重渲仍按档位提交）",
		run.Plan.TotalS,
	)}
}

// concatVideoClips 多段拼接：先流拷贝（无损快），失败或成片实测时长对不上再统一重编码。
// 教训：-c copy 是容器级操作、根本不解码，分段参数/时间戳异常时不会报错，
// 产物“成功”但播放器在坏点冻住最后一帧、总时长虚高（线上 bug：3×8s 拼出 1:38 定格）。
// 所以判据不能只看命令退出码，必须拿各段实测时长之和对账成片。
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

	expected := 0.0
	for _, clip := range clips {
		if info, err := mediaProbe(ctx, clip); err == nil && info.Duration > 0 {
			expected += info.Duration
		}
	}

	copyErr := runCommand(ctx, ffmpeg, "-y", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", output)
	if copyErr == nil && concatDurationSane(ctx, output, expected) {
		return nil
	}
	if err := runCommand(ctx, ffmpeg,
		"-y", "-f", "concat", "-safe", "0", "-i", listPath,
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast",
		"-pix_fmt", "yuv420p", "-c:a", "aac",
		output,
	); err != nil {
		if copyErr != nil {
			return copyErr
		}
		return err
	}
	if !concatDurationSane(ctx, output, expected) {
		return fmt.Errorf("拼接产物时长与分段实测之和相差过大（预期 %.1fs）：分段文件的时间戳或编码参数可能有问题，请重新渲染对应段", expected)
	}
	return nil
}

// concatDurationSane 成片实测时长 ≈ 各段实测之和才算正常；测不出时不阻断（后续环节还有防线）。
// 容差取 0.5s 与 2% 的较大值：AAC priming 每段只多零点几秒，时间戳空洞则是秒级跳变。
func concatDurationSane(ctx context.Context, merged string, expected float64) bool {
	if expected <= 0 {
		return true
	}
	info, err := mediaProbe(ctx, merged)
	if err != nil || info.Duration <= 0 {
		return true
	}
	return math.Abs(info.Duration-expected) <= math.Max(0.5, expected*0.02)
}

// normalizeClipsBeforeConcat 拼接前把各段统一到同一画布/帧率/音频有无。
// 方舟首帧任务跟随素材自身比例出段，last_frame 链上任一段都可能与别的段尺寸不同；
// 参数不齐就 -c copy 硬拼正是“前 8 秒正常、后面定格”的温床。
// 实测参数完全一致时不重编码（快路径不变），有差异才逐段归一。
func normalizeClipsBeforeConcat(ctx context.Context, ffmpeg, outputDir string, clips []composeClip, cfg VideoRunConfig) ([]string, map[string]string, []string) {
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
	infos := make([]mediaInfo, len(clips))
	for index, clip := range clips {
		info, err := mediaProbe(ctx, clip.Path)
		if err != nil || info.Width <= 0 || info.Height <= 0 {
			reason := "没能读出画面尺寸"
			if err != nil {
				reason = err.Error()
			}
			return paths, composed, append(warnings, "分段参数实测失败，按原样拼接："+reason)
		}
		infos[index] = info
	}
	// 公共画布以第一段为基准换算（与拼接后归一的算法一致，单段/多段结果不会跳变）
	width, height, _ := targetCanvas(cfg.AspectRatio, infos[0].Width, infos[0].Height)
	if width <= 0 || height <= 0 {
		width, height = infos[0].Width, infos[0].Height
	}
	fps := infos[0].FPS
	if fps <= 0 {
		for _, info := range infos {
			if info.FPS > 0 {
				fps = info.FPS
				break
			}
		}
	}
	if fps <= 0 {
		fps = 24
		if cfg.Fps > 0 {
			fps = float64(cfg.Fps)
		}
	}
	needNormalize := false
	for _, info := range infos {
		if info.Width != width || info.Height != height ||
			(info.FPS > 0 && math.Abs(info.FPS-fps) > 0.5) ||
			info.HasAudio != infos[0].HasAudio {
			needNormalize = true
			break
		}
	}
	if !needNormalize {
		return paths, composed, warnings
	}
	for index, clip := range clips {
		normalized := filepath.Join(outputDir, fmt.Sprintf("norm-%s.mp4", clip.SegmentID))
		if err := normalizeClipToCanvas(ctx, ffmpeg, clip.Path, normalized, width, height, fps, infos[index]); err != nil {
			// 归一失败不阻断：保持原样入拼，拼接环节还有实测时长防线
			warnings = append(warnings, fmt.Sprintf("第 %d 段统一画幅失败，按原样拼接（接缝可能卡顿）: %v", index+1, err))
			continue
		}
		paths[index] = normalized
		composed[clip.SegmentID] = normalized
	}
	warnings = append(warnings, fmt.Sprintf(
		"分段画幅/帧率/音轨不一致，已统一为 %dx%d@%.6gfps 后再拼接（避免接缝定格）", width, height, fps))
	return paths, composed, warnings
}

// normalizeClipToCanvas 把一段素材重编码到公共画布：尺寸不同用模糊填充兜底（与整片归一同款），
// 帧率统一 -r，无音轨的段补静音轨（有/无音轨混拼时 -c copy 会坏）。
func normalizeClipToCanvas(ctx context.Context, ffmpeg, input, output string, width, height int, fps float64, info mediaInfo) error {
	args := []string{"-y", "-i", input}
	vmap := "0:v"
	amap := "0:a?"
	var filters []string
	if info.Width != width || info.Height != height {
		filters = append(filters, fmt.Sprintf(
			"[0:v]split=2[bg][fg];"+
				"[bg]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=20:2[bgb];"+
				"[fg]scale=%d:%d:force_original_aspect_ratio=decrease[fgb];"+
				"[bgb][fgb]overlay=(W-w)/2:(H-h)/2,setsar=1[vout]",
			width, height, width, height, width, height,
		))
		vmap = "[vout]"
	}
	if !info.HasAudio {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=44100")
		amap = "1:a"
	}
	if len(filters) > 0 {
		args = append(args, "-filter_complex", filters[0])
	}
	args = append(args, "-map", vmap, "-map", amap)
	if info.Duration > 0 {
		// 封顶到实测时长：无限长的静音源要跟着视频停
		args = append(args, "-t", fmt.Sprintf("%.5f", info.Duration))
	}
	args = append(args,
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%.6g", fps),
		"-c:a", "aac", "-ar", "44100", "-movflags", "+faststart", output,
	)
	return runCommand(ctx, ffmpeg, args...)
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

// minEditClipS 一个片段裁剪后至少保留的时长；再短就没有可看的画面。
const minEditClipS = 0.25

// renderBySegment 索引成功且文件仍在的渲染结果。
func renderBySegment(run *VideoRunContext) map[string]VideoRender {
	out := make(map[string]VideoRender, len(run.Renders))
	for _, render := range run.Renders {
		if render.Status == "succeeded" && fileExists(render.VideoURI) {
			out[render.SegmentID] = render
		}
	}
	return out
}

// orderedEditClips 按 Order 排序的剪辑片段副本；Order 相同的保持原顺序。
func orderedEditClips(clips []VideoEditClip) []VideoEditClip {
	out := make([]VideoEditClip, len(clips))
	copy(out, clips)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// clipWindow 把入出点规范成 [in, out) 窗口；OutPoint≤0 或超出源时长视为到片尾。
func clipWindow(clip VideoEditClip, sourceDuration float64) (in, out float64, ok bool) {
	if sourceDuration <= 0 {
		return 0, 0, false
	}
	in = clampFloat(round2(clip.InPoint), 0, math.Max(0, sourceDuration-minEditClipS))
	out = round2(clip.OutPoint)
	if out <= 0 || out > sourceDuration {
		out = round2(sourceDuration)
	}
	if out-in < minEditClipS {
		return in, out, false
	}
	return in, out, true
}

// sourceDurationOf 取片段源时长：优先 clip 里已 probe 的值，缺失时现场 probe/退回渲染记录。
func sourceDurationOf(ctx context.Context, clip VideoEditClip, render VideoRender) float64 {
	if clip.SourceDurationS > 0 {
		return round2(clip.SourceDurationS)
	}
	if info, err := mediaProbe(ctx, render.VideoURI); err == nil && info.Duration > 0 {
		return round2(info.Duration)
	}
	if render.DurationS > 0 {
		return round2(render.DurationS)
	}
	return 0
}

// normalizeVideoEditClips 校验并规范化剪辑计划：片段必须仍可读、入出点合法、无重复。
// last_frame 续接模式下顺序必须与渲染顺序一致，防止破坏尾帧衔接。
func normalizeVideoEditClips(ctx context.Context, cfg VideoRunConfig, run *VideoRunContext, clips []VideoEditClip, strictOrder bool) ([]VideoEditClip, []string, error) {
	warnings := make([]string, 0, 2)
	if len(clips) == 0 {
		return nil, warnings, errors.New("剪辑计划为空，至少保留一个片段")
	}
	renders := renderBySegment(run)
	ordered := orderedEditClips(clips)
	seen := make(map[string]bool, len(ordered))
	out := make([]VideoEditClip, 0, len(ordered))
	for _, clip := range ordered {
		segmentID := strings.TrimSpace(clip.SegmentID)
		if segmentID == "" {
			return nil, warnings, errors.New("剪辑片段缺少 segmentId")
		}
		if seen[segmentID] {
			return nil, warnings, fmt.Errorf("片段 %s 重复出现", segmentID)
		}
		seen[segmentID] = true
		render, ok := renders[segmentID]
		if !ok {
			return nil, warnings, fmt.Errorf("片段 %s 没有可用的渲染文件，请先重新生成该段", segmentID)
		}
		source := sourceDurationOf(ctx, clip, render)
		inPoint, outPoint, ok := clipWindow(clip, source)
		if !ok {
			return nil, warnings, fmt.Errorf("片段 %s 的入/出点无效（入点 %.2fs，出点 %.2fs，源时长 %.2fs）", segmentID, clip.InPoint, clip.OutPoint, source)
		}
		if clip.OutPoint > 0 && math.Abs(clip.OutPoint-source) > 0.05 && clip.OutPoint < source {
			// 用户显式设了出点：保留，但清空依赖自动裁剪的缓存
			warnings = append(warnings, fmt.Sprintf("片段 %s 已按出点 %.2fs 截断", segmentID, outPoint))
		}
		out = append(out, VideoEditClip{
			SegmentID:       segmentID,
			ShotID:          strings.TrimSpace(clip.ShotID),
			Order:           len(out),
			Enabled:         clip.Enabled,
			InPoint:         round2(inPoint),
			OutPoint:        round2(outPoint),
			SourceDurationS: round2(source),
		})
	}
	if strictOrder {
		current := make([]string, 0, len(renders))
		for _, render := range run.Renders {
			if render.Status == "succeeded" && fileExists(render.VideoURI) {
				current = append(current, render.SegmentID)
			}
		}
		submitted := make([]string, 0, len(out))
		for _, clip := range out {
			submitted = append(submitted, clip.SegmentID)
		}
		if strings.Join(current, "|") != strings.Join(submitted, "|") {
			return nil, warnings, errors.New("尾帧延续模式下不支持调整片段顺序；如需排序请把续接方式改为硬切或重叠帧")
		}
		// 禁用中间片段会打断上一段尾帧与下一段首帧的衔接：允许，但必须明确提示
		for index, clip := range out {
			if clip.Enabled || index == 0 || index == len(out)-1 {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("片段 %s 已禁用：会打断上一段尾帧与下一段首帧的画面衔接", clip.SegmentID))
		}
	}
	return out, warnings, nil
}

// videoEditChanged 判断两份剪辑计划是否有实质变化（顺序/启停/入出点）。
func videoEditChanged(old, next *VideoEditPlan) bool {
	if old == nil || next == nil {
		return old != next
	}
	if len(old.Clips) != len(next.Clips) {
		return true
	}
	compare := func(a, b []VideoEditClip) (string, bool) {
		aOrdered, bOrdered := orderedEditClips(a), orderedEditClips(b)
		for index := range aOrdered {
			left, right := aOrdered[index], bOrdered[index]
			if left.SegmentID != right.SegmentID || left.Enabled != right.Enabled {
				return left.SegmentID, true
			}
			if math.Abs(left.InPoint-right.InPoint) >= 0.01 || math.Abs(left.OutPoint-right.OutPoint) >= 0.01 {
				return left.SegmentID, true
			}
		}
		return "", false
	}
	_, changed := compare(old.Clips, next.Clips)
	return changed
}

// buildDefaultVideoEditPlan 用成功渲染结果生成默认剪辑计划：渲染顺序、启用、整段保留。
func buildDefaultVideoEditPlan(ctx context.Context, run *VideoRunContext) (*VideoEditPlan, []string, error) {
	renders := renderBySegment(run)
	order := make([]string, 0, len(renders))
	if run.Plan != nil {
		for _, segment := range run.Plan.Segments {
			if _, ok := renders[segment.ID]; ok {
				order = append(order, segment.ID)
			}
		}
	}
	// 计划里没有的（旧任务/临时补渲）按渲染顺序补在后面
	for _, render := range run.Renders {
		if _, ok := renders[render.SegmentID]; !ok {
			continue
		}
		found := false
		for _, id := range order {
			if id == render.SegmentID {
				found = true
				break
			}
		}
		if !found {
			order = append(order, render.SegmentID)
		}
	}
	if len(order) == 0 {
		return nil, nil, errors.New("没有成功且文件可读的视频片段，无法进入剪辑")
	}
	warnings := make([]string, 0, 1)
	clips := make([]VideoEditClip, 0, len(order))
	for index, segmentID := range order {
		render := renders[segmentID]
		duration := sourceDurationOf(ctx, VideoEditClip{SegmentID: segmentID}, render)
		if duration <= 0 {
			warnings = append(warnings, fmt.Sprintf("片段 %s 读不到时长，按渲染记录 %.1fs 处理", segmentID, render.DurationS))
			duration = render.DurationS
		}
		clips = append(clips, VideoEditClip{
			SegmentID:       segmentID,
			ShotID:          render.ShotID,
			Order:           index,
			Enabled:         true,
			InPoint:         0,
			OutPoint:        round2(duration),
			SourceDurationS: round2(duration),
		})
	}
	return &VideoEditPlan{Clips: clips, Revision: 0, UpdatedAt: time.Now().Format(time.RFC3339)}, warnings, nil
}

// editSubtitleSpans 按剪辑计划顺序与入出点生成字幕时间窗。
// 已有同版本画布时用实际入片文件测时；否则按源片段与入出点计算。
func editSubtitleSpans(ctx context.Context, run *VideoRunContext) ([]SubtitleSpan, map[string]SubtitleSpan, error) {
	spans := make([]SubtitleSpan, 0, len(run.Edit.Clips))
	bySegment := make(map[string]SubtitleSpan, len(run.Edit.Clips))
	renders := renderBySegment(run)
	textBySegment := make(map[string]string)
	if run.Plan != nil {
		for _, segment := range run.Plan.Segments {
			textBySegment[segment.ID] = segment.Dialogue
		}
	}
	useComposed := run.Compose != nil && run.Compose.EditRevision == run.Edit.Revision
	offset := 0.0
	for _, clip := range orderedEditClips(run.Edit.Clips) {
		if !clip.Enabled {
			continue
		}
		render, ok := renders[clip.SegmentID]
		if !ok {
			continue
		}
		duration := 0.0
		if useComposed {
			if path, ok := run.Compose.ComposedBySegment[clip.SegmentID]; ok && fileExists(path) {
				if info, err := mediaProbe(ctx, path); err == nil && info.Duration > 0 {
					duration = info.Duration
				}
			}
		}
		if duration <= 0 {
			in, out, ok := clipWindow(clip, sourceDurationOf(ctx, clip, render))
			if !ok {
				continue
			}
			duration = out - in
		}
		hasAudio := false
		if probePath := firstNonEmpty(composedPath(run, clip.SegmentID), render.VideoURI); probePath != "" {
			if info, err := mediaProbe(ctx, probePath); err == nil {
				hasAudio = info.HasAudio
			}
		}
		span := SubtitleSpan{
			SegmentID:  clip.SegmentID,
			Start:      round2(offset),
			Duration:   round2(duration),
			Text:       textBySegment[clip.SegmentID],
			FillWindow: hasAudio,
		}
		spans = append(spans, span)
		bySegment[clip.SegmentID] = span
		offset += duration
	}
	if len(spans) == 0 {
		return nil, nil, errors.New("剪辑计划里没有启用的片段")
	}
	return spans, bySegment, nil
}

// composedPath 取当前版本画布里的入片文件；版本不一致时返回空。
func composedPath(run *VideoRunContext, segmentID string) string {
	if run == nil || run.Compose == nil {
		return ""
	}
	if run.Edit != nil && run.Compose.EditRevision != run.Edit.Revision {
		return ""
	}
	return run.Compose.ComposedBySegment[segmentID]
}

// composeEditClips 把剪辑计划翻译成待拼接片段：按入出点裁剪，标出需要跳过冻帧检测的一侧。
func composeEditClips(ctx context.Context, ffmpeg, outputDir string, run *VideoRunContext) ([]composeClip, []string, error) {
	if run.Edit == nil || len(run.Edit.Clips) == 0 {
		return nil, nil, errors.New("缺少剪辑计划")
	}
	renders := renderBySegment(run)
	clips := make([]composeClip, 0, len(run.Edit.Clips))
	warnings := make([]string, 0, 1)
	for _, editClip := range orderedEditClips(run.Edit.Clips) {
		if !editClip.Enabled {
			continue
		}
		render, ok := renders[editClip.SegmentID]
		if !ok {
			continue
		}
		source := sourceDurationOf(ctx, editClip, render)
		in, out, ok := clipWindow(editClip, source)
		if !ok {
			return nil, warnings, fmt.Errorf("片段 %s 的入/出点无效", editClip.SegmentID)
		}
		trimmed := in > 0.01 || out < source-0.01
		path := render.VideoURI
		if trimmed {
			cut := filepath.Join(outputDir, fmt.Sprintf("cut-%s.mp4", editClip.SegmentID))
			if err := trimClipRange(ctx, ffmpeg, render.VideoURI, cut, in, out); err != nil {
				return nil, warnings, fmt.Errorf("裁剪片段 %s 失败: %w", editClip.SegmentID, err)
			}
			path = cut
		}
		clips = append(clips, composeClip{
			SegmentID:      editClip.SegmentID,
			Path:           path,
			SkipHeadFreeze: in > 0.01,
			SkipTailFreeze: out < source-0.01,
		})
	}
	if len(clips) == 0 {
		return nil, warnings, errors.New("至少保留一个启用的片段")
	}
	return clips, warnings, nil
}

// relayoutSubtitleCues 剪辑变化后重映射字幕：有 SegmentID 的按段内 OffsetS 跟随片段移动，
// 被禁用/删除的段其字幕一并移除；没有 SegmentID 的旧字幕按绝对时间保留，越界逐条告警。
func relayoutSubtitleCues(cues []VideoSubtitleCue, oldSpans, newSpans map[string]SubtitleSpan, total float64, cfg VideoRunConfig) ([]VideoSubtitleCue, []string) {
	warnings := make([]string, 0, 2)
	out := make([]VideoSubtitleCue, 0, len(cues))
	for position, cue := range cues {
		index := cue.Index
		if index <= 0 {
			index = position + 1
		}
		segmentID := strings.TrimSpace(cue.SegmentID)
		if segmentID == "" {
			// 旧数据：按绝对时间保留，越界交给 normalize 逐条处理
			out = append(out, cue)
			continue
		}
		newSpan, ok := newSpans[segmentID]
		if !ok || newSpan.Duration < subtitleCueMinS {
			warnings = append(warnings, fmt.Sprintf("第 %d 条字幕所属片段已禁用或移除，字幕一并移除", index))
			continue
		}
		offset := cue.OffsetS
		if oldSpan, ok := oldSpans[segmentID]; ok {
			offset = cue.Start - oldSpan.Start
		}
		maxOffset := math.Max(0, newSpan.Duration-subtitleCueMinS)
		offset = clampFloat(round2(offset), 0, maxOffset)
		start := round2(newSpan.Start + offset)
		duration := cue.End - cue.Start
		end := round2(start + duration)
		if limit := round2(newSpan.Start + newSpan.Duration); end > limit {
			end = limit
		}
		if end-start < subtitleCueMinS {
			end = round2(math.Min(start+subtitleCueMinS, newSpan.Start+newSpan.Duration))
		}
		if end-start < subtitleCueMinS {
			warnings = append(warnings, fmt.Sprintf("第 %d 条字幕在片段 %s 内放不下，已移除", index, segmentID))
			continue
		}
		cue.Start, cue.End = start, end
		cue.OffsetS = round2(offset)
		out = append(out, cue)
	}
	normalized, issues := normalizeSubtitleCues(out, total, cfg)
	for _, issue := range issues {
		if issue.Level == "error" {
			warnings = append(warnings, issue.Message)
		}
	}
	return normalized, warnings
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
		parts := splitDialogueSentences(text, cfg.Language)
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
				start := round2(span.Start + startLocal)
				cues = append(cues, VideoSubtitleCue{
					Index:     len(cues) + 1,
					Start:     start,
					End:       round2(span.Start + display),
					Text:      wrapSubtitleText(part, cfg.Language),
					SegmentID: span.SegmentID,
					OffsetS:   round2(start - span.Start),
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
func burnSubtitles(ctx context.Context, ffmpeg, input, assPath, output, fontDir string) error {
	if assPath == "" || !fileExists(assPath) {
		return errors.New("缺少 ASS 字幕文件")
	}
	return runCommand(ctx, ffmpeg,
		"-y", "-i", input,
		"-vf", assFilter(assPath, fontDir),
		"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "copy",
		"-movflags", "+faststart",
		output,
	)
}

// assFilter 拼接 ass 滤镜；fontDir 非空时指到内置字体目录，跨机器不依赖系统安装。
func assFilter(assPath, fontDir string) string {
	filter := "ass=" + escapeFilterPath(assPath)
	if strings.TrimSpace(fontDir) != "" {
		filter += ":fontsdir=" + escapeFilterPath(fontDir)
	}
	return filter
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

// assEscapeText 把用户文本转成 ASS 安全文本：{} 与反斜杠映射成全角等价字符，
// 换行按行拆开再用 \N 连接。顺序必须是「先按行拆、再逐行转义」，否则 \N 会被二次处理。
func assEscapeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		line = strings.ReplaceAll(line, `\`, "＼")
		line = strings.ReplaceAll(line, "{", "｛")
		line = strings.ReplaceAll(line, "}", "｝")
		lines[index] = line
	}
	return strings.Join(lines, `\N`)
}

// renderSRT 文本保持原文，只替换会破坏 SRT 结构的箭头。
func renderSRT(cues []VideoSubtitleCue) string {
	var builder strings.Builder
	for index, cue := range cues {
		text := strings.ReplaceAll(cue.Text, "-->", "→")
		builder.WriteString(fmt.Sprintf("%d\n", index+1))
		builder.WriteString(fmt.Sprintf("%s --> %s\n", srtTime(cue.Start), srtTime(cue.End)))
		builder.WriteString(text)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

// renderASS PlayRes 与视频分辨率一致，字号随分辨率线性缩放（§6.3）。
// 逐条字幕可覆盖对齐锚点（\an）、垂直留白（Dialogue MarginV）与字号（\fs）。
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
		text := assEscapeText(cue.Text)
		// 逐条位置：没写锚点就沿用全局样式；写了就用 Dialogue 的 MarginV 字段推到底部/顶部对应距离
		cueMarginV := marginV
		overrides := ""
		if cue.Anchor >= 1 && cue.Anchor <= 9 {
			overrides += fmt.Sprintf(`\an%d`, cue.Anchor)
		}
		if cue.MarginVPct > 0 {
			cueMarginV = math.Max(0, cue.MarginVPct/100*float64(height))
		}
		if cue.FontSizePct > 0 {
			overrides += fmt.Sprintf(`\fs%.0f`, math.Max(10, cue.FontSizePct/100*float64(height)))
		}
		if overrides != "" {
			text = "{" + overrides + "}" + text
		}
		builder.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Default,,0,0,%.0f,,%s\n", assTime(cue.Start), assTime(cue.End), cueMarginV, text))
	}
	return builder.String()
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

	fontDir := ""
	if assPath != "" {
		fontDir = bundledFontDir()
		if fontDir == "" {
			warnings = append(warnings, "内置字幕字体解压失败，已退回系统字体；若出现方块/缺字请检查工作目录权限")
		}
	}

	if err := renderFinalWithAssets(ctx, ffmpeg, input, output, assPath, overlayPath, bgmPath, fontDir); err == nil {
		return true, assPath != "", warnings
	} else {
		warnings = append(warnings, "字幕/模板资产一次渲染失败，已降级为逐项处理: "+err.Error())
	}

	// 降级一：只烧字幕（模板资产放弃，但成片仍可交付）
	if assPath != "" {
		if err := burnSubtitles(ctx, ffmpeg, input, assPath, output, fontDir); err == nil {
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
func renderFinalWithAssets(ctx context.Context, ffmpeg, input, output, assPath, overlayPath, bgmPath, fontDir string) error {
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
		filters = append(filters, last+assFilter(assPath, fontDir)+"[vsub]")
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

// composePrepUsable 判断缓存的预处理产物是否还能用：画布与每个入片文件都必须存在，
// 剪辑版本一致，且入片段集合与当前启用的剪辑计划一致（重渲/改入出点就得重新拼）。
func composePrepUsable(prep *VideoComposePrep, run *VideoRunContext) bool {
	if prep == nil || !prep.HasFFmpeg || prep.CanvasPath == "" || len(prep.ComposedBySegment) == 0 {
		return false
	}
	if !fileExists(prep.CanvasPath) {
		return false
	}
	revision := editRevisionOf(run)
	if prep.EditRevision != revision {
		return false
	}
	// 新版本字段：有剪辑计划时必须知道画布时长，否则前端时间轴没有上限
	if run != nil && run.Edit != nil && prep.DurationS <= 0 {
		return false
	}
	for _, path := range prep.ComposedBySegment {
		if !fileExists(path) {
			return false
		}
	}
	expected := make(map[string]bool, len(prep.ComposedBySegment))
	if run != nil && run.Edit != nil && len(run.Edit.Clips) > 0 {
		renders := renderBySegment(run)
		for _, clip := range run.Edit.Clips {
			if !clip.Enabled {
				continue
			}
			if _, ok := renders[clip.SegmentID]; !ok {
				continue
			}
			expected[clip.SegmentID] = true
		}
	} else if run != nil {
		for _, render := range run.Renders {
			if render.Status == "succeeded" && fileExists(render.VideoURI) {
				expected[render.SegmentID] = true
			}
		}
	}
	if len(expected) != len(prep.ComposedBySegment) {
		return false
	}
	for id := range expected {
		if _, ok := prep.ComposedBySegment[id]; !ok {
			return false
		}
	}
	return true
}

// editRevisionOf 剪辑计划版本号；没有 Edit 的旧任务按 0 处理。
func editRevisionOf(run *VideoRunContext) int {
	if run != nil && run.Edit != nil {
		return run.Edit.Revision
	}
	return 0
}

// prepareComposeCanvas 合成前的剪辑预处理：选段 → 冻帧裁剪 → 拼接 → 画幅归一。
// 字幕阶段先调用它拿到真实画布（时间轴与用户预览都要对得上），合成阶段复用同一份结果：
// 既省掉一次重编码，也保证用户调好的字幕不会被重新切过的片段带偏。
func prepareComposeCanvas(ctx context.Context, workDir, taskID string, cfg VideoRunConfig, run *VideoRunContext) (*VideoComposePrep, []string, error) {
	warnings := make([]string, 0, 3)
	outputDir := filepath.Join(workDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, warnings, fmt.Errorf("创建输出目录失败: %w", err)
	}

	selected := make([]composeClip, 0, len(run.Renders))
	var selectionWarnings []string
	ffmpeg, hasFFmpeg := ffmpegPath()
	if run.Edit != nil && len(run.Edit.Clips) > 0 {
		clips, editWarnings, err := composeEditClips(ctx, ffmpeg, outputDir, run)
		selectionWarnings = append(selectionWarnings, editWarnings...)
		if err != nil {
			return nil, append(warnings, selectionWarnings...), err
		}
		selected = clips
	} else {
		for _, render := range run.Renders {
			if render.Status == "succeeded" && fileExists(render.VideoURI) {
				selected = append(selected, composeClip{SegmentID: render.SegmentID, Path: render.VideoURI})
			}
		}
	}
	warnings = append(warnings, selectionWarnings...)
	if len(selected) == 0 {
		return nil, warnings, errors.New("没有可合成的视频片段")
	}
	prep := &VideoComposePrep{
		ComposedBySegment: make(map[string]string, len(selected)),
		SegmentIDs:        make([]string, 0, len(selected)),
		EditRevision:      editRevisionOf(run),
		UpdatedAt:         time.Now().Format(time.RFC3339),
	}
	clips := make([]string, 0, len(selected))
	for _, clip := range selected {
		clips = append(clips, clip.Path)
		prep.ComposedBySegment[clip.SegmentID] = clip.Path
		prep.SegmentIDs = append(prep.SegmentIDs, clip.SegmentID)
	}

	if !hasFFmpeg {
		prep.MergedPath = clips[0]
		prep.CanvasPath = clips[0]
		warnings = append(warnings, "未检测到 ffmpeg：已保留分段视频，跳过拼接与字幕烧录")
		return prep, warnings, nil
	}
	prep.HasFFmpeg = true

	composedBySegment := prep.ComposedBySegment
	pending := selected
	if len(pending) > 1 {
		// 尾帧延续下服务商常在下一段开头重放条件尾帧并冻住几帧；逐帧 SSIM 比对、删冻帧后再拼，接缝才不顿。
		// 衔接模式为 none/overlap 时同样走这条检测：0.995 的阈值只有真在重放静止画面才会命中，创作内容不会被误删
		freezeCtx, cancelFreeze := context.WithTimeout(ctx, 20*time.Minute)
		headPaths, headComposed, freezeWarnings := trimFrozenClipHeads(freezeCtx, ffmpeg, workDir, outputDir, pending, cfg.Fps)
		cancelFreeze()
		clips = headPaths
		composedBySegment = headComposed
		rebuilt := make([]composeClip, len(pending))
		for index := range pending {
			rebuilt[index] = composeClip{SegmentID: pending[index].SegmentID, Path: clips[index]}
		}
		pending = rebuilt
		warnings = append(warnings, freezeWarnings...)
	}
	// 段尾冻帧裁剪：分镜被 nearestAllowedDuration 夹到模型档位后，多出的空窗常被静止帧收尾填充；
	// 单段任务和 none 衔接同样会中招，因此不限段数与衔接模式
	tailCtx, cancelTail := context.WithTimeout(ctx, 20*time.Minute)
	var tailWarnings []string
	clips, composedBySegment, tailWarnings = trimFrozenClipTails(tailCtx, ffmpeg, outputDir, pending, cfg.Fps)
	cancelTail()
	warnings = append(warnings, tailWarnings...)

	merged := clips[0]
	if len(clips) > 1 {
		// 拼接前先统一各段参数：-c copy 对参数不齐不报错，产物会在接缝处定格且总时长虚高
		concatInput := make([]composeClip, len(pending))
		for index := range pending {
			concatInput[index] = composeClip{SegmentID: pending[index].SegmentID, Path: clips[index]}
		}
		normCtx, cancelNorm := context.WithTimeout(ctx, 30*time.Minute)
		var normWarnings []string
		clips, composedBySegment, normWarnings = normalizeClipsBeforeConcat(normCtx, ffmpeg, outputDir, concatInput, cfg)
		cancelNorm()
		warnings = append(warnings, normWarnings...)

		merged = filepath.Join(outputDir, "merged.mp4")
		concatCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		err := concatVideoClips(concatCtx, clips, merged)
		cancel()
		if err != nil {
			return nil, warnings, fmt.Errorf("拼接视频失败: %w", err)
		}
		warnings = append(warnings, "多段拼接使用硬切（concat）；尾帧延续已在生成阶段完成")
	}
	prep.MergedPath = merged
	prep.ComposedBySegment = composedBySegment
	// 档位夹取与冻帧裁剪之后，按实际入片时长修正计划时间轴（预算轴与任务详情都读它）
	warnings = append(warnings, syncPlanWithComposedDurations(ctx, run, composedBySegment)...)

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
	prep.CanvasPath = canvas
	prep.CanvasWidth = canvasWidth
	prep.CanvasHeight = canvasHeight
	if info, err := mediaProbe(ctx, canvas); err == nil && info.Duration > 0 {
		prep.DurationS = round2(info.Duration)
	} else {
		total := 0.0
		for _, clip := range selected {
			if info, probeErr := mediaProbe(ctx, clip.Path); probeErr == nil && info.Duration > 0 {
				total += info.Duration
			}
		}
		prep.DurationS = round2(total)
	}
	if rel := relativeUnderWorkDir(workDir, canvas); rel != "" {
		prep.CanvasUrl = videoMediaURL(taskID, rel)
	}
	return prep, warnings, nil
}

// measureSubtitleCues 建字幕时间轴的唯一入口：先用片段实测时长，再尝试声学对齐。
// 测不出时退回预算时间轴（estimate），保证“有台词就有字幕”。
func measureSubtitleCues(ctx context.Context, workDir string, cfg VideoRunConfig, run *VideoRunContext, canvas string) ([]VideoSubtitleCue, string, string, []string) {
	warnings := make([]string, 0, 2)
	composed := map[string]string{}
	if run.Compose != nil {
		composed = run.Compose.ComposedBySegment
	}
	var spans []SubtitleSpan
	var spanErr error
	if run.Edit != nil && len(run.Edit.Clips) > 0 {
		spans, _, spanErr = editSubtitleSpans(ctx, run)
	} else {
		spans, spanErr = measuredSubtitleSpans(ctx, run.Plan, run.Renders, composed)
	}
	if spanErr != nil {
		if run.Plan == nil {
			return nil, "", "", append(warnings, "生成字幕时间轴失败: "+spanErr.Error())
		}
		return buildSubtitleCuesFromSpans(cfg, plannedSubtitleSpans(*run.Plan)), "estimate", "",
			append(warnings, "无法按片段实测时长建时间轴，已沿用预算时间轴: "+spanErr.Error())
	}
	cues := buildSubtitleCuesFromSpans(cfg, spans)
	ffmpeg, hasFFmpeg := ffmpegPath()
	if !hasFFmpeg || canvas == "" {
		return cues, "measured", "", warnings
	}
	// 声学对齐：成片画布提取音频 → 说话区间检测 → 台词只铺在真有声音的地方
	aligned, ok, message := alignCuesToSpeechActivity(ctx, ffmpeg, workDir, canvas, cfg, spans)
	if ok {
		return aligned, "aligned", canvas,
			append(warnings, fmt.Sprintf("字幕已按音频活动对齐：%s，共 %d 条", message, len(aligned)))
	}
	return cues, "measured", "", append(warnings, "字幕声学对齐未生效，按片段测时轴: "+message)
}

// subtitleCanvasSize 字幕 ASS 的 PlayRes：优先用已经算好的成片画布，退而按任务画幅推算。
// 时间轴与预览都必须对得上这一块画布，否则用户调好的位置烧出来会偏。
func subtitleCanvasSize(cfg VideoRunConfig, prep *VideoComposePrep) (int, int) {
	if prep != nil && prep.CanvasWidth > 0 && prep.CanvasHeight > 0 {
		return prep.CanvasWidth, prep.CanvasHeight
	}
	return aspectResolution(cfg.AspectRatio)
}

func composeVideo(ctx context.Context, workDir, taskID string, cfg VideoRunConfig, run *VideoRunContext) (VideoOutput, []string, error) {
	warnings := make([]string, 0, 3)
	output := VideoOutput{FinishedAt: time.Now().Format(time.RFC3339)}

	// 预处理（选段/裁剪/拼接/画幅）优先复用剪辑确认时已经算好的结果，避免二次重编码
	prep := run.Compose
	recomposed := false
	if !composePrepUsable(prep, run) {
		built, prepWarnings, err := prepareComposeCanvas(ctx, workDir, taskID, cfg, run)
		warnings = append(warnings, prepWarnings...)
		if err != nil {
			return output, warnings, err
		}
		prep = built
		run.Compose = prep
		recomposed = true
	}
	if !prep.HasFFmpeg {
		output.MergedPath = prep.MergedPath
		output.Note = "未检测到 ffmpeg：已保留分段视频，跳过拼接与字幕烧录"
		return output, warnings, nil
	}
	output.MergedPath = prep.MergedPath
	canvas := prep.CanvasPath
	ffmpeg, _ := ffmpegPath()

	if cfg.Subtitles {
		subtitleWidth, subtitleHeight := subtitleCanvasSize(cfg, prep)
		if run.Subtitles != nil && run.Subtitles.Confirmed {
			// 用户在剪辑关卡确认过：时间轴与位置以用户为准（包括删成 0 条）。
			// 如果画布因剪辑版本变化刚被重建，先把 cue 按新时间轴重映射，避免烧到旧时间点。
			cues := run.Subtitles.Cues
			if recomposed && run.Edit != nil {
				_, spans, spanErr := editSubtitleSpans(ctx, run)
				if spanErr != nil {
					warnings = append(warnings, "字幕按新剪辑重映射失败，沿用原时间轴: "+spanErr.Error())
				} else {
					relaid, notes := relayoutSubtitleCues(cues, nil, spans, composeTotalS(ctx, cfg, prep), cfg)
					warnings = append(warnings, notes...)
					cues = relaid
				}
			}
			subtitles, err := writeSubtitleFiles(workDir, taskID, cfg, cues,
				firstNonEmpty(run.Subtitles.TimingSource, "measured"), "", subtitleWidth, subtitleHeight)
			if err != nil {
				warnings = append(warnings, "生成字幕文件失败: "+err.Error())
			} else {
				subtitles.Confirmed = true
				subtitles.EditedAt = run.Subtitles.EditedAt
				run.Subtitles = subtitles
				warnings = append(warnings, fmt.Sprintf("按已确认的字幕烧录：%d 条（不再自动重排时间轴）", len(subtitles.Cues)))
			}
		} else {
			cues, timingSource, audioSource, measureWarnings := measureSubtitleCues(ctx, workDir, cfg, run, canvas)
			warnings = append(warnings, measureWarnings...)
			if len(cues) > 0 {
				subtitles, err := writeSubtitleFiles(workDir, taskID, cfg, cues, timingSource, audioSource, subtitleWidth, subtitleHeight)
				if err != nil {
					warnings = append(warnings, "生成字幕文件失败: "+err.Error())
				} else {
					run.Subtitles = subtitles
				}
			}
		}
	}

	final := filepath.Join(filepath.Join(workDir, "output"), "final.mp4")
	finalCtx, cancelFinal := context.WithTimeout(ctx, 45*time.Minute)
	rendered, burned, finalWarnings := finalizeVideo(finalCtx, ffmpeg, canvas, final, run)
	cancelFinal()
	warnings = append(warnings, finalWarnings...)

	if !rendered {
		if copyErr := runCommand(ctx, ffmpeg, "-y", "-i", canvas, "-c", "copy", "-movflags", "+faststart", final); copyErr != nil {
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
		if run.Subtitles != nil && run.Subtitles.Confirmed && len(run.Subtitles.Cues) == 0 {
			output.Note = "已按你的设置移除全部字幕"
		} else if cfg.Subtitles {
			output.Note = "成片未烧录字幕（字幕文件已生成）"
		} else {
			output.Note = "字幕已关闭：成片不含字幕，需要时可在新建任务时勾选「生成字幕」"
		}
	}
	return output, warnings, nil
}
