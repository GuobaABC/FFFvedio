package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"strings"
)

// errScriptBinding 台词绑定硬失败：不允许回退本地规则，必须卡住让用户重生成/修改。
var errScriptBinding = errors.New("台词绑定未通过")

/* ------------------------------------------------------------------
   Agent1 两阶段：企划（角色 + 故事骨架）→ 分镜（只做可视化）
   ------------------------------------------------------------------ */

// shotCountRange 按题材/时长推荐镜头数。长视频放宽上限，让 60 秒也能做快切；
// 同时仍受“单段 4-12 秒”硬约束限制，避免推荐出物理上放不下的镜头数。
func shotCountRange(cfg VideoRunConfig) (int, int) {
	duration := cfg.DurationS
	var low, high int
	switch {
	case duration <= 15:
		low, high = 2, 4
	case duration <= 30:
		low, high = 3, 6
	case duration <= 45:
		low, high = 4, 9
	case duration <= 60:
		low, high = 6, 12
	default:
		low, high = 8, 16
	}
	if maxByDuration := int(math.Floor(float64(duration) / 4)); maxByDuration > 0 && maxByDuration < high {
		high = maxByDuration
	}
	if minByDuration := int(math.Ceil(float64(duration) / 12)); minByDuration > low {
		low = minByDuration
	}
	if low < 2 {
		low = 2
	}
	if high < low {
		high = low
	}
	return low, high
}

func shotCountHint(cfg VideoRunConfig) string {
	low, high := shotCountRange(cfg)
	pacing := "生活情景/剧情 3-5 秒一镜"
	if cfg.DurationS >= 40 {
		pacing = "生活情景/剧情 4-6 秒一镜；60 秒优先 8-12 镜做快切"
	}
	return fmt.Sprintf("推荐 %d-%d 个镜头（总时长 %d 秒；%s；单段仍须 4-12 秒）", low, high, cfg.DurationS, pacing)
}

func shotCountWarnings(shots []VideoShot, cfg VideoRunConfig) []string {
	low, high := shotCountRange(cfg)
	if len(shots) < low {
		return []string{fmt.Sprintf("分镜只有 %d 个镜头，低于 %d 秒视频推荐的 %d-%d 镜；节奏可能偏慢，可在关卡①里拆分", len(shots), cfg.DurationS, low, high)}
	}
	if len(shots) > high {
		return []string{fmt.Sprintf("分镜有 %d 个镜头，高于 %d 秒视频推荐的 %d-%d 镜；单段可能过短，注意每段仍要 4 秒以上", len(shots), cfg.DurationS, low, high)}
	}
	return nil
}

// castRecommendation 按垂类给出角色配置建议；仍由企划模型决定最终人数。
func castRecommendation(cfg VideoRunConfig) string {
	vertical := strings.TrimSpace(cfg.Vertical)
	switch {
	case strings.Contains(vertical, "口播"), strings.Contains(vertical, "知识"),
		strings.Contains(vertical, "游戏"), strings.Contains(vertical, "数码"):
		return "口播/知识/游戏/数码：1 个主讲角色即可；也可以 0 角色、全旁白"
	case strings.Contains(vertical, "生活"), strings.Contains(vertical, "剧情"),
		strings.Contains(vertical, "情感"):
		return "生活情景/剧情：2-3 个角色（主角 + 对手/家人 + 旁白），最多 3 个开口角色"
	case strings.Contains(vertical, "美食"), strings.Contains(vertical, "母婴"):
		return "美食/母婴：1-2 个角色（主角 + 家人/店主），产品不是角色"
	default:
		return "通用：1-3 个角色；用户已有角色时最多补 1 个功能角色"
	}
}

// treatmentSystemPrompt 第一阶段：只选角、定故事骨架，不写镜头。
func treatmentSystemPrompt(cfg VideoRunConfig) string {
	return `【角色】你是中文短视频总编剧兼选角导演。你只输出「故事企划」，不写分镜、不写最终台词逐字稿（锁定剧本模式除外）。你的输出会被程序直接解析，不要 Markdown、不要解释。

【目标】先让故事成立，再交给分镜师。观众看完要能说出：谁想要什么、遇到什么阻碍、最后发生了什么变化。

【输入判断】
1. 用户给的是主题：按主题原创故事，先定立意与节拍。
2. 用户给的是完整剧本：原样提取角色、场景与台词，不重写、不压缩、不换说话人。
3. 用户已有角色：以用户角色为准，最多补 1 个功能性临时角色；不要另造主角。
4. 用户没有角色：按题材推荐 1-4 个临时角色，并给每个角色写清动物种类、外观锚点与声音风格。
   本任务推荐：` + castRecommendation(cfg) + `。

【叙事骨架】
- premise：一句话立意 = 主角 + 想要什么 + 阻碍 + 反转/代价 + 观众获得什么。
- beats：3-6 个节拍，必须按顺序覆盖 hook（钩子）→ setup（铺垫）→ turn（转折）→ payoff（兑现）；短时长可合并 setup/turn，但钩子和兑现不能省。
- 每个 beat 必须写 newInfo（这一拍给观众的新信息）与 link（与上一拍的因果：因为/但是/所以）。
- 禁止同义重复、空镜堆砌、没有变化的过场。

【角色与声音】
- 无任何图片输入时，所有角色必须是动物/拟人动物，不得出现真人、人脸、人体；这是硬约束。
- 每个角色必须有 voiceHint（声音风格提示词）：音色、语速、语气、情绪变化至少写到两项，例如“温和偏低的女声，语速偏慢，先犹豫后坚定”。
- 角色关系、目标、口吻要能驱动台词；不要只写外观。
- 有用户角色时，沿用其 name / appearance / persona / voiceHint，不得改写。

【输出结构】
{"cast":[{"roleId":"char_1","name":"角色名","species":"拟人化橘猫","appearance":"外观锚点","persona":"性格与口吻","voiceHint":"声音风格提示词"}],"premise":"一句话立意","beats":[{"id":"b1","function":"hook","summary":"发生了什么","newInfo":"新信息","emotion":"情绪变化","location":"地点","characters":["char_1"],"props":["关键道具"],"link":"与上一拍的关系"}],"continuity":["时间/地点/道具/服装连续性约定"]}
- roleId 用 char_1、char_2… 顺序编号；如果用户角色表已有 roleId，必须沿用用户的 roleId；characters 只能引用 cast 里的 roleId；旁白写 "narration"。
- 只输出企划 JSON；不要输出 shots、durationS、camera、mood。

【硬性约束】
- ` + subjectDirective(cfg) + `；
- 用户参数（时长、画幅、语言、语速、平台、垂类、风格/运镜/情绪、额外要求、负向约束）都是权威输入；
- 额外要求必须落到具体节拍或台词意图里；负向约束必须全片不出现。

【自检】
① 故事有钩子、转折、兑现？② 每一拍都有新信息？③ 角色都有声音风格？④ 无图时全是动物？⑤ 没有新增用户没给的主角？`
}

// storyboardSystemPrompt 第二阶段：只把企划可视化，不重新编故事。
func storyboardSystemPrompt(cfg VideoRunConfig, treatment *VideoTreatment) string {
	firstFrameRule := "第一段自由构图（本任务没有首帧图）"
	if hasUserFirstFrame(cfg) {
		firstFrameRule = "第一段从首帧的主体身份（脸型、毛色、服装）出发，在开头 0.5–1 秒内自然过渡到分镜描述的新场景，首帧只是起点，不锁死它的背景与机位"
	}
	lockedRule := ""
	if treatment != nil && treatment.Mode == VideoScriptModeLocked {
		lockedRule = `
【锁定台词】本任务台词已锁定：dialogueLines 必须逐字复用企划里的 lockedLines，顺序、说话人、标点都不得改写；超预算只报告，不删改。`
	}
	return `【角色】你是短视频分镜师。企划已经定稿：你只决定每个节拍怎么拍，不新增角色、不新增情节、不新增反转、不改写锁定台词。你的输出会被程序直接解析，不要 Markdown、不要解释。

【输出结构】
{"logline":"一句话主题","shots":[{"id":"s1","durationS":5,"beatId":"b1","function":"hook","newInfo":"本镜新信息","roles":["char_1"],"sceneIntent":"画面意图","dialogueLines":[{"speaker":"char_1","text":"台词"}],"reaction":"反应镜头","transition":"转场","camera":"运镜","mood":"情绪","audioCue":"环境音"}]}
- id 从 s1 起顺序编号；每镜 durationS 为数字秒；所有 durationS 之和必须等于目标总时长。
- beatId 必须逐字引用企划 beats 的 id；一镜只服务一个 beat，一个 beat 可以拆成 1-2 镜。
- roles 只能引用企划 cast 的 roleId；角色只要入镜就必须写进 roles。
- 示例里的 char_1 / auto_1 只是占位：实际必须逐字使用输入 cast 里的 roleId，不要自创 ID。
- dialogueLines 按开口顺序排列；每行 speaker 必须是 cast 的 roleId 或 "narration"；无台词给空数组。
- 不允许输出企划 cast/beats 以外的角色、地点、道具或剧情。

【镜头节奏】
- ` + shotCountHint(cfg) + `；
- 单段 4-12 秒；镜头越短，一段里能发生的动作越少，别在 4 秒里塞完整故事；
- 第一段承担钩子：开场直接抛冲突、悬念或反常识结论，不要寒暄、自我介绍、空镜铺垫；
- 相邻两段至少在景别、机位或动作方向上有一项不同；每镜必须带来新信息或新反应。

【台词绑定（硬约束）】
- 每行 dialogueLines.speaker 必须显式填写，不能空着；角色台词填 cast 的 roleId，画外解说填 "narration"。
- 角色说话时，该 roleId 必须在本段 roles 里；如果不在，系统会直接判为不合格。
- 不允许写“他/她/大家/老板”这类模糊归属，也不允许自创 cast 里没有的角色名。
- 旁白必须显式写 "narration"，不能留空；旁白段画面内不安排主体对口型。
- 每行台词 ≤ 20 字；一段最多两行（多人对话/一问一答也控制在两行内）；总字数 ≤（段时长 - 0.3 秒）× 语速。
- 台词要像人在说话：有态度、有潜台词、有回应；不要说明书式旁白，不要成语堆叠、括号说明、表情符号。
- 每个角色的台词必须贴合其 voiceHint（音色/语速/语气），不要所有角色一个腔调。

【画面规则】
- sceneIntent 只写看得见的东西：具体主体 + 可度量的动作 + 环境与光线；不写字幕、文字、标题、logo。
- camera / mood / audioCue 各用 2-8 字具体描述；不要“电影感”“高级”这类空词。
- 一段只允许一个连续动作；写反应镜头；保持时间、地点、道具、服装、屏幕方向连续。
- ` + firstFrameRule + `；
- 画面主体约束：` + subjectDirective(cfg) + `。` + lockedRule + `

【自检】
① 每镜都绑定 beatId，且没有新增 cast/情节？② 每行 speaker 都是 roleId 或 narration？③ 角色说话都在画面里？④ 镜头数与时长在范围内？⑤ 台词不超预算且贴合 voiceHint？`
}

/* ------------------------------------------------------------------
   两阶段调用
   ------------------------------------------------------------------ */

func (a *App) generateScriptTwoStage(ctx context.Context, provider Provider, cfg VideoRunConfig, tpl *VideoTemplateRun, feedback string) (VideoScript, []string, error) {
	warnings := make([]string, 0, 3)
	if cfg.ScriptLocked || looksLikeLockedScript(cfg.Topic) {
		if !cfg.ScriptLocked {
			warnings = append(warnings, "检测到完整剧本，已自动进入锁定台词模式：只做分镜，不改写台词")
		}
		script, lockWarnings, err := a.generateLockedScript(ctx, provider, cfg, tpl, feedback)
		return script, append(warnings, lockWarnings...), err
	}

	treatment, legacy, treatWarnings, err := a.llmTreatment(ctx, provider, cfg, tpl, feedback)
	warnings = append(warnings, treatWarnings...)
	if err != nil {
		return VideoScript{}, warnings, err
	}
	if legacy != nil {
		// 兼容只返回 shots 的旧模型/旧提示词：不强行要求两段，但不能假装它是新企划。
		normalized := normalizeScript(*legacy, cfg, tpl)
		bound, speakerWarnings := bindSpeakers(normalized, cfg)
		warnings = append(warnings, "模型返回单阶段分镜，已按兼容模式处理")
		warnings = append(warnings, speakerWarnings...)
		return bound, warnings, nil
	}
	if len(treatment.Beats) == 0 {
		return VideoScript{}, warnings, errors.New("企划没有返回任何节拍")
	}

	cfgWithCast, castWarnings := applyTreatmentCast(cfg, treatment)
	warnings = append(warnings, castWarnings...)
	script, err := a.llmStoryboard(ctx, provider, cfgWithCast, *treatment, tpl, feedback)
	if err != nil {
		return VideoScript{}, warnings, err
	}
	script = normalizeScript(script, cfgWithCast, tpl)
	script.Treatment = treatment
	script.Mode = treatment.Mode
	warnings = append(warnings, shotCountWarnings(script.Shots, cfg)...)
	bound, bindWarnings, err := a.validateAndBindScript(ctx, provider, cfgWithCast, script, *treatment, false)
	warnings = append(warnings, bindWarnings...)
	if err != nil {
		return VideoScript{}, warnings, err
	}
	return bound, warnings, nil
}

// llmTreatment 第一阶段：返回企划；若模型直接返回 shots，则作为旧协议兼容结果返回。
func (a *App) llmTreatment(ctx context.Context, provider Provider, cfg VideoRunConfig, tpl *VideoTemplateRun, feedback string) (*VideoTreatment, *VideoScript, []string, error) {
	userPrompt := buildTreatmentUserPrompt(cfg, tpl)
	userPrompt += feedbackDirective(feedback)
	raw, warnings, _, err := a.chatCompletionTraced(ctx, provider, treatmentSystemPrompt(cfg), userPrompt, firstFrameImages(cfg))
	if err != nil {
		return nil, nil, warnings, err
	}
	var payload struct {
		Cast       []VideoCastMember `json:"cast"`
		Premise    string            `json:"premise"`
		Beats      []VideoBeat       `json:"beats"`
		Continuity []string          `json:"continuity"`
		Logline    string            `json:"logline"`
		Language   string            `json:"language"`
		Shots      []VideoShot       `json:"shots"`
	}
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return nil, nil, warnings, fmt.Errorf("解析企划 JSON 失败: %w", err)
	}
	if len(payload.Shots) > 0 {
		return nil, &VideoScript{Logline: payload.Logline, Language: payload.Language, Source: "llm", Shots: payload.Shots}, warnings, nil
	}
	if len(payload.Beats) == 0 {
		return nil, nil, warnings, errors.New("企划没有返回 beats")
	}
	mergedCast, castWarnings := mergeTreatmentCast(cfg, payload.Cast)
	warnings = append(warnings, castWarnings...)
	beats := normalizeTreatmentBeats(payload.Beats)
	treatment := &VideoTreatment{
		Mode:       VideoModeOriginal,
		Premise:    strings.TrimSpace(payload.Premise),
		Cast:       mergedCast,
		Beats:      beats,
		Continuity: normalizeStringList(payload.Continuity),
		Model:      provider.Model,
		Revision:   1,
	}
	if treatment.Premise == "" {
		treatment.Premise = firstNonEmpty(cfg.Topic, "未命名故事")
	}
	return treatment, nil, warnings, nil
}

// llmStoryboard 第二阶段：只把企划可视化。
func (a *App) llmStoryboard(ctx context.Context, provider Provider, cfg VideoRunConfig, treatment VideoTreatment, tpl *VideoTemplateRun, feedback string) (VideoScript, error) {
	userPrompt := buildStoryboardUserPrompt(cfg, treatment, tpl)
	userPrompt += feedbackDirective(feedback)
	raw, _, _, err := a.chatCompletionTraced(ctx, provider, storyboardSystemPrompt(cfg, &treatment), userPrompt, firstFrameImages(cfg))
	if err != nil {
		return VideoScript{}, err
	}
	var payload struct {
		Logline  string      `json:"logline"`
		Language string      `json:"language"`
		Shots    []VideoShot `json:"shots"`
	}
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return VideoScript{}, fmt.Errorf("解析分镜 JSON 失败: %w", err)
	}
	if len(payload.Shots) == 0 {
		return VideoScript{}, errors.New("分镜阶段没有返回 shots")
	}
	// 缺 beatId 时按顺序兜底，避免整份分镜因为一个字段被废掉；真正的剧情漂移仍由
	// “不新增角色/节拍”的文本校验兜住。
	for index := range payload.Shots {
		if strings.TrimSpace(payload.Shots[index].BeatID) == "" && len(treatment.Beats) > 0 {
			payload.Shots[index].BeatID = treatment.Beats[index%len(treatment.Beats)].ID
		}
	}
	return VideoScript{
		Logline:   firstNonEmpty(payload.Logline, treatment.Premise),
		Language:  payload.Language,
		Source:    "llm",
		Mode:      treatment.Mode,
		Treatment: &treatment,
		Shots:     payload.Shots,
	}, nil
}

func buildTreatmentUserPrompt(cfg VideoRunConfig, tpl *VideoTemplateRun) string {
	lines := userParams(cfg)
	lines = append(lines, "用户角色表：\n"+characterTable(cfg, tpl))
	lines = append(lines, "本任务建议："+shotCountHint(cfg))
	if hints := templateScriptHints(tpl, cfg); hints != "" {
		lines = append(lines, hints)
	}
	lines = append(lines,
		"请先判断输入是主题还是完整剧本；只输出企划 JSON（cast/premise/beats/continuity），不要输出 shots。",
		"每个角色都必须写 voiceHint（音色、语速、语气、情绪变化）；无图任务的角色只能是动物/拟人动物。",
	)
	return strings.Join(lines, "\n")
}

func buildStoryboardUserPrompt(cfg VideoRunConfig, treatment VideoTreatment, tpl *VideoTemplateRun) string {
	lines := userParams(cfg)
	lines = append(lines, "角色表（含声音风格）：\n"+characterTable(cfg, tpl))
	lines = append(lines, "本任务镜头建议："+shotCountHint(cfg))
	treatmentJSON, _ := json.MarshalIndent(treatment, "", "  ")
	lines = append(lines, "故事企划（只能可视化，不得新增角色/情节/反转）：\n"+string(treatmentJSON))
	if len(treatment.LockedLines) > 0 {
		lockedJSON, _ := json.MarshalIndent(treatment.LockedLines, "", "  ")
		lines = append(lines, "锁定台词（必须逐字复用，顺序与 speaker 不得改变）：\n"+string(lockedJSON))
	}
	lines = append(lines,
		"请输出分镜 JSON：每镜绑定 beatId，dialogueLines 每行 speaker 必须是 cast roleId 或 \"narration\"；角色说话必须在本镜 roles 里。",
	)
	return strings.Join(lines, "\n")
}

/* ------------------------------------------------------------------
   角色：用户角色优先，无角色时自动创建动物临时角色
   ------------------------------------------------------------------ */

func mergeTreatmentCast(cfg VideoRunConfig, incoming []VideoCastMember) ([]VideoCastMember, []string) {
	warnings := make([]string, 0, 2)
	merged := make([]VideoCastMember, 0, len(cfg.CharacterRefs)+len(incoming))
	seenRole := map[string]bool{}
	seenName := map[string]bool{}

	add := func(member VideoCastMember) bool {
		roleID := strings.TrimSpace(member.RoleID)
		name := strings.TrimSpace(member.Name)
		if roleID == "" || name == "" || seenRole[roleID] || seenName[name] {
			return false
		}
		member.RoleID = roleID
		member.Name = name
		member.Species = strings.TrimSpace(member.Species)
		member.Appearance = strings.TrimSpace(member.Appearance)
		member.Persona = strings.TrimSpace(member.Persona)
		member.VoiceHint = strings.TrimSpace(member.VoiceHint)
		if member.VoiceHint == "" {
			member.VoiceHint = defaultVoiceHint(member)
			warnings = append(warnings, fmt.Sprintf("角色「%s」缺少声音风格，已补默认值：%s", name, member.VoiceHint))
		}
		seenRole[roleID] = true
		seenName[name] = true
		merged = append(merged, member)
		return true
	}

	userCount := 0
	autoCount := 0
	for _, ref := range cfg.CharacterRefs {
		if strings.TrimSpace(ref.RoleID) == "" {
			continue
		}
		auto := ref.AutoCreated
		if add(VideoCastMember{
			RoleID:      ref.RoleID,
			Name:        firstNonEmpty(ref.Name, ref.RoleID),
			Species:     ref.Species,
			Appearance:  ref.AppearanceBlock,
			Persona:     ref.Persona,
			VoiceHint:   ref.VoiceHint,
			AutoCreated: auto,
			Ephemeral:   auto,
		}) {
			if auto {
				autoCount++
			} else {
				userCount++
			}
		}
	}
	autoLimit := 4
	if userCount > 0 {
		autoLimit = 1
	}
	for _, member := range incoming {
		if userCount == 0 && len(merged) >= 4 {
			break
		}
		if userCount > 0 && autoCount >= autoLimit {
			break
		}
		auto := member
		auto.AutoCreated = true
		auto.Ephemeral = true
		if auto.Species == "" {
			auto.Species = pickAnimalSpecies(auto.Name + cfg.Vertical)
		}
		if !hasImageInput(cfg) && looksHumanSpecies(auto.Species) {
			warnings = append(warnings, fmt.Sprintf("角色「%s」被企划写成人类，已按约束改为拟人动物", auto.Name))
			auto.Species = pickAnimalSpecies(auto.Name + cfg.Vertical)
		}
		if auto.Appearance == "" {
			auto.Appearance = "拟人化" + auto.Species + "，" + auto.Name + "，面部与毛色特征稳定，穿着符合场景"
		}
		if add(auto) {
			autoCount++
		}
	}
	return merged, warnings
}

func applyTreatmentCast(cfg VideoRunConfig, treatment *VideoTreatment) (VideoRunConfig, []string) {
	merged, warnings := mergeTreatmentCast(cfg, treatment.Cast)
	treatment.Cast = merged
	refs := make([]VideoCharacterRef, 0, len(merged))
	for _, member := range merged {
		ref := VideoCharacterRef{
			RoleID:          member.RoleID,
			Name:            member.Name,
			Species:         member.Species,
			AutoCreated:     member.AutoCreated,
			AppearanceBlock: member.Appearance,
			Persona:         member.Persona,
			VoiceHint:       member.VoiceHint,
			Images:          []string{},
			ImageSource:     "auto",
		}
		for _, userRef := range cfg.CharacterRefs {
			if userRef.RoleID == member.RoleID {
				ref.Images = append([]string{}, userRef.Images...)
				ref.ImageSource = userRef.ImageSource
				ref.CharacterID = userRef.CharacterID
				ref.NegativePrompt = userRef.NegativePrompt
				ref.Perspective = userRef.Perspective
				ref.VoiceProfile = userRef.VoiceProfile
				ref.AutoCreated = userRef.AutoCreated
				if ref.VoiceHint == "" {
					ref.VoiceHint = userRef.VoiceHint
				}
				break
			}
		}
		if ref.VoiceHint == "" {
			ref.VoiceHint = defaultVoiceHint(member)
		}
		refs = append(refs, ref)
	}
	cfg.CharacterRefs = refs
	return cfg, warnings
}

func defaultVoiceHint(member VideoCastMember) string {
	label := firstNonEmpty(strings.TrimSpace(member.Species), strings.TrimSpace(member.Name), "角色")
	if persona := strings.TrimSpace(member.Persona); persona != "" {
		return label + "的声音风格：" + persona + "；中速口语，情绪随台词变化"
	}
	return label + "的声音风格：自然口语，中速，先克制后释放，情绪随台词变化"
}

func pickAnimalSpecies(seed string) string {
	pool := []string{"橘猫", "柴犬", "柯基", "小猪", "布偶猫", "金毛", "垂耳兔", "小羊"}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(seed))
	return pool[int(hasher.Sum32())%len(pool)]
}

func looksHumanSpecies(species string) bool {
	key := strings.ToLower(strings.TrimSpace(species))
	for _, token := range []string{"人", "human", "真人", "男", "女"} {
		if strings.Contains(key, token) {
			return true
		}
	}
	return false
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func normalizeTreatmentBeats(beats []VideoBeat) []VideoBeat {
	out := make([]VideoBeat, 0, len(beats))
	for index, beat := range beats {
		beat.ID = firstNonEmpty(strings.TrimSpace(beat.ID), fmt.Sprintf("b%d", index+1))
		beat.Function = firstNonEmpty(strings.TrimSpace(beat.Function), beatFunctionForIndex(index, len(beats)))
		beat.Summary = strings.TrimSpace(beat.Summary)
		beat.NewInfo = firstNonEmpty(strings.TrimSpace(beat.NewInfo), beat.Summary)
		beat.Emotion = strings.TrimSpace(beat.Emotion)
		beat.Location = strings.TrimSpace(beat.Location)
		beat.Characters = normalizeStringList(beat.Characters)
		beat.Props = normalizeStringList(beat.Props)
		beat.Link = strings.TrimSpace(beat.Link)
		out = append(out, beat)
	}
	return out
}

func beatFunctionForIndex(index, total int) string {
	switch {
	case index == 0:
		return "hook"
	case index == total-1:
		return "payoff"
	case index == total-2:
		return "turn"
	default:
		return "setup"
	}
}

func syncTreatmentCastToConfig(cfg *VideoRunConfig, script *VideoScript) {
	if cfg == nil || script == nil || script.Treatment == nil || len(script.Treatment.Cast) == 0 {
		return
	}
	updated, _ := applyTreatmentCast(*cfg, script.Treatment)
	*cfg = updated
}

/* ------------------------------------------------------------------
   严格台词绑定：空归属/未知角色直接报错，不再静默降级旁白
   ------------------------------------------------------------------ */

func validateScriptBindings(script *VideoScript, cfg VideoRunConfig) []string {
	issues := make([]string, 0, 4)
	for shotIndex := range script.Shots {
		shot := &script.Shots[shotIndex]
		roleSet := make(map[string]bool, len(shot.Roles))
		roles := make([]string, 0, len(shot.Roles))
		for _, raw := range shot.Roles {
			roleID, ok := matchSpeakerRole(cfg, raw)
			if !ok {
				issues = append(issues, fmt.Sprintf("%s 的出场角色「%s」不在角色表里", shot.ID, raw))
				continue
			}
			if !roleSet[roleID] {
				roleSet[roleID] = true
				roles = append(roles, roleID)
			}
		}
		shot.Roles = roles
		lines := normalizeDialogueLines(shot.DialogueLines, shot.Dialogue, shot.Speaker)
		for lineIndex := range lines {
			raw := strings.TrimSpace(lines[lineIndex].Speaker)
			switch {
			case raw == "":
				issues = append(issues, fmt.Sprintf("%s 第 %d 句台词没有 speaker（必须显式写 roleId 或 narration）", shot.ID, lineIndex+1))
			case isNarrationSpeaker(raw):
				lines[lineIndex].Speaker = VideoSpeakerNarration
			default:
				roleID, ok := matchSpeakerRole(cfg, raw)
				if !ok {
					issues = append(issues, fmt.Sprintf("%s 第 %d 句台词的 speaker「%s」不在角色表里", shot.ID, lineIndex+1, raw))
					continue
				}
				lines[lineIndex].Speaker = roleID
				if !roleSet[roleID] {
					roleSet[roleID] = true
					shot.Roles = append(shot.Roles, roleID)
				}
			}
		}
		shot.DialogueLines = lines
		shot.Dialogue = dialogueText(lines)
		shot.Speaker = dialogueSpeakerSummary(lines)
	}
	return issues
}

func (a *App) validateAndBindScript(ctx context.Context, provider Provider, cfg VideoRunConfig, script VideoScript, treatment VideoTreatment, locked bool) (VideoScript, []string, error) {
	issues := validateScriptBindings(&script, cfg)
	if locked {
		issues = append(issues, validateLockedLines(script, treatment.LockedLines)...)
		issues = append(issues, validateLockedBudget(script, cfg)...)
	}
	if len(issues) > 0 {
		repaired, repairErr := a.repairScriptBindings(ctx, provider, cfg, script, treatment, issues, locked)
		if repairErr == nil {
			script = normalizeScript(repaired, cfg)
			issues = validateScriptBindings(&script, cfg)
			if locked {
				issues = append(issues, validateLockedLines(script, treatment.LockedLines)...)
				issues = append(issues, validateLockedBudget(script, cfg)...)
			}
		}
	}
	if len(issues) > 0 {
		return script, nil, fmt.Errorf("%w（已自动修复 1 次）：%s", errScriptBinding, strings.Join(issues, "；"))
	}
	bound, warnings := bindSpeakers(script, cfg)
	return bound, warnings, nil
}

func (a *App) repairScriptBindings(ctx context.Context, provider Provider, cfg VideoRunConfig, script VideoScript, treatment VideoTreatment, issues []string, locked bool) (VideoScript, error) {
	shotsJSON, _ := json.MarshalIndent(script.Shots, "", "  ")
	lockedJSON, _ := json.MarshalIndent(treatment.LockedLines, "", "  ")
	userPrompt := "只修复下面分镜的台词绑定问题，不改台词文本、不增删镜头、不新增角色；输出完整 JSON：{\"shots\":[...]}。\n" +
		"错误清单：\n- " + strings.Join(issues, "\n- ") + "\n\n分镜：\n" + string(shotsJSON)
	if locked {
		userPrompt += "\n\n锁定台词（必须逐字保留、顺序与 speaker 不变）：\n" + string(lockedJSON)
	}
	raw, _, _, err := a.chatCompletionTraced(ctx, provider, `你是 JSON 修复器。只修复 speaker / roles 绑定，台词文本与镜头数量保持不变。未知角色必须改成 cast 里对应的 roleId；不能把角色台词改成 narration 来绕过校验。只输出 JSON。`, userPrompt, nil)
	if err != nil {
		return VideoScript{}, err
	}
	var payload struct {
		Shots []VideoShot `json:"shots"`
	}
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return VideoScript{}, err
	}
	if len(payload.Shots) == 0 {
		return VideoScript{}, errors.New("修复结果没有 shots")
	}
	return VideoScript{Logline: script.Logline, Language: script.Language, Source: script.Source, Mode: script.Mode, Treatment: script.Treatment, Shots: payload.Shots}, nil
}

func validateLockedLines(script VideoScript, locked []VideoLockedLine) []string {
	if len(locked) == 0 {
		return nil
	}
	actual := make([]VideoDialogueLine, 0, len(locked))
	for _, shot := range script.Shots {
		actual = append(actual, shot.DialogueLines...)
	}
	if len(actual) != len(locked) {
		return []string{fmt.Sprintf("锁定台词共 %d 句，分镜输出 %d 句；锁定模式不允许增删台词", len(locked), len(actual))}
	}
	issues := make([]string, 0, 2)
	for index := range locked {
		if strings.TrimSpace(actual[index].Text) != strings.TrimSpace(locked[index].Text) {
			issues = append(issues, fmt.Sprintf("第 %d 句锁定台词被改写：原文「%s」，输出「%s」", index+1, locked[index].Text, actual[index].Text))
		}
		if strings.TrimSpace(actual[index].Speaker) != strings.TrimSpace(locked[index].Speaker) {
			issues = append(issues, fmt.Sprintf("第 %d 句锁定台词 speaker 被改：应为「%s」，实际「%s」", index+1, locked[index].Speaker, actual[index].Speaker))
		}
	}
	return issues
}

// validateLockedBudget 锁定台词不允许模型删改，只能把“放不下”明确报出来，
// 由用户决定是加时长、删句还是拆镜头；不能像普通模式那样靠模型压缩台词。
func validateLockedBudget(script VideoScript, cfg VideoRunConfig) []string {
	issues := make([]string, 0, 2)
	for _, shot := range script.Shots {
		text := dialogueText(shot.DialogueLines)
		units := speechUnitCount(text, cfg.Language)
		budget := speechUnitBudget(shot.DurationS, cfg.SpeechRate, cfg.Language)
		if units > budget {
			issues = append(issues, fmt.Sprintf("%s 锁定台词 %d 字/词，超出 %.1fs 的语速预算 %d；请加时长或删句，不能改写台词", shot.ID, units, shot.DurationS, budget))
		}
	}
	return issues
}

/* ------------------------------------------------------------------
   锁定剧本：本地解析台词原文，只让模型做分镜
   ------------------------------------------------------------------ */

type lockedScene struct {
	Header  string
	Actions []string
	Lines   []VideoLockedLine
}

type parsedLockedScript struct {
	Scenes []lockedScene
	Lines  []VideoLockedLine
	Cast   []VideoCastMember
}

var (
	sceneHeaderPattern = regexp.MustCompile(`^(【[^】]*】|镜头\s*\d+|第\s*\d+\s*[场镜])`)
	speakerLinePattern = regexp.MustCompile(`^([^：:，。！？；、（）()]{1,16})(（[^）]*）)?[：:]\s*(.*)$`)
)

func looksLikeLockedScript(topic string) bool {
	parsed, err := parseLockedScript(topic)
	if err != nil || len(parsed.Lines) < 2 {
		return false
	}
	if sceneHeaderPattern.MatchString(topic) {
		return true
	}
	names := map[string]bool{}
	for _, line := range parsed.Lines {
		if line.Speaker != VideoSpeakerNarration {
			names[line.Speaker] = true
		}
	}
	if len(parsed.Lines) >= 3 && len(names) >= 2 {
		for _, marker := range []string{"剧本", "镜头", "场景", "对白"} {
			if strings.Contains(topic, marker) {
				return true
			}
		}
	}
	return false
}

// parseLockedScript 支持常见的「【镜头1】/ 老婆：xxx / 老婆：\nxxx」格式。
// 解析成功后台词文本按源行原样保留；分镜阶段不允许改写。
func parseLockedScript(topic string) (parsedLockedScript, error) {
	cleaned := strings.ReplaceAll(strings.ReplaceAll(topic, "\r\n", "\n"), "\r", "\n")
	rawLines := strings.Split(cleaned, "\n")
	scenes := []lockedScene{{}}
	pendingSpeaker := ""
	for _, raw := range rawLines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if sceneHeaderPattern.MatchString(line) {
			if len(scenes[len(scenes)-1].Actions) > 0 || len(scenes[len(scenes)-1].Lines) > 0 || scenes[len(scenes)-1].Header != "" {
				scenes = append(scenes, lockedScene{})
			}
			scenes[len(scenes)-1].Header = line
			pendingSpeaker = ""
			continue
		}
		if match := speakerLinePattern.FindStringSubmatch(line); match != nil {
			name := strings.TrimSpace(match[1])
			rest := strings.TrimSpace(match[3])
			if name != "" && !strings.ContainsAny(name, "/\\") && !strings.HasPrefix(name, "http") {
				pendingSpeaker = name
				if rest != "" {
					addLockedLine(&scenes[len(scenes)-1], name, rest)
					pendingSpeaker = ""
				}
				continue
			}
		}
		if pendingSpeaker != "" {
			addLockedLine(&scenes[len(scenes)-1], pendingSpeaker, line)
			continue
		}
		scenes[len(scenes)-1].Actions = append(scenes[len(scenes)-1].Actions, line)
	}
	parsed := parsedLockedScript{}
	lineIndex := 0
	for sceneIndex := range scenes {
		scene := &scenes[sceneIndex]
		for index := range scene.Lines {
			lineIndex++
			scene.Lines[index].ID = fmt.Sprintf("L%d", lineIndex)
			scene.Lines[index].Scene = sceneIndex + 1
		}
		parsed.Lines = append(parsed.Lines, scene.Lines...)
		if len(scene.Actions) > 0 || len(scene.Lines) > 0 {
			parsed.Scenes = append(parsed.Scenes, *scene)
		}
	}
	if len(parsed.Lines) == 0 {
		return parsedLockedScript{}, errors.New("没有识别到完整剧本对白")
	}
	parsed.Cast = buildLockedCast(parsed)
	return parsed, nil
}

func addLockedLine(scene *lockedScene, speaker, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	scene.Lines = append(scene.Lines, VideoLockedLine{Speaker: speaker, Text: text})
}

func buildLockedCast(parsed parsedLockedScript) []VideoCastMember {
	cast := make([]VideoCastMember, 0, 4)
	seen := map[string]string{}
	for _, line := range parsed.Lines {
		name := strings.TrimSpace(line.Speaker)
		if name == "" || isNarrationSpeaker(name) {
			line.Speaker = VideoSpeakerNarration
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		roleID := fmt.Sprintf("auto_%d", len(cast)+1)
		seen[name] = roleID
		species := pickAnimalSpecies(name)
		cast = append(cast, VideoCastMember{
			RoleID:      roleID,
			Name:        name,
			Species:     species,
			Appearance:  "拟人化" + species + "，" + name + "，面部与毛色特征稳定，穿着符合场景",
			Persona:     "按原剧本台词表现性格",
			VoiceHint:   "拟人化" + species + "的嗓音，自然口语，中速，情绪随台词变化",
			AutoCreated: true,
			Ephemeral:   true,
		})
	}
	// 回填每句台词的 roleId；旁白保持 narration。
	for index := range parsed.Lines {
		name := strings.TrimSpace(parsed.Lines[index].Speaker)
		if isNarrationSpeaker(name) {
			parsed.Lines[index].Speaker = VideoSpeakerNarration
			continue
		}
		if roleID, ok := seen[name]; ok {
			parsed.Lines[index].Speaker = roleID
		}
	}
	return cast
}

// mapLockedCastToUser 把锁定剧本里自动创建的 auto_N 角色按名字映射到用户已有角色，
// 避免同一角色在用户角色表与锁定 cast 里各有一份，导致台词 speaker 对不上。
func mapLockedCastToUser(parsed *parsedLockedScript, cfg VideoRunConfig) {
	byName := map[string]string{}
	for _, ref := range cfg.CharacterRefs {
		if strings.TrimSpace(ref.RoleID) == "" {
			continue
		}
		if name := normalizeRoleKey(ref.Name); name != "" {
			byName[name] = ref.RoleID
		}
		if role := normalizeRoleKey(ref.RoleID); role != "" {
			byName[role] = ref.RoleID
		}
	}
	for index := range parsed.Cast {
		member := &parsed.Cast[index]
		roleID, ok := byName[normalizeRoleKey(member.Name)]
		if !ok {
			continue
		}
		old := member.RoleID
		member.RoleID = roleID
		member.AutoCreated = false
		member.Ephemeral = false
		for lineIndex := range parsed.Lines {
			if parsed.Lines[lineIndex].Speaker == old {
				parsed.Lines[lineIndex].Speaker = roleID
			}
		}
	}
}

func (a *App) generateLockedScript(ctx context.Context, provider Provider, cfg VideoRunConfig, tpl *VideoTemplateRun, feedback string) (VideoScript, []string, error) {
	parsed, err := parseLockedScript(cfg.Topic)
	if err != nil {
		return VideoScript{}, nil, err
	}
	mapLockedCastToUser(&parsed, cfg)
	warnings := []string{"锁定剧本模式：台词原文锁定，只调用分镜阶段"}
	treatment := buildLockedTreatment(cfg, parsed)
	treatment.Model = provider.Model
	cfgWithCast, castWarnings := applyTreatmentCast(cfg, &treatment)
	warnings = append(warnings, castWarnings...)
	script, err := a.llmStoryboard(ctx, provider, cfgWithCast, treatment, tpl, feedback)
	if err != nil {
		return VideoScript{}, warnings, err
	}
	script = normalizeScript(script, cfgWithCast, tpl)
	script.Treatment = &treatment
	script.Mode = VideoScriptModeLocked
	script.Source = "llm_locked"
	warnings = append(warnings, shotCountWarnings(script.Shots, cfg)...)
	bound, bindWarnings, err := a.validateAndBindScript(ctx, provider, cfgWithCast, script, treatment, true)
	warnings = append(warnings, bindWarnings...)
	if err != nil {
		return VideoScript{}, warnings, err
	}
	return bound, warnings, nil
}

func buildLockedTreatment(cfg VideoRunConfig, parsed parsedLockedScript) VideoTreatment {
	roleBySpeaker := map[string]string{}
	for _, member := range parsed.Cast {
		roleBySpeaker[member.Name] = member.RoleID
		roleBySpeaker[member.RoleID] = member.RoleID
	}
	beats := make([]VideoBeat, 0, len(parsed.Scenes))
	for index, scene := range parsed.Scenes {
		characters := make([]string, 0, 2)
		for _, line := range scene.Lines {
			if line.Speaker == VideoSpeakerNarration {
				continue
			}
			if roleID, ok := roleBySpeaker[line.Speaker]; ok {
				characters = append(characters, roleID)
			}
		}
		summary := scene.Header
		if summary == "" && len(scene.Actions) > 0 {
			summary = scene.Actions[0]
		}
		if summary == "" && len(scene.Lines) > 0 {
			summary = scene.Lines[0].Text
		}
		newInfo := summary
		if len(scene.Actions) > 1 {
			newInfo = scene.Actions[len(scene.Actions)-1]
		}
		beats = append(beats, VideoBeat{
			ID:         fmt.Sprintf("b%d", index+1),
			Function:   beatFunctionForIndex(index, len(parsed.Scenes)),
			Summary:    summary,
			NewInfo:    newInfo,
			Characters: dedupeStrings(characters),
			Location:   scene.Header,
			Link:       "接着上一场",
		})
	}
	premise := firstNonEmpty(cfg.Topic, "锁定剧本")
	if len(parsed.Scenes) > 0 {
		if header := firstNonEmpty(parsed.Scenes[0].Header, firstAction(parsed.Scenes[0])); header != "" {
			premise = truncateRunes(header, 80)
		}
	}
	return VideoTreatment{
		Mode:        VideoScriptModeLocked,
		Premise:     premise,
		Cast:        parsed.Cast,
		Beats:       beats,
		LockedLines: parsed.Lines,
		Continuity:  []string{"锁定剧本：场景顺序、台词原文、说话人不得改写"},
		Model:       "local-parse",
		Revision:    1,
	}
}

func firstAction(scene lockedScene) string {
	if len(scene.Actions) > 0 {
		return scene.Actions[0]
	}
	return ""
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
