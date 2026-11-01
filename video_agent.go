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
const animalCastDirective = `出镜主体优先使用动物角色（猫、狗、猪、鸡等家畜家禽），画面里不要出现真人面孔、人体、人手或人物剪影；需要“说话人”时同样由动物担当，可以拟人化（直立、穿衣、做表情），但动物外形必须清晰可辨`

const scriptSystemPrompt = `你是中文短视频编剧兼导演，擅长 15-60 秒的口播与剧情短视频。
用户会给出主题、目标时长、语速与首帧信息。
只输出一个 JSON 对象，不要 Markdown 代码块，不要任何解释。
JSON 结构：
{"logline":"一句话主题","language":"zh-CN","shots":[{"id":"s1","durationS":6,"roles":["char_a"],"sceneIntent":"画面意图：具体主体+物理可度量的动作+环境","dialogue":"口播台词","camera":"运镜","mood":"情绪","audioCue":"环境音"}]}
硬性约束：
1. 输出 2-4 个分镜，durationS 之和必须等于目标总时长；
2. 第一段承担“3 秒钩子”：开场就抛冲突、悬念或反常识结论，不要寒暄与自我介绍；
3. 台词长度受语速预算约束：单段台词字数 <= 段时长 × 语速，并给停顿留出 0.3 秒余量；超预算就精简台词；
4. 台词是说出来给人听的口语短句，单句不超过 20 字，避免书面语、成语堆叠、括号说明与表情符号；
5. 画面只写看得见的东西：具体主体、可度量的动作、环境与光线；不要写“字幕”“文字”“标题”“logo”等画面文字要求；
6. 若提供首帧图，第一段画面必须与首帧衔接，后续段落保持主体外观、服装与光线一致；
7. 台词口语化，没有台词的段落 dialogue 用空字符串；
8. roles 只能使用用户给出的角色 role_id；只要该角色出现在画面里（含手部、背影等局部入镜）就必须写进 roles，确实无角色出场时才给空数组；
9. ` + animalCastDirective + `
10. 台词里的称呼、自称与比喻也要贴合动物主体，不要出现只有人类才成立的身份动作（如上班打卡、对着镜子化妆的手部特写）。`

func (a *App) llmScript(ctx context.Context, provider Provider, cfg VideoRunConfig, tpl *VideoTemplateRun) (VideoScript, []string, error) {
	roles := make([]string, 0, len(cfg.CharacterRefs))
	for _, ref := range cfg.CharacterRefs {
		label := fmt.Sprintf("%s(%s)", ref.RoleID, ref.Name)
		// 用户亲自传了图的角色形象已经钉死，分镜不要再编外貌细节去和图对不上
		if ref.ImageSource == CharacterImageSourceUser && len(ref.Images) > 0 {
			label += "（形象以上传的定妆图为准）"
		}
		roles = append(roles, label)
	}
	firstFrame := "未提供"
	if cfg.FirstFrame != "" {
		firstFrame = cfg.FirstFrame
	}

	userPrompt := fmt.Sprintf(`主题：%s
目标总时长：%d 秒
语言：%s
语速预算：%.1f %s/秒
画幅：%s
首帧：%s
可出场角色：%s
风格偏好：%s；运镜偏好：%s；情绪偏好：%s
请按 system 中的 JSON 结构输出分镜。`,
		cfg.Topic, cfg.DurationS, cfg.Language, cfg.SpeechRate,
		unitName(cfg.Language), cfg.AspectRatio, firstFrame,
		firstNonEmpty(strings.Join(roles, "、"), "无"),
		firstNonEmpty(cfg.VideoParams.Style, "不限"),
		firstNonEmpty(cfg.VideoParams.Camera, "不限"),
		firstNonEmpty(cfg.VideoParams.Mood, "不限"),
	)
	// 模板只提供骨架：风格词表、角色外观锚点与开场钩子候选（§2 §3 §4）
	if hints := templateScriptHints(tpl); hints != "" {
		userPrompt += "\n" + hints
	}

	// 首帧作为多模态输入（文档 §5 的 B 类能力）：模型支持读图时能让分镜与首帧外观一致；
	// 不支持读图的文本模型只是忽略图片块，仍按纯文本返回。
	images := make([]string, 0, 1)
	if strings.TrimSpace(cfg.FirstFrame) != "" {
		images = append(images, cfg.FirstFrame)
	}

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

func unitName(lang string) string {
	if isEnglishLanguage(lang) {
		return "词"
	}
	return "字"
}

// generateScript 先尝试文本模型，失败或未配置时降级为本地规则（设计文档 §9 的最小闭环思路）。
func (a *App) generateScript(ctx context.Context, cfg VideoRunConfig, templates ...*VideoTemplateRun) (VideoScript, []string) {
	tpl := firstTemplateRun(templates)
	warnings := make([]string, 0, 2)
	provider := a.pickTextProvider(cfg)
	if provider.ID != "" {
		script, llmWarnings, err := a.llmScript(ctx, provider, cfg, tpl)
		warnings = append(warnings, llmWarnings...)
		if err == nil {
			normalized := normalizeScript(script, cfg, tpl)
			if len(normalized.Shots) > 0 {
				return normalized, warnings
			}
			err = errors.New("模型返回的分镜归一化后为空")
		}
		warnings = append(warnings, fmt.Sprintf("文本模型生成分镜失败，已回退本地规则：%v", err))
	} else {
		warnings = append(warnings, "未配置文本模型，已使用本地规则生成分镜（可在「系统设置 - 提供商」登记文本模型）")
	}
	return normalizeScript(localVideoScript(cfg, tpl), cfg, tpl), warnings
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

const promptSystemPrompt = `你是图像生视频（i2v）与文生视频的提示词工程师。把分镜展开为可直接给视频模型执行的画面提示词，并原样保留台词。
只输出 JSON：{"prompts":[{"shotId":"s1","visualPrompt":"...","dialogue":"..."}]}
提示词写法（槽位顺序固定）：
[镜头/景别+运动] + [主体及其可辨识特征] + [物理可度量的动作] + [环境/道具/时间天气] + [光线方向/质感/色调] + [风格与氛围]
硬性约束：
1. 第一句写镜头（景别、角度、运动、速度），并锁定画幅；
2. 主体用具体特征（服装/发型/年龄/材质），禁止“美女/帅哥”这类空词；
3. 动作写成物理量（重心、步伐、接触面），禁止“很美地走动”这类描述；
4. 禁止空洞标签（cinematic、8K、高级感、masterpiece）；
5. 只描述“从首帧开始发生的变化”，不要重复描述首帧已有内容；
6. 每段只允许一个连续动作，不要出现 then/然后/接着 导致的段内切镜；
7. 画面内不要出现任何文字、字幕、水印、logo 或 UI 元素，也不要要求模型生成文字；
8. 相邻分镜的景别或运动至少一项不同，避免全片一个机位；
9. 台词必须原样返回，不要改写、翻译或添加旁白标记；
10. ` + animalCastDirective + `。`

func (a *App) llmPrompts(ctx context.Context, provider Provider, cfg VideoRunConfig, script VideoScript, tpl *VideoTemplateRun) (map[string]VideoPromptEdit, error) {
	shotsJSON, err := json.Marshal(script.Shots)
	if err != nil {
		return nil, err
	}
	userPrompt := fmt.Sprintf(`主题：%s
画幅：%s；语言：%s；风格：%s；运镜：%s；情绪：%s
总时长：%d 秒；语速预算：%.1f %s/秒（台词偏长时只提示，不要改写台词）
分镜（JSON）：
%s
请为每个 shot 输出 visualPrompt 与 dialogue；visualPrompt 中不得出现任何画面文字要求。`,
		cfg.Topic, cfg.AspectRatio, cfg.Language,
		firstNonEmpty(cfg.VideoParams.Style, "不限"),
		firstNonEmpty(cfg.VideoParams.Camera, "不限"),
		firstNonEmpty(cfg.VideoParams.Mood, "不限"),
		cfg.DurationS, cfg.SpeechRate, unitName(cfg.Language),
		string(shotsJSON),
	)
	if hints := templateScriptHints(tpl); hints != "" {
		userPrompt += "\n" + hints
	}

	raw, err := a.chatCompletion(ctx, provider, promptSystemPrompt, userPrompt)
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
func (a *App) generatePrompts(ctx context.Context, cfg VideoRunConfig, script VideoScript, caps VideoCapabilities, templates ...*VideoTemplateRun) ([]VideoPromptItem, []string) {
	tpl := firstTemplateRun(templates)
	warnings := make([]string, 0, 2)
	visuals := map[string]VideoPromptEdit{}

	provider := a.pickTextProvider(cfg)
	if provider.ID != "" {
		items, err := a.llmPrompts(ctx, provider, cfg, script, tpl)
		if err == nil {
			visuals = items
		} else {
			warnings = append(warnings, fmt.Sprintf("文本模型展开提示词失败，已回退本地模板：%v", err))
		}
	} else {
		warnings = append(warnings, "未配置文本模型，提示词由本地模板生成")
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
	parts = append(parts, "画面内不出现任何文字、字幕、水印与 logo")
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
硬性约束：
1. 槽位顺序固定：[镜头/景别+运动] + [主体及其可辨识特征] + [物理可度量的动作] + [环境/道具/时间天气] + [光线方向/质感/色调] + [风格与氛围]；
2. 第一句写镜头（景别、角度、运动、速度），并锁定画幅；
3. 只描述“从首帧开始发生的变化”，不要重复首帧已有内容；
4. 每段只允许一个连续动作，禁止 then/然后/接着 导致段内切镜；
5. 画面内不得出现任何文字、字幕、水印、logo 或 UI 元素；
6. 禁止空洞标签（cinematic、8K、高级感、masterpiece）；
7. 针对用户给出不满点做定向重写，其余意图保持稳定；
8. ` + animalCastDirective + `。`

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
