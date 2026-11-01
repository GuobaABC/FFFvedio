package main

import (
	"context"
	"errors"
	"fmt"
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
	case VideoStageConfirmScript, VideoStageConfirmPrompt, VideoStageConfirmMeta:
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
		next = VideoStageRender
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
	case VideoStageRender, VideoStageCompose, VideoStageDone:
		from = VideoStageRender
	case VideoStageConfirmMeta:
		// gate③ 重生成只回 Agent4，不动视频
		from = VideoStageBuildMeta
	case VideoStageDraftScript, VideoStageBuildPrompt, VideoStageBuildMeta:
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
		task.Run.Script.Shots = normalizeScriptShots(edits.Shots, float64(task.Config.DurationS))
		// 剧本改动 → Agent2 全部 + 渲染 + 合成失效
		task.Run.Prompts = nil
		task.Run.Plan = nil
		task.Run.Subtitles = nil
		task.Run.Renders = nil
		task.Run.Output = nil
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

		// 字幕默认关闭：未开启时不生成 SRT/ASS，成片也不烧录
		task.Run.Subtitles = nil
		if task.Config.Subtitles {
			if workDir, dirErr := a.ensureTaskWorkDir(task.ID); dirErr != nil {
				task.Run.Warnings = append(task.Run.Warnings, "字幕文件重算失败: "+dirErr.Error())
			} else if subtitles, subErr := prepareSubtitleFiles(workDir, task.ID, task.Config, &plan); subErr != nil {
				task.Run.Warnings = append(task.Run.Warnings, "字幕文件重算失败: "+subErr.Error())
			} else {
				task.Run.Subtitles = subtitles
			}
		}

		// 提示词/台词改动 → 渲染 + 合成失效，字幕按新时间轴重生
		task.Run.Renders = nil
		task.Run.Output = nil
		task.Run.PublishMeta = nil
		task.Run.Gates[VideoGatePrompt] = videoGatePending
		task.Run.Gates[VideoGateMeta] = videoGatePending
		task.Status = VideoStatusWaiting
		task.Error = ""
		task.Run.Logs = append(task.Run.Logs, stageLog("提示词已修改，渲染计划已重算（字幕未开启）"))

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

	default:
		return task, errors.New("仅可在人工确认关卡修改产物")
	}

	task.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := a.updateVideoTaskRecord(task); err != nil {
		return task, err
	}
	return task, nil
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

// normalizeScriptShots 用户改动分镜后，重算时长与时间轴，保证口径一致。
func normalizeScriptShots(shots []VideoShot, total float64) []VideoShot {
	normalized := make([]VideoShot, 0, len(shots))
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
		if shot.DurationS <= 0 {
			shot.DurationS = total / float64(len(shots))
		}
		normalized = append(normalized, shot)
	}
	if len(normalized) == 0 {
		return normalized
	}

	sum := 0.0
	for _, shot := range normalized {
		sum += shot.DurationS
	}
	scale := 1.0
	if sum > 0 && total > 0 {
		scale = total / sum
	}
	cursor := 0.0
	for i := range normalized {
		duration := round2(mathMax(0.5, normalized[i].DurationS*scale))
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
		prompts[i].VisualPrompt = strings.TrimSpace(edit.VisualPrompt)
		prompts[i].Dialogue = strings.TrimSpace(edit.Dialogue)
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
	case VideoStageRender:
		task.Run.Gates[VideoGatePrompt] = videoGateApproved
	}

	runner := &videoRunner{app: a, task: &task, ctx: context.Background()}
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
			runner.task.Stage = VideoStageCompose
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

// runScriptStage Agent1：剧本/分镜。
func (r *videoRunner) runScriptStage() error {
	if tpl := r.task.Run.Template; tpl != nil {
		r.logf("按模板「%s」v%s 执行：%d 个字段来自模板，%d 个字段本次改过", tpl.Name, tpl.Version, len(tpl.Sources), len(tpl.Overrides))
	}
	script, warnings := r.app.generateScript(r.ctx, r.task.Config, r.task.Run.Template)
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
	r.task.Run.PublishMeta = nil
	r.task.Run.Gates[VideoGateScript] = videoGatePending
	r.task.Run.Gates[VideoGatePrompt] = videoGatePending
	r.task.Run.Gates[VideoGateMeta] = videoGatePending
	r.logf("Agent1 完成：%d 段分镜（来源 %s）", len(script.Shots), script.Source)
	return nil
}

// runPromptStage Agent2：提示词 + 字幕 + 成本计划。
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

	prompts, promptWarnings := r.app.generatePrompts(r.ctx, r.task.Config, *r.task.Run.Script, caps, r.task.Run.Template)
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

	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return err
	}
	if r.task.Config.Subtitles {
		if subtitles, subErr := prepareSubtitleFiles(workDir, r.task.ID, r.task.Config, &plan); subErr != nil {
			r.warn("生成字幕文件失败: %v", subErr)
		} else {
			r.task.Run.Subtitles = subtitles
		}
	} else {
		// 字幕默认关闭：不产出 SRT/ASS，成片也不烧录
		r.task.Run.Subtitles = nil
		r.logf("字幕未开启，本次不生成字幕文件")
	}

	// 重算后旧渲染结果作废
	r.task.Run.Renders = nil
	r.task.Run.Output = nil
	r.task.Run.PublishMeta = nil
	r.task.Run.Gates[VideoGatePrompt] = videoGatePending
	r.task.Run.Gates[VideoGateMeta] = videoGatePending
	r.logf("Agent2 完成：%d 段提示词，计划 %d 个片段（%.1fs）", len(prompts), plan.ClipCount, plan.TotalS)
	return nil
}

// runRenderStage Agent3：按段串行生成，支持尾帧延续与断点续跑。
// 逐段确认开启时，每段新渲染成功即返回 paused=true，由流水线置 waiting 停住等用户动作。
func (r *videoRunner) runRenderStage() (bool, error) {
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

		outputPath := filepath.Join(workDir, "clips", fmt.Sprintf("%02d-%s.mp4", segment.Index+1, segment.ID))
		request := VideoGenRequest{
			Prompt:          composeRenderPrompt(segment, caps),
			FirstFrame:      firstFrame,
			CharacterImages: characterImagesForSegment(cfg, r.task.Run.Script, segment),
			DurationS:       segment.DurationS,
			AspectRatio:     cfg.AspectRatio,
			Fps:             cfg.Fps,
			Seed:            cfg.VideoParams.Seed,
			OutputPath:      outputPath,
		}

		clipCtx, cancel := context.WithTimeout(r.ctx, 30*time.Minute)
		result, genErr := adapter.Generate(clipCtx, genCtx, request)
		cancel()

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

	meta, warnings := r.app.generatePublishMeta(r.ctx, r.task.Config, r.task.Run.Script, r.task.Run.Prompts)
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
func characterImagesForSegment(cfg VideoRunConfig, script *VideoScript, segment VideoSegment) []string {
	if script == nil || len(cfg.CharacterRefs) == 0 {
		return nil
	}
	roles := map[string]bool{}
	for _, shot := range script.Shots {
		if shot.ID != segment.ShotID {
			continue
		}
		for _, role := range shot.Roles {
			roles[role] = true
		}
	}
	if len(roles) == 0 {
		return nil
	}
	images := make([]string, 0, 4)
	for _, ref := range cfg.CharacterRefs {
		if !roles[ref.RoleID] {
			continue
		}
		images = append(images, ref.Images...)
	}
	return images
}
