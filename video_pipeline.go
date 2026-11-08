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
	case VideoStageConfirmScript, VideoStageConfirmPrompt, VideoStageConfirmEdit, VideoStageConfirmMeta:
		return errors.New("当前处于人工确认关卡，请先确认或重新生成")
	case VideoStageDone:
		return errors.New("任务已完成；如需重做请使用「重新生成」")
	case VideoStageRender:
		if task.Run.ReviewPendingSegment != "" {
			return errors.New("当前处于片段确认关卡，请先确认或重新生成当前片段")
		}
		if task.Run.ReviewPendingPromptSegment != "" {
			return errors.New("当前处于分段提示词确认关卡，请先确认提示词或修改后再生成本段")
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
	case VideoStageConfirmEdit:
		// 剪辑确认后合成只负责按最终计划重建画布并烧录：时间轴以用户的设置为准，不再回测重排
		ensureVideoGates(&task.Run)
		if task.Run.Subtitles != nil {
			task.Run.Subtitles.Confirmed = true
			if task.Run.Subtitles.EditedAt == "" {
				task.Run.Subtitles.EditedAt = time.Now().Format(time.RFC3339)
			}
		}
		task.Run.Gates[VideoGateEdit] = videoGateApproved
		task.Run.Logs = append(task.Run.Logs, stageLog("剪辑与字幕已确认，合成阶段按这份时间轴烧录"))
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
	discardEdit := false
	switch task.Stage {
	case VideoStageConfirmScript:
		from = VideoStageDraftScript
	case VideoStageConfirmPrompt:
		from = VideoStageBuildPrompt
	case VideoStageConfirmEdit:
		// 「丢弃草稿并重新生成」：连同画布缓存一起清掉，BUILD_EDIT 才会重算默认计划
		from = VideoStageBuildEdit
		discardEdit = true
	case VideoStageRender, VideoStageCompose, VideoStageDone:
		from = VideoStageRender
	case VideoStageConfirmMeta:
		// gate③ 重生成只回 Agent4，不动视频
		from = VideoStageBuildMeta
	case VideoStageDraftScript, VideoStageBuildPrompt, VideoStageBuildEdit, VideoStageBuildMeta:
		// 保持当前阶段
	default:
		from = VideoStageDraftScript
	}

	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if discardEdit {
		// 先落库再起协程：runVideoPipeline 会重新读库，读不到清理结果就白丢了草稿
		task.Run.Edit = nil
		task.Run.Compose = nil
		task.Run.Subtitles = nil
		task.Run.SubtitleIssues = nil
		ensureVideoGates(&task.Run)
		task.Run.Gates[VideoGateEdit] = videoGatePending
		task.Run.Logs = append(task.Run.Logs, stageLog("已丢弃剪辑草稿，按现有视频重新生成默认剪辑计划与字幕"))
		task.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := a.updateVideoTaskRecord(task); err != nil {
			a.endVideoRun(id)
			return err
		}
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
	if task.Run.ReviewPendingPromptSegment != "" {
		return errors.New("请先确认待生成的段落提示词")
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

// ApproveVideoSegmentPrompt 逐段确认的「生成前」关卡：确认或修改下一段提示词后，才开始消耗额度。
// 这是需求 11 的核心：用户应该在提交视频模型之前看到并改掉下一段的画面写法。
func (a *App) ApproveVideoSegmentPrompt(id string, edit VideoSegmentRegen) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if task.Stage != VideoStageRender {
		return errors.New("仅可在视频生成阶段确认段落提示词")
	}
	pending := strings.TrimSpace(task.Run.ReviewPendingPromptSegment)
	if pending == "" {
		return errors.New("当前没有待确认的段落提示词")
	}
	segmentID := strings.TrimSpace(edit.SegmentID)
	if segmentID == "" {
		segmentID = pending
	}
	if segmentID != pending {
		return errors.New("提交的片段不是当前待确认的段落")
	}
	if task.Run.Plan == nil || len(task.Run.Plan.Segments) == 0 {
		return errors.New("任务缺少渲染计划，请先确认提示词")
	}
	target := -1
	for index, segment := range task.Run.Plan.Segments {
		if segment.ID == segmentID {
			target = index
			break
		}
	}
	if target < 0 {
		return errors.New("待确认的片段不在渲染计划中")
	}
	if err := applySegmentEdit(&task.Run, target, edit, task.Config); err != nil {
		return err
	}

	task.Run.ReviewPendingPromptSegment = ""
	markSegmentPromptApproved(&task.Run, segmentID)
	task.Run.ReviewPendingSegment = ""
	task.Status = VideoStatusRunning
	task.Error = ""
	task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf(
		"第 %d/%d 段提示词已确认，开始生成", target+1, len(task.Run.Plan.Segments))))
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

// applySegmentEdit 把逐段修改写进渲染计划，并同步回提示词关卡的权威产物。
func applySegmentEdit(run *VideoRunContext, target int, edit VideoSegmentRegen, cfg VideoRunConfig) error {
	if run.Plan == nil || target < 0 || target >= len(run.Plan.Segments) {
		return errors.New("要修改的片段不在渲染计划中")
	}
	segment := &run.Plan.Segments[target]
	if prompt := strings.TrimSpace(edit.VisualPrompt); prompt != "" {
		segment.Prompt = prompt
	}

	lines := normalizeDialogueLines(edit.DialogueLines, edit.Dialogue, edit.Speaker)
	firstOfShot := true
	for i := 0; i < target; i++ {
		if run.Plan.Segments[i].ShotID == segment.ShotID {
			firstOfShot = false
			break
		}
	}
	if !firstOfShot && len(lines) > 0 {
		// 长镜头拆段后台词只落在首段：允许第 2 段填台词会造出“有声音、无字幕”的片段。
		return fmt.Errorf("第 %d 段的台词属于镜头 %s 的首段，请在首段修改台词与说话人", target+1, segment.ShotID)
	}
	if firstOfShot {
		roles := shotRolesOf(run.Script, segment.ShotID)
		if len(lines) == 1 && strings.TrimSpace(lines[0].Speaker) == "" && strings.TrimSpace(edit.Speaker) == "" {
			// 只改台词没提交归属：沿用这段原本的说话人，不要悄悄退回旁白。
			lines[0].Speaker = segment.Speaker
		}
		roleSet := make(map[string]bool, len(roles))
		for _, role := range roles {
			roleSet[role] = true
		}
		for i := range lines {
			speaker, _ := resolveSpeaker(cfg, lines[i].Speaker, roles, segment.ShotID, i, roleSet)
			lines[i].Speaker = speaker
			if speaker != "" && speaker != VideoSpeakerNarration {
				roleSet[speaker] = true
			}
		}
		segment.DialogueLines = lines
		segment.Dialogue = dialogueText(lines)
		segment.Speaker = dialogueSpeakerSummary(lines)
		segment.SpeakerName = dialogueSpeakerName(cfg, lines)
	} else {
		segment.DialogueLines = nil
		segment.Dialogue = ""
		segment.Speaker = ""
		segment.SpeakerName = ""
	}
	return syncSegmentEditToPrompt(run, target, cfg)
}

// shotRolesOf 按 shotId 取分镜的出场角色，供段落编辑时解析说话人兜底。
func shotRolesOf(script *VideoScript, shotID string) []string {
	if script == nil {
		return nil
	}
	for _, shot := range script.Shots {
		if shot.ID == shotID {
			return append([]string{}, shot.Roles...)
		}
	}
	return nil
}

// segmentPromptApproved 该段提示词是否已在生成前被用户确认过。
func segmentPromptApproved(run *VideoRunContext, segmentID string) bool {
	segmentID = strings.TrimSpace(segmentID)
	for _, approved := range run.ApprovedSegmentPrompts {
		if approved == segmentID {
			return true
		}
	}
	return false
}

func markSegmentPromptApproved(run *VideoRunContext, segmentID string) {
	segmentID = strings.TrimSpace(segmentID)
	if segmentID == "" || segmentPromptApproved(run, segmentID) {
		return
	}
	run.ApprovedSegmentPrompts = append(run.ApprovedSegmentPrompts, segmentID)
}

func clearSegmentPromptApprovals(run *VideoRunContext) {
	run.ApprovedSegmentPrompts = nil
}

// clearSegmentPromptApprovalsFrom 作废 target 及其后所有片段的提示词确认。
// 前面的片段已经生成过，不受影响；后面的片段必须重新走生成前确认。
func clearSegmentPromptApprovalsFrom(run *VideoRunContext, plan *VideoPlan, target int) {
	if plan == nil {
		clearSegmentPromptApprovals(run)
		return
	}
	keep := make(map[string]bool, target)
	for index := 0; index < target && index < len(plan.Segments); index++ {
		keep[plan.Segments[index].ID] = true
	}
	filtered := make([]string, 0, len(run.ApprovedSegmentPrompts))
	for _, approved := range run.ApprovedSegmentPrompts {
		if keep[approved] {
			filtered = append(filtered, approved)
		}
	}
	run.ApprovedSegmentPrompts = filtered
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

	segment := &task.Run.Plan.Segments[target]
	if err := applySegmentEdit(&task.Run, target, edit, task.Config); err != nil {
		return err
	}
	// 用户刚在逐段确认里改过这一段，本次重渲不再二次弹提示词确认；
	// 但后续片段依赖重渲后的画面，必须重新确认提示词。
	clearSegmentPromptApprovalsFrom(&task.Run, task.Run.Plan, target)
	markSegmentPromptApproved(&task.Run, segment.ID)

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
	task.Run.ReviewPendingPromptSegment = ""
	task.Status = VideoStatusRunning
	task.Error = ""
	task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf("第 %d 段提示词已修改（已同步回提示词关卡），重新生成该段", target+1)))
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

// syncSegmentEditToPrompt 把逐段重生成的修改回写 Run.Prompts。
// Run.Prompts 是提示词关卡的唯一权威源：只改 Plan 的话，重算计划或回到关卡②时，
// 用户逐段改过的提示词会悄悄丢失，表现就是「改了没用」。
func syncSegmentEditToPrompt(run *VideoRunContext, target int, cfg VideoRunConfig) error {
	if run.Plan == nil || target < 0 || target >= len(run.Plan.Segments) {
		return errors.New("要重生成的片段不在渲染计划中")
	}
	segment := run.Plan.Segments[target]
	promptIndex := -1
	for i := range run.Prompts {
		if run.Prompts[i].ShotID == segment.ShotID {
			promptIndex = i
			break
		}
	}
	if promptIndex < 0 {
		return fmt.Errorf("片段 %s 对应的分镜提示词已丢失，请回到提示词关卡重新生成", segment.ID)
	}

	prompt := &run.Prompts[promptIndex]
	if visual := strings.TrimSpace(segment.Prompt); visual != "" {
		prompt.VisualPrompt = visual
	}

	// 台词是镜头级的：只有该镜头的第一段才回写。
	// 长镜头拆成多段时台词只落在首段，重渲第 2 段不能把首段台词清空。
	firstOfShot := true
	for i := 0; i < target; i++ {
		if run.Plan.Segments[i].ShotID == segment.ShotID {
			firstOfShot = false
			break
		}
	}
	if firstOfShot {
		prompt.DialogueLines = append([]VideoDialogueLine{}, segment.DialogueLines...)
		prompt.Dialogue = segment.Dialogue
		prompt.Speaker = segment.Speaker
		duration := prompt.End - prompt.Start
		prompt.CharCount = speechUnitCount(prompt.Dialogue, cfg.Language)
		prompt.EstSpeechS = estimateSpeechSeconds(prompt.Dialogue, cfg.Language, cfg.SpeechRate)
		prompt.Fits = prompt.EstSpeechS <= duration+0.1
		if prompt.Fits {
			prompt.Note = ""
		} else {
			prompt.Note = fmt.Sprintf("台词约 %.1fs，超出本段 %.1fs，建议精简或改排期", prompt.EstSpeechS, duration)
		}
	}
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
		task.Run.SubtitleIssues = nil
		task.Run.Edit = nil
		task.Run.Renders = nil
		task.Run.Output = nil
		task.Run.Compose = nil
		task.Run.ReviewPendingSegment = ""
		task.Run.ReviewPendingPromptSegment = ""
		clearSegmentPromptApprovals(&task.Run)
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
		_, adapter, provider, _, _ := a.resolveVideoAdapter(task.Config)
		caps := adapterCapabilities(adapter, provider.Model)
		if err := applyPromptEdits(task.Run.Prompts, edits.Prompts, task.Config, caps); err != nil {
			return task, err
		}
		// 段时长再写回分镜：下次重新生成提示词时不能把用户改过的时长丢掉。
		syncPromptTimelineToScript(&task.Config, task.Run.Script, task.Run.Prompts)
		// 用户改过段时长后，任务目标时长跟着提示词时间轴走；渲染计划再按模型档位取整。
		if total := promptTotalSeconds(task.Run.Prompts); total > 0 {
			task.Config.DurationS = int(math.Round(total))
		}
		plan := buildVideoPlan(task.Config, task.Run.Prompts, caps, task.Run.Adapter)
		task.Run.Plan = &plan

		// 字幕在视频生成之后才铺轴：这里只作废旧字幕
		task.Run.Subtitles = nil
		task.Run.SubtitleIssues = nil
		task.Run.Edit = nil
		task.Run.Compose = nil

		// 提示词/台词改动 → 渲染 + 合成失效，字幕等视频出来后重生
		task.Run.Renders = nil
		task.Run.Output = nil
		task.Run.PublishMeta = nil
		task.Run.ReviewPendingSegment = ""
		task.Run.ReviewPendingPromptSegment = ""
		clearSegmentPromptApprovals(&task.Run)
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

	case VideoStageConfirmEdit:
		if edits.Subtitles == nil {
			return task, errors.New("没有提交字幕修改")
		}
		if task.Run.Subtitles == nil {
			return task, errors.New("任务还没有字幕，请先生成视频")
		}
		if err := a.applySubtitleEdits(&task, edits.Subtitles.Cues); err != nil {
			return task, err
		}
		task.Run.Gates[VideoGateEdit] = videoGatePending
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
	total := composeTotalS(context.Background(), task.Config, prep)
	if total <= 0 {
		total = float64(task.Config.DurationS)
	}

	normalized, issues := normalizeSubtitleCues(cues, total, task.Config)
	task.Run.SubtitleIssues = issues
	for _, issue := range issues {
		if issue.Level != "error" {
			continue
		}
		duplicate := false
		for _, existing := range task.Run.Warnings {
			if existing == issue.Message {
				duplicate = true
				break
			}
		}
		if !duplicate {
			task.Run.Warnings = append(task.Run.Warnings, issue.Message)
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

// ValidateVideoEdit 实时校验剪辑草稿：不写库，返回字幕问题；片段本身不合法直接返回错误。
func (a *App) ValidateVideoEdit(id string, edit VideoEditEdit) ([]VideoSubtitleIssue, error) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return nil, err
	}
	return a.validateVideoEditPayload(context.Background(), &task, edit)
}

func (a *App) validateVideoEditPayload(ctx context.Context, task *VideoTask, edit VideoEditEdit) ([]VideoSubtitleIssue, error) {
	clips, _, err := normalizeVideoEditClips(ctx, task.Config, &task.Run, edit.Clips,
		task.Config.ContinuationMode == "last_frame")
	if err != nil {
		return nil, err
	}
	candidate := &VideoEditPlan{Clips: clips}
	if task.Run.Edit != nil {
		candidate.Revision = task.Run.Edit.Revision
		if videoEditChanged(task.Run.Edit, candidate) {
			candidate.Revision++
		}
	} else if len(clips) > 0 {
		candidate.Revision = 1
	}
	probe := *task
	runCopy := task.Run
	runCopy.Edit = candidate
	probe.Run = runCopy
	total := float64(task.Config.DurationS)
	if spans, _, spanErr := editSubtitleSpans(ctx, &runCopy); spanErr == nil && len(spans) > 0 {
		last := spans[len(spans)-1]
		total = round2(last.Start + last.Duration)
	}
	cues := edit.Cues
	if cues == nil && task.Run.Subtitles != nil {
		cues = task.Run.Subtitles.Cues
	}
	_, issues := normalizeSubtitleCues(cues, total, task.Config)
	return issues, nil
}

// SaveVideoEdit 保存剪辑计划与字幕草稿：只写库、跑校验，不重建预览、不标记已确认。
// 片段顺序/启停/入出点变化时 Revision+1；last_frame 下改变片段顺序直接拒绝。
func (a *App) SaveVideoEdit(id string, edit VideoEditEdit) (VideoTask, error) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return task, err
	}
	if a.isVideoTaskRunning(id) {
		return task, errors.New("任务正在执行，无法保存修改")
	}
	if task.Stage != VideoStageConfirmEdit {
		return task, errors.New("仅可在剪辑确认关卡保存剪辑")
	}
	if len(edit.Clips) == 0 {
		return task, errors.New("剪辑计划为空，至少保留一个片段")
	}

	// 先把 cue 的段内偏移按「旧剪辑 + 当前画布」算好，再换新计划
	oldSpans := map[string]SubtitleSpan{}
	if task.Run.Edit != nil {
		if spans, bySegment, spanErr := editSubtitleSpans(context.Background(), &task.Run); spanErr == nil {
			_ = spans
			oldSpans = bySegment
		}
	}
	cues := make([]VideoSubtitleCue, len(edit.Cues))
	copy(cues, edit.Cues)
	for index := range cues {
		segmentID := strings.TrimSpace(cues[index].SegmentID)
		if segmentID == "" {
			continue
		}
		if span, ok := oldSpans[segmentID]; ok {
			cues[index].OffsetS = round2(math.Max(0, cues[index].Start-span.Start))
		}
	}

	clips, clipWarnings, err := normalizeVideoEditClips(context.Background(), task.Config, &task.Run, edit.Clips,
		task.Config.ContinuationMode == "last_frame")
	if err != nil {
		return task, err
	}
	nextEdit := &VideoEditPlan{Clips: clips, UpdatedAt: time.Now().Format(time.RFC3339)}
	if task.Run.Edit != nil {
		nextEdit.Revision = task.Run.Edit.Revision
		if videoEditChanged(task.Run.Edit, nextEdit) {
			nextEdit.Revision++
		}
	} else {
		nextEdit.Revision = 1
	}
	task.Run.Edit = nextEdit

	total := float64(task.Config.DurationS)
	if spans, _, spanErr := editSubtitleSpans(context.Background(), &task.Run); spanErr == nil && len(spans) > 0 {
		last := spans[len(spans)-1]
		total = round2(last.Start + last.Duration)
	}
	normalized, issues := normalizeSubtitleCues(cues, total, task.Config)
	task.Run.SubtitleIssues = issues
	if hasSubtitleErrors(issues) {
		messages := make([]string, 0, 3)
		for _, issue := range issues {
			if issue.Level != "error" {
				continue
			}
			messages = append(messages, issue.Message)
			if len(messages) >= 3 {
				break
			}
		}
		return task, errors.New("字幕存在错误，未保存：" + strings.Join(messages, "；"))
	}

	workDir, err := a.ensureTaskWorkDir(task.ID)
	if err != nil {
		return task, err
	}
	width, height := subtitleCanvasSize(task.Config, task.Run.Compose)
	timingSource := "measured"
	editedAt := ""
	if task.Run.Subtitles != nil {
		timingSource = firstNonEmpty(task.Run.Subtitles.TimingSource, "measured")
		editedAt = task.Run.Subtitles.EditedAt
	}
	subtitles, err := writeSubtitleFiles(workDir, task.ID, task.Config, normalized, timingSource, "", width, height)
	if err != nil {
		return task, fmt.Errorf("写入字幕草稿失败: %w", err)
	}
	subtitles.EditedAt = editedAt
	task.Run.Subtitles = subtitles
	for _, warning := range clipWarnings {
		task.Run.Warnings = append(task.Run.Warnings, warning)
	}
	task.Run.Gates[VideoGateEdit] = videoGatePending
	task.Status = VideoStatusWaiting
	task.Error = ""
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := a.updateVideoTaskRecord(task); err != nil {
		return task, err
	}
	task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf("剪辑草稿已保存：%d 个片段、%d 条字幕（尚未应用与确认）",
		len(nextEdit.Clips), len(normalized))))
	return task, nil
}

// ApplyVideoEdit 异步应用剪辑：重建预览画布、按新时间轴重映射字幕，完成后回剪辑页。
func (a *App) ApplyVideoEdit(id string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if task.Stage != VideoStageConfirmEdit {
		return errors.New("仅可在剪辑确认关卡应用剪辑")
	}
	if task.Run.Edit == nil || len(task.Run.Edit.Clips) == 0 {
		return errors.New("缺少剪辑计划，请先保存草稿")
	}
	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	go func() {
		defer a.endVideoRun(id)
		a.runApplyVideoEdit(id)
	}()
	return nil
}

// runApplyVideoEdit 执行 ApplyVideoEdit：期间任务处于 BUILD_EDIT/running；失败也回剪辑页并留下原因。
func (a *App) runApplyVideoEdit(id string) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return
	}
	task.Stage = VideoStageBuildEdit
	task.Status = VideoStatusRunning
	task.Error = ""
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	runner := &videoRunner{app: a, task: &task, ctx: withUsageTask(context.Background(), id), feedbackStage: VideoStageBuildEdit}
	runner.logf("开始应用剪辑计划…")
	_ = runner.save()

	if applyErr := runner.runApplyEditStage(); applyErr != nil {
		runner.warn("应用剪辑失败，已保留原预览：%v", applyErr)
	}
	runner.task.Stage = VideoStageConfirmEdit
	runner.task.Status = VideoStatusWaiting
	runner.task.Run.Gates[VideoGateEdit] = videoGatePending
	_ = runner.save()
}

// RelayoutVideoSubtitles 按当前剪辑与画布重新测时、声学对齐并覆盖字幕（会丢弃手工修改）。
func (a *App) RelayoutVideoSubtitles(id string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	if task.Stage != VideoStageConfirmEdit {
		return errors.New("仅可在剪辑确认关卡重铺字幕")
	}
	if !a.beginVideoRun(id) {
		return errors.New("任务正在执行，请稍候")
	}
	go func() {
		defer a.endVideoRun(id)
		a.runRelayoutVideoSubtitles(id)
	}()
	return nil
}

func (a *App) runRelayoutVideoSubtitles(id string) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return
	}
	task.Stage = VideoStageBuildEdit
	task.Status = VideoStatusRunning
	task.Error = ""
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	runner := &videoRunner{app: a, task: &task, ctx: withUsageTask(context.Background(), id), feedbackStage: VideoStageBuildEdit}
	runner.logf("开始按当前时间轴重铺字幕（手工修改将被覆盖）…")
	_ = runner.save()

	if layoutErr := runner.relayoutSubtitleStage(); layoutErr != nil {
		runner.warn("重铺字幕失败：%v", layoutErr)
	}
	runner.task.Stage = VideoStageConfirmEdit
	runner.task.Status = VideoStatusWaiting
	runner.task.Run.Gates[VideoGateEdit] = videoGatePending
	_ = runner.save()
}

// relayoutSubtitleStage 重新测时并重建 SRT/ASS；不复用旧 cue。
func (r *videoRunner) relayoutSubtitleStage() error {
	cfg := r.task.Config
	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return err
	}
	if r.task.Run.Edit == nil || len(r.task.Run.Edit.Clips) == 0 {
		edit, warnings, buildErr := buildDefaultVideoEditPlan(r.ctx, &r.task.Run)
		for _, warning := range warnings {
			r.warn("%s", warning)
		}
		if buildErr != nil {
			return buildErr
		}
		r.task.Run.Edit = edit
	}
	if !composePrepUsable(r.task.Run.Compose, &r.task.Run) {
		built, prepWarnings, prepErr := prepareComposeCanvas(r.ctx, workDir, r.task.ID, cfg, &r.task.Run)
		for _, warning := range prepWarnings {
			r.warn("%s", warning)
		}
		if prepErr != nil {
			return prepErr
		}
		r.task.Run.Compose = built
	}
	prep := r.task.Run.Compose
	canvas := ""
	if prep != nil {
		canvas = prep.CanvasPath
	}
	cues, timingSource, audioSource, measureWarnings := measureSubtitleCues(r.ctx, workDir, cfg, &r.task.Run, canvas)
	for _, warning := range measureWarnings {
		r.warn("%s", warning)
	}
	total := composeTotalS(r.ctx, cfg, prep)
	normalized, issues := normalizeSubtitleCues(cues, total, cfg)
	r.task.Run.SubtitleIssues = issues
	if len(normalized) == 0 {
		r.task.Run.Subtitles = nil
		r.logf("重铺后没有需要字幕的台词")
		return nil
	}
	width, height := subtitleCanvasSize(cfg, prep)
	subtitles, err := writeSubtitleFiles(workDir, r.task.ID, cfg, normalized, timingSource, audioSource, width, height)
	if err != nil {
		return err
	}
	r.task.Run.Subtitles = subtitles
	r.logf("字幕已按新时间轴重铺：%d 条，来源 %s", len(normalized), timingSource)
	return nil
}

// SetVideoSubtitlesEnabled 旧任务兼容：为本次开启/关闭字幕并生成时间轴。
// 新任务默认开启字幕，不需要这个开关。
func (a *App) SetVideoSubtitlesEnabled(id string, enabled bool) (VideoTask, error) {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return task, err
	}
	if a.isVideoTaskRunning(id) {
		return task, errors.New("任务正在执行，请稍候")
	}
	if task.Stage != VideoStageConfirmEdit {
		return task, errors.New("仅在剪辑确认关卡可以开启/关闭字幕")
	}
	task.Config.Subtitles = enabled
	if !enabled {
		task.Run.Subtitles = nil
		task.Run.SubtitleIssues = nil
		task.Run.Logs = append(task.Run.Logs, stageLog("已关闭本任务字幕：成片不会烧录字幕"))
		task.UpdatedAt = time.Now().Format(time.RFC3339)
		return task, a.updateVideoTaskRecord(task)
	}
	workDir, err := a.ensureTaskWorkDir(task.ID)
	if err != nil {
		return task, err
	}
	if task.Run.Edit == nil || len(task.Run.Edit.Clips) == 0 {
		edit, warnings, buildErr := buildDefaultVideoEditPlan(context.Background(), &task.Run)
		for _, warning := range warnings {
			task.Run.Warnings = append(task.Run.Warnings, warning)
		}
		if buildErr != nil {
			return task, buildErr
		}
		task.Run.Edit = edit
	}
	if !composePrepUsable(task.Run.Compose, &task.Run) {
		built, prepWarnings, prepErr := prepareComposeCanvas(context.Background(), workDir, task.ID, task.Config, &task.Run)
		for _, warning := range prepWarnings {
			task.Run.Warnings = append(task.Run.Warnings, warning)
		}
		if prepErr != nil {
			return task, prepErr
		}
		task.Run.Compose = built
	}
	prep := task.Run.Compose
	canvas := ""
	if prep != nil {
		canvas = prep.CanvasPath
	}
	cues, timingSource, audioSource, measureWarnings := measureSubtitleCues(context.Background(), workDir, task.Config, &task.Run, canvas)
	for _, warning := range measureWarnings {
		task.Run.Warnings = append(task.Run.Warnings, warning)
	}
	total := composeTotalS(context.Background(), task.Config, prep)
	normalized, issues := normalizeSubtitleCues(cues, total, task.Config)
	task.Run.SubtitleIssues = issues
	if len(normalized) == 0 {
		task.Run.Subtitles = nil
		task.Run.Logs = append(task.Run.Logs, stageLog("已为本次开启字幕，但本片没有需要字幕的台词"))
		task.UpdatedAt = time.Now().Format(time.RFC3339)
		return task, a.updateVideoTaskRecord(task)
	}
	width, height := subtitleCanvasSize(task.Config, prep)
	subtitles, err := writeSubtitleFiles(workDir, task.ID, task.Config, normalized, timingSource, audioSource, width, height)
	if err != nil {
		return task, err
	}
	task.Run.Subtitles = subtitles
	task.Run.Logs = append(task.Run.Logs, stageLog(fmt.Sprintf("已为本次开启字幕：%d 条，可在剪辑页继续调整", len(normalized))))
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	return task, a.updateVideoTaskRecord(task)
}
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
		shot.DialogueLines = normalizeDialogueLines(shot.DialogueLines, shot.Dialogue, shot.Speaker)
		shot.Dialogue = dialogueText(shot.DialogueLines)
		shot.Speaker = dialogueSpeakerSummary(shot.DialogueLines)
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

// applyPromptEdits 保存关卡②的提示词、台词、说话人与本段时长，并重排整条时间轴。
// 提示词是 Agent2 定稿、用户在关卡②确认过的最终版本：这里不再补主体约束，
// 否则用户手动删掉或改写的句式会被程序悄悄加回来。
// 改时长时后续镜头整体顺延：只改一段不能把后面的时间轴留在原地，否则字幕与渲染计划都会错位。
func applyPromptEdits(prompts []VideoPromptItem, edits []VideoPromptEdit, cfg VideoRunConfig, caps VideoCapabilities) error {
	index := make(map[string]int, len(prompts))
	for i, prompt := range prompts {
		index[strings.TrimSpace(prompt.ShotID)] = i
	}
	for _, edit := range edits {
		shotID := strings.TrimSpace(edit.ShotID)
		i, ok := index[shotID]
		if !ok {
			// 静默忽略会让用户以为改动保存成功，实际整条被丢掉
			return fmt.Errorf("提示词修改里的分镜 %q 不存在，请刷新任务后重试", shotID)
		}
		visual := strings.TrimSpace(edit.VisualPrompt)
		if visual == "" {
			return fmt.Errorf("分镜 %s 的画面提示词不能为空", shotID)
		}
		prompts[i].VisualPrompt = visual
		// 台词与归属：多说话人按行提交；旧客户端只传 dialogue/speaker 时自动升级为一行。
		lines := normalizeDialogueLines(edit.DialogueLines, edit.Dialogue, edit.Speaker)
		if len(lines) == 1 && strings.TrimSpace(edit.Speaker) == "" && strings.TrimSpace(lines[0].Speaker) == "" {
			// 关卡只改了台词、没动归属：沿用原归属，别把它退回旁白。
			lines[0].Speaker = prompts[i].Speaker
		}
		for lineIndex := range lines {
			raw := strings.TrimSpace(lines[lineIndex].Speaker)
			speaker := normalizeSpeaker(cfg, raw)
			if speaker == "" {
				speaker = raw
			}
			if speaker == "" {
				speaker = VideoSpeakerNarration
			}
			lines[lineIndex].Speaker = speaker
		}
		prompts[i].DialogueLines = lines
		prompts[i].Dialogue = dialogueText(lines)
		prompts[i].Speaker = dialogueSpeakerSummary(lines)
		if edit.DurationS > 0 {
			duration := clampFloat(edit.DurationS, 0.5, 600)
			prompts[i].End = prompts[i].Start + duration
		}
	}

	// 从 0 开始重排时间轴：每段时长以用户提交的值为准，后续镜头整体顺延。
	cursor := 0.0
	for i := range prompts {
		duration := prompts[i].End - prompts[i].Start
		if duration <= 0 {
			duration = float64(cfg.DurationS) / float64(len(prompts))
		}
		if duration <= 0 {
			duration = 1
		}
		prompts[i].Start = round2(cursor)
		prompts[i].End = round2(cursor + duration)
		cursor += duration

		prompts[i].CharCount = speechUnitCount(prompts[i].Dialogue, cfg.Language)
		prompts[i].EstSpeechS = estimateSpeechSeconds(prompts[i].Dialogue, cfg.Language, cfg.SpeechRate)
		prompts[i].Fits = prompts[i].EstSpeechS <= duration+0.1
		if prompts[i].Fits {
			prompts[i].Note = ""
		} else {
			prompts[i].Note = fmt.Sprintf("台词约 %.1fs，超出本段 %.1fs，建议精简或改排期", prompts[i].EstSpeechS, duration)
		}
		prompts[i].RenderDurationS = nearestAllowedDuration(duration, caps.AllowedDurations)
	}
	return nil
}

// promptTotalSeconds 提示词时间轴总时长；用于把用户改过的段时长同步回任务目标时长。
func promptTotalSeconds(prompts []VideoPromptItem) float64 {
	if len(prompts) == 0 {
		return 0
	}
	total := 0.0
	for _, prompt := range prompts {
		if duration := prompt.End - prompt.Start; duration > 0 {
			total += duration
		}
	}
	return round2(total)
}

// syncPromptTimelineToScript 把关卡②改过的段时长写回分镜。
// 否则下一次重新生成提示词会从旧分镜重建时间轴，用户刚改的时长又被打回原样。
func syncPromptTimelineToScript(cfg *VideoRunConfig, script *VideoScript, prompts []VideoPromptItem) {
	byShot := make(map[string]VideoPromptItem, len(prompts))
	for _, prompt := range prompts {
		byShot[prompt.ShotID] = prompt
	}
	apply := func(shots []VideoShot) {
		for i := range shots {
			prompt, ok := byShot[shots[i].ID]
			if !ok {
				continue
			}
			start := round2(prompt.Start)
			end := round2(prompt.End)
			if end <= start {
				continue
			}
			shots[i].DurationS = round2(end - start)
			shots[i].TimeRange = [2]float64{start, end}
		}
	}
	if script != nil {
		apply(script.Shots)
	}
	if cfg != nil && cfg.manualScriptEnabled() {
		apply(cfg.ManualShots)
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
	case VideoStageBuildEdit, VideoStageRender:
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

		case VideoStageBuildEdit:
			// 剪辑排在视频之后：生成默认剪辑计划、预览画布与字幕时间轴，再让用户调
			if err := runner.runEditStage(); err != nil {
				runner.fail(err)
				return
			}
			runner.task.Stage = VideoStageConfirmEdit
			runner.task.Run.Gates[VideoGateEdit] = videoGatePending
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
			runner.task.Stage = VideoStageBuildEdit
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
	hadPrompts := len(r.task.Run.Prompts) > 0

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
	if hadPrompts {
		r.logf("提示词已整批重算：你在关卡②里手工改过的内容以本次生成结果为准（重算前请先确认不再需要旧稿）")
	}
	clearSegmentPromptApprovals(&r.task.Run)

	plan := buildVideoPlan(r.task.Config, prompts, caps, adapterID)
	for _, warning := range plan.Warnings {
		r.warn("%s", warning)
	}
	if adapter == nil {
		r.warn("当前没有可用的视频模型：请先配置提供商，或在任务中选择「本地联调占位」")
	}
	r.task.Run.Plan = &plan

	// 字幕不在这一阶段做：台词等 gate② 定稿，视频出来后才由 BUILD_EDIT 铺轴
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

// composeTotalS 当前预览画布的时长：优先用预处理缓存，测不到时退回任务目标时长。
func composeTotalS(ctx context.Context, cfg VideoRunConfig, prep *VideoComposePrep) float64 {
	if prep != nil && prep.DurationS > 0 {
		return prep.DurationS
	}
	if prep != nil && prep.CanvasPath != "" && fileExists(prep.CanvasPath) {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		info, err := mediaProbe(probeCtx, prep.CanvasPath)
		cancel()
		if err == nil && info.Duration > 0 {
			return round2(info.Duration)
		}
	}
	return float64(cfg.DurationS)
}

// runEditStage 剪辑阶段：生成默认剪辑计划 → 按计划重建预览画布 → 自动铺字幕。
func (r *videoRunner) runEditStage() error {
	cfg := r.task.Config
	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return err
	}

	if r.task.Run.Edit == nil || len(r.task.Run.Edit.Clips) == 0 {
		edit, editWarnings, buildErr := buildDefaultVideoEditPlan(r.ctx, &r.task.Run)
		for _, warning := range editWarnings {
			r.warn("%s", warning)
		}
		if buildErr != nil {
			return buildErr
		}
		r.task.Run.Edit = edit
		r.logf("已生成默认剪辑计划：%d 个片段（渲染顺序、整段保留）", len(edit.Clips))
	}

	if !composePrepUsable(r.task.Run.Compose, &r.task.Run) {
		built, prepWarnings, prepErr := prepareComposeCanvas(r.ctx, workDir, r.task.ID, cfg, &r.task.Run)
		for _, warning := range prepWarnings {
			r.warn("%s", warning)
		}
		if prepErr != nil {
			return fmt.Errorf("重建预览画布失败: %w", prepErr)
		}
		r.task.Run.Compose = built
		r.logf("预览画布已就绪：%.1fs（%dx%d），可在剪辑页直接播放",
			built.DurationS, built.CanvasWidth, built.CanvasHeight)
	}

	if !cfg.Subtitles {
		r.task.Run.Subtitles = nil
		r.task.Run.SubtitleIssues = nil
		r.logf("该任务未开启字幕：仍可剪辑片段；需要字幕时可在此页一键开启")
		return nil
	}

	prep := r.task.Run.Compose
	canvas := ""
	if prep != nil {
		canvas = prep.CanvasPath
	}
	cues, timingSource, audioSource, measureWarnings := measureSubtitleCues(r.ctx, workDir, cfg, &r.task.Run, canvas)
	for _, warning := range measureWarnings {
		r.warn("%s", warning)
	}
	total := composeTotalS(r.ctx, cfg, prep)
	normalized, issues := normalizeSubtitleCues(cues, total, cfg)
	if len(normalized) == 0 {
		r.task.Run.Subtitles = nil
		r.task.Run.SubtitleIssues = issues
		r.logf("本片没有需要字幕的台词，字幕时间轴为空")
		return nil
	}
	width, height := subtitleCanvasSize(cfg, prep)
	subtitles, err := writeSubtitleFiles(workDir, r.task.ID, cfg, normalized, timingSource, audioSource, width, height)
	if err != nil {
		return fmt.Errorf("生成字幕文件失败: %w", err)
	}
	r.task.Run.Subtitles = subtitles
	r.task.Run.SubtitleIssues = issues
	r.logf("剪辑与字幕已生成：%d 个片段、%d 条字幕，时间轴来源 %s（可在剪辑页预览、裁剪、调时间轴）",
		len(r.task.Run.Edit.Clips), len(normalized), subtitles.TimingSource)
	if subtitles.TimingSource == "aligned" || subtitles.TimingSource == "measured" {
		if measured := measuredSpeechRate(subtitles.Cues, cfg.Language); measured > 0 {
			unit := "字"
			if isEnglishLanguage(cfg.Language) {
				unit = "词"
			}
			configured := cfg.SpeechRate
			if configured <= 0 {
				configured = defaultSpeechRate(cfg.Language)
			}
			r.logf("实测语速约 %.1f %s/秒（设置 %.1f）：字幕轴按真实音频活动铺；模型读字快慢仍由模型自身决定",
				measured, unit, configured)
		}
	}
	return nil
}

// runApplyEditStage 应用剪辑计划：重建预览画布，并按新时间轴重映射字幕草稿。
// 只改字幕（Revision 未变）时跳过重编码，也保留用户手工调整的 cue。
func (r *videoRunner) runApplyEditStage() error {
	cfg := r.task.Config
	workDir, err := r.app.ensureTaskWorkDir(r.task.ID)
	if err != nil {
		return err
	}
	if r.task.Run.Edit == nil || len(r.task.Run.Edit.Clips) == 0 {
		return errors.New("没有可应用的剪辑计划")
	}
	clips, clipWarnings, err := normalizeVideoEditClips(r.ctx, cfg, &r.task.Run, r.task.Run.Edit.Clips,
		cfg.ContinuationMode == "last_frame")
	for _, warning := range clipWarnings {
		r.warn("%s", warning)
	}
	if err != nil {
		return err
	}
	nextEdit := *r.task.Run.Edit
	nextEdit.Clips = clips
	nextEdit.UpdatedAt = time.Now().Format(time.RFC3339)
	r.task.Run.Edit = &nextEdit

	revisionChanged := r.task.Run.Compose == nil ||
		r.task.Run.Compose.EditRevision != r.task.Run.Edit.Revision ||
		!composePrepUsable(r.task.Run.Compose, &r.task.Run)
	if revisionChanged {
		built, prepWarnings, prepErr := prepareComposeCanvas(r.ctx, workDir, r.task.ID, cfg, &r.task.Run)
		for _, warning := range prepWarnings {
			r.warn("%s", warning)
		}
		if prepErr != nil {
			return fmt.Errorf("重建预览画布失败: %w", prepErr)
		}
		r.task.Run.Compose = built
		r.logf("预览画布已重建：%.1fs（%dx%d）", built.DurationS, built.CanvasWidth, built.CanvasHeight)
	} else {
		r.logf("剪辑版本未变化：沿用现有预览画布，保留当前字幕草稿")
	}

	if !cfg.Subtitles {
		return nil
	}
	if !revisionChanged && r.task.Run.Subtitles != nil {
		return nil
	}
	prep := r.task.Run.Compose
	total := composeTotalS(r.ctx, cfg, prep)
	var cues []VideoSubtitleCue
	timingSource := "measured"
	if r.task.Run.Subtitles != nil {
		timingSource = firstNonEmpty(r.task.Run.Subtitles.TimingSource, "measured")
		_, spans, spanErr := editSubtitleSpans(r.ctx, &r.task.Run)
		if spanErr != nil {
			return spanErr
		}
		// 旧时间轴由 SaveVideoEdit 按保存前的剪辑算好 OffsetS，这里按新段起点重映射
		relayout, notes := relayoutSubtitleCues(r.task.Run.Subtitles.Cues, nil, spans, total, cfg)
		for _, note := range notes {
			r.warn("%s", note)
		}
		cues = relayout
	} else {
		canvas := ""
		if prep != nil {
			canvas = prep.CanvasPath
		}
		generated, source, audioSource, measureWarnings := measureSubtitleCues(r.ctx, workDir, cfg, &r.task.Run, canvas)
		for _, warning := range measureWarnings {
			r.warn("%s", warning)
		}
		cues, timingSource = generated, source
		_ = audioSource
	}
	normalized, issues := normalizeSubtitleCues(cues, total, cfg)
	r.task.Run.SubtitleIssues = issues
	if len(normalized) == 0 {
		r.task.Run.Subtitles = nil
		r.logf("应用剪辑后没有剩余字幕")
		return nil
	}
	width, height := subtitleCanvasSize(cfg, prep)
	subtitles, err := writeSubtitleFiles(workDir, r.task.ID, cfg, normalized, timingSource, "", width, height)
	if err != nil {
		return fmt.Errorf("重写字幕文件失败: %w", err)
	}
	r.task.Run.Subtitles = subtitles
	r.logf("剪辑已应用：字幕按新时间轴重映射为 %d 条（未确认前不会烧录）", len(normalized))
	return nil
}

// measuredSpeechRate 从字幕停留时长反算实际语速，用于校准「设置语速」与模型真实读字速度的差距。
func measuredSpeechRate(cues []VideoSubtitleCue, lang string) float64 {
	units := 0
	seconds := 0.0
	for _, cue := range cues {
		units += speechUnitCount(cue.Text, lang)
		if duration := cue.End - cue.Start; duration > 0 {
			seconds += duration
		}
	}
	if units == 0 || seconds <= 0 {
		return 0
	}
	return round2(float64(units) / seconds)
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
	r.task.Run.ReviewPendingPromptSegment = ""

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
	// 画面要重出了：旧画布、旧剪辑计划与旧字幕时间轴都不再可信，渲完后 BUILD_EDIT 会重铺
	r.task.Run.Compose = nil
	r.task.Run.Subtitles = nil
	r.task.Run.SubtitleIssues = nil
	r.task.Run.Edit = nil

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

		// 逐段确认的「生成前」关卡（需求 11）：下一段的提示词在提交视频模型前先让用户过目。
		// 第 1 段已在提示词关卡整体确认过，不再重复打断；用户刚在逐段确认里改过的段也视为已确认。
		if taskReview && segment.Index > 0 && !segmentPromptApproved(&r.task.Run, segment.ID) {
			r.task.Run.ReviewPendingPromptSegment = segment.ID
			r.task.Run.Renders = append([]VideoRender{}, renders...)
			r.logf("第 %d/%d 段待生成：提示词已展示，确认后再提交视频模型（可在确认面板里直接改写提示词/台词）",
				segment.Index+1, len(segments))
			_ = r.save()
			return true, nil
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
		// 提示词由 Agent2 定稿、用户在关卡②确认：渲染层只按定稿原样提交，
		// 不再追加或改写主体约束，保证“看到的提示词就是发给视频模型的提示词”。
		renderPrompt := composeRenderPrompt(segment, caps, cfg)
		request := VideoGenRequest{
			Prompt:           renderPrompt,
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
				if roleID, ok := matchSpeakerRole(cfg, role); ok {
					role = roleID
				}
				if key := normalizeRoleKey(role); key != "" {
					roles[key] = true
				}
			}
		}
	}
	// 说话人即使没写进 roles，也得算出场：台词归他、画面却没参考过他，脸一定翻。
	// 多说话人时逐个带上，不能只看单值 Speaker。
	if speaker := strings.TrimSpace(segment.Speaker); speaker != "" && speaker != VideoSpeakerNarration {
		if roleID, ok := matchSpeakerRole(cfg, speaker); ok {
			speaker = roleID
		}
		if key := normalizeRoleKey(speaker); key != "" {
			roles[key] = true
		}
	}
	for _, line := range normalizeDialogueLines(segment.DialogueLines, segment.Dialogue, segment.Speaker) {
		if line.Speaker == "" || line.Speaker == VideoSpeakerNarration {
			continue
		}
		speaker := line.Speaker
		if roleID, ok := matchSpeakerRole(cfg, speaker); ok {
			speaker = roleID
		}
		if key := normalizeRoleKey(speaker); key != "" {
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
		lines := normalizeDialogueLines(item.DialogueLines, item.Dialogue, item.Speaker)
		switch {
		case len(lines) == 0:
			parts = append(parts, item.ShotID+"=无台词")
		default:
			parts = append(parts, item.ShotID+"="+dialogueSpeakerName(cfg, lines))
		}
	}
	return strings.Join(parts, "／")
}

// normalizeRoleKey role 标识大小写 / 空格不敏感，便于容忍模型返回的写法差异。
func normalizeRoleKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// roleMatches 只做精确匹配（role_id / 角色名）。分镜角色在入库前已归一成 role_id，
// 这里再放宽到包含关系会让 char_1 抢走 char_12 的参考图与台词。
func roleMatches(roles map[string]bool, ref VideoCharacterRef) bool {
	if len(roles) == 0 {
		return false
	}
	return roles[normalizeRoleKey(ref.RoleID)] || roles[normalizeRoleKey(ref.Name)]
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
	if roleID, ok := matchSpeakerRole(cfg, role); ok {
		role = roleID
	}
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
