package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
)

// RunVideoTask 从当前阶段继续执行。确认关卡不允许直接跑，必须先确认或重新生成。
func (a *App) RunVideoTask(id string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	switch task.Stage {
	case VideoStageConfirmScript, VideoStageConfirmPrompt, VideoStageConfirmSubs, VideoStageConfirmMeta:
		return errors.New("当前处于人工确认关卡，请先确认或重新生成")
	case VideoStageDone:
		return errors.New("任务已完成；如需重做请使用「重新生成」")
	case VideoStageRender:
		if task.Run.ReviewPendingSegment != "" {
			return errors.New("当前处于片段确认关卡，请先确认或重新生成当前片段")
		}
	}

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	go func() {
		defer a.endVideoRun(id)
		a.runVideoPipeline(id, task.Stage, "")
	}()
	return nil
}

// ApproveVideoGate 确认关卡并进入下一阶段（贵操作都在人工确认之后）。
func (a *App) ApproveVideoGate(id string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}

	// gate③ 是最后一道关卡：确认即完成，不需要再跑任何自动阶段
	if task.Stage == VideoStageConfirmMeta {
		ensureVideoGates(&task.Run)
		task.Stage = VideoStageDone
		task.Status = VideoStatusDone
		task.Error = ""
		task.Run.Gates[VideoGateMeta] = videoGateApproved
		task.Run.Logs = append(task.Run.Logs, stageLog("发布元数据已确认，任务完成"))
		task.UpdatedAt = time.Now().Format(time.RFC3339)
		return a.updateVideoTaskRecord(task)
	}

	next := ""
	switch task.Stage {
	case VideoStageConfirmScript:
		next = VideoStageBuildPrompt
	case VideoStageConfirmPrompt:
		// 提示词确认后就烧额度出片：字幕排在视频之后，拿真实片段铺轴才对得上画面
		next = VideoStageRender
	case VideoStageConfirmSubs:
		// 字幕确认后合成只负责烧录：把用户调好的时间轴钉成最终版，不再回测重排
		ensureVideoGates(&task.Run)
		if task.Run.Subtitles != nil {
			task.Run.Subtitles.Confirmed = true
			if task.Run.Subtitles.EditedAt == "" {
				task.Run.Subtitles.EditedAt = time.Now().Format(time.RFC3339)
			}
		}
		task.Run.Gates[VideoGateSubs] = videoGateApproved
		task.Run.Logs = append(task.Run.Logs, stageLog("字幕已确认，合成阶段按这份时间轴烧录"))
		task.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := a.updateVideoTaskRecord(task); err != nil {
			return err
		}
		next = VideoStageCompose
	default:
		return errors.New("当前阶段没有待确认的关卡")
	}

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	go func() {
		defer a.endVideoRun(id)
		a.runVideoPipeline(id, next, "")
	}()
	return nil
}

// RegenerateVideoStage 携带用户反馈回到本阶段重出，上游产物保持不变。
func (a *App) RegenerateVideoStage(id, feedback string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}

	from := task.Stage
	switch task.Stage {
	case VideoStageConfirmScript:
		from = VideoStageDraftScript
	case VideoStageConfirmPrompt:
		from = VideoStageBuildPrompt
	case VideoStageConfirmSubs:
		from = VideoStageBuildSubs
	case VideoStageRender, VideoStageCompose, VideoStageDone:
		from = VideoStageRender
	case VideoStageConfirmMeta:
		// gate③ 重生成只回 Agent4，不动视频
		from = VideoStageBuildMeta
	case VideoStageDraftScript, VideoStageBuildPrompt, VideoStageBuildSubs, VideoStageBuildMeta:
		// 保持当前阶段
	default:
		from = VideoStageDraftScript
	}

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	go func() {
		defer a.endVideoRun(id)
		a.runVideoPipeline(id, from, feedback)
	}()
	return nil
}

// RegenerateVideoMeta 只重算发布元数据（含切换发布平台），视频与字幕保持不变（§2.1）。
func (a *App) RegenerateVideoMeta(id string, platforms []string, feedback string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if task.Run.Script == nil || len(task.Run.Script.Shots) == 0 {
		return errors.New("任务还没有分镜，无法生成发布元数据")
	}

	if len(platforms) > 0 {
		normalized := normalizePlatforms(platforms)
		if len(normalized) == 0 {
			return errors.New("请选择至少一个已支持的发布平台")
		}
		task.Config.Platforms = normalized
	}

	ensureVideoGates(&task.Run)
	task.Stage = VideoStageBuildMeta
	task.Status = VideoStatusRunning
	task.Error = ""
	task.Run.Gates[VideoGateMeta] = videoGatePending
	task.UpdatedAt = time.Now().Format(time.RFC3339)

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if err := a.updateVideoTaskRecord(task); err != nil {
		a.endVideoRun(id)
		return err
	}
	go func() {
		defer a.endVideoRun(id)
		a.runVideoPipeline(id, VideoStageBuildMeta, feedback)
	}()
	return nil
}

// ApproveVideoSegment 确认当前待审核片段，继续生成下一段（最后一段确认后进入合成）。
func (a *App) ApproveVideoSegment(id string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if task.Stage != VideoStageRender || task.Run.ReviewPendingSegment == "" {
		return errors.New("当前没有待确认的视频片段")
	}

	task.Run.ReviewPendingSegment = ""
	task.Status = VideoStatusRunning
	task.Error = ""
	task.Run.Logs = append(task.Run.Logs, stageLog("片段已确认，继续生成下一段"))
	task.UpdatedAt = time.Now().Format(time.RFC3339)

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if err := a.updateVideoTaskRecord(task); err != nil {
		a.endVideoRun(id)
		return err
	}
	go func() {
		defer a.endVideoRun(id)
		a.runVideoPipeline(id, VideoStageRender, "")
	}()
	return nil
}

// RegenerateVideoSegment 修改单段提示词/台词后只重渲该段：
// 之前已成功的片段直接复用（断点续跑逻辑），之后的片段因尾帧链失效一并重渲。
// 允许在“待确认”或“渲染失败停在 RENDER_VIDEO”两种状态下调用。
func (a *App) RegenerateVideoSegment(id string, edit VideoSegmentRegen) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if task.Stage != VideoStageRender {
		return errors.New("仅可在视频生成阶段重渲单段")
	}
	if task.Run.Plan == nil || len(task.Run.Plan.Segments) == 0 {
		return errors.New("任务缺少渲染计划，请先确认提示词")
	}

	target := -1
	for index, segment := range task.Run.Plan.Segments {
		if segment.ID == strings.TrimSpace(edit.SegmentID) {
			target = index
			break
		}
	}
	if target < 0 {
		return errors.New("要重生成的片段不在渲染计划中")
	}

	if prompt := strings.TrimSpace(edit.VisualPrompt); prompt != "" {
		task.Run.Plan.Segments[target].Prompt = prompt
	}
	task.Run.Plan.Segments[target].Dialogue = strings.TrimSpace(edit.Dialogue)

	// 作废目标段及其后的渲染记录：尾帧延续下，下游片段依赖重渲后的画面
	kept := make([]VideoRender, 0, target)
	for _, render := range task.Run.Renders {
		renderIndex := -1
		for index, segment := range task.Run.Plan.Segments {
			if segment.ID == render.SegmentID {
				renderIndex = index
				break
			}
		}
		if renderIndex >= 0 && renderIndex < target {
			kept = append(kept, render)
		}
	}
	task.Run.Renders = kept
	task.Run.Output = nil
	task.Run.Compose = nil
	task.Run.ReviewPendingSegment = ""
	task.Status = VideoStatusRunning
	task.Error = ""
	task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf("第 %d 段提示词已修改，重新生成该段", target+1)))
	task.UpdatedAt = time.Now().Format(time.RFC3339)

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if err := a.updateVideoTaskRecord(task); err != nil {
		a.endVideoRun(id)
		return err
	}
	go func() {
		defer a.endVideoRun(id)
		a.runVideoPipeline(id, VideoStageRender, "")
	}()
	return nil
}

// SaveVideoStageEdits 处理关卡上的局部修改：只重算受影响的下游（最小重算闭包）。
func (a *App) SaveVideoStageEdits(id string, edits VideoGateEdits) (VideoTask, error) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return task, err
	}
	if a.isVideoTaskRunning(id) {
		return task, errors.New("任务正在执行，无法保存修改")
	}

	switch task.Stage {
	case VideoStageConfirmScript:
		if len(edits.Shots) == 0 {
			return task, errors.New("没有提交分镜修改")
		}
		if task.Run.Script == nil {
			return task, errors.New("任务缺少分镜产物，请先重新生成")
		}
		if task.Config.manualScriptEnabled() {
			// 手动分段：关卡里改的段时长保真，不按目标时长缩放，总时长跟着分段之和走
			manualCfg := task.Config
			manualCfg.ManualShots = edits.Shots
			shots, shotWarnings, shotErr := normalizeManualShots(manualCfg)
			if shotErr != nil {
				return task, shotErr
			}
			bound, speakerWarnings := bindSpeakers(VideoScript{Shots: shots}, manualCfg)
			task.Run.Script.Shots = bound.Shots
			task.Config.ManualShots = bound.Shots
			task.Config.DurationS = int(math.Round(manualTotalSeconds(bound.Shots)))
			for _, warning := range append(shotWarnings, speakerWarnings...) {
				task.Run.Warnings = append(task.Run.Warnings, warning)
			}
		} else {
			shots := normalizeScriptShots(edits.Shots, float64(task.Config.DurationS))
			task.Run.Script.Shots = shots
			// 段时长以你填的为准，总时长跟着分段之和走：
			// 早先按目标时长等比缩放，关卡里改段长会被缩回去，看起来就是「改了没生效」
			if total := manualTotalSeconds(shots); total > 0 {
				task.Config.DurationS = int(math.Round(total))
				task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf("分镜时长已按你的修改保留，总时长调为 %ds", task.Config.DurationS)))
			}
		}
		// 剧本改动 → Agent2 全部 + 渲染 + 合成失效
		task.Run.Prompts = nil
		task.Run.Plan = nil
		task.Run.Subtitles = nil
		task.Run.Renders = nil
		task.Run.Output = nil
		task.Run.Compose = nil
		// 分镜改动 → Agent4 的标签/标题来源也变了
		task.Run.PublishMeta = nil
		task.Run.Gates[VideoGateScript] = videoGatePending
		task.Run.Gates[VideoGatePrompt] = videoGatePending
		task.Run.Gates[VideoGateMeta] = videoGatePending
		task.Status = VideoStatusWaiting
		task.Error = ""
		task.Run.Logs = append(task.Run.Logs, stageLog("分镜已修改，下游产物全部失效"))

	case VideoStageConfirmPrompt:
		if len(edits.Prompts) == 0 {
			return task, errors.New("没有提交提示词修改")
		}
		if len(task.Run.Prompts) == 0 {
			return task, errors.New("任务缺少提示词产物，请先重新生成")
		}
		applyPromptEdits(task.Run.Prompts, edits.Prompts, task.Config)

		_, adapter, provider, _, _ := a.resolveVideoAdapter(task.Config)
		caps := adapterCapabilities(adapter, provider.Model)
		plan := buildVideoPlan(task.Config, task.Run.Prompts, caps, task.Run.Adapter)
		task.Run.Plan = &plan

		// 字幕在视频生成之后才铺轴：这里只作废旧字幕
		task.Run.Subtitles = nil
		task.Run.Compose = nil

		// 提示词/台词改动 → 渲染 + 合成失效，字幕等视频出来后重生
		task.Run.Renders = nil
		task.Run.Output = nil
		task.Run.PublishMeta = nil
		task.Run.Gates[VideoGatePrompt] = videoGatePending
		task.Run.Gates[VideoGateMeta] = videoGatePending
		task.Status = VideoStatusWaiting
		task.Error = ""
		if task.Config.Subtitles {
			task.Run.Logs = append(task.Run.Logs, stageLog("提示词已修改，渲染计划已重算（字幕将在视频生成后重铺）"))
		} else {
			task.Run.Logs = append(task.Run.Logs, stageLog("提示词已修改，渲染计划已重算（字幕未开启）"))
		}

	case VideoStageConfirmMeta:
		if edits.Meta == nil {
			return task, errors.New("没有提交发布元数据修改")
		}
		if task.Run.PublishMeta == nil {
			return task, errors.New("任务缺少发布元数据，请先重新生成")
		}
		applyPublishEdits(task.Run.PublishMeta, *edits.Meta, task.Config)
		task.Run.Gates[VideoGateMeta] = videoGatePending
		task.Status = VideoStatusWaiting
		task.Error = ""
		task.Run.Logs = append(task.Run.Logs, stageLog("发布元数据已修改，等待确认"))

	case VideoStageConfirmSubs:
		if edits.Subtitles == nil {
			return task, errors.New("没有提交字幕修改")
		}
		if task.Run.Subtitles == nil {
			return task, errors.New("任务还没有字幕，请先生成视频")
		}
		if err := a.applySubtitleEdits(&task, edits.Subtitles.Cues); err != nil {
			return task, err
		}
		task.Run.Gates[VideoGateSubs] = videoGatePending
		task.Status = VideoStatusWaiting
		task.Error = ""

	default:
		return task, errors.New("仅可在人工确认关卡修改产物")
	}

	task.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := a.updateVideoTaskRecord(task); err != nil {
		return task, err
	}
	return task, nil
}

// SaveVideoSubtitles 字幕关卡里保存用户的逐条修改：时间、文本、位置、字号，支持新增与删除。
// 前端只需传完整的 cue 列表（增删都在列表里体现），后端不靠 index 对齐旧数据。
func (a *App) SaveVideoSubtitles(id string, edit VideoSubtitleEdit) (VideoTask, error) {
	return a.SaveVideoStageEdits(id, VideoGateEdits{Subtitles: &edit})
}

// applySubtitleEdits 校验并重写用户的字幕：按当前成片画布落 SRT/ASS，并标记为已确认。
// 确认后的时间轴归用户：合成阶段不再回测重排，否则调好的位置与时间会被默默覆盖。
func (a *App) applySubtitleEdits(task *VideoTask, cues []VideoSubtitleCue) error {
	workDir, err := a.ensureTaskWorkDir(task.ID)
	if err != nil {
		return err
	}
	prep := task.Run.Compose
	// 上限拿成片真实时长：字幕不能铺到画布之外，测不到时退回任务目标时长
	total := 0.0
	if prep != nil && prep.CanvasPath != "" && fileExists(prep.CanvasPath) {
		probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if info, probeErr := mediaProbe(probeCtx, prep.CanvasPath); probeErr == nil {
			total = info.Duration
		}
		cancel()
	}
	if total <= 0 {
		total = float64(task.Config.DurationS)
	}

	normalized, notes := normalizeSubtitleCues(cues, total)
	for _, note := range notes {
		duplicate := false
		for _, existing := range task.Run.Warnings {
			if existing == note {
				duplicate = true
				break
			}
		}
		if !duplicate {
			task.Run.Warnings = append(task.Run.Warnings, note)
		}
	}

	width, height := subtitleCanvasSize(task.Config, prep)
	subtitles, err := writeSubtitleFiles(workDir, task.ID, task.Config, normalized,
		firstNonEmpty(task.Run.Subtitles.TimingSource, "measured"), "", width, height)
	if err != nil {
		return fmt.Errorf("写入字幕文件失败: %w", err)
	}
	subtitles.Confirmed = true
	subtitles.EditedAt = time.Now().Format(time.RFC3339)
	task.Run.Subtitles = subtitles
	task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf("字幕已保存：%d 条（时间/位置/字号以你的设置为准，合成不再重排）", len(normalized))))
	if len(normalized) == 0 {
		task.Run.Logs = append(task.Run.Logs, stageLog("字幕已全部删除：成片将不含字幕，需要时可重新添加"))
	}
	return nil
}

// applyPublishEdits 只改文案与选定时段，平台字数与数量上限仍按档位夹取。
func applyPublishEdits(meta *PublishMeta, edit VideoPublishEdit, cfg VideoRunConfig) {
	profile := platformProfile(meta.Platform)

	if title := strings.TrimSpace(edit.Title); title != "" {
		meta.Title.Primary = clampTitle(title, profile.TitleMaxLen)
		meta.Title.CharCount = runeLen(meta.Title.Primary)
	}
	if edit.Hashtags != nil {
		meta.Tags.Hashtags = normalizeHashtags(edit.Hashtags, profile)
	}
	if edit.Keywords != nil {
		meta.Tags.Keywords = trimmedKeywords(edit.Keywords, meta.Tags.Hashtags, profile.KeywordMax)
	}
	meta.Tags.Counts = PublishTagCounts{Hashtag: len(meta.Tags.Hashtags), Keyword: len(meta.Tags.Keywords)}

	if description := strings.TrimSpace(edit.Description); description != "" {
		meta.Description = truncateRunes(description, profile.DescriptionMax)
	}
	if slot := strings.TrimSpace(edit.Slot); slot != "" {
		selected := PublishTimeCandidate{Slot: slot, Reason: "用户指定时段", Score: 1}
		rest := make([]PublishTimeCandidate, 0, len(meta.PublishTime.Candidates))
		for _, candidate := range meta.PublishTime.Candidates {
			if candidate.Slot == slot {
				continue
			}
			rest = append(rest, candidate)
		}
		meta.PublishTime.Candidates = append([]PublishTimeCandidate{selected}, rest...)
	}
	if edit.CoverTS != nil && *edit.CoverTS >= 0 {
		meta.Cover.FrameTS = round2(clampFloat(*edit.CoverTS, 0, float64(cfg.DurationS)))
	}

	meta.Title.Compliance = complianceHits(append(append([]string{}, meta.Title.Alternatives...), meta.Title.Primary))
	if len(meta.Title.Compliance) > 0 {
		meta.Note = "标题命中合规红线，请替换后再发布：" + strings.Join(meta.Title.Compliance, "；")
	} else if strings.HasPrefix(meta.Note, "标题命中合规红线") {
		meta.Note = ""
	}
}

// normalizeScriptShots 用户改动分镜后，重算时间轴，保证口径一致。
// 段时长以用户填的为准（不再按目标总时长等比缩回）：总时长由段之和反写，
// 真要让模型档位取整时由 buildVideoPlan 告警，不在这里默默改掉用户的数字。
func normalizeScriptShots(shots []VideoShot, total float64) []VideoShot {
	normalized := make([]VideoShot, 0, len(shots))
	knownSum, knownCount := 0.0, 0
	for index, shot := range shots {
		shot.SceneIntent = strings.TrimSpace(shot.SceneIntent)
		shot.Dialogue = strings.TrimSpace(shot.Dialogue)
		shot.Camera = strings.TrimSpace(shot.Camera)
		shot.Mood = strings.TrimSpace(shot.Mood)
		shot.AudioCue = strings.TrimSpace(shot.AudioCue)
		shot.ID = firstNonEmpty(shot.ID, fmt.Sprintf("s%d", index+1))
		if shot.Roles == nil {
			shot.Roles = []string{}
		}
		shot.DurationS = round2(math.Max(0, shot.DurationS))
		if shot.DurationS > 0 {
			knownSum += shot.DurationS
			knownCount++
		}
		normalized = append(normalized, shot)
	}
	if len(normalized) == 0 {
		return normalized
	}

	// 没填时长的段：按已知段的均值补，全都没填才按目标总时长均分
	fallback := total / float64(len(normalized))
	if knownCount > 0 {
		fallback = knownSum / float64(knownCount)
	}
	if fallback < 0.5 {
		fallback = 0.5
	}
	cursor := 0.0
	for i := range normalized {
		duration := round2(math.Max(0.5, normalized[i].DurationS))
		if normalized[i].DurationS <= 0 {
			duration = round2(fallback)
		}
		normalized[i].DurationS = duration
		normalized[i].TimeRange = [2]float64{round2(cursor), round2(cursor + duration)}
		cursor += duration
	}
	return normalized
}

// applyPromptEdits 只改提示词与台词，并按语速预算重新校验。
func applyPromptEdits(prompts []VideoPromptItem, edits []VideoPromptEdit, cfg VideoRunConfig) {
	index := make(map[string]int, len(prompts))
	for i, prompt := range prompts {
		index[prompt.ShotID] = i
	}
	for _, edit := range edits {
		i, ok := index[strings.TrimSpace(edit.ShotID)]
		if !ok {
			continue
		}
		prompts[i].VisualPrompt = appendCastDirective(strings.TrimSpace(edit.VisualPrompt))
		prompts[i].Dialogue = strings.TrimSpace(edit.Dialogue)
		// 台词归属：关卡里改了就按关卡，没改就保留原归属；台词被清空则归属也清空
		if speaker := strings.TrimSpace(edit.Speaker); speaker != "" {
			prompts[i].Speaker = firstNonEmpty(normalizeSpeaker(cfg, speaker), speaker)
		}
		if prompts[i].Dialogue == "" {
			prompts[i].Speaker = ""
		}
		prompts[i].CharCount = speechUnitCount(prompts[i].Dialogue, cfg.Language)
		prompts[i].EstSpeechS = estimateSpeechSeconds(prompts[i].Dialogue, cfg.Language, cfg.SpeechRate)
		duration := prompts[i].End - prompts[i].Start
		prompts[i].Fits = prompts[i].EstSpeechS <= duration+0.1
		if prompts[i].Fits {
			prompts[i].Note = ""
		} else {
			prompts[i].Note = fmt.Sprintf("台词约 %.1fs，超出本段 %.1fs，建议精简或改排期", prompts[i].EstSpeechS, duration)
		}
	}
}

type videoRunner struct {
	app  *App
	task *VideoTask
	ctx  context.Context
	// feedbackStage 本轮返工意见归属的阶段：只有用户按下「重生成」的那个阶段才能消费它
	feedbackStage string
}

func stageLog(message string) string {
	return time.Now().Format("15:04:05") + " " + message
}

func (r *videoRunner) logf(format string, args ...any) {
	r.task.Run.Logs = append(r.task.Run.Logs, stageLog(fmt.Sprintf(format, args...)))
	if len(r.task.Run.Logs) > 80 {
		r.task.Run.Logs = r.task.Run.Logs[len(r.task.Run.Logs)-80:]
	}
}

func (r *videoRunner) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, existing := range r.task.Run.Warnings {
		if existing == message {
			return
		}
	}
	r.task.Run.Warnings = append(r.task.Run.Warnings, message)
	r.logf("警告: %s", message)
}

// usageCtx 给本阶段的模型调用标上用途与任务身份，首页大屏才能把消耗拆到「分镜/提示词/渲染」上。
func (r *videoRunner) usageCtx(purpose string) context.Context {
	return withUsage(r.ctx, purpose, r.task.ID)
}

// consumeFeedback 取走本轮人工返工意见并清空：不拼进模型输入的话，“按意见重生成”只是拿同一份提示词重掷一次骰子。
// 只让意见归属的那个阶段读到它：渲染阶段顺带预生成发布元数据时，
// 针对画面的意见不该被 Agent4 拿去改标题。
func (r *videoRunner) consumeFeedback(stage string) string {
	if r.feedbackStage != stage {
		return ""
	}
	feedback := strings.TrimSpace(r.task.Run.Feedback)
	if feedback == "" {
		return ""
	}
	r.task.Run.Feedback = ""
	r.logf("本轮按人工返工意见重出：%s", truncateRunes(feedback, 80))
	return feedback
}

func (r *videoRunner) save() error {
	r.task.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := r.app.updateVideoTaskRecord(*r.task); err != nil {
		r.task.Run.Logs = append(r.task.Run.Logs, stageLog("保存任务状态失败: "+err.Error()))
		return err
	}
	return nil
}

func (r *videoRunner) fail(err error) {
	r.task.Status = VideoStatusFailed
	r.task.Error = err.Error()
	r.logf("阶段 %s 失败: %v", r.task.Stage, err)
	_ = r.save()
}

// runVideoPipeline 串行推进状态机；遇到确认关卡即停住，等待用户动作。
func (a *App) runVideoPipeline(id, from, feedback string) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return
	}
	ensureVideoGates(&task.Run)
	task.Status = VideoStatusRunning
	task.Stage = from
	task.Error = ""
	task.Run.Feedback = strings.TrimSpace(feedback)

	switch from {
	case VideoStageBuildPrompt:
		task.Run.Gates[VideoGateScript] = videoGateApproved
	case VideoStageBuildSubs, VideoStageRender:
		task.Run.Gates[VideoGatePrompt] = videoGateApproved
	}

	runner := &videoRunner{app: a, task: &task, ctx: withUsageTask(context.Background(), id), feedbackStage: from}
	runner.logf("开始执行阶段 %s", from)
	_ = runner.save()

	for {
		switch runner.task.Stage {
		case VideoStageDraftScript:
			if err := runner.runScriptStage(); err != nil {
				runner.fail(err)
				return
			}
			runner.task.Stage = VideoStageConfirmScript
			runner.task.Run.Gates[VideoGateScript] = videoGatePending
			runner.task.Status = VideoStatusWaiting
			_ = runner.save()
			return

		case VideoStageBuildPrompt:
			if err := runner.runPromptStage(); err != nil {
				runner.fail(err)
				return
			}
			runner.task.Stage = VideoStageConfirmPrompt
			runner.task.Run.Gates[VideoGatePrompt] = videoGatePending
			runner.task.Status = VideoStatusWaiting
			_ = runner.save()
			return

		case VideoStageBuildSubs:
			// 字幕排在视频之后：先拿真实片段拼好的画布铺轴，再让用户逐条调
			if err := runner.runSubtitleStage(); err != nil {
				runner.fail(err)
				return
			}
			if runner.task.Run.Subtitles == nil {
				// 字幕未开启或本片没有台词：没东西可确认，直接合成
				runner.task.Stage = VideoStageCompose
				runner.task.Status = VideoStatusRunning
				_ = runner.save()
				continue
			}
			runner.task.Stage = VideoStageConfirmSubs
			runner.task.Run.Gates[VideoGateSubs] = videoGatePending
			runner.task.Status = VideoStatusWaiting
			_ = runner.save()
			return

		case VideoStageRender:
			// Agent4 只依赖已确认的提示词与剧本，可与渲染并行准备（§10.1）
			if runner.task.Run.PublishMeta == nil {
				if err := runner.runMetaStage(); err != nil {
					runner.warn("发布元数据预生成失败，将在合成后重试: %v", err)
				}
			}
			paused, err := runner.runRenderStage()
			if err != nil {
				runner.fail(err)
				return
			}
			if paused {
				// 逐段确认关卡：停住等用户确认/重生成，确认后从下一段继续
				runner.task.Status = VideoStatusWaiting
				_ = runner.save()
				return
			}
			runner.task.Stage = VideoStageBuildSubs
			runner.task.Status = VideoStatusRunning
			_ = runner.save()

		case VideoStageCompose:
			if err := runner.runComposeStage(); err != nil {
				runner.fail(err)
				return
			}
			if runner.task.Run.PublishMeta == nil {
				runner.task.Stage = VideoStageBuildMeta
				runner.task.Status = VideoStatusRunning
				_ = runner.save()
				continue
			}
			runner.task.Stage = VideoStageConfirmMeta
			runner.task.Run.Gates[VideoGateMeta] = videoGatePending
			runner.task.Status = VideoStatusWaiting
			_ = runner.save()
			return

		case VideoStageBuildMeta:
			if err := runner.runMetaStage(); err != nil {
				runner.fail(err)
				return
			}
			runner.task.Stage = VideoStageConfirmMeta
			runner.task.Run.Gates[VideoGateMeta] = videoGatePending
			runner.task.Status = VideoStatusWaiting
			_ = runner.save()
			return

		default:
			// 非可执行阶段（多为人工确认关卡）不应把任务挂在 running 状态
			runner.task.Status = VideoStatusWaiting
			_ = runner.save()
			return
		}
	}
}

// runScriptStage 分镜阶段：默认交给 Agent1；手动分段模式直接用用户填的段，不调模型。
func (r *videoRunner) runScriptStage() error {
	if tpl := r.task.Run.Template; tpl != nil {
		r.logf("按模板「%s」v%s 执行：%d 个字段来自模板，%d 个字段本次改过", tpl.Name, tpl.Version, len(tpl.Sources), len(tpl.Overrides))
	}

	var script VideoScript
	var warnings []string
	manual := r.task.Config.manualScriptEnabled()
	if manual {
		built, notes, err := buildManualScript(r.task.Config)
		if err != nil {
			return err
		}
		script, warnings = built, notes
		r.logf("手动分段模式：沿用你填写的 %d 段（合计 %.1fs），跳过 Agent1", len(script.Shots), manualTotalSeconds(script.Shots))
	} else {
		script, warnings = r.app.generateScript(r.usageCtx(UsagePurposeScript), r.task.Config, r.consumeFeedback(VideoStageDraftScript), r.task.Run.Template)
		// 把真正带进模型输入的用户参数写进日志：参数有没有生效，用户要能一眼验证
		r.logf("Agent1 输入参数：%s", userParamDigest(r.task.Config))
	}
	for _, warning := range warnings {
		r.warn("%s", warning)
	}
	if len(script.Shots) == 0 {
		return errors.New("没有生成任何分镜")
	}

	r.task.Run.Script = &script
	// 重跑剧本 → 下游全部失效
	r.task.Run.Prompts = nil
	r.task.Run.Plan = nil
	r.task.Run.Subtitles = nil
	r.task.Run.Renders = nil
	r.task.Run.Output = nil
	r.task.Run.Compose = nil
	r.task.Run.PublishMeta = nil
	r.task.Run.Gates[VideoGateScript] = videoGatePending
	r.task.Run.Gates[VideoGatePrompt] = videoGatePending
	r.task.Run.Gates[VideoGateMeta] = videoGatePending
	if manual {
		r.logf("手动分段完成：%d 段分镜，总时长 %.1fs（可在关卡①里再改）", len(script.Shots), manualTotalSeconds(script.Shots))
	} else {
		r.logf("Agent1 完成：%d 段分镜（来源 %s）", len(script.Shots), script.Source)
	}
	return nil
}

// runPromptStage Agent2：提示词 + 成本计划。字幕排在视频生成之后，这里不落。
func (r *videoRunner) runPromptStage() error {
	if r.task.Run.Script == nil || len(r.task.Run.Script.Shots) == 0 {
		return errors.New("缺少分镜脚本，无法展开提示词")
	}

	adapterID, adapter, provider, warnings, err := r.app.resolveVideoAdapter(r.task.Config)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		r.warn("%s", warning)
	}
	caps := adapterCapabilities(adapter, provider.Model)
	r.task.Run.Adapter = adapterID

	prompts, promptWarnings := r.app.generatePrompts(r.usageCtx(UsagePurposePrompt), r.task.Config, *r.task.Run.Script, caps,
		r.consumeFeedback(VideoStageBuildPrompt), r.task.Run.Template)
	r.logf("Agent2 输入参数：%s／能力：%s", userParamDigest(r.task.Config), capabilityHint(caps))
	r.logf("台词归属：%s", speakerDigest(r.task.Config, prompts))
	for _, warning := range promptWarnings {
		r.warn("%s", warning)
	}
	if len(prompts) == 0 {
		return errors.New("没有生成可用的提示词")
	}
	r.task.Run.Prompts = prompts

	plan := buildVideoPlan(r.task.Config, prompts, caps, adapterID)
	for _, warning := range plan.Warnings {
		r.warn("%s", warning)
	}
	if adapter == nil {
		r.warn("当前没有可用的视频模型：请先配置提供商，或在任务中选择「本地联调占位」")
	}
	r.task.Run.Plan = &plan

	// 字幕不在这一阶段做：台词等 gate② 定稿，视频出来后才由 BUILD_SUBS 铺轴
	r.task.Run.Subtitles = nil
	if r.task.Config.Subtitles {
		r.logf("字幕将在视频生成后铺轴（真实片段的时长与声音为准，可在字幕关卡逐条调）")
	} else {
		// 字幕默认关闭：不产出 SRT/ASS，成片也不烧录
		r.logf("字幕未开启，本次不生成字幕文件")
	}

	// 重算后旧渲染结果作废
	r.task.Run.Renders = nil
	r.task.Run.Output = nil
	r.task.Run.Compose = nil
	r.task.Run.PublishMeta = nil
	r.task.Run.Gates[VideoGatePrompt] = videoGatePending
	r.task.Run.Gates[VideoGateMeta] = videoGatePending
	r.logf("Agent2 完成：%d 段提示词，计划 %d 个片段（%.1fs）", len(prompts), plan.ClipCount, plan.TotalS)
	return nil
}

// runSubtitleStage 字幕阶段：排在视频生成之后。
// 先把合成前的剪辑预处理做完（选段/裁剪/拼接/画幅）拿到真实画布，再按片段实测时长 + 声学对齐铺轴；
// 预处理结果缓在 run.Compose，合成阶段直接复用，既省一次重编码也让用户调好的字幕不会被重新切过的片段带偏。
func (r *videoRunner) runSubtitleStage() error {
	cfg := r.task.Config
	if !cfg.Subtitles {
		// 字幕默认关闭：不产出 SRT/ASS，成片也不烧录
		r.task.Run.Subtitles = nil
		r.logf("字幕未开启，跳过字幕阶段")
		return nil
	}

	plan := r.task.Run.Plan
	if plan == nil || len(plan.Segments) == 0 {
		// 兜底：直接用已确认的提示词重建计划，免得旧任务卡在这一步
		if len(r.task.Run.Prompts) == 0 {
			return errors.New("缺少已确认的提示词，无法生成字幕")
		}
		_, adapter, provider, _, _ := r.app.resolveVideoAdapter(cfg)
		rebuilt := buildVideoPlan(cfg, r.task.Run.Prompts, adapterCapabilities(adapter, provider.Model), r.task.Run.Adapter)
		plan = &rebuilt
		r.task.Run.Plan = plan
	}

	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return err
	}

	// 成片画布：已有预处理结果可用就直接复用（重渲过某段则重算）
	prep := r.task.Run.Compose
	if !composePrepUsable(prep, r.task.Run.Renders) {
		built, prepWarnings, prepErr := prepareComposeCanvas(r.ctx, workDir, r.task.ID, cfg, &r.task.Run)
		for _, warning := range prepWarnings {
			r.warn("%s", warning)
		}
		if prepErr != nil {
			// 预处理失败不阻断字幕：还能按片段实测铺轴，只是没有画布预览与声学对齐
			r.warn("合成预处理失败，字幕改按片段时长铺轴: %v", prepErr)
			prep = nil
			r.task.Run.Compose = nil
		} else {
			prep = built
			r.task.Run.Compose = prep
		}
	}

	canvas := ""
	if prep != nil {
		canvas = prep.CanvasPath
	}
	cues, timingSource, audioSource, measureWarnings := measureSubtitleCues(r.ctx, workDir, cfg, &r.task.Run, canvas)
	for _, warning := range measureWarnings {
		r.warn("%s", warning)
	}
	if len(cues) == 0 {
		r.task.Run.Subtitles = nil
		r.logf("本片没有需要字幕的台词，跳过字幕确认")
		return nil
	}

	width, height := subtitleCanvasSize(cfg, prep)
	subtitles, err := writeSubtitleFiles(workDir, r.task.ID, cfg, cues, timingSource, audioSource, width, height)
	if err != nil {
		return fmt.Errorf("生成字幕文件失败: %w", err)
	}
	r.task.Run.Subtitles = subtitles
	r.logf("字幕已生成：%d 条，时间轴来源 %s（基于成片画布 %dx%d，可在字幕关卡里逐条改时间/位置/字号）",
		len(subtitles.Cues), subtitles.TimingSource, width, height)
	return nil
}

// runRenderStage Agent3：按段串行生成，支持尾帧延续与断点续跑。
// 逐段确认开启时，每段新渲染成功即返回 paused=true，由流水线置 waiting 停住等用户动作。
func (r *videoRunner) runRenderStage() (bool, error) {
	// 渲染阶段没有文本模型参与：重跑只是按现有提示词重新生成画面。
	// 不跟用户说清楚去哪改，他会以为 AI 听懂了意见。
	if pending := strings.TrimSpace(r.task.Run.Feedback); pending != "" && r.feedbackStage == VideoStageRender {
		r.warn("返工意见「%s」本轮未被采纳：渲染阶段只按现有提示词重跑画面；要改画面写法请用逐段确认里的「AI 优化提示词」，或回到提示词关卡重新生成",
			truncateRunes(pending, 60))
		r.task.Run.Feedback = ""
	}
	cfg := r.task.Config
	adapterID, adapter, provider, warnings, err := r.app.resolveVideoAdapter(cfg)
	if err != nil {
		return false, err
	}
	for _, warning := range warnings {
		r.warn("%s", warning)
	}
	if adapter == nil {
		return false, errors.New("未配置可用的视频生成模型：请在「系统设置 - 提供商」登记多模态生成模型，或在任务中选择「本地联调占位」")
	}
	caps := adapterCapabilities(adapter, provider.Model)
	r.task.Run.Adapter = adapterID
	r.logf("渲染通道：%s｜模型 %s｜Base %s", adapterID,
		firstNonEmpty(provider.Model, "未填写"), firstNonEmpty(provider.BaseURL, "未填写"))
	if cfg.strictFirstFrameEnabled() {
		r.logf("首帧约束：严格 —— 带首帧的片段一律按「图生视频-首帧」提交，画面必须从这张图开始（与角色参考图互斥时首帧优先）")
	} else {
		r.logf("首帧约束：宽松 —— 首帧图与角色参考图一起走全模态参考，锁帧靠提示词里的「图片1」约定")
	}
	// 用户给了首帧/尾帧就要真锁帧：通道做不到时当场停下，不能让它静默失效后白烧额度
	if strings.TrimSpace(cfg.FirstFrame) != "" && !caps.I2VFirstFrame {
		if adapterID == "mock" {
			r.warn("本地联调占位通道不消费首帧图：你给的首帧/尾帧不会成为新视频的第一帧画面，只用于走通流程")
		} else {
			return false, fmt.Errorf("通道 %s 不支持以首帧图起片，无法保证新视频从你提供的首帧/尾帧开始：请换支持图生视频首帧的通道（如火山方舟 Seedance），或清空首帧后重生", adapterID)
		}
	}

	if r.task.Run.Plan == nil || len(r.task.Run.Plan.Segments) == 0 {
		if len(r.task.Run.Prompts) == 0 {
			return false, errors.New("缺少提示词，无法生成视频")
		}
		plan := buildVideoPlan(cfg, r.task.Run.Prompts, caps, adapterID)
		r.task.Run.Plan = &plan
	}
	segments := r.task.Run.Plan.Segments

	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return false, err
	}

	// 进入渲染即清空上一轮的待确认标记：确认/重生成后重新评定，失败时不会残留
	r.task.Run.ReviewPendingSegment = ""

	// 断点续跑：已成功且文件仍在的片段直接复用
	existing := make(map[string]VideoRender, len(r.task.Run.Renders))
	for _, render := range r.task.Run.Renders {
		if render.Status == "succeeded" && fileExists(render.VideoURI) {
			existing[render.SegmentID] = render
		}
	}
	taskReview := cfg.segmentReviewEnabled()
	r.task.Run.Renders = nil
	r.task.Run.Output = nil
	// 画面要重出了：旧画布与旧字幕时间轴都不再可信，渲完后 BUILD_SUBS 会重铺
	r.task.Run.Compose = nil
	r.task.Run.Subtitles = nil

	genCtx := videoGenContext{Provider: provider, Model: provider.Model, AdapterID: adapterID, WorkDir: workDir}
	renders := make([]VideoRender, 0, len(segments))
	previousClip := ""
	// 服务商行为提示（如画幅被改成 adaptive）逐段重复，只进一次日志
	adapterNotes := make(map[string]bool)
	newlyRendered := 0

	for _, segment := range segments {
		if done, ok := existing[segment.ID]; ok {
			renders = append(renders, done)
			previousClip = done.VideoURI
			continue
		}

		firstFrame := cfg.FirstFrame
		if segment.Index > 0 && previousClip != "" && cfg.ContinuationMode == "last_frame" && caps.LastFrame {
			framePath := filepath.Join(workDir, "clips", segment.ID+"-tail.png")
			if err := extractLastFrame(r.ctx, previousClip, framePath); err != nil {
				r.warn("抽取上一段尾帧失败，本段沿用首帧条件: %v", err)
			} else {
				firstFrame = framePath
			}
		}

		render := VideoRender{
			SegmentID:  segment.ID,
			ShotID:     segment.ShotID,
			Status:     "running",
			FirstFrame: firstFrame,
			Seed:       cfg.VideoParams.Seed,
			UpdatedAt:  time.Now().Format(time.RFC3339),
		}
		renders = append(renders, render)
		r.task.Run.Renders = append([]VideoRender{}, renders...)
		_ = r.save()
		r.logf("开始生成第 %d/%d 段：%ds（%s）", segment.Index+1, len(segments), segment.DurationS, adapterID)

		charImages, charWarning := characterImagesForSegment(cfg, r.task.Run.Script, segment)
		if charWarning != "" && !adapterNotes[charWarning] {
			adapterNotes[charWarning] = true
			r.warn("%s", charWarning)
		}
		if len(charImages) > 0 {
			// 把“本段到底带了哪几张角色图”写进日志：首帧延续与角色一致性必须同时生效，不能只靠返工发现
			names := make([]string, 0, len(charImages))
			for _, image := range charImages {
				label := firstNonEmpty(image.Name, image.RoleID)
				if image.Source == CharacterImageSourceUser {
					label += "（用户上传）"
				}
				names = append(names, label)
			}
			suffix := ""
			if firstFrame != "" {
				if cfg.strictFirstFrameEnabled() {
					suffix = "；严格首帧优先，任务类型互斥的通道（如火山方舟）本段不提交角色图，形象由首帧画面与外观锚点保持"
				} else {
					suffix = "，与首帧图一起按全模态参考提交"
				}
			}
			r.logf("第 %d 段附带角色参考图 %d 张（%s）%s", segment.Index+1, len(charImages), strings.Join(names, "、"), suffix)
		}

		outputPath := filepath.Join(workDir, "clips", fmt.Sprintf("%02d-%s.mp4", segment.Index+1, segment.ID))
		request := VideoGenRequest{
			Prompt:           composeRenderPrompt(segment, caps),
			FirstFrame:       firstFrame,
			CharacterImages:  charImages,
			StrictFirstFrame: cfg.strictFirstFrameEnabled(),
			DurationS:        segment.DurationS,
			AspectRatio:      cfg.AspectRatio,
			Fps:              cfg.Fps,
			Seed:             cfg.VideoParams.Seed,
			OutputPath:       outputPath,
		}

		usageCtx := r.usageCtx(UsagePurposeVideoRender)
		clipCtx, cancel := context.WithTimeout(usageCtx, 30*time.Minute)
		renderStart := time.Now()
		result, genErr := adapter.Generate(clipCtx, genCtx, request)
		cancel()
		// 本地联调占位不调用任何厂商接口，不记账：混进来会把「真实消耗」稀释成假数字
		if adapterID != "mock" {
			r.app.recordVideoUsage(usageCtx, provider, adapterID, result.ModelVer, result.DurationS, time.Since(renderStart), genErr)
		}

		if genErr != nil {
			render.Status = "failed"
			render.Note = genErr.Error()
			render.UpdatedAt = time.Now().Format(time.RFC3339)
			renders[len(renders)-1] = render
			r.task.Run.Renders = append([]VideoRender{}, renders...)
			_ = r.save()
			return false, fmt.Errorf("第 %d 段生成失败: %w", segment.Index+1, genErr)
		}

		render.Status = "succeeded"
		render.VideoURI = result.VideoURI
		if relative := relativeUnderWorkDir(workDir, result.VideoURI); relative != "" {
			render.MediaURL = videoMediaURL(r.task.ID, relative)
		}
		render.TaskID = result.TaskID
		render.ModelVer = result.ModelVer
		render.DurationS = result.DurationS
		render.Seed = result.Seed
		render.UpdatedAt = time.Now().Format(time.RFC3339)
		for _, note := range result.Notes {
			if !adapterNotes[note] {
				adapterNotes[note] = true
				r.warn("%s", note)
			}
		}
		renders[len(renders)-1] = render
		r.task.Run.Renders = append([]VideoRender{}, renders...)
		previousClip = result.VideoURI
		newlyRendered++

		// 最后一段成功且开启保留尾帧：抽尾帧存入资产库（失败只警告，不阻断流程）
		if cfg.KeepLastFrame && segment.Index == len(segments)-1 {
			tailPath := filepath.Join(workDir, "clips", segment.ID+"-final-tail.png")
			if err := extractLastFrame(r.ctx, result.VideoURI, tailPath); err != nil {
				r.warn("保留尾帧失败: %v", err)
			} else if err := r.app.replaceTailFrameAsset(r.task.ID, r.task.Title, segment.ID, tailPath); err != nil {
				r.warn("尾帧资产登记失败: %v", err)
			} else {
				r.logf("尾帧已存入资产库：%s", tailPath)
			}
		}

		// 逐段确认：本段新渲染完成即停住，等用户确认后再生成下一段
		if taskReview {
			r.task.Run.ReviewPendingSegment = segment.ID
			r.task.Run.Renders = append([]VideoRender{}, renders...)
			r.logf("第 %d/%d 段已生成，等待确认后继续", segment.Index+1, len(segments))
			_ = r.save()
			return true, nil
		}
		_ = r.save()
	}

	r.task.Run.Renders = renders
	if newlyRendered == 0 {
		r.logf("全部片段已有产物，跳过渲染阶段")
	} else {
		r.logf("视频生成完成：%d 段", len(renders))
	}
	return false, nil
}

// runMetaStage Agent4：发布元数据（标题 / 标签 / 发布时段 / 封面），与视频解耦。
func (r *videoRunner) runMetaStage() error {
	if r.task.Run.Script == nil || len(r.task.Run.Script.Shots) == 0 {
		return errors.New("缺少分镜，无法生成发布元数据")
	}

	meta, warnings := r.app.generatePublishMeta(r.usageCtx(UsagePurposePublishMeta), r.task.Config, r.task.Run.Script, r.task.Run.Prompts,
		r.consumeFeedback(VideoStageBuildMeta))
	for _, warning := range warnings {
		r.warn("%s", warning)
	}
	if meta == nil {
		return errors.New("发布元数据生成失败")
	}
	r.task.Run.PublishMeta = meta
	r.task.Run.Gates[VideoGateMeta] = videoGatePending
	r.logf("Agent4 完成：%s · 标题 %d/%d 字 · 话题 %d 个 · 关键词 %d 个 · 时段候选 %d 个（来源 %s）",
		meta.PlatformLabel, meta.Title.CharCount, meta.Title.Limit,
		len(meta.Tags.Hashtags), len(meta.Tags.Keywords), len(meta.PublishTime.Candidates), meta.Source)
	return nil
}

// runComposeStage 合成阶段：拼接 + 烧字幕 + 输出成片。
func (r *videoRunner) runComposeStage() error {
	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return err
	}
	output, warnings, err := composeVideo(r.ctx, workDir, r.task.ID, r.task.Config, &r.task.Run)
	for _, warning := range warnings {
		r.warn("%s", warning)
	}
	if err != nil {
		return err
	}
	r.task.Run.Output = &output
	r.logf("合成完成：%s", firstNonEmpty(output.FinalPath, output.MergedPath))
	return nil
}

// relativeUnderWorkDir 计算任务工作目录内的相对路径，用于生成 /media URL。
func relativeUnderWorkDir(workDir, path string) string {
	relative, err := filepath.Rel(workDir, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return ""
	}
	return filepath.ToSlash(relative)
}

// characterImagesForSegment 只把该段出场角色的参考图交给适配器。
// 分镜没标角色、或标的 role_id 与角色表对不上时，退化为附上全部角色参考图：
// 宁多给几张图，也不要让角色一致性静默失效（静默丢图正是“参考图没被参考”的常见成因）。
func characterImagesForSegment(cfg VideoRunConfig, script *VideoScript, segment VideoSegment) ([]VideoCharacterImage, string) {
	if len(cfg.CharacterRefs) == 0 {
		return nil, ""
	}
	roles := map[string]bool{}
	if script != nil {
		for _, shot := range script.Shots {
			if shot.ID != segment.ShotID {
				continue
			}
			for _, role := range shot.Roles {
				if key := normalizeRoleKey(role); key != "" {
					roles[key] = true
				}
			}
		}
	}
	// 说话人即使没写进 roles，也得算出场：台词归他、画面却没参考过他，脸一定翻。
	if segment.Speaker != "" && segment.Speaker != VideoSpeakerNarration {
		if key := normalizeRoleKey(segment.Speaker); key != "" {
			roles[key] = true
		}
	}

	matched := make([]VideoCharacterImage, 0, 4)
	for _, ref := range cfg.CharacterRefs {
		if !roleMatches(roles, ref) {
			continue
		}
		matched = append(matched, characterImagesOf(ref)...)
	}
	if len(matched) > 0 {
		return orderCharacterImages(matched), ""
	}

	all := make([]VideoCharacterImage, 0, 4)
	for _, ref := range cfg.CharacterRefs {
		all = append(all, characterImagesOf(ref)...)
	}
	if len(all) == 0 {
		return nil, ""
	}
	return orderCharacterImages(all), fmt.Sprintf(
		"第 %d 段没有匹配到出场角色（分镜 roles 为空或与角色表不一致），已附带全部 %d 张角色参考图",
		segment.Index+1, len(all),
	)
}

// speakerDigest 一行说明每段台词归谁：“谁在说”是硬信息，不该藏在 JSON 里让人点开看。
func speakerDigest(cfg VideoRunConfig, prompts []VideoPromptItem) string {
	parts := make([]string, 0, len(prompts))
	for _, item := range prompts {
		switch {
		case item.Dialogue == "":
			parts = append(parts, item.ShotID+"=无台词")
		case item.Speaker == "":
			parts = append(parts, item.ShotID+"=未定")
		default:
			parts = append(parts, item.ShotID+"="+speakerLabel(cfg, item.Speaker))
		}
	}
	return strings.Join(parts, "／")
}

// normalizeRoleKey role 标识大小写 / 空格不敏感，便于容忍模型返回的写法差异。
func normalizeRoleKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// roleMatches 先按 role_id / 角色名精确比，再放宽到包含关系（模型常把「主理人（橘猫）」写成「主理人」）。
func roleMatches(roles map[string]bool, ref VideoCharacterRef) bool {
	if len(roles) == 0 {
		return false
	}
	for _, candidate := range []string{normalizeRoleKey(ref.RoleID), normalizeRoleKey(ref.Name)} {
		if candidate == "" {
			continue
		}
		if roles[candidate] {
			return true
		}
		for got := range roles {
			if strings.Contains(got, candidate) || strings.Contains(candidate, got) {
				return true
			}
		}
	}
	return false
}

// characterImagesOf 把一个角色的定妆图展开成带身份的图片条目。
func characterImagesOf(ref VideoCharacterRef) []VideoCharacterImage {
	images := make([]VideoCharacterImage, 0, len(ref.Images))
	for _, image := range ref.Images {
		if strings.TrimSpace(image) == "" {
			continue
		}
		images = append(images, VideoCharacterImage{
			RoleID: ref.RoleID, Name: ref.Name, URL: image, Source: ref.ImageSource,
		})
	}
	return images
}

// orderCharacterImages 把用户上传的定妆图排到前面：方舟的全模态参考有张数上限（Seedance 2.0 系列 9 张），
// 超限截断时先丢模板生成图；同时序号靠前的图在提示词里天然是更强的锚点。
func orderCharacterImages(in []VideoCharacterImage) []VideoCharacterImage {
	out := make([]VideoCharacterImage, 0, len(in))
	for _, item := range in {
		if item.Source == CharacterImageSourceUser {
			out = append(out, item)
		}
	}
	for _, item := range in {
		if item.Source != CharacterImageSourceUser {
			out = append(out, item)
		}
	}
	return out
}

// roleUsesUserImages 判断某个出镜角色吃的是用户上传的定妆图（而不是模板里的生成图）。
// 两者的权威程度不同：上传图是用户亲自认定的形象，提示词里的文字描述只能兜底。
func roleUsesUserImages(cfg VideoRunConfig, role string) bool {
	key := normalizeRoleKey(role)
	if key == "" {
		return false
	}
	roles := map[string]bool{key: true}
	for _, ref := range cfg.CharacterRefs {
		if ref.ImageSource != CharacterImageSourceUser || len(ref.Images) == 0 {
			continue
		}
		if roleMatches(roles, ref) {
			return true
		}
	}
	return false
}
