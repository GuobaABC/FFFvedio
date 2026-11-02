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

// animalCastDirective 出镜主体统一约束：一律用猫/狗/猪/鸡等动物担当，避免真人形象（肖像与合规风险）。
// 三段 Agent 提示词与最终下发给视频模型的提示词都会带上这条约束。
// 刻意写短（约 60 字）：它会被拼到每段提示词末尾，而 Seedance 单镜头提示词的舒适区只有几十到一百多字，
// 一条 90 字以上的尾巴会直接挤掉动作与光线描述。
const animalCastDirective = `画面主体只用动物角色（猫狗猪鸡等家畜家禽），可拟人化（直立、穿衣、做表情）但外形必须清晰可辨，不出现真人面孔与人体`

const scriptSystemPrompt = `【角色】你是中文短视频分镜编剧兼导演，擅长 15-60 秒口播与剧情短片。你的输出会被程序直接解析，不要 Markdown 代码块、不要任何解释。

【输出结构】
{"logline":"一句话主题","shots":[{"id":"s1","durationS":6,"roles":["char_a"],"speaker":"char_a","sceneIntent":"画面意图","dialogue":"台词","camera":"运镜","mood":"情绪","audioCue":"环境音"}]}
- id 从 s1 起顺序编号；durationS 为数字（秒）；roles 只能取用户给出的 role_id
- speaker 是这句台词的说话人：填 roles 里的 role_id，或填 "narration" 表示旁白（画外音）；无台词时填空字符串
- 不要输出 timeRange / language / source：时间轴与语言由系统计算

【规划规则】
1. 先定镜头数：总时长 ÷ 8 四舍五入，再限制在 2-8 个；镜头越短，一段里能发生的动作越少，别在 4 秒里塞完整故事；
2. 单段时长 4-12 秒（渲染单段上限 15 秒，超过 12 秒的叙事请拆成两个镜头）；
3. 各段 durationS 之和必须等于目标总时长；
4. 第一段承担 3 秒钩子：开场直接抛冲突、悬念或反常识结论，不要寒暄、自我介绍、空镜铺垫；
5. 相邻两段至少在景别、机位或动作方向上有一项不同，避免全片一个机位。

【台词规则】
6. 台词字数上限 =（段时长 - 0.3 秒）× 语速；宁可少写，不要靠压缩停顿蒙混；
7. 单句不超 20 字，一段最多两句；口语短句，不用成语堆叠、括号说明、表情符号与书面语；
8. 台词必须写清是谁在说：speaker 填该段的 role_id，画外解说填 "narration"（旁白），不允许空着或写“他/她/大家”；无台词段落 dialogue 与 speaker 都留空；台词里的称呼、自称与比喻要贴合动物主体，不要出现“上班打卡”“对着镜子化妆”这类只有人类才成立的身份动作。

【画面意图规则】
9. sceneIntent 只写看得见的东西：具体主体 + 可度量的动作 + 环境与光线；不写“字幕”“文字”“标题”“logo”等画面文字要求；
10. camera / mood / audioCue 各用 2-8 字的具体描述（如“缓慢推近”“克制/期待”“雨声垫底”），不要“电影感”“高级”这类空词；
11. 若提供首帧图，第一段必须与首帧衔接（同主体、同光线方向），后续段落保持主体外观、服装与光线一致；
12. 角色只要入镜（含手部、背影等局部）就必须写进 roles，确实无角色出场时才给空数组。

【硬性约束】
13. ` + animalCastDirective + `；
14. 用户给出的参数（时长、画幅、语言、语速、平台、垂类、风格/运镜/情绪、额外要求、负向约束）都是权威输入，不得改写；与上述规则冲突时，优先保证“不超时长与台词预算”；
15. 额外要求必须落到具体画面或台词里，负向约束必须全片都不出现。

【输出前自检】
① 段数在 2-8 且 durationS 之和 = 总时长？② 每段 4-12 秒？
③ 每段台词字数 ≤（时长-0.3）× 语速、单句 ≤ 20 字？④ roles 全部是给定 role_id？
⑤ 没有画面文字要求？⑥ 第一段是钩子且衔接首帧？⑦ 没有真人或人类专属动作？
⑧ 有台词的段落 speaker 都填了，并且是 role_id 或 "narration"？
自检不合格就改到合格再输出。`

func (a *App) llmScript(ctx context.Context, provider Provider, cfg VideoRunConfig, tpl *VideoTemplateRun, feedback string) (VideoScript, []string, error) {
	userPrompt := buildScriptUserPrompt(cfg, tpl)
	// 用户在分镜关卡敲的返工意见：不拼进去就等于没听见
	userPrompt += feedbackDirective(feedback)

	// 首帧作为多模态输入（文档 §5 的 B 类能力）：模型支持读图时能让分镜与首帧外观一致；
	// 不支持读图的文本模型只是忽略图片块，仍按纯文本返回。
	images := firstFrameImages(cfg)

	raw, imageWarnings, _, err := a.chatCompletionTraced(ctx, provider, scriptSystemPrompt, userPrompt, images)
	if err != nil {
		return VideoScript{}, imageWarnings, err
	}

	var payload struct {
		Logline  string `json:"logline"`
		Language string `json:"language"`
		Shots    []struct {
			ID          string    `json:"id"`
			DurationS   float64   `json:"durationS"`
			TimeRange   []float64 `json:"timeRange"`
			Roles       []string  `json:"roles"`
			SceneIntent string    `json:"sceneIntent"`
			Dialogue    string    `json:"dialogue"`
			Camera      string    `json:"camera"`
			Mood        string    `json:"mood"`
			AudioCue    string    `json:"audioCue"`
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
		script.Shots = append(script.Shots, VideoShot{
			ID:          item.ID,
			DurationS:   duration,
			Roles:       item.Roles,
			SceneIntent: item.SceneIntent,
			Dialogue:    item.Dialogue,
			Camera:      item.Camera,
			Mood:        item.Mood,
			AudioCue:    item.AudioCue,
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
		return "已提供首帧图（随消息附图）：第一段画面必须从这张图开始，主体与光线方向保持一致"
	}
	return "已提供首帧图（随消息附图，作画面起点参考，可与角色参考图同时使用）"
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

// characterTable 角色表：role_id、名字、形象来源与外观锚点。
// Agent2 只看得到 shots 里的 role_id 时写不出可辨识特征，四段会各画一只不同的猫。
func characterTable(cfg VideoRunConfig, tpl *VideoTemplateRun) string {
	if len(cfg.CharacterRefs) == 0 {
		return "无固定角色（画面主体由分镜决定）"
	}
	lines := make([]string, 0, len(cfg.CharacterRefs))
	for _, ref := range cfg.CharacterRefs {
		line := fmt.Sprintf("- %s（%s）", ref.RoleID, firstNonEmpty(ref.Name, ref.RoleID))
		switch {
		case ref.ImageSource == CharacterImageSourceUser && len(ref.Images) > 0:
			line += "｜形象：用户上传的定妆图（以此为准，不要另编外貌）"
		case len(ref.Images) > 0:
			line += "｜形象：模板定妆图"
		default:
			line += "｜形象：无图，只能靠文字描述"
		}
		// 模板角色的外观锚点已由 templateScriptHints 列出，这里只在模板没给时补
		if tpl != nil {
			if character := templateCharacterByRole(&tpl.VideoTemplate, ref.RoleID); character != nil && strings.TrimSpace(character.AppearanceBlock) != "" {
				lines = append(lines, line)
				continue
			}
		}
		if strings.TrimSpace(ref.VoiceHint) != "" {
			line += "｜声音：" + ref.VoiceHint
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
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
	if hints := templateScriptHints(tpl); hints != "" {
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
	if hints := templateScriptHints(tpl); hints != "" {
		lines = append(lines, hints)
	}
	lines = append(lines,
		"分镜（JSON，shotId 必须逐字复用）：\n"+shotsJSON,
		"请为每个 shot 输出 visualPrompt 与原样 dialogue；单段 60-120 字，不要写否定句与画面文字要求。")
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
// 但意见不能凌驾结构与安全硬约束（JSON 格式、台词原样保留、动物主体），所以写成“冲突时仍守硬性约束”。
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

// generateScript 先尝试文本模型，失败或未配置时降级为本地规则（设计文档 §9 的最小闭环思路）。
func (a *App) generateScript(ctx context.Context, cfg VideoRunConfig, feedback string, templates ...*VideoTemplateRun) (VideoScript, []string) {
	tpl := firstTemplateRun(templates)
	warnings := make([]string, 0, 2)
	provider := a.pickTextProvider(cfg)
	if provider.ID != "" {
		script, llmWarnings, err := a.llmScript(ctx, provider, cfg, tpl, feedback)
		warnings = append(warnings, llmWarnings...)
		if err == nil {
			normalized := normalizeScript(script, cfg, tpl)
			if len(normalized.Shots) > 0 {
				bound, speakerWarnings := bindSpeakers(normalized, cfg)
				return bound, append(warnings, speakerWarnings...)
			}
			err = errors.New("模型返回的分镜归一化后为空")
		}
		// 用户关掉本地兜底后不许偷偷降级：返回空分镜交给上层报错，别让他以为这是模型写的
		if !cfg.localFallbackEnabled() {
			return VideoScript{}, append(warnings, fmt.Sprintf("已关闭本地兜底：文本模型未产出可用分镜（%v）", err))
		}
		warnings = append(warnings, fmt.Sprintf("文本模型生成分镜失败，已回退本地规则：%v", err))
	} else {
		if !cfg.localFallbackEnabled() {
			return VideoScript{}, append(warnings, "已关闭本地兜底：本任务需要文本模型产出分镜，请在「系统设置 - 提供商」登记后再试")
		}
		warnings = append(warnings, "未配置文本模型，已使用本地规则生成分镜（可在「系统设置 - 提供商」登记文本模型）")
	}
	if warning := feedbackDroppedWarning(feedback); warning != "" {
		warnings = append(warnings, warning)
	}
	bound, speakerWarnings := bindSpeakers(normalizeScript(localVideoScript(cfg, tpl), cfg, tpl), cfg)
	return bound, append(warnings, speakerWarnings...)
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

// templateScriptHints 把模板的风格骨架 / 角色外观 / 开场钩子喂给 Agent1。
func templateScriptHints(tpl *VideoTemplateRun) string {
	if tpl == nil {
		return ""
	}
	lines := make([]string, 0, 6)
	lines = append(lines, fmt.Sprintf("模板：%s v%s（%s）——模板只提供默认值，不约束本次创意", tpl.Name, tpl.Version, templateScopeLabel(tpl.Scope)))
	if summary := templateStyleSummary(&tpl.VideoTemplate); summary != "" {
		lines = append(lines, "风格骨架（受控词表）："+summary)
	}
	for _, character := range tpl.Characters {
		detail := fmt.Sprintf("角色 %s（%s）外观锚点：%s", character.RoleID, character.Name, character.AppearanceBlock)
		if character.Perspective != "" {
			detail += "；" + character.Perspective
		}
		lines = append(lines, detail)
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

// matchSpeakerRole 把说话人写法映回角色表：模型可能写 role_id、角色名，或“主理人（橘猫）”里的片段。
func matchSpeakerRole(cfg VideoRunConfig, value string) (string, bool) {
	key := normalizeRoleKey(value)
	if key == "" {
		return "", false
	}
	set := map[string]bool{key: true}
	for _, ref := range cfg.CharacterRefs {
		if roleMatches(set, ref) {
			return ref.RoleID, true
		}
	}
	return "", false
}

// normalizeSpeakerOrRaw 优先归一到 role_id；角色表里没有但这个值本身是合法标识时原样保留。
func normalizeSpeakerOrRaw(cfg VideoRunConfig, role string) string {
	if roleID, ok := matchSpeakerRole(cfg, role); ok {
		return roleID
	}
	return strings.TrimSpace(role)
}

// normalizeSpeaker 把外部传来的台词归属（关卡编辑 / 模型回显）归一成 role_id 或 narration。
func normalizeSpeaker(cfg VideoRunConfig, value string) string {
	if isNarrationSpeaker(value) {
		return VideoSpeakerNarration
	}
	if roleID, ok := matchSpeakerRole(cfg, value); ok {
		return roleID
	}
	return ""
}

// promptMentionsSpeaker 画面提示词里有没有出现这个说话人：role_id、角色名，或名字的中文部分。
// 名字常带括号说明（“主理人（橘猫）”），逐段写全名不自然，所以允许只写前半段。
func promptMentionsSpeaker(visual string, cfg VideoRunConfig, speaker string) bool {
	if strings.TrimSpace(speaker) == "" {
		return false
	}
	if strings.Contains(visual, speaker) {
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
		if strings.Contains(visual, name) {
			return true
		}
		if idx := strings.IndexAny(name, "（("); idx > 0 {
			return strings.Contains(visual, strings.TrimSpace(name[:idx]))
		}
		return false
	}
	return false
}

// bindSpeakers 给每段有台词的镜头钉上“谁在说”：角色台词必须是角色表里的 role_id，
// 画外解说落到 narration。台词归属不定，渲染层就不知道该给谁拍口型，字幕与配音也会含糊。
// 模型没写时先看本段是否只有一个出场角色，再看是不是旁白；猜不准就按旁白处理并明说原因，
// 不静默替用户选一个“大概是他”。
func bindSpeakers(script VideoScript, cfg VideoRunConfig) (VideoScript, []string) {
	warnings := make([]string, 0, 2)
	shots := make([]VideoShot, len(script.Shots))
	for i, shot := range script.Shots {
		raw := strings.TrimSpace(shot.Speaker)
		shot.Speaker = ""
		if shot.Dialogue == "" {
			// 没台词就没有说话人：留着归属只会让渲染层去拍一张不存在的嘴
			shots[i] = shot
			continue
		}
		switch {
		case raw != "" && isNarrationSpeaker(raw):
			shot.Speaker = VideoSpeakerNarration
		case raw != "":
			if roleID, ok := matchSpeakerRole(cfg, raw); ok {
				shot.Speaker = roleID
			} else if len(shot.Roles) == 1 {
				// 写了个角色表里没有的名字，但本段只出现一个角色：大概率就是他
				shot.Speaker = normalizeSpeakerOrRaw(cfg, shot.Roles[0])
				warnings = append(warnings, fmt.Sprintf("%s 的说话人「%s」不在角色表里，已按本段唯一出场角色「%s」处理", shot.ID, raw, shot.Speaker))
			} else {
				shot.Speaker = VideoSpeakerNarration
				warnings = append(warnings, fmt.Sprintf("%s 的说话人「%s」不在角色表里，已按旁白（画外音）处理", shot.ID, raw))
			}
		case len(shot.Roles) == 1:
			shot.Speaker = normalizeSpeakerOrRaw(cfg, shot.Roles[0])
		case len(shot.Roles) == 0:
			// 画里没人，台词只能是画外音
			shot.Speaker = VideoSpeakerNarration
		default:
			shot.Speaker = VideoSpeakerNarration
			warnings = append(warnings, fmt.Sprintf("%s 有台词但没写说话人，本段出现多个角色，已按旁白（画外音）处理；可在逐段确认里改成某个角色说话", shot.ID))
		}
		shots[i] = shot
	}
	script.Shots = shots
	return script, warnings
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
		dialogue := strings.TrimSpace(raw.Dialogue)
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
			ID:          fmt.Sprintf("s%d", len(shots)+1),
			TimeRange:   [2]float64{round2(cursor), round2(cursor + duration)},
			DurationS:   round2(duration),
			Roles:       roles,
			SceneIntent: scene,
			Dialogue:    dialogue,
			Speaker:     strings.TrimSpace(raw.Speaker),
			Camera:      strings.TrimSpace(raw.Camera),
			Mood:        strings.TrimSpace(raw.Mood),
			AudioCue:    strings.TrimSpace(raw.AudioCue),
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
		shot.Dialogue = strings.TrimSpace(shot.Dialogue)
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
		Logline:  firstNonEmpty(script.Logline, cfg.Topic),
		Language: language,
		Source:   source,
		Shots:    shots,
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
	shotCount := clampInt(int(math.Round(float64(cfg.DurationS)/6)), 1, 5)
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
		shots = append(shots, VideoShot{
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
		})
		cursor += duration
	}

	// 开场白只占 shot#1 的台词槽位，并按语速预算裁剪：模板不能绕过字数/时长校验（§4）
	if tpl != nil && strings.TrimSpace(tpl.OpeningLine) != "" && len(shots) > 0 {
		shots[0].Dialogue = trimDialogueToBudget(tpl.OpeningLine, shots[0].DurationS, cfg.SpeechRate, cfg.Language)
	}

	return VideoScript{
		Logline:  defaultVideoTitle(cfg.Topic),
		Language: cfg.Language,
		Source:   "local",
		Shots:    shots,
	}
}

// trimDialogueToBudget 把台词裁到「本段时长 × 语速」预算内，中文按字、英文按词。
func trimDialogueToBudget(text string, duration, rate float64, lang string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if rate <= 0 {
		rate = defaultSpeechRate(lang)
	}
	budget := int(duration * rate)
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

const promptSystemPrompt = `【角色】你是图像生视频（i2v）与文生视频的提示词工程师。分镜结构与台词已经定稿：你只决定“画面怎么写”，不改剧情、不改台词、不改段数。你的输出会被程序直接解析，不要 Markdown 代码块、不要解释。

【输出结构】
{"prompts":[{"shotId":"s1","visualPrompt":"...","dialogue":"...","speaker":"char_a"}]}
- shotId 必须与输入分镜的 id 逐字一致（写成别的值会导致该段被系统丢弃）
- 每段都要输出，条数必须等于输入分镜数
- dialogue 原样返回，不要翻译、改写或加旁白标记；台词偏长由系统提示，不由你压缩
- speaker 原样返回输入里该分镜的归属（role_id 或 "narration"），不要改归属、也不要新增说话人

【单段写法】按槽位一次写完，总长 60-120 字：
[镜头/景别+运动] + [主体：角色名 + 1 个可辨识特征] + [一个可度量的动作] + [场景与锚点物件] + [光位/色温] + [风格氛围]

【台词归属必须落到画面】
- speaker 是角色的分段：说话人必须在画面里（正面或侧脸），口型、神态跟着台词走，镜头对向说话人；不要让他背对镜头念台词；
- speaker 是 "narration" 的分段：不安排任何主体对口型，用空镜、环境细节或主体反应镜头承载画外音；
- 同一段只允许一个主体说话；
- 这一节是硬约束：画面里找不到说话人，台词就会变成“画外飘音”，配音与口型对不上。

【写法规则】
1. 第一句写镜头（景别、角度、运动、速度）；
2. 主体用具体特征；禁止“美女/帅哥/一个人”这类空词，也禁止 cinematic、8K、高级感、masterpiece 这类空洞标签；
3. 动作写成物理量（重心、步伐、接触面）；一段只允许一个连续动作，不要 then/然后/接着 造成的段内切镜；
4. 只写画面里发生的变化；不要复述角色整段外观与服装（下游会统一追加外观锚点，重复写只会稀释动作指令）；
5. 不要写“不出现文字/水印/logo”这类否定句：本模型按正向描述理解画面，直接写“画面只有角色与实物道具”即可；
6. 相邻分镜的景别或运动至少一项不同；
7. 段落分工：第一段从首帧图开始，只写之后发生的变化；第二段起接续上一段结尾画面，不重复上一段已完成的动作；
8. 用户给的画幅、风格、运镜、情绪、额外要求、负向约束都是权威输入，必须体现；负向约束改写成正面表述（“保持 X”而不是“不要 Y”）；
9. ` + animalCastDirective + `。

【输出前自检】
① 条数等于输入分镜数、shotId 全部逐字命中？② 每段 60-120 字、只有一个连续动作？
③ 没有否定句与空洞标签？④ 台词逐字未改？⑤ 相邻段景别或运动有变化？
⑥ 有台词的段落都拍到了说话人，或者明确按旁白处理？
自检不合格就改到合格再输出。`

func (a *App) llmPrompts(ctx context.Context, provider Provider, cfg VideoRunConfig, script VideoScript, caps VideoCapabilities, tpl *VideoTemplateRun, feedback string) (map[string]VideoPromptEdit, error) {
	shotsJSON, err := json.Marshal(script.Shots)
	if err != nil {
		return nil, err
	}
	userPrompt := buildPromptUserPrompt(cfg, string(shotsJSON), caps, tpl)
	// 提示词关卡上敲的意见（“整体偏冷调”“主体动作更克制”）只能在这里交给模型
	userPrompt += feedbackDirective(feedback)

	// 首帧图也给 Agent2：它要写“相对首帧的变化”，不能凭空想象首帧长什么样
	raw, err := a.chatCompletionWithImages(ctx, provider, promptSystemPrompt, userPrompt, firstFrameImages(cfg))
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

	allowed := caps.AllowedDurations
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}

	prompts := make([]VideoPromptItem, 0, len(script.Shots))
	for _, shot := range script.Shots {
		dialogue := shot.Dialogue
		visual := ""
		if item, ok := visuals[shot.ID]; ok {
			if strings.TrimSpace(item.Dialogue) != "" {
				dialogue = strings.TrimSpace(item.Dialogue)
			}
			visual = strings.TrimSpace(item.VisualPrompt)
		}
		if visual == "" {
			visual = localVisualPrompt(cfg, shot)
		}
		visual = decorateVisualPrompt(cfg, shot, visual)
		visual = applyTemplatePromptExtras(visual, shot, caps, tpl, cfg)
		// 动物主体约束钉在末尾：即使文本模型漏写、或模板角色写成了真人，也能在最后一刻纠偏
		visual = appendCastDirective(visual)

		// 台词归属以分镜为准（模型只负责把它写进画面）：说话人写错了不会丢台词，但会直接让口型对不上嘴。
		speaker := shot.Speaker
		if speaker == "" && dialogue != "" {
			speaker = normalizeSpeaker(cfg, visuals[shot.ID].Speaker)
		}
		if dialogue == "" {
			speaker = ""
		}
		// 说好是角色在讲，画面提示词里却找不到他：配音出声、画面里无人对嘴
		if speaker != "" && speaker != VideoSpeakerNarration && !promptMentionsSpeaker(visual, cfg, speaker) {
			warnings = append(warnings, fmt.Sprintf("%s 的台词属于「%s」，但画面提示词没提到这个角色，渲染时可能找不到对口型的说话人",
				shot.ID, speakerLabel(cfg, speaker)))
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
			Speaker:         speaker,
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
func localVisualPrompt(cfg VideoRunConfig, shot VideoShot) string {
	parts := make([]string, 0, 8)
	parts = append(parts, fmt.Sprintf("%s镜头，%s", cfg.AspectRatio, firstNonEmpty(shot.Camera, "固定机位微动")))
	parts = append(parts, "主体："+firstNonEmpty(shot.SceneIntent, cfg.Topic))
	if len(shot.Roles) > 0 {
		labels := make([]string, 0, len(shot.Roles))
		for _, role := range shot.Roles {
			labels = append(labels, characterDisplayName(cfg, role))
		}
		parts = append(parts, "出场角色："+strings.Join(labels, "、")+"，其形体、毛色与服饰必须与随本段提交的角色参考图完全一致")
	}
	if shot.AudioCue != "" {
		parts = append(parts, "环境："+shot.AudioCue)
	}
	parts = append(parts, "氛围："+firstNonEmpty(shot.Mood, "自然"))
	parts = append(parts, "风格："+firstNonEmpty(cfg.VideoParams.Style, "写实电影感"))
	parts = append(parts, "只描述从首帧开始发生的连续运动，保持主体、光线与色彩一致")
	// 正向表述：画面文字是模型的常见幻觉，但“不要文字”这种否定句会被模型当成要写文字
	parts = append(parts, "画面只保留角色与实物道具，字幕在后期单独叠加")
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

// appendCastDirective 把出镜主体约束追加到画面提示词末尾（已带同一约束时不重复拼接）。
func appendCastDirective(visual string) string {
	if strings.TrimSpace(visual) == "" || strings.Contains(visual, animalCastDirective) {
		return visual
	}
	return visual + "；" + animalCastDirective
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
// 外观文字锚点无条件保留：支持主体参考的模型除了喂图还要一份文字特征（两者不矛盾），
// 不支持的模型（方舟 Seedance 1.x 等）完全依赖它做跨段一致性。
// 但用户自己上传过定妆图的角色要补一句裁决规则：模板里的外观描述可能与上传图不一致，
// 文字留着兜底，冲突时以图为准。
func applyTemplatePromptExtras(visual string, shot VideoShot, caps VideoCapabilities, tpl *VideoTemplateRun, cfg VideoRunConfig) string {
	if tpl == nil || strings.TrimSpace(visual) == "" {
		return visual
	}
	extras := make([]string, 0, 3)
	if snippet := templateStyleSnippet(&tpl.VideoTemplate, caps.PromptStyle); snippet != "" {
		extras = append(extras, "风格参考："+snippet)
	}
	for _, role := range shot.Roles {
		character := templateCharacterByRole(&tpl.VideoTemplate, role)
		if character == nil || strings.TrimSpace(character.AppearanceBlock) == "" {
			continue
		}
		anchor := fmt.Sprintf("角色外观（%s）：%s", character.Name, character.AppearanceBlock)
		if roleUsesUserImages(cfg, role) {
			anchor += "（该角色已附用户上传的定妆图，文字描述与图片不一致时一律以图片为准）"
		}
		extras = append(extras, anchor)
	}
	if len(extras) == 0 {
		return visual
	}
	return visual + "；" + strings.Join(extras, "；")
}

const segmentPromptSuggestSystemPrompt = `你是图像生视频（i2v）与文生视频的提示词工程师。用户对某一已生成片段不满意，需要你重写一段更能达成意图的画面提示词。
只输出新的提示词正文，不要解释、引号、JSON 或 Markdown。
【写法】按槽位一次写完，总长 60-120 字：
[镜头/景别+运动] + [主体：角色名 + 1 个可辨识特征] + [一个可度量的动作] + [场景与锚点物件] + [光位/色温] + [风格氛围]
【硬性约束】
1. 第一句写镜头（景别、角度、运动、速度）；
2. 只写画面里发生的变化；一段只允许一个连续动作，不要 then/然后/接着 造成的段内切镜；
3. 用正向描述，不写否定句（“保持 X”而不是“不要 Y”）；画面文字由后期叠加，提示词里不提；
4. 禁止空洞标签（cinematic、8K、高级感、masterpiece）；
5. 针对用户的不满点做定向重写，其余意图保持稳定；
6. ` + animalCastDirective + `。`

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
	userPrompt := fmt.Sprintf(`主题：%s
画幅：%s；语言：%s；风格：%s；运镜偏好：%s；情绪偏好：%s
片段：第 %d 段（%d 秒）
镜头场景意图：%s；镜头运镜：%s；镜头情绪：%s
当前提示词：
%s
台词（不要改写，也不要写进画面文字要求）：%s
用户对该段生成效果的不满（请定向重写）：%s
请输出重写后的画面提示词正文。`,
		cfg.Topic, cfg.AspectRatio, cfg.Language,
		firstNonEmpty(cfg.VideoParams.Style, "不限"),
		firstNonEmpty(cfg.VideoParams.Camera, "不限"),
		firstNonEmpty(cfg.VideoParams.Mood, "不限"),
		target.Index+1, target.DurationS,
		sceneIntent, shotCamera, shotMood,
		target.Prompt,
		firstNonEmpty(target.Dialogue, "无"),
		firstNonEmpty(strings.TrimSpace(feedback), "效果不佳，请更具体、更可执行"),
	)

	// 逐段返工从 Wails 事件进来，没有流水线上下文，这里自己补上用途与任务 ID
	raw, err := a.chatCompletion(withUsage(context.Background(), UsagePurposeSegmentFix, task.ID),
		provider, segmentPromptSuggestSystemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("AI 生成提示词失败: %w", err)
	}
	suggestion := cleanPromptSuggestion(raw)
	if suggestion == "" {
		return "", errors.New("模型没有返回可用的提示词")
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
