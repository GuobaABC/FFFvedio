package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const agentRequestTimeout = 3 * time.Minute

// pickTextProvider 选择 Agent1/Agent2 使用的文本模型：
// 优先任务指定的提供商，其次第一个「文本模型」，最后退到「多模态模型」。
func (a *App) pickTextProvider(cfg VideoRunConfig) Provider {
	providers, err := a.ListProviders()
	if err != nil || len(providers) == 0 {
		return Provider{}
	}
	if cfg.TextProviderID != "" {
		for _, p := range providers {
			if p.ID == cfg.TextProviderID {
				return p
			}
		}
	}
	for _, p := range providers {
		if p.Category == CategoryText {
			return p
		}
	}
	for _, p := range providers {
		if p.Category == CategoryMultimodal {
			return p
		}
	}
	return Provider{}
}

// chatCompletionsEndpoint 拼出 OpenAI 兼容的对话接口地址。
// Base URL 允许填到版本段（.../api/v3），也允许误填成具体接口（.../chat/completions、.../responses）：
// 统一先剥掉操作段再拼，避免出现 .../responses/v1/chat/completions 这类 404 路径。
func chatCompletionsEndpoint(baseURL string) string {
	base := apiRoot(baseURL)
	if base == "" {
		return ""
	}
	for _, suffix := range versionSuffixes {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			return base + "/chat/completions"
		}
	}
	return base + "/v1/chat/completions"
}

// extractJSONBlock 兼容模型偶发的 Markdown 代码块与前后解释文字。
func extractJSONBlock(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```")
		if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
			trimmed = trimmed[:idx]
		}
		trimmed = strings.TrimSpace(trimmed)
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		return trimmed[start : end+1]
	}
	return trimmed
}

// animalCastDirective 出镜主体默认约束：任务完全没有图片输入时，一律用猫/狗/猪/鸡等动物担当，
// 避免真人形象（肖像与合规风险）。只要用户给了首帧或角色参考图，就换用以图为准的约束。
// 三段 Agent 提示词都会带上选中的这条约束，由 Agent2 写进每段最终提示词；渲染层不再追加或改写。
// 刻意写短（约 60 字）：它会被拼到每段提示词里，而 Seedance 单镜头提示词的舒适区只有几十到一百多字，
// 一条 90 字以上的尾巴会直接挤掉动作与光线描述。
const animalCastDirective = `画面主体只用动物角色（猫狗猪鸡等家畜家禽），可拟人化（直立、穿衣、做表情）但外形必须清晰可辨，不出现真人面孔与人体`

// imageCastDirective 有图时的主体约束：首帧与角色参考图都算，形象交给图，文字不再另编外观。
// 措辞同时覆盖“全模态参考”与“严格首帧”两种提交方式，渲染期不需要再改写。
const imageCastDirective = `画面主体与外观必须与用户提供的参考图或本段起始帧保持一致，不要另编与图不一致的形象`

// textAnchorCastDirective 有角色图、但模型读不到图且没有首帧可锁时的兜底：
// 只能用文字外观锚点，绝不回退成动物主体。
const textAnchorCastDirective = `角色形象必须与分镜里给出的外观描述一致，全程保持同一形象，不要逐段改变`

// firstFrameSemanticsDirective 首帧只是起始状态，不是最终画面。
// 用户给首帧是为了锁定主体身份与开场，不是让模型把背景、构图和动作也一并锁死；
// 提示词描述的是首帧之后的变化，冲突时以提示词的新画面为准。
const firstFrameSemanticsDirective = `首帧是起始状态，不是最终画面：提示词描述的是首帧之后发生的变化。当首帧的环境、构图、动作与提示词冲突时，以提示词的新画面为准，在开头 0.5–1 秒内自然过渡到新场景，只保持主体身份（脸型、毛色、服装）连续，不锁死首帧的背景与机位。`

// firstFrameSemantics 只在用户显式提供首帧时返回约束；没有首帧时不能凭空多出一层限制。
func firstFrameSemantics(cfg VideoRunConfig) string {
	if !hasUserFirstFrame(cfg) {
		return ""
	}
	return firstFrameSemanticsDirective
}

// firstFrameSemanticsBlock 给 Agent 提示词用的独立小节；没有首帧时返回空串。
func firstFrameSemanticsBlock(cfg VideoRunConfig) string {
	semantics := firstFrameSemantics(cfg)
	if semantics == "" {
		return ""
	}
	return "\n【首帧语义】" + semantics + `
- 这一节是硬约束：用户是按“首帧之后要变成什么”写的提示词，不要让首帧把新画面锁回去。
`
}

// hasCharacterReferenceImages 任务是否配了角色参考图：任一角色有至少一张非空图即算
// （用户上传与模板定妆图都算）。首帧图不是角色参考图，见 hasUserFirstFrame。
func hasCharacterReferenceImages(cfg VideoRunConfig) bool {
	for _, ref := range cfg.CharacterRefs {
		for _, image := range ref.Images {
			if strings.TrimSpace(image) != "" {
				return true
			}
		}
	}
	return false
}

// hasUserFirstFrame 用户是否提供了首帧图。只认任务配置里的 FirstFrame：
// 渲染时自动抽取的上一段尾帧不算，尾帧是画面延续载体，不是新的主体依据。
func hasUserFirstFrame(cfg VideoRunConfig) bool {
	return strings.TrimSpace(cfg.FirstFrame) != ""
}

// hasImageInput 任务配置里是否有任何图片输入：首帧或角色参考图。
func hasImageInput(cfg VideoRunConfig) bool {
	return hasUserFirstFrame(cfg) || hasCharacterReferenceImages(cfg)
}

// subjectDirective 按任务配置选出镜主体约束：只要用户给了图就以图为准，完全没图才用动物兜底。
func subjectDirective(cfg VideoRunConfig) string {
	if hasImageInput(cfg) {
		return imageCastDirective
	}
	return animalCastDirective
}

// promptSubjectDirective Agent2 用的主体约束：在 subjectDirective 基础上考虑模型能力。
// 有角色图、但模型不支持参考图、且没有首帧可锁时，只能退回文字外观锚点。
func promptSubjectDirective(cfg VideoRunConfig, caps VideoCapabilities) string {
	if hasCharacterReferenceImages(cfg) && !caps.CharacterReference && !hasUserFirstFrame(cfg) {
		return textAnchorCastDirective
	}
	return subjectDirective(cfg)
}

// scriptSystemPrompt 兼容入口：新流程把 Agent1 拆成「企划」与「分镜」两段，
// 这里把两段规则拼起来，旧测试与旧调用仍能拿到完整约束。
func scriptSystemPrompt(cfg VideoRunConfig) string {
	return treatmentSystemPrompt(cfg) + "\n\n" + storyboardSystemPrompt(cfg, nil)
}

func (a *App) llmScript(ctx context.Context, provider Provider, cfg VideoRunConfig, tpl *VideoTemplateRun, feedback string) (VideoScript, []string, error) {
	userPrompt := buildScriptUserPrompt(cfg, tpl)
	// 用户在分镜关卡敲的返工意见：不拼进去就等于没听见
	userPrompt += feedbackDirective(feedback)

	// 首帧作为多模态输入（文档 §5 的 B 类能力）：模型支持读图时能让分镜与首帧外观一致；
	// 不支持读图的文本模型只是忽略图片块，仍按纯文本返回。
	images := firstFrameImages(cfg)

	raw, imageWarnings, _, err := a.chatCompletionTraced(ctx, provider, scriptSystemPrompt(cfg), userPrompt, images)
	if err != nil {
		return VideoScript{}, imageWarnings, err
	}

	var payload struct {
		Logline  string `json:"logline"`
		Language string `json:"language"`
		Shots    []struct {
			ID            string              `json:"id"`
			DurationS     float64             `json:"durationS"`
			TimeRange     []float64           `json:"timeRange"`
			Roles         []string            `json:"roles"`
			SceneIntent   string              `json:"sceneIntent"`
			Dialogue      string              `json:"dialogue"`
			Speaker       string              `json:"speaker"`
			DialogueLines []VideoDialogueLine `json:"dialogueLines"`
			Camera        string              `json:"camera"`
			Mood          string              `json:"mood"`
			AudioCue      string              `json:"audioCue"`
		} `json:"shots"`
	}
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return VideoScript{}, imageWarnings, fmt.Errorf("解析分镜 JSON 失败: %w", err)
	}
	if len(payload.Shots) == 0 {
		return VideoScript{}, imageWarnings, errors.New("模型没有返回分镜")
	}

	script := VideoScript{Logline: payload.Logline, Language: payload.Language, Source: "llm"}
	for _, item := range payload.Shots {
		duration := item.DurationS
		if duration <= 0 && len(item.TimeRange) == 2 {
			duration = item.TimeRange[1] - item.TimeRange[0]
		}
		lines := normalizeDialogueLines(item.DialogueLines, item.Dialogue, item.Speaker)
		script.Shots = append(script.Shots, VideoShot{
			ID:            item.ID,
			DurationS:     duration,
			Roles:         item.Roles,
			SceneIntent:   item.SceneIntent,
			Dialogue:      dialogueText(lines),
			Speaker:       dialogueSpeakerSummary(lines),
			DialogueLines: lines,
			Camera:        item.Camera,
			Mood:          item.Mood,
			AudioCue:      item.AudioCue,
		})
	}
	return script, imageWarnings, nil
}

/* ------------------------------------------------------------------
   用户参数契约：用户在任务里填的每一项，都要真的进模型输入
   否则“填了参数”跟“没填”对生成结果毫无区别。
   ------------------------------------------------------------------ */

// firstFrameImages 首帧图作为多模态输入（文档 §5 的 B 类能力）。
func firstFrameImages(cfg VideoRunConfig) []string {
	if strings.TrimSpace(cfg.FirstFrame) == "" {
		return nil
	}
	return []string{cfg.FirstFrame}
}

// firstFrameHint 只说明首帧“有没有、起什么作用”，绝不把本地路径塞进模型输入：
// 模型读不懂 C:\...\first.png，本地绝对路径也没有理由发给第三方。
func firstFrameHint(cfg VideoRunConfig) string {
	if strings.TrimSpace(cfg.FirstFrame) == "" {
		return "无首帧（纯文生视频，第一段可自由构图）"
	}
	if cfg.strictFirstFrameEnabled() {
		return "已提供首帧图（随消息附图）：第一段从这张图的主体身份出发，在开头 0.5–1 秒内自然过渡到分镜描述的新场景；首帧只是起点，不锁死背景与机位"
	}
	return "已提供首帧图（随消息附图，作画面起点参考，可与角色参考图同时使用；首帧只是起点，不锁死背景与机位）"
}

// continuationHint 段与段之间怎么接：尾帧续接时后一段的第一帧就是前一段最后画面。
func continuationHint(cfg VideoRunConfig) string {
	switch strings.ToLower(strings.TrimSpace(cfg.ContinuationMode)) {
	case "overlap":
		return "相邻段重叠过渡（后段开头接前段结尾画面）"
	case "none":
		return "不续接（每段独立构图）"
	default:
		return "尾帧续接（后段从上一段最后的画面继续，不要重复上一段已完成的动作）"
	}
}

// audienceHint 发布平台/垂类/受众时区：它们决定钩子强度与节奏，
// 只喂给 Agent4 等于创作阶段没用上，这里补进两段 Agent 的输入。
func audienceHint(cfg VideoRunConfig) string {
	parts := make([]string, 0, 3)
	if len(cfg.Platforms) > 0 {
		parts = append(parts, "主平台 "+platformProfile(primaryPlatform(cfg)).Label)
	} else {
		parts = append(parts, "平台未指定（按通用短视频节奏）")
	}
	if cfg.Vertical != "" {
		parts = append(parts, "垂类 "+cfg.Vertical)
	}
	if strings.TrimSpace(cfg.Region) != "" {
		parts = append(parts, "受众时区 "+cfg.Region)
	}
	return strings.Join(parts, "；")
}

// userParams 把用户填的每一项参数压成权威清单（两段 Agent 共用）。
// 这是“参数真正参与生成”的唯一入口：没进这份清单的参数，模型压根不知道它存在。
func userParams(cfg VideoRunConfig) []string {
	lines := []string{
		"主题：" + cfg.Topic,
		fmt.Sprintf("目标总时长：%d 秒（各段时长之和必须等于它）", cfg.DurationS),
		"画幅：" + cfg.AspectRatio,
		fmt.Sprintf("语言：%s；语速预算：%.1f %s/秒", cfg.Language, cfg.SpeechRate, unitName(cfg.Language)),
		"发布：" + audienceHint(cfg),
		fmt.Sprintf("风格偏好：%s；运镜偏好：%s；情绪偏好：%s",
			firstNonEmpty(cfg.VideoParams.Style, "不限"),
			firstNonEmpty(cfg.VideoParams.Camera, "不限"),
			firstNonEmpty(cfg.VideoParams.Mood, "不限")),
		"首帧：" + firstFrameHint(cfg),
		"续接方式：" + continuationHint(cfg),
	}
	if extra := strings.TrimSpace(cfg.VideoParams.ExtraPrompt); extra != "" {
		lines = append(lines, "必须落实的额外要求："+extra)
	}
	if negative := strings.TrimSpace(cfg.VideoParams.NegativePrompt); negative != "" {
		lines = append(lines, "必须全片避开的元素（用正面表述达成）："+negative)
	}
	if cfg.Subtitles {
		lines = append(lines, "字幕：后期烧录，台词要能独立成句、便于按句断行")
	}
	return lines
}

// videoCharacterByRole 从任务角色参考里找对应角色：先精确比 role_id / 角色名，
// 再放宽到包含关系（模型常把「主理人（橘猫）」写成「主理人」）。
func videoCharacterByRole(cfg VideoRunConfig, role string) *VideoCharacterRef {
	key := normalizeRoleKey(role)
	if key == "" {
		return nil
	}
	for index := range cfg.CharacterRefs {
		ref := &cfg.CharacterRefs[index]
		if normalizeRoleKey(ref.RoleID) == key || normalizeRoleKey(ref.Name) == key {
			return ref
		}
	}
	roles := map[string]bool{key: true}
	for index := range cfg.CharacterRefs {
		if roleMatches(roles, cfg.CharacterRefs[index]) {
			return &cfg.CharacterRefs[index]
		}
	}
	return nil
}

// characterProfileForRole 取角色档案：任务里的角色参考优先，模板快照只补空缺字段。
// 这样旧任务（只有 role_id + 图）和新角色库任务（带外观/人设）走同一套解析路径。
func characterProfileForRole(cfg VideoRunConfig, tpl *VideoTemplateRun, role string) (VideoCharacterRef, bool) {
	var profile VideoCharacterRef
	found := false
	if ref := videoCharacterByRole(cfg, role); ref != nil {
		profile = *ref
		found = true
	} else if tpl != nil {
		if character := templateCharacterByRole(&tpl.VideoTemplate, role); character != nil {
			profile = VideoCharacterRef{
				RoleID:          character.RoleID,
				Name:            character.Name,
				Images:          append([]string{}, character.Images...),
				ImageSource:     CharacterImageSourceTemplate,
				AppearanceBlock: character.AppearanceBlock,
				VoiceHint:       character.VoiceHint,
				Perspective:     character.Perspective,
			}
			found = true
		}
	}
	if !found {
		return profile, false
	}
	// 模板快照补缺：任务 ref 可能只改了声色/图片，档案字段沿用模板
	if tpl != nil {
		if character := templateCharacterByRole(&tpl.VideoTemplate, firstNonEmpty(profile.RoleID, role)); character != nil {
			if profile.AppearanceBlock == "" {
				profile.AppearanceBlock = character.AppearanceBlock
			}
			if profile.VoiceHint == "" {
				profile.VoiceHint = character.VoiceHint
			}
			if profile.Perspective == "" {
				profile.Perspective = character.Perspective
			}
			if profile.Name == "" {
				profile.Name = character.Name
			}
		}
	}
	return profile, true
}

// characterTable 角色表：role_id、名字、形象来源与角色档案。
// Agent2 只看得到 shots 里的 role_id 时写不出可辨识特征，四段会各画一只不同的猫。
// 外观锚点、人设、负向约束、声音、视角只在这里输出一次，模板提示词不再重复列。
func characterTable(cfg VideoRunConfig, tpl *VideoTemplateRun) string {
	if len(cfg.CharacterRefs) == 0 && (tpl == nil || len(tpl.Characters) == 0) {
		return "无固定角色（画面主体由分镜决定）"
	}
	lines := make([]string, 0, len(cfg.CharacterRefs)+2)
	for _, ref := range cfg.CharacterRefs {
		lines = append(lines, characterProfileLine(cfg, tpl, ref))
	}
	// 模板声明了、但任务 ref 里没有的角色（手动删光/老任务）仍然列出，避免 Agent 不知道有这些角色
	if tpl != nil {
		for _, character := range tpl.Characters {
			if existing := videoCharacterByRole(cfg, character.RoleID); existing != nil {
				// 已由 ref 输出；若 ref 的角色名与模板不同，也以 ref 为准
				_ = existing
				continue
			}
			profile := VideoCharacterRef{
				RoleID:          character.RoleID,
				Name:            character.Name,
				Images:          append([]string{}, character.Images...),
				ImageSource:     CharacterImageSourceTemplate,
				AppearanceBlock: character.AppearanceBlock,
				VoiceHint:       character.VoiceHint,
				Perspective:     character.Perspective,
			}
			lines = append(lines, characterProfileLine(cfg, tpl, profile))
		}
	}
	return strings.Join(lines, "\n")
}

// characterProfileLine 单个角色的档案行：外观锚点让位给用户上传的定妆图。
func characterProfileLine(cfg VideoRunConfig, tpl *VideoTemplateRun, ref VideoCharacterRef) string {
	profile, _ := characterProfileForRole(cfg, tpl, firstNonEmpty(ref.RoleID, ref.Name))
	line := fmt.Sprintf("- %s（%s）", firstNonEmpty(ref.RoleID, profile.RoleID), firstNonEmpty(ref.Name, profile.Name, ref.RoleID, profile.RoleID))
	if strings.TrimSpace(profile.Species) != "" {
		line += "｜物种：" + profile.Species
	}
	switch {
	case profile.ImageSource == CharacterImageSourceUser && len(profile.Images) > 0:
		line += "｜形象：用户上传的定妆图（以此为准，不要另编外貌）"
	case len(profile.Images) > 0:
		line += "｜形象：模板定妆图"
	default:
		line += "｜形象：无图，只能靠文字描述"
	}
	// 有用户上传图时以图为准，不再用文字外观覆盖图片
	if strings.TrimSpace(profile.AppearanceBlock) != "" && !roleUsesUserImages(cfg, profile.RoleID) {
		line += "｜外观锚点：" + profile.AppearanceBlock
	}
	if strings.TrimSpace(profile.Persona) != "" {
		line += "｜人设/口吻：" + profile.Persona
	}
	if strings.TrimSpace(profile.NegativePrompt) != "" {
		line += "｜角色禁止：" + profile.NegativePrompt
	}
	if strings.TrimSpace(profile.Perspective) != "" {
		line += "｜视角：" + profile.Perspective
	}
	if strings.TrimSpace(profile.VoiceHint) != "" {
		line += "｜声音：" + profile.VoiceHint
	}
	if detail := voiceProfileDetail(profile.VoiceProfile); detail != "" {
		line += "｜音色参数：" + detail
	}
	return line
}

// capabilityHint 让 Agent2 知道“这台机器能干什么”：能力不同，写法就该不同。
func capabilityHint(caps VideoCapabilities) string {
	parts := []string{fmt.Sprintf("单段时长 %s 秒（只能是整数秒）", durationRangeText(caps.AllowedDurations))}
	if caps.CharacterReference {
		parts = append(parts, "角色参考图：支持（形象交给图，文字只补图里没有的信息）")
	} else {
		parts = append(parts, "角色参考图：不支持（必须用 2 个静态外观锚点把形象写死）")
	}
	if caps.NativeAudio {
		parts = append(parts, "原生音频：支持（台词会随提示词下发，画面不要另写配音指令）")
	} else {
		parts = append(parts, "原生音频：不支持（台词只走字幕与后期配音，画面里不要写说话或口型动作）")
	}
	return strings.Join(parts, "；")
}

func durationRangeText(allowed []int) string {
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}
	return fmt.Sprintf("%d-%d", allowed[0], allowed[len(allowed)-1])
}

// userParamDigest 一行说明“本次生成真正带上了哪些用户参数”，写进阶段日志：
// 参数有没有参与，用户应该能在日志里一眼验证，而不是靠猜。
func userParamDigest(cfg VideoRunConfig) string {
	parts := []string{
		fmt.Sprintf("主题「%s」", truncateRunes(cfg.Topic, 16)),
		fmt.Sprintf("时长 %ds", cfg.DurationS),
		cfg.AspectRatio,
		cfg.Language,
		fmt.Sprintf("%.1f %s/秒", cfg.SpeechRate, unitName(cfg.Language)),
	}
	if len(cfg.Platforms) > 0 {
		parts = append(parts, "平台 "+platformProfile(primaryPlatform(cfg)).Label)
	}
	if cfg.Vertical != "" {
		parts = append(parts, "垂类 "+cfg.Vertical)
	}
	if cfg.Region != "" {
		parts = append(parts, "受众 "+cfg.Region)
	}
	for _, item := range []struct{ label, value string }{
		{"风格", cfg.VideoParams.Style},
		{"运镜", cfg.VideoParams.Camera},
		{"情绪", cfg.VideoParams.Mood},
	} {
		if strings.TrimSpace(item.value) != "" {
			parts = append(parts, item.label+" "+truncateRunes(item.value, 12))
		}
	}
	if cfg.FirstFrame != "" {
		parts = append(parts, "首帧 有")
	}
	if len(cfg.CharacterRefs) > 0 {
		parts = append(parts, fmt.Sprintf("角色 %d 个", len(cfg.CharacterRefs)))
	}
	if cfg.VideoParams.ExtraPrompt != "" {
		parts = append(parts, "额外要求 有")
	}
	if cfg.VideoParams.NegativePrompt != "" {
		parts = append(parts, "负向约束 有")
	}
	if cfg.Subtitles {
		parts = append(parts, "烧录字幕")
	}
	return strings.Join(parts, "／")
}

// buildScriptUserPrompt Agent1 的完整输入：参数清单 + 角色表 + 模板骨架 + 首帧说明。
func buildScriptUserPrompt(cfg VideoRunConfig, tpl *VideoTemplateRun) string {
	lines := userParams(cfg)
	lines = append(lines, "可出场角色：\n"+characterTable(cfg, tpl))
	if hints := templateScriptHints(tpl, cfg); hints != "" {
		lines = append(lines, hints)
	}
	lines = append(lines, "请按 system 中的 JSON 结构输出分镜。")
	return strings.Join(lines, "\n")
}

// buildPromptUserPrompt Agent2 的完整输入：参数清单 + 模型能力 + 角色表 + 分镜 JSON。
// Agent2 看到的必须和 Agent1、渲染层同一套参数，否则两段 Agent 各拧一股绳。
func buildPromptUserPrompt(cfg VideoRunConfig, shotsJSON string, caps VideoCapabilities, tpl *VideoTemplateRun) string {
	lines := userParams(cfg)
	lines = append(lines, "本段模型能力："+capabilityHint(caps))
	lines = append(lines, "角色表：\n"+characterTable(cfg, tpl))
	if hints := templateScriptHints(tpl, cfg); hints != "" {
		lines = append(lines, hints)
	}
	lines = append(lines,
		"分镜（JSON，shotId 必须逐字复用）：\n"+shotsJSON,
		"请为每个 shot 输出 visualPrompt 与原样 dialogueLines；多说话人段落按台词顺序让每个人轮流开口；单段 60-120 字，不要写否定句与画面文字要求。")
	return strings.Join(lines, "\n")
}

func unitName(lang string) string {
	if isEnglishLanguage(lang) {
		return "词"
	}
	return "字"
}

// feedbackDirective 把人工返工意见拼进 Agent 的输入末尾。
// 关卡上敲的意见必须真的进模型输入，否则“按意见重生成”只是拿同一份提示词重掷一次骰子；
// 但意见不能凌驾结构与安全硬约束（JSON 格式、台词原样保留、主体约束），所以写成“冲突时仍守硬性约束”。
func feedbackDirective(feedback string) string {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return ""
	}
	return "\n用户返工意见（本轮重生成必须逐条落实；与上面的硬性约束冲突时仍守硬性约束）：" + feedback
}

// feedbackDroppedWarning 降级到本地规则时没人读懂自然语言意见，必须明说“没生效”，
// 不能让用户看到“已重新生成”就以为意见被采纳了。
func feedbackDroppedWarning(feedback string) string {
	if strings.TrimSpace(feedback) == "" {
		return ""
	}
	return "本轮返工意见未生效：本地规则不能理解自然语言，请在「系统设置 - 提供商」登记文本模型后重生成"
}

// generateScript 兼容入口：返回脚本与降级提示；需要区分“硬失败”时用 generateScriptChecked。
func (a *App) generateScript(ctx context.Context, cfg VideoRunConfig, feedback string, templates ...*VideoTemplateRun) (VideoScript, []string) {
	script, warnings, err := a.generateScriptChecked(ctx, cfg, feedback, templates...)
	if err != nil {
		warnings = append(warnings, "剧本生成未通过："+err.Error())
	}
	return script, warnings
}

// generateScriptChecked 新版 Agent1：企划 → 分镜两阶段。
// - 用户提供了完整剧本时进入锁定模式：台词原文锁定，只做分镜；
// - 无角色时由企划阶段自动创建动物临时角色；
// - 台词归属不合法时先修复一次，仍失败则硬失败（不允许静默降级旁白）。
func (a *App) generateScriptChecked(ctx context.Context, cfg VideoRunConfig, feedback string, templates ...*VideoTemplateRun) (VideoScript, []string, error) {
	tpl := firstTemplateRun(templates)
	warnings := make([]string, 0, 3)
	lockedMode := cfg.ScriptLocked || looksLikeLockedScript(cfg.Topic)
	provider := a.pickTextProvider(cfg)
	if provider.ID != "" {
		script, llmWarnings, err := a.generateScriptTwoStage(ctx, provider, cfg, tpl, feedback)
		warnings = append(warnings, llmWarnings...)
		if err == nil && len(script.Shots) > 0 {
			return script, warnings, nil
		}
		// 锁定剧本不能回退本地规则：本地规则会改写/丢失用户台词，等于破坏锁定承诺。
		if lockedMode {
			return VideoScript{}, warnings, err
		}
		// 绑定失败是硬失败：不许用本地规则“补一个能跑的剧本”掩盖问题
		if errors.Is(err, errScriptBinding) {
			return VideoScript{}, warnings, err
		}
		if !cfg.localFallbackEnabled() {
			return VideoScript{}, append(warnings, fmt.Sprintf("已关闭本地兜底：文本模型未产出可用剧本（%v）", err)), err
		}
		warnings = append(warnings, fmt.Sprintf("文本模型生成剧本失败，已回退本地规则：%v", err))
	} else {
		if lockedMode {
			err := errors.New("锁定剧本模式需要文本模型：请先登记文本模型，台词只能在分镜阶段原样保留")
			return VideoScript{}, append(warnings, err.Error()), err
		}
		if !cfg.localFallbackEnabled() {
			err := errors.New("本任务需要文本模型产出剧本，请在「系统设置 - 提供商」登记后再试")
			return VideoScript{}, append(warnings, "已关闭本地兜底："+err.Error()), err
		}
		warnings = append(warnings, "未配置文本模型，已使用本地规则生成分镜（可在「系统设置 - 提供商」登记文本模型）")
	}
	if warning := feedbackDroppedWarning(feedback); warning != "" {
		warnings = append(warnings, warning)
	}
	bound, speakerWarnings := bindSpeakers(normalizeScript(localVideoScript(cfg, tpl), cfg, tpl), cfg)
	return bound, append(warnings, speakerWarnings...), nil
}

// firstTemplateRun 取首个非空模板快照，便于保持旧调用点兼容。
func firstTemplateRun(templates []*VideoTemplateRun) *VideoTemplateRun {
	for _, tpl := range templates {
		if tpl != nil {
			return tpl
		}
	}
	return nil
}

// templateScriptHints 把模板的风格骨架 / 开场钩子喂给 Agent1。
// 角色档案统一由 characterTable 输出，这里不再重复列外观，避免同一条锚点在输入里出现两遍。
func templateScriptHints(tpl *VideoTemplateRun, _ VideoRunConfig) string {
	if tpl == nil {
		return ""
	}
	lines := make([]string, 0, 6)
	lines = append(lines, fmt.Sprintf("模板：%s v%s（%s）——模板只提供默认值，不约束本次创意", tpl.Name, tpl.Version, templateScopeLabel(tpl.Scope)))
	if summary := templateStyleSummary(&tpl.VideoTemplate); summary != "" {
		lines = append(lines, "风格骨架（受控词表）："+summary)
	}
	if len(tpl.OpeningAll) > 0 {
		lines = append(lines, "开场钩子候选（shot#1 台词优先采用其一，可自然接上主题）："+strings.Join(tpl.OpeningAll, " / "))
	}
	return strings.Join(lines, "\n")
}

func templateScopeLabel(scope string) string {
	switch scope {
	case TemplateScopeBuiltin:
		return "内置模板"
	case TemplateScopeTeam:
		return "团队模板"
	default:
		return "个人模板"
	}
}

// isNarrationSpeaker 模型可能把旁白写成 narration / 旁白 / 画外音 / VO，这里统一认出来。
// 注意：它只能在角色表没有精确命中时使用，否则「解说员」这类角色名会被旁白抢走。
func isNarrationSpeaker(value string) bool {
	key := normalizeRoleKey(value)
	if key == "" {
		return false
	}
	if key == "vo" {
		return true
	}
	for _, alias := range []string{"narration", "voiceover", "旁白", "画外音", "解说"} {
		if strings.Contains(key, alias) {
			return true
		}
	}
	return false
}

// matchSpeakerRoleExact 只按 role_id / 角色名精确匹配，大小写与首尾空格不敏感。
// 精确匹配必须先于任何包含匹配，否则 char_1 会抢走 char_12 的台词。
func matchSpeakerRoleExact(cfg VideoRunConfig, value string) (string, bool) {
	key := normalizeRoleKey(value)
	if key == "" {
		return "", false
	}
	for _, ref := range cfg.CharacterRefs {
		if normalizeRoleKey(ref.RoleID) == key || normalizeRoleKey(ref.Name) == key {
			return ref.RoleID, true
		}
	}
	return "", false
}

// matchSpeakerRoleFuzzy 在精确匹配失败后做包含匹配；只有唯一候选时才认，
// 多个候选说明模型写法有歧义，猜错比不猜更伤。
func matchSpeakerRoleFuzzy(cfg VideoRunConfig, value string) (string, bool) {
	key := normalizeRoleKey(value)
	if key == "" {
		return "", false
	}
	matched := ""
	for _, ref := range cfg.CharacterRefs {
		candidates := []string{normalizeRoleKey(ref.RoleID), normalizeRoleKey(ref.Name)}
		hit := false
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if strings.Contains(key, candidate) || strings.Contains(candidate, key) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if matched != "" && matched != ref.RoleID {
			return "", false
		}
		matched = ref.RoleID
	}
	return matched, matched != ""
}

// matchSpeakerRole 把说话人写法映回角色表：先全局精确匹配，再唯一模糊匹配。
func matchSpeakerRole(cfg VideoRunConfig, value string) (string, bool) {
	if roleID, ok := matchSpeakerRoleExact(cfg, value); ok {
		return roleID, true
	}
	return matchSpeakerRoleFuzzy(cfg, value)
}

// normalizeSpeakerOrRaw 优先归一到 role_id；角色表里没有但这个值本身是合法标识时原样保留。
func normalizeSpeakerOrRaw(cfg VideoRunConfig, role string) string {
	if roleID, ok := matchSpeakerRole(cfg, role); ok {
		return roleID
	}
	return strings.TrimSpace(role)
}

// normalizeSpeaker 把外部传来的台词归属（关卡编辑 / 模型回显）归一成 role_id 或 narration。
// 顺序必须是「角色优先、旁白兜底」：角色名里带“解说/旁白”时仍然是角色在说话。
func normalizeSpeaker(cfg VideoRunConfig, value string) string {
	if roleID, ok := matchSpeakerRole(cfg, value); ok {
		return roleID
	}
	if isNarrationSpeaker(value) {
		return VideoSpeakerNarration
	}
	return ""
}

// promptMentionsSpeaker 画面提示词里有没有出现这个说话人：role_id、角色名，或名字的中文部分。
// 名字常带括号说明（“主理人（橘猫）”），逐段写全名不自然，所以允许只写前半段。
// 英文角色名不区分大小写，避免 Host / host 这种大小写差异误报。
func promptMentionsSpeaker(visual string, cfg VideoRunConfig, speaker string) bool {
	speaker = strings.TrimSpace(speaker)
	if speaker == "" {
		return false
	}
	lowerVisual := strings.ToLower(visual)
	if strings.Contains(lowerVisual, strings.ToLower(speaker)) {
		return true
	}
	for _, ref := range cfg.CharacterRefs {
		if ref.RoleID != speaker {
			continue
		}
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			return false
		}
		if strings.Contains(lowerVisual, strings.ToLower(name)) {
			return true
		}
		if idx := strings.IndexAny(name, "（("); idx > 0 {
			return strings.Contains(lowerVisual, strings.ToLower(strings.TrimSpace(name[:idx])))
		}
		return false
	}
	return false
}

// canonicalizeShotRoles 把分镜里的角色写法归一成角色表里的 role_id：
// 精确匹配 → 唯一模糊匹配；未知角色丢弃并告警，重复项去重。
func canonicalizeShotRoles(cfg VideoRunConfig, roles []string, shotID string) ([]string, []string) {
	warnings := make([]string, 0, 2)
	out := make([]string, 0, len(roles))
	seen := make(map[string]bool, len(roles))
	for _, raw := range roles {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		roleID, ok := matchSpeakerRole(cfg, raw)
		if !ok {
			if len(cfg.CharacterRefs) > 0 {
				warnings = append(warnings, fmt.Sprintf("%s 的角色「%s」不在角色表里，已忽略", shotID, raw))
			}
			continue
		}
		if seen[roleID] {
			continue
		}
		seen[roleID] = true
		out = append(out, roleID)
	}
	return out, warnings
}

// bindSpeakers 给每行台词钉上“谁在说”：角色台词必须是角色表里的 role_id，
// 画外解说落到 narration。一段允许多个说话人，每行独立解析，不再把整段压成一个归属。
// 解析顺序：精确角色 → 唯一模糊角色 → 旁白别名 → 本段唯一出场角色 → 旁白并告警。
func bindSpeakers(script VideoScript, cfg VideoRunConfig) (VideoScript, []string) {
	warnings := make([]string, 0, 2)
	shots := make([]VideoShot, len(script.Shots))
	for i, shot := range script.Shots {
		roles, roleWarnings := canonicalizeShotRoles(cfg, shot.Roles, shot.ID)
		warnings = append(warnings, roleWarnings...)
		shot.Roles = roles

		lines := normalizeDialogueLines(shot.DialogueLines, shot.Dialogue, shot.Speaker)
		if len(lines) == 0 {
			// 没台词就没有说话人：留着归属只会让渲染层去拍一张不存在的嘴
			shot.Dialogue = ""
			shot.Speaker = ""
			shot.DialogueLines = nil
			shots[i] = shot
			continue
		}

		roleSet := make(map[string]bool, len(roles))
		for _, role := range roles {
			roleSet[role] = true
		}
		for lineIndex := range lines {
			raw := strings.TrimSpace(lines[lineIndex].Speaker)
			speaker, warning := resolveSpeaker(cfg, raw, shot.Roles, shot.ID, lineIndex, roleSet)
			if warning != "" {
				warnings = append(warnings, warning)
			}
			lines[lineIndex].Speaker = speaker
			// 角色开口却不在 roles 里：把他补进本段出场角色，否则参考图与画面都找不到这张脸。
			if speaker != "" && speaker != VideoSpeakerNarration && !roleSet[speaker] {
				roleSet[speaker] = true
				shot.Roles = append(shot.Roles, speaker)
			}
		}
		shot.DialogueLines = lines
		shot.Dialogue = dialogueText(lines)
		shot.Speaker = dialogueSpeakerSummary(lines)
		shots[i] = shot
	}
	script.Shots = shots
	return script, warnings
}

// resolveSpeaker 解析单行台词的归属；找不到确定答案时按旁白兜底并说明原因，
// 不静默替用户选一个“大概是他”。
func resolveSpeaker(cfg VideoRunConfig, raw string, roles []string, shotID string, lineIndex int, roleSet map[string]bool) (string, string) {
	label := shotID
	if lineIndex > 0 {
		label = fmt.Sprintf("%s 第 %d 句台词", shotID, lineIndex+1)
	}
	switch {
	case raw != "":
		if roleID, ok := matchSpeakerRole(cfg, raw); ok {
			return roleID, ""
		}
		if isNarrationSpeaker(raw) {
			return VideoSpeakerNarration, ""
		}
		if len(roles) == 1 {
			// 写了个角色表里没有的名字，但本段只出现一个角色：大概率就是他
			speaker := normalizeSpeakerOrRaw(cfg, roles[0])
			return speaker, fmt.Sprintf("%s 的说话人「%s」不在角色表里，已按本段唯一出场角色「%s」处理", label, raw, speaker)
		}
		return VideoSpeakerNarration, fmt.Sprintf("%s 的说话人「%s」不在角色表里，已按旁白（画外音）处理", label, raw)
	case len(roles) == 1:
		return normalizeSpeakerOrRaw(cfg, roles[0]), ""
	case len(roles) == 0:
		// 画里没人，台词只能是画外音
		return VideoSpeakerNarration, ""
	default:
		return VideoSpeakerNarration, fmt.Sprintf("%s 有台词但没写说话人，本段出现多个角色，已按旁白（画外音）处理；可在逐段确认里改成某个角色说话", label)
	}
}

// 手动分段的通用可生成区间与段数上限：越界先夹取并告警，
// 更细的模型档位对齐（如 5/10 秒档）交给 buildVideoPlan 按适配器能力处理。
const (
	manualShotMinS     = 4.0
	manualShotMaxS     = 15.0
	manualShotMaxNum   = 12
	defaultManualShotS = 6.0
)

// buildManualScript 手动分段模式：不调用 Agent1，直接把用户逐段填写的内容整理成分镜。
// 与 normalizeScript 的区别是段时长保真：不按目标时长缩放，总时长反过来由各段之和决定。
func buildManualScript(cfg VideoRunConfig) (VideoScript, []string, error) {
	shots, warnings, err := normalizeManualShots(cfg)
	if err != nil {
		return VideoScript{}, warnings, err
	}
	script := VideoScript{
		Logline:  firstNonEmpty(strings.TrimSpace(cfg.Topic), "手动分段"),
		Language: normalizeLanguage(cfg.Language),
		Source:   "manual",
		Shots:    shots,
	}
	script, speakerWarnings := bindSpeakers(script, cfg)
	return script, append(warnings, speakerWarnings...), nil
}

// normalizeManualShots 把手填分段整理成可渲染的分镜：
// 段时长保真（只夹到通用可生成区间），时间轴按填写顺序累加，未知角色剔除并告警。
func normalizeManualShots(cfg VideoRunConfig) ([]VideoShot, []string, error) {
	if len(cfg.ManualShots) > manualShotMaxNum {
		return nil, nil, fmt.Errorf("手动分段最多 %d 段（当前 %d 段）", manualShotMaxNum, len(cfg.ManualShots))
	}
	warnings := make([]string, 0, 4)
	shots := make([]VideoShot, 0, len(cfg.ManualShots))
	cursor := 0.0
	for index, raw := range cfg.ManualShots {
		label := fmt.Sprintf("第 %d 段", index+1)
		scene := strings.TrimSpace(raw.SceneIntent)
		lines := normalizeDialogueLines(raw.DialogueLines, raw.Dialogue, raw.Speaker)
		dialogue := dialogueText(lines)
		// 边填边加段时常见的空行：跳过并说清楚，不至于让整个任务起不来
		if scene == "" && dialogue == "" && len(nonEmptyStrings(raw.Roles)) == 0 {
			warnings = append(warnings, label+"还是空的，已跳过")
			continue
		}
		if scene == "" {
			return nil, warnings, fmt.Errorf("%s 写了台词或角色，但没写画面内容；画面是分段的必填项", label)
		}
		duration := raw.DurationS
		if duration <= 0 {
			duration = defaultManualShotS
		}
		if duration < manualShotMinS {
			warnings = append(warnings, fmt.Sprintf("%s 时长 %.1fs 低于单段下限 %.0fs，已按 %.0fs 生成", label, duration, manualShotMinS, manualShotMinS))
			duration = manualShotMinS
		}
		if duration > manualShotMaxS {
			warnings = append(warnings, fmt.Sprintf("%s 时长 %.1fs 超出单段上限 %.0fs，已按 %.0fs 生成（更长的段会在渲染计划里自动拆成多段）", label, duration, manualShotMaxS, manualShotMaxS))
			duration = manualShotMaxS
		}
		roles, roleWarnings := manualShotRoles(cfg, raw.Roles, label)
		warnings = append(warnings, roleWarnings...)
		shots = append(shots, VideoShot{
			ID:            fmt.Sprintf("s%d", len(shots)+1),
			TimeRange:     [2]float64{round2(cursor), round2(cursor + duration)},
			DurationS:     round2(duration),
			Roles:         roles,
			SceneIntent:   scene,
			Dialogue:      dialogue,
			Speaker:       dialogueSpeakerSummary(lines),
			DialogueLines: lines,
			Camera:        strings.TrimSpace(raw.Camera),
			Mood:          strings.TrimSpace(raw.Mood),
			AudioCue:      strings.TrimSpace(raw.AudioCue),
		})
		cursor += duration
	}
	if len(shots) == 0 {
		return nil, warnings, errors.New("手动分段模式至少需要一段有效内容（填了画面内容才算一段）")
	}
	return shots, warnings, nil
}

// manualShotRoles 只留下真实存在的角色：手填时打错名字不该被悄悄送进渲染。
func manualShotRoles(cfg VideoRunConfig, roles []string, label string) ([]string, []string) {
	out := make([]string, 0, len(roles))
	warnings := make([]string, 0, 2)
	seen := make(map[string]bool, len(roles))
	for _, raw := range roles {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		roleID, ok := matchSpeakerRole(cfg, raw)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s 的角色「%s」不在角色表里，已忽略（先在角色参考图里登记它）", label, raw))
			continue
		}
		if seen[roleID] {
			continue
		}
		seen[roleID] = true
		out = append(out, roleID)
	}
	return out, warnings
}

func manualTotalSeconds(shots []VideoShot) float64 {
	total := 0.0
	for _, shot := range shots {
		total += shot.DurationS
	}
	return round2(total)
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// normalizeScript 把任意来源的分镜整理成"时长之和等于总时长、时间轴连续"的稳定结构。
func normalizeScript(script VideoScript, cfg VideoRunConfig, templates ...*VideoTemplateRun) VideoScript {
	tpl := firstTemplateRun(templates)
	shots := make([]VideoShot, 0, len(script.Shots))
	for _, shot := range script.Shots {
		shot.SceneIntent = strings.TrimSpace(shot.SceneIntent)
		shot.DialogueLines = normalizeDialogueLines(shot.DialogueLines, shot.Dialogue, shot.Speaker)
		shot.Dialogue = dialogueText(shot.DialogueLines)
		shot.Speaker = dialogueSpeakerSummary(shot.DialogueLines)
		shot.Camera = strings.TrimSpace(shot.Camera)
		shot.Mood = strings.TrimSpace(shot.Mood)
		shot.AudioCue = strings.TrimSpace(shot.AudioCue)
		if shot.Roles == nil {
			shot.Roles = []string{}
		}
		shots = append(shots, shot)
	}
	if len(shots) == 0 {
		return localVideoScript(cfg, tpl)
	}

	sum := 0.0
	for _, shot := range shots {
		if shot.DurationS > 0 {
			sum += shot.DurationS
		}
	}
	if sum <= 0 {
		per := float64(cfg.DurationS) / float64(len(shots))
		for i := range shots {
			shots[i].DurationS = per
		}
		sum = float64(cfg.DurationS)
	}

	scale := float64(cfg.DurationS) / sum
	cursor := 0.0
	for i := range shots {
		duration := round2(mathMax(0.5, shots[i].DurationS*scale))
		shots[i].ID = firstNonEmpty(shots[i].ID, fmt.Sprintf("s%d", i+1))
		shots[i].DurationS = duration
		shots[i].TimeRange = [2]float64{round2(cursor), round2(cursor + duration)}
		cursor += duration
	}

	language := normalizeLanguage(firstNonEmpty(script.Language, cfg.Language))
	source := firstNonEmpty(script.Source, "llm")
	return VideoScript{
		Logline:   firstNonEmpty(script.Logline, cfg.Topic),
		Language:  language,
		Source:    source,
		Mode:      script.Mode,
		Treatment: script.Treatment,
		Shots:     shots,
	}
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// splitTopicClauses 把主题按标点切成可做台词的短语，供本地规则使用。
func splitTopicClauses(topic string) []string {
	replaced := strings.NewReplacer("。", "|", "！", "|", "？", "|", "；", "|", "，", "|", ",", "|", ";", "|", "\n", "|").Replace(topic)
	clauses := make([]string, 0, 6)
	for _, part := range strings.Split(replaced, "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		clauses = append(clauses, part)
		if len(clauses) == 6 {
			break
		}
	}
	if len(clauses) == 0 {
		clauses = append(clauses, strings.TrimSpace(topic))
	}
	return clauses
}

// localVideoScript 无模型可用的确定性降级：按总时长切段，台词取自主题短语。
func localVideoScript(cfg VideoRunConfig, templates ...*VideoTemplateRun) VideoScript {
	tpl := firstTemplateRun(templates)
	clauses := splitTopicClauses(cfg.Topic)
	lowShots, highShots := shotCountRange(cfg)
	shotCount := clampInt(int(math.Round(float64(cfg.DurationS)/6)), lowShots, highShots)
	if shotCount < 2 && len(clauses) >= 3 {
		shotCount = 2
	}
	cameras := []string{"缓慢推近", "横向跟拍", "环绕主体", "固定机位微动", "低角度仰拍"}
	moods := []string{"温暖/期待", "克制/专注", "明亮/轻快", "沉静/收束"}
	roles := make([]string, 0, len(cfg.CharacterRefs))
	for _, ref := range cfg.CharacterRefs {
		roles = append(roles, ref.RoleID)
	}
	// 本地模式的台词取自主题短语，不带任何说话人信息：一个角色就当他说的，
	// 多个角色（无人可归）一律按旁白——虽然短，但总比让下游猜“谁在说”强。
	localSpeaker := VideoSpeakerNarration
	if len(roles) == 1 {
		localSpeaker = roles[0]
	}

	shots := make([]VideoShot, 0, shotCount)
	per := float64(cfg.DurationS) / float64(shotCount)
	cursor := 0.0
	for i := 0; i < shotCount; i++ {
		duration := round2(per)
		if i == shotCount-1 {
			duration = round2(float64(cfg.DurationS) - cursor)
		}
		camera := firstNonEmpty(cfg.VideoParams.Camera, cameras[i%len(cameras)])
		mood := firstNonEmpty(cfg.VideoParams.Mood, moods[i%len(moods)])
		dialogue := clauses[i%len(clauses)]
		// 台词按语速预算截断，避免本地模式一上来就超预算
		if isEnglishLanguage(cfg.Language) && speechUnitCount(dialogue, cfg.Language) > int(duration*cfg.SpeechRate) {
			words := strings.Fields(dialogue)
			limit := clampInt(int(duration*cfg.SpeechRate), 1, len(words))
			dialogue = strings.Join(words[:limit], " ")
		}
		sceneIntent := fmt.Sprintf("%s，以「%s」为主体，动作清晰可度量", camera, firstNonEmpty(clauses[i%len(clauses)], cfg.Topic))
		shot := VideoShot{
			ID:          fmt.Sprintf("s%d", i+1),
			TimeRange:   [2]float64{round2(cursor), round2(cursor + duration)},
			Roles:       append([]string{}, roles...),
			SceneIntent: sceneIntent,
			Dialogue:    dialogue,
			Speaker:     localSpeaker,
			Camera:      camera,
			Mood:        mood,
			AudioCue:    "环境音垫底，保持整体氛围连续",
			DurationS:   duration,
		}
		shot.DialogueLines = normalizeDialogueLines(nil, shot.Dialogue, shot.Speaker)
		shot.Dialogue = dialogueText(shot.DialogueLines)
		shot.Speaker = dialogueSpeakerSummary(shot.DialogueLines)
		shots = append(shots, shot)
		cursor += duration
	}

	// 开场白只占 shot#1 的台词槽位，并按语速预算裁剪：模板不能绕过字数/时长校验（§4）
	if tpl != nil && strings.TrimSpace(tpl.OpeningLine) != "" && len(shots) > 0 {
		shots[0].Dialogue = trimDialogueToBudget(tpl.OpeningLine, shots[0].DurationS, cfg.SpeechRate, cfg.Language)
		shots[0].DialogueLines = normalizeDialogueLines(nil, shots[0].Dialogue, shots[0].Speaker)
	}

	return VideoScript{
		Logline:  defaultVideoTitle(cfg.Topic),
		Language: cfg.Language,
		Source:   "local",
		Shots:    shots,
	}
}

// trimDialogueToBudget 把台词裁到「（本段时长 - 停顿缓冲）× 语速」预算内，中文按字、英文按词。
// 预算公式必须与 estimateSpeechSeconds / Agent1 提示词保持一致，否则会出现
// 「按语速刚好写满，却被判超时」的自相矛盾。
func trimDialogueToBudget(text string, duration, rate float64, lang string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	budget := speechUnitBudget(duration, rate, lang)
	if budget <= 0 {
		return ""
	}
	if speechUnitCount(text, lang) <= budget {
		return text
	}
	if isEnglishLanguage(lang) {
		words := strings.Fields(text)
		return strings.Join(words[:clampInt(budget, 1, len(words))], " ")
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:clampInt(budget, 1, len(runes))]))
}

func promptSystemPrompt(cfg VideoRunConfig, caps VideoCapabilities) string {
	return `【角色】你是图像生视频（i2v）与文生视频的提示词工程师。分镜结构与台词已经定稿：你只决定“画面怎么写”，不改剧情、不改台词、不改段数。你的输出会被程序直接解析，不要 Markdown 代码块、不要解释。

【输出结构】
{"prompts":[{"shotId":"s1","visualPrompt":"...","dialogueLines":[{"speaker":"char_a","text":"..."}]}]}
- shotId 必须与输入分镜的 id 逐字一致（写成别的值会导致该段被系统丢弃）
- 每段都要输出，条数必须等于输入分镜数
- dialogueLines 原样返回，不要翻译、改写、增删或改变顺序；每行的 speaker 也原样保留（role_id 或 "narration"），不要改归属、也不要新增说话人
- 台词偏长由系统提示，不由你压缩

【单段写法】按槽位一次写完，总长 60-120 字：
[镜头/景别+运动] + [主体：角色名 + 1 个可辨识特征] + [一个可度量的动作] + [场景与锚点物件] + [光位/色温] + [风格氛围]

【台词归属必须落到画面】
- 本段每行台词的 speaker 是角色：该角色必须在画面里（正面或侧脸），口型与神态跟着台词走，镜头给到当时开口的人；不要让他背对镜头念台词；
- 本段有多个角色轮流说话：按台词顺序安排表演，轮到谁开口就拍到谁，其他角色嘴唇闭合；不要让他们同时动嘴；
- speaker 是 "narration" 的台词：画面内无人开口、不安排任何主体对口型，用空镜、环境细节或主体反应镜头承载画外音；
- 这一节是硬约束：画面里找不到说话人，台词就会变成“画外飘音”，配音与口型对不上。

【写法规则】
1. 第一句写镜头（景别、角度、运动、速度）；
2. 主体用具体特征；禁止“美女/帅哥/一个人”这类空词，也禁止 cinematic、8K、高级感、masterpiece 这类空洞标签；
3. 动作写成物理量（重心、步伐、接触面）；一段只允许一个连续动作，不要 then/然后/接着 造成的段内切镜；
4. 只写画面里发生的变化；不要复述角色整段外观与服装（下游会统一追加外观锚点，重复写只会稀释动作指令）；
5. 不要写“不出现文字/水印/logo”这类否定句：本模型按正向描述理解画面，直接写“画面只有角色与实物道具”即可；
6. 相邻分镜的景别或运动至少一项不同；
7. 段落分工：第一段从首帧的主体出发，只写之后发生的变化；第二段起接续上一段结尾画面，不重复上一段已完成的动作；
8. 用户给的画幅、风格、运镜、情绪、额外要求、负向约束都是权威输入，必须体现；负向约束改写成正面表述（“保持 X”而不是“不要 Y”）；
9. 每段 visualPrompt 必须原样写入本任务的主体约束，不要改写、不要省略：` + promptSubjectDirective(cfg, caps) + `。` + firstFrameSemanticsBlock(cfg) + `

【输出前自检】
① 条数等于输入分镜数、shotId 全部逐字命中？② 每段 60-120 字、只有一个连续动作？
③ 没有否定句与空洞标签？④ dialogueLines 与输入逐行一致？⑤ 相邻段景别或运动有变化？
⑥ 每个角色说话人都被拍进画面，旁白段没有安排对口型？
⑦ 每段都原样写上了主体约束句？
自检不合格就改到合格再输出。`
}

func (a *App) llmPrompts(ctx context.Context, provider Provider, cfg VideoRunConfig, script VideoScript, caps VideoCapabilities, tpl *VideoTemplateRun, feedback string) (map[string]VideoPromptEdit, error) {
	shotsJSON, err := json.Marshal(script.Shots)
	if err != nil {
		return nil, err
	}
	userPrompt := buildPromptUserPrompt(cfg, string(shotsJSON), caps, tpl)
	// 提示词关卡上敲的意见（“整体偏冷调”“主体动作更克制”）只能在这里交给模型
	userPrompt += feedbackDirective(feedback)

	// 首帧图也给 Agent2：它要写“相对首帧的变化”，不能凭空想象首帧长什么样
	raw, err := a.chatCompletionWithImages(ctx, provider, promptSystemPrompt(cfg, caps), userPrompt, firstFrameImages(cfg))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Prompts []VideoPromptEdit `json:"prompts"`
	}
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return nil, fmt.Errorf("解析提示词 JSON 失败: %w", err)
	}
	result := make(map[string]VideoPromptEdit, len(payload.Prompts))
	for _, item := range payload.Prompts {
		if strings.TrimSpace(item.ShotID) == "" {
			continue
		}
		result[item.ShotID] = item
	}
	if len(result) == 0 {
		return nil, errors.New("模型没有返回可用提示词")
	}
	return result, nil
}

// generatePrompts 逐段展开提示词并做"字数 → 秒数"预算校验（§4.2）。
func (a *App) generatePrompts(ctx context.Context, cfg VideoRunConfig, script VideoScript, caps VideoCapabilities, feedback string, templates ...*VideoTemplateRun) ([]VideoPromptItem, []string) {
	tpl := firstTemplateRun(templates)
	warnings := make([]string, 0, 2)
	visuals := map[string]VideoPromptEdit{}

	// 关①确认后分镜可能被用户改过台词，也可能来自旧任务（当时还没有台词归属）：
	// 进模型前再钉一次，保证每句台词都有确定归宿。
	script, speakerWarnings := bindSpeakers(script, cfg)
	warnings = append(warnings, speakerWarnings...)

	provider := a.pickTextProvider(cfg)
	if provider.ID != "" {
		items, err := a.llmPrompts(ctx, provider, cfg, script, caps, tpl, feedback)
		if err == nil {
			visuals = items
		} else if !cfg.localFallbackEnabled() {
			return nil, append(warnings, fmt.Sprintf("已关闭本地兜底：文本模型展开提示词失败（%v）", err))
		} else {
			warnings = append(warnings, fmt.Sprintf("文本模型展开提示词失败，已回退本地模板：%v", err))
		}
	} else if !cfg.localFallbackEnabled() {
		return nil, append(warnings, "已关闭本地兜底：本任务需要文本模型展开提示词，请在「系统设置 - 提供商」登记后再试")
	} else {
		warnings = append(warnings, "未配置文本模型，提示词由本地模板生成")
	}
	// 模型漏段或写错 shotId 时，那几段会退回本地模板：必须说出来，
	// 否则用户以为整片提示词都是 AI 写的，实际是半自动拼接。
	if len(visuals) > 0 && len(visuals) < len(script.Shots) {
		missing := make([]string, 0, len(script.Shots)-len(visuals))
		for _, shot := range script.Shots {
			if _, ok := visuals[shot.ID]; !ok {
				missing = append(missing, shot.ID)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, fmt.Sprintf("提示词只覆盖 %d/%d 段：%s 已改用本地模板（模型漏写或 shotId 不一致）",
				len(visuals), len(script.Shots), strings.Join(missing, "、")))
		}
	}
	if len(visuals) == 0 {
		if warning := feedbackDroppedWarning(feedback); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if tpl != nil && len(tpl.Characters) > 0 && !caps.CharacterReference {
		warnings = append(warnings, "当前模型不支持主体参考图：已用模板的外观描述块做文本降级，跨段一致性依赖文字锚点")
	}
	if hasCharacterReferenceImages(cfg) && !caps.CharacterReference {
		if hasUserFirstFrame(cfg) {
			warnings = append(warnings, "当前模型不支持角色参考图：你上传的角色图不会被读取，主体与外观以首帧图为准")
		} else {
			warnings = append(warnings, "当前模型不支持角色参考图：你上传的角色图不会被读取，角色一致性只能靠提示词里的文字外观锚点，请检查并补足外观描述")
		}
	}

	allowed := caps.AllowedDurations
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}

	prompts := make([]VideoPromptItem, 0, len(script.Shots))
	for _, shot := range script.Shots {
		lines := normalizeDialogueLines(shot.DialogueLines, shot.Dialogue, shot.Speaker)
		visual := ""
		if item, ok := visuals[shot.ID]; ok {
			// 分镜是台词归属的权威源：Agent2 只在分镜没给台词时补充，
			// 不允许它悄悄改写说话人（模型回显的归属不可信）。
			if len(lines) == 0 {
				lines = normalizeDialogueLines(item.DialogueLines, item.Dialogue, item.Speaker)
			}
			visual = strings.TrimSpace(item.VisualPrompt)
		}
		if visual == "" {
			visual = localVisualPrompt(cfg, shot, caps)
		}
		visual = decorateVisualPrompt(cfg, shot, visual)
		visual = applyTemplatePromptExtras(visual, shot, caps, tpl, cfg)
		// 主体约束由 Agent2 写进提示词；渲染层只按定稿提示词执行，不再在最后一刻追加或改写。

		// 逐行解析台词归属：一段允许多个说话人，每行独立归一，不再压成单一 speaker。
		roleSet := make(map[string]bool, len(shot.Roles))
		for _, role := range shot.Roles {
			roleSet[role] = true
		}
		for i := range lines {
			speaker, warning := resolveSpeaker(cfg, lines[i].Speaker, shot.Roles, shot.ID, i, roleSet)
			if warning != "" {
				warnings = append(warnings, warning)
			}
			lines[i].Speaker = speaker
			if speaker != "" && speaker != VideoSpeakerNarration && !roleSet[speaker] {
				roleSet[speaker] = true
				shot.Roles = append(shot.Roles, speaker)
			}
		}
		dialogue := dialogueText(lines)
		speakerSummary := dialogueSpeakerSummary(lines)
		// 说好是角色在讲，画面提示词里却找不到他：配音出声、画面里无人对嘴
		for _, speaker := range dialogueSpeakerList(lines) {
			if speaker == VideoSpeakerNarration {
				continue
			}
			if !promptMentionsSpeaker(visual, cfg, speaker) {
				warnings = append(warnings, fmt.Sprintf("%s 的台词属于「%s」，但画面提示词没提到这个角色，渲染时可能找不到对口型的说话人",
					shot.ID, speakerLabel(cfg, speaker)))
			}
		}

		duration := shot.DurationS
		if duration <= 0 {
			duration = shot.TimeRange[1] - shot.TimeRange[0]
		}
		charCount := speechUnitCount(dialogue, cfg.Language)
		estimate := estimateSpeechSeconds(dialogue, cfg.Language, cfg.SpeechRate)
		fits := estimate <= duration+0.1
		note := ""
		if !fits {
			note = fmt.Sprintf("台词约 %.1fs，超出本段 %.1fs，建议精简或改排期", estimate, duration)
		}

		prompts = append(prompts, VideoPromptItem{
			ShotID:          shot.ID,
			VisualPrompt:    visual,
			Dialogue:        dialogue,
			Speaker:         speakerSummary,
			DialogueLines:   lines,
			Start:           shot.TimeRange[0],
			End:             shot.TimeRange[1],
			CharCount:       charCount,
			EstSpeechS:      estimate,
			Fits:            fits,
			RenderDurationS: nearestAllowedDuration(duration, allowed),
			Note:            note,
		})
	}
	return prompts, warnings
}

// localVisualPrompt 本地模板：固定槽位拼装，避免空洞标签。
// 没有 Agent2 参与时，主体约束也必须在这里补齐，否则关闭文本模型后提示词会完全失去约束。
func localVisualPrompt(cfg VideoRunConfig, shot VideoShot, caps VideoCapabilities) string {
	parts := make([]string, 0, 8)
	parts = append(parts, fmt.Sprintf("%s镜头，%s", cfg.AspectRatio, firstNonEmpty(shot.Camera, "固定机位微动")))
	parts = append(parts, "主体："+firstNonEmpty(shot.SceneIntent, cfg.Topic))
	if len(shot.Roles) > 0 {
		labels := make([]string, 0, len(shot.Roles))
		for _, role := range shot.Roles {
			labels = append(labels, characterDisplayName(cfg, role))
		}
		if hasCharacterReferenceImages(cfg) && caps.CharacterReference {
			parts = append(parts, "出场角色："+strings.Join(labels, "、")+"，其形体、毛色与服饰必须与随本段提交的角色参考图完全一致")
		} else {
			parts = append(parts, "出场角色："+strings.Join(labels, "、")+"，其形象按分镜描述保持一致")
		}
	}
	if shot.AudioCue != "" {
		parts = append(parts, "环境："+shot.AudioCue)
	}
	parts = append(parts, "氛围："+firstNonEmpty(shot.Mood, "自然"))
	parts = append(parts, "风格："+firstNonEmpty(cfg.VideoParams.Style, "写实电影感"))
	if hasUserFirstFrame(cfg) {
		parts = append(parts, "只描述首帧之后发生的变化：首帧是起始状态，环境、构图、动作与提示词冲突时以提示词的新画面为准，开头 0.5–1 秒内自然过渡，只保持主体身份（脸型、毛色、服装）连续，不锁死首帧背景与机位")
	} else {
		parts = append(parts, "只描述连续的物理运动，保持主体、光线与色彩一致")
	}
	// 正向表述：画面文字是模型的常见幻觉，但“不要文字”这种否定句会被模型当成要写文字
	parts = append(parts, "画面只保留角色与实物道具，字幕在后期单独叠加")
	parts = append(parts, promptSubjectDirective(cfg, caps))
	return strings.Join(parts, "；")
}

// decorateVisualPrompt 追加用户额外要求与负向提示。
func decorateVisualPrompt(cfg VideoRunConfig, shot VideoShot, visual string) string {
	extras := make([]string, 0, 2)
	if cfg.VideoParams.ExtraPrompt != "" {
		extras = append(extras, cfg.VideoParams.ExtraPrompt)
	}
	if cfg.VideoParams.NegativePrompt != "" {
		extras = append(extras, "避免出现："+cfg.VideoParams.NegativePrompt)
	}
	if len(extras) == 0 {
		return visual
	}
	return visual + "；" + strings.Join(extras, "；")
}

// characterDisplayName 把 role_id 翻成用户看得懂的角色名（找不到就回退原标识）。
func characterDisplayName(cfg VideoRunConfig, role string) string {
	key := strings.ToLower(strings.TrimSpace(role))
	for _, ref := range cfg.CharacterRefs {
		if strings.EqualFold(strings.TrimSpace(ref.RoleID), key) || strings.EqualFold(strings.TrimSpace(ref.Name), key) {
			if strings.TrimSpace(ref.Name) != "" {
				return ref.Name
			}
			return ref.RoleID
		}
	}
	return role
}

// applyTemplatePromptExtras 在固定槽位后注入模板风格片段与角色外观锚点（§2 §3）。
// 角色外观统一从任务角色档案解析：角色库或模板快照都走这一条路，
// 不支持的模型（方舟 Seedance 1.x 等）完全依赖它做跨段一致性。
// 用户自己上传过定妆图的角色不再注入文字外观：图片就是权威形象，模板文字写死猫狗会把用户图架空。
func applyTemplatePromptExtras(visual string, shot VideoShot, caps VideoCapabilities, tpl *VideoTemplateRun, cfg VideoRunConfig) string {
	if strings.TrimSpace(visual) == "" {
		return visual
	}
	extras := make([]string, 0, 3)
	if tpl != nil {
		if snippet := templateStyleSnippet(&tpl.VideoTemplate, caps.PromptStyle); snippet != "" {
			extras = append(extras, "风格参考："+snippet)
		}
	}
	for _, role := range shot.Roles {
		profile, ok := characterProfileForRole(cfg, tpl, role)
		if !ok || strings.TrimSpace(profile.AppearanceBlock) == "" {
			continue
		}
		// 用户上传的定妆图就是该角色的权威形象：文字外观不再另编，
		// 否则提示词会一边说“以图为准”，一边把猫/狗的外观写死。
		if roleUsesUserImages(cfg, role) {
			continue
		}
		name := firstNonEmpty(profile.Name, profile.RoleID, role)
		extras = append(extras, fmt.Sprintf("角色外观（%s）：%s", name, profile.AppearanceBlock))
	}
	if len(extras) == 0 {
		return visual
	}
	return visual + "；" + strings.Join(extras, "；")
}

func segmentPromptSuggestSystemPrompt(cfg VideoRunConfig, caps VideoCapabilities) string {
	return `你是图像生视频（i2v）与文生视频的提示词工程师。用户对某一已生成片段不满意，需要你重写一段更能达成意图的画面提示词。
只输出新的提示词正文，不要解释、引号、JSON 或 Markdown。
【写法】按槽位一次写完，总长 60-120 字：
[镜头/景别+运动] + [主体：角色名 + 1 个可辨识特征] + [一个可度量的动作] + [场景与锚点物件] + [光位/色温] + [风格氛围]
【硬性约束】
1. 第一句写镜头（景别、角度、运动、速度）；
2. 只写画面里发生的变化；一段只允许一个连续动作，不要 then/然后/接着 造成的段内切镜；
3. 用正向描述，不写否定句（“保持 X”而不是“不要 Y”）；画面文字由后期叠加，提示词里不提；
4. 禁止空洞标签（cinematic、8K、高级感、masterpiece）；
5. 针对用户的不满点做定向重写，其余意图保持稳定；
6. 有角色说话的段落，说话人必须留在画面里：镜头给到其面部（正面或侧脸），口型随台词动作；多个角色轮流说话时，轮到谁就拍到谁，其他角色嘴唇闭合；旁白段落画面内无人开口；
7. ` + promptSubjectDirective(cfg, caps) + `。` + firstFrameSemanticsBlock(cfg)
}

// SuggestSegmentPrompt 结合用户反馈重写单段画面提示词：逐段确认关卡里的 AI 辅助。
func (a *App) SuggestSegmentPrompt(taskID, segmentID, feedback string) (string, error) {
	task, err := a.videoTaskByID(taskID)
	if err != nil {
		return "", err
	}
	if task.Run.Plan == nil || len(task.Run.Plan.Segments) == 0 {
		return "", errors.New("任务还没有渲染计划：请先确认提示词生成渲染计划")
	}
	segmentID = strings.TrimSpace(segmentID)
	target := VideoSegment{}
	found := false
	for _, segment := range task.Run.Plan.Segments {
		if segment.ID == segmentID {
			target = segment
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("要优化的片段不在渲染计划中")
	}

	sceneIntent, shotCamera, shotMood := "不限", "不限", "不限"
	if task.Run.Script != nil {
		for _, shot := range task.Run.Script.Shots {
			if shot.ID == target.ShotID {
				sceneIntent = firstNonEmpty(shot.SceneIntent, "不限")
				shotCamera = firstNonEmpty(shot.Camera, "不限")
				shotMood = firstNonEmpty(shot.Mood, "不限")
				break
			}
		}
	}

	provider := a.pickTextProvider(task.Config)
	if provider.ID == "" {
		return "", errors.New("没有可用的文本模型：请先在「系统设置 - 提供商」登记文本模型，再使用 AI 优化提示词")
	}

	cfg := task.Config
	lines := normalizeDialogueLines(target.DialogueLines, target.Dialogue, target.Speaker)
	dialogueBlock := "无"
	if len(lines) > 0 {
		parts := make([]string, 0, len(lines))
		for _, line := range lines {
			parts = append(parts, fmt.Sprintf("%s：%s", speakerLabel(cfg, line.Speaker), line.Text))
		}
		dialogueBlock = strings.Join(parts, " / ")
	}
	userPrompt := fmt.Sprintf(`主题：%s
画幅：%s；语言：%s；风格：%s；运镜偏好：%s；情绪偏好：%s
片段：第 %d 段（%d 秒）
镜头场景意图：%s；镜头运镜：%s；镜头情绪：%s
当前提示词：
%s
台词与说话人（不要改写；角色说话必须留在画面里，旁白不安排对口型）：%s
用户对该段生成效果的不满（请定向重写）：%s
请输出重写后的画面提示词正文。`,
		cfg.Topic, cfg.AspectRatio, cfg.Language,
		firstNonEmpty(cfg.VideoParams.Style, "不限"),
		firstNonEmpty(cfg.VideoParams.Camera, "不限"),
		firstNonEmpty(cfg.VideoParams.Mood, "不限"),
		target.Index+1, target.DurationS,
		sceneIntent, shotCamera, shotMood,
		target.Prompt,
		dialogueBlock,
		firstNonEmpty(strings.TrimSpace(feedback), "效果不佳，请更具体、更可执行"),
	)

	// 逐段返工从 Wails 事件进来，没有流水线上下文：主体约束按任务实际视频通道的能力算，
	// 否则“模型读不到角色图”时会写出做不到的以图为准要求。
	caps := defaultVideoCapabilities()
	if _, adapter, videoProvider, _, err := a.resolveVideoAdapter(cfg); err == nil {
		caps = adapterCapabilities(adapter, videoProvider.Model)
	}
	raw, err := a.chatCompletion(withUsage(context.Background(), UsagePurposeSegmentFix, task.ID),
		provider, segmentPromptSuggestSystemPrompt(cfg, caps), userPrompt)
	if err != nil {
		return "", fmt.Errorf("AI 生成提示词失败: %w", err)
	}
	suggestion := cleanPromptSuggestion(raw)
	if suggestion == "" {
		return "", errors.New("模型没有返回可用的提示词")
	}
	// 模型重写后把说话人写没，是逐段返工最常见的翻车点：这里做一次确定性兜底，
	// 保证改完的提示词仍然把说话人留在画面里。
	missing := make([]string, 0, 2)
	seen := make(map[string]bool)
	for _, line := range lines {
		speaker := strings.TrimSpace(line.Speaker)
		if speaker == "" || speaker == VideoSpeakerNarration || seen[speaker] {
			continue
		}
		if promptMentionsSpeaker(suggestion, cfg, speaker) {
			continue
		}
		seen[speaker] = true
		missing = append(missing, speakerLabel(cfg, speaker))
	}
	if len(missing) > 0 {
		suggestion = strings.TrimSpace(suggestion) + fmt.Sprintf("；画面中必须出现说话人 %s，镜头给到其面部，口型随台词动作", strings.Join(missing, "、"))
	}
	return suggestion, nil
}

// cleanPromptSuggestion 剥掉模型偶发的代码块与首尾引号（含嵌套包裹），并限制长度避免超长回馈。
func cleanPromptSuggestion(raw string) string {
	trimmed := strings.TrimSpace(raw)
	for {
		before := trimmed
		if strings.HasPrefix(trimmed, "```") {
			trimmed = strings.TrimPrefix(trimmed, "```json")
			trimmed = strings.TrimPrefix(trimmed, "```")
			if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
				trimmed = trimmed[:idx]
			}
		}
		trimmed = strings.TrimSpace(trimmed)
		trimmed = strings.Trim(trimmed, "\"'“”‘’")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == before {
			break
		}
	}
	if runes := []rune(trimmed); len(runes) > 600 {
		trimmed = string(runes[:600])
	}
	return trimmed
}
