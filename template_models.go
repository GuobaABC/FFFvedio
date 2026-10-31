package main

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 模板功能（《视频生成工具·模板功能最佳实践》）。
// 模板是「默认值来源」而不是「硬约束」：它预填 RunConfig，三道确认门照常让人改，
// 运行时按「单次 RunConfig 覆盖 > 选中模板 > 系统内置默认」解析出一份生效配置，
// 并为每个字段标注来源，便于一眼看出哪些值是模板带进来的、哪些本次改过。

// 模板作用域（§6）：内置 / 团队 / 个人；团队模板走审核，内置模板不可改。
const (
	TemplateScopeBuiltin  = "builtin"
	TemplateScopeTeam     = "team"
	TemplateScopePersonal = "personal"
)

// 生效配置的字段来源（§1）。
const (
	TemplateSourceTemplate = "template"
	TemplateSourceOverride = "override"
	TemplateSourceDefault  = "default"
)

// 可来自模板的配置字段，键名与 VideoRunConfig 的 JSON 字段一致。
const (
	templateFieldDuration     = "durationS"
	templateFieldFps          = "fps"
	templateFieldAspect       = "aspectRatio"
	templateFieldContinuation = "continuationMode"
	templateFieldLanguage     = "language"
	templateFieldSpeechRate   = "speechRate"
	templateFieldStyle        = "videoParams.style"
	templateFieldCamera       = "videoParams.camera"
	templateFieldMood         = "videoParams.mood"
	templateFieldExtraPrompt  = "videoParams.extraPrompt"
	templateFieldFontName     = "subtitleStyle.fontName"
	templateFieldFontSize     = "subtitleStyle.fontSizePct"
	templateFieldPrimaryColor = "subtitleStyle.primaryColor"
	templateFieldOutlineColor = "subtitleStyle.outlineColor"
	templateFieldOutlineWidth = "subtitleStyle.outlineWidth"
	templateFieldAlignment    = "subtitleStyle.alignment"
	templateFieldMarginV      = "subtitleStyle.marginVpct"
	templateFieldBold         = "subtitleStyle.bold"
	templateFieldPlatforms    = "platforms"
	templateFieldRegion       = "region"
	templateFieldVertical     = "vertical"
	templateFieldCharacters   = "characterRefs"
)

// VideoTemplateStyle 风格：受控 token 词表 + 按模型分装的风格片段（§2）。
// tokens 键：lighting / grade / lens / camera / texture / mood；
// promptSnippets 键：模型的 prompt_style（kling / veo / runway / generic…）。
type VideoTemplateStyle struct {
	Tokens         map[string]string `json:"tokens"`
	PromptSnippets map[string]string `json:"promptSnippets"`
}

// VideoTemplateCharacter 人物插图：定妆图 + 外观描述块 + 声音提示，图文成对（§3）。
type VideoTemplateCharacter struct {
	RoleID          string   `json:"roleId"`
	Name            string   `json:"name"`
	Images          []string `json:"images"`
	AppearanceBlock string   `json:"appearanceBlock"`
	VoiceHint       string   `json:"voiceHint"`
	Perspective     string   `json:"perspective"`
}

// VideoTemplateOpening 默认开场白：1~3 个钩子变体 + 变量 + 建议语速（§4）。
type VideoTemplateOpening struct {
	Variants   []string `json:"variants"`
	Vars       []string `json:"vars"`
	SpeechRate float64  `json:"speechRate"`
}

// VideoTemplateSubtitleStyle 字幕样式（部分定义：未填字段沿层 fall through，§5）。
type VideoTemplateSubtitleStyle struct {
	FontName     string   `json:"fontName"`
	FontSizePct  *float64 `json:"fontSizePct"`
	PrimaryColor string   `json:"primaryColor"`
	OutlineColor string   `json:"outlineColor"`
	OutlineWidth *float64 `json:"outlineWidth"`
	Alignment    int      `json:"alignment"`
	MarginVPct   *float64 `json:"marginVpct"`
	Bold         *bool    `json:"bold"`
}

// VideoTemplatePublishDefaults 发布档：直接带出 Agent4 的平台 / 时区 / 垂类。
type VideoTemplatePublishDefaults struct {
	Platform  string   `json:"platform"`
	Platforms []string `json:"platforms"`
	Region    string   `json:"region"`
	Vertical  string   `json:"vertical"`
}

// VideoTemplateDefaults 流水线默认值；零值表示「本模板不定义该字段」。
type VideoTemplateDefaults struct {
	AspectRatio      string   `json:"aspectRatio"`
	DurationS        int      `json:"durationS"`
	Fps              int      `json:"fps"`
	SpeechRate       *float64 `json:"speechRate"`
	ContinuationMode string   `json:"continuationMode"`
	Language         string   `json:"language"`
}

// VideoTemplateAssets 品牌资产：logo / 水印（合成叠加）与 BGM（混音）。
type VideoTemplateAssets struct {
	Logo      string `json:"logo"`
	Watermark string `json:"watermark"`
	Bgm       string `json:"bgm"`
}

// VideoTemplate 模板实体。
type VideoTemplate struct {
	ID              string                       `json:"id"`
	Name            string                       `json:"name"`
	Version         string                       `json:"version"`
	Scope           string                       `json:"scope"`
	Description     string                       `json:"description"`
	Tags            []string                     `json:"tags"`
	Style           VideoTemplateStyle           `json:"style"`
	Characters      []VideoTemplateCharacter     `json:"characters"`
	Opening         VideoTemplateOpening         `json:"opening"`
	SubtitleStyle   VideoTemplateSubtitleStyle   `json:"subtitleStyle"`
	PublishDefaults VideoTemplatePublishDefaults `json:"publishDefaults"`
	Defaults        VideoTemplateDefaults        `json:"defaults"`
	Assets          VideoTemplateAssets          `json:"assets"`
	CreatedAt       string                       `json:"createdAt"`
	UpdatedAt       string                       `json:"updatedAt"`
}

// VideoTemplateRef 任务对模板的引用：锁定版本 + 记录本次显式改动 + 开场白变量。
type VideoTemplateRef struct {
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Overrides   map[string]string `json:"overrides"`
	OpeningVars map[string]string `json:"openingVars"`
}

// VideoTemplateRun 任务内锁定的模板快照：可复现、可审计（§6 版本化）。
type VideoTemplateRun struct {
	VideoTemplate
	Sources      map[string]string `json:"sources"`
	Overrides    []string          `json:"overrides"`
	OpeningLine  string            `json:"openingLine"`
	OpeningAll   []string          `json:"openingAll"`
	StyleSnippet string            `json:"styleSnippet"`
	Compliance   []string          `json:"compliance"`
	Note         string            `json:"note"`
}

// VideoTemplatePreview 选模板时的「预览与最小样」（§6）：不必盲选。
type VideoTemplatePreview struct {
	Opening      string            `json:"opening"`
	OpeningAll   []string          `json:"openingAll"`
	StyleSummary string            `json:"styleSummary"`
	StyleSnippet string            `json:"styleSnippet"`
	StyleTokens  map[string]string `json:"styleTokens"`
	Characters   []string          `json:"characters"`
	Subtitle     string            `json:"subtitle"`
	Publish      string            `json:"publish"`
	Defaults     string            `json:"defaults"`
	Assets       []string          `json:"assets"`
	Compliance   []string          `json:"compliance"`
	Warnings     []string          `json:"warnings"`
}

// templateTokenOrder 受控词表的稳定输出顺序（§2）。
var templateTokenOrder = []struct {
	Key   string
	Label string
}{
	{"lighting", "光线"},
	{"grade", "色调"},
	{"lens", "焦段"},
	{"camera", "运镜"},
	{"texture", "质感"},
	{"mood", "情绪"},
}

// templateFillerWords 无增益空词：命中只提示、不阻断，引导改成可度量描述。
var templateFillerWords = []string{"cinematic", "8k", "4k", "masterpiece", "高级感", "电影级", "超清", "极致"}

var templateVarPattern = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

func newVideoTemplateID() string {
	return fmt.Sprintf("tpl_%d_%s", time.Now().UnixNano(), randomHex(3))
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// normalizeVideoTemplate 入口归一化：模板可以「部分定义」，但填了的字段要合法。
func normalizeVideoTemplate(tpl VideoTemplate) (VideoTemplate, error) {
	tpl.Name = strings.TrimSpace(tpl.Name)
	if tpl.Name == "" {
		return tpl, errors.New("模板名称不能为空")
	}
	tpl.ID = strings.TrimSpace(tpl.ID)
	if tpl.ID == "" {
		tpl.ID = newVideoTemplateID()
	}
	tpl.Version = firstNonEmpty(strings.TrimSpace(tpl.Version), "1.0")
	tpl.Scope = strings.ToLower(strings.TrimSpace(tpl.Scope))
	switch tpl.Scope {
	case TemplateScopeBuiltin, TemplateScopeTeam, TemplateScopePersonal:
	default:
		tpl.Scope = TemplateScopePersonal
	}
	tpl.Description = strings.TrimSpace(tpl.Description)
	tpl.Tags = uniqueStrings(tpl.Tags)

	tokens := make(map[string]string, len(tpl.Style.Tokens))
	for key, value := range tpl.Style.Tokens {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		tokens[key] = value
	}
	tpl.Style.Tokens = tokens
	snippets := make(map[string]string, len(tpl.Style.PromptSnippets))
	for key, value := range tpl.Style.PromptSnippets {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		snippets[key] = value
	}
	tpl.Style.PromptSnippets = snippets

	characters := make([]VideoTemplateCharacter, 0, len(tpl.Characters))
	for index, character := range tpl.Characters {
		character.RoleID = strings.TrimSpace(character.RoleID)
		character.Name = strings.TrimSpace(character.Name)
		if character.RoleID == "" {
			character.RoleID = fmt.Sprintf("char_%d", index+1)
		}
		if character.Name == "" {
			character.Name = character.RoleID
		}
		character.Images = uniqueStrings(character.Images)
		character.AppearanceBlock = strings.TrimSpace(character.AppearanceBlock)
		character.VoiceHint = strings.TrimSpace(character.VoiceHint)
		character.Perspective = strings.TrimSpace(character.Perspective)
		if len(character.Images) == 0 && character.AppearanceBlock == "" {
			continue
		}
		characters = append(characters, character)
	}
	tpl.Characters = characters

	// 开场白只保留 1~3 个钩子变体（§4）
	variants := uniqueStrings(tpl.Opening.Variants)
	if len(variants) > 3 {
		variants = variants[:3]
	}
	tpl.Opening.Variants = variants
	tpl.Opening.Vars = normalizeOpeningVars(tpl.Opening.Vars, variants)
	tpl.Opening.SpeechRate = round2(clampFloat(tpl.Opening.SpeechRate, 0, 10))

	sub := tpl.SubtitleStyle
	sub.FontName = strings.TrimSpace(sub.FontName)
	sub.PrimaryColor = strings.TrimSpace(sub.PrimaryColor)
	sub.OutlineColor = strings.TrimSpace(sub.OutlineColor)
	if sub.FontSizePct != nil {
		value := round2(clampFloat(*sub.FontSizePct, 1.2, 12))
		sub.FontSizePct = &value
	}
	if sub.OutlineWidth != nil {
		value := round2(clampFloat(*sub.OutlineWidth, 0, 8))
		sub.OutlineWidth = &value
	}
	if sub.MarginVPct != nil {
		value := round2(clampFloat(*sub.MarginVPct, 0, 30))
		sub.MarginVPct = &value
	}
	if sub.Alignment < 1 || sub.Alignment > 9 {
		sub.Alignment = 0
	}
	tpl.SubtitleStyle = sub

	publish := tpl.PublishDefaults
	publish.Platform = strings.ToLower(strings.TrimSpace(publish.Platform))
	if len(publish.Platforms) == 0 && publish.Platform != "" {
		publish.Platforms = []string{publish.Platform}
	}
	publish.Platforms = normalizePlatforms(publish.Platforms)
	publish.Platform = ""
	publish.Region = strings.TrimSpace(publish.Region)
	publish.Vertical = strings.TrimSpace(publish.Vertical)
	tpl.PublishDefaults = publish

	defaults := tpl.Defaults
	switch defaults.AspectRatio {
	case "16:9", "9:16", "1:1":
	default:
		defaults.AspectRatio = ""
	}
	defaults.DurationS = clampInt(defaults.DurationS, 0, 600)
	defaults.Fps = clampInt(defaults.Fps, 0, 60)
	if defaults.SpeechRate != nil {
		value := round2(clampFloat(*defaults.SpeechRate, 1, 10))
		defaults.SpeechRate = &value
	}
	switch defaults.ContinuationMode {
	case "none", "last_frame", "overlap":
	default:
		defaults.ContinuationMode = ""
	}
	defaults.Language = strings.TrimSpace(defaults.Language)
	tpl.Defaults = defaults

	assets := tpl.Assets
	assets.Logo = strings.TrimSpace(assets.Logo)
	assets.Watermark = strings.TrimSpace(assets.Watermark)
	assets.Bgm = strings.TrimSpace(assets.Bgm)
	tpl.Assets = assets

	return tpl, nil
}

// normalizeOpeningVars 变量来自显式声明 + 开场白里的占位符，取并集。
func normalizeOpeningVars(declared, variants []string) []string {
	vars := append([]string{}, declared...)
	for _, variant := range variants {
		for _, match := range templateVarPattern.FindAllStringSubmatch(variant, -1) {
			vars = append(vars, match[1])
		}
	}
	vars = uniqueStrings(vars)
	if len(vars) > 8 {
		vars = vars[:8]
	}
	return vars
}

// templateStyleSummary 按受控词表顺序拼出风格摘要，未经声明的 token 排在其后。
func templateStyleSummary(tpl *VideoTemplate) string {
	if tpl == nil {
		return ""
	}
	used := map[string]bool{}
	parts := make([]string, 0, len(tpl.Style.Tokens))
	for _, item := range templateTokenOrder {
		used[item.Key] = true
		if value := strings.TrimSpace(tpl.Style.Tokens[item.Key]); value != "" {
			parts = append(parts, value)
		}
	}
	for _, key := range sortedMapKeys(tpl.Style.Tokens) {
		if used[key] {
			continue
		}
		if value := strings.TrimSpace(tpl.Style.Tokens[key]); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "；")
}

// templateStyleSnippet 按模型的 prompt_style 取风格片段，缺失时回退 generic / kling / 任意片段。
func templateStyleSnippet(tpl *VideoTemplate, promptStyle string) string {
	if tpl == nil {
		return ""
	}
	candidates := []string{strings.ToLower(strings.TrimSpace(promptStyle)), "generic", "kling"}
	for _, key := range candidates {
		if key == "" {
			continue
		}
		if value := strings.TrimSpace(tpl.Style.PromptSnippets[key]); value != "" {
			return value
		}
	}
	for _, key := range sortedMapKeys(tpl.Style.PromptSnippets) {
		if value := strings.TrimSpace(tpl.Style.PromptSnippets[key]); value != "" {
			return value
		}
	}
	return templateStyleSummary(tpl)
}

func templateCharacterByRole(tpl *VideoTemplate, roleID string) *VideoTemplateCharacter {
	if tpl == nil {
		return nil
	}
	roleID = strings.TrimSpace(roleID)
	if roleID == "" {
		return nil
	}
	for index := range tpl.Characters {
		if tpl.Characters[index].RoleID == roleID {
			return &tpl.Characters[index]
		}
	}
	return nil
}

// templateSpeechRate 取模板语速：开场白建议语速优先于流水线默认语速。
func templateSpeechRate(tpl *VideoTemplate) float64 {
	if tpl == nil {
		return 0
	}
	if tpl.Opening.SpeechRate > 0 {
		return tpl.Opening.SpeechRate
	}
	if tpl.Defaults.SpeechRate != nil {
		return *tpl.Defaults.SpeechRate
	}
	return 0
}

// applyVideoTemplate 分层覆盖：模板值写进配置，本次显式改动的字段保持用户值并标 override。
func applyVideoTemplate(tpl *VideoTemplate, cfg VideoRunConfig, overrides map[string]string) (VideoRunConfig, map[string]string, []string) {
	sources := make(map[string]string, 16)
	warnings := make([]string, 0, 2)
	if tpl == nil {
		return cfg, sources, warnings
	}

	isOverridden := func(key string) bool {
		value, ok := overrides[key]
		return ok && strings.TrimSpace(value) != ""
	}
	// apply 模板提供该字段时：本次改过 → override；否则用模板值 → template
	apply := func(key string, set func()) {
		if isOverridden(key) {
			sources[key] = TemplateSourceOverride
			return
		}
		sources[key] = TemplateSourceTemplate
		set()
	}

	defaults := tpl.Defaults
	if defaults.AspectRatio != "" {
		value := defaults.AspectRatio
		apply(templateFieldAspect, func() { cfg.AspectRatio = value })
	}
	if defaults.DurationS > 0 {
		value := defaults.DurationS
		apply(templateFieldDuration, func() { cfg.DurationS = value })
	}
	if defaults.Fps > 0 {
		value := defaults.Fps
		apply(templateFieldFps, func() { cfg.Fps = value })
	}
	if defaults.Language != "" {
		value := defaults.Language
		apply(templateFieldLanguage, func() { cfg.Language = value })
	}
	if defaults.ContinuationMode != "" {
		value := defaults.ContinuationMode
		apply(templateFieldContinuation, func() { cfg.ContinuationMode = value })
	}
	if rate := templateSpeechRate(tpl); rate > 0 {
		value := rate
		apply(templateFieldSpeechRate, func() { cfg.SpeechRate = value })
	}

	if summary := templateStyleSummary(tpl); summary != "" {
		value := summary
		apply(templateFieldStyle, func() { cfg.VideoParams.Style = value })
	}
	if camera := strings.TrimSpace(tpl.Style.Tokens["camera"]); camera != "" {
		value := camera
		apply(templateFieldCamera, func() { cfg.VideoParams.Camera = value })
	}
	if mood := strings.TrimSpace(tpl.Style.Tokens["mood"]); mood != "" {
		value := mood
		apply(templateFieldMood, func() { cfg.VideoParams.Mood = value })
	}

	sub := tpl.SubtitleStyle
	if sub.FontName != "" {
		value := sub.FontName
		apply(templateFieldFontName, func() { cfg.SubtitleStyle.FontName = value })
	}
	if sub.FontSizePct != nil {
		value := *sub.FontSizePct
		apply(templateFieldFontSize, func() { cfg.SubtitleStyle.FontSizePct = value })
	}
	if sub.PrimaryColor != "" {
		value := sub.PrimaryColor
		apply(templateFieldPrimaryColor, func() { cfg.SubtitleStyle.PrimaryColor = value })
	}
	if sub.OutlineColor != "" {
		value := sub.OutlineColor
		apply(templateFieldOutlineColor, func() { cfg.SubtitleStyle.OutlineColor = value })
	}
	if sub.OutlineWidth != nil {
		value := *sub.OutlineWidth
		apply(templateFieldOutlineWidth, func() { cfg.SubtitleStyle.OutlineWidth = value })
	}
	if sub.Alignment >= 1 && sub.Alignment <= 9 {
		value := sub.Alignment
		apply(templateFieldAlignment, func() { cfg.SubtitleStyle.Alignment = value })
	}
	if sub.MarginVPct != nil {
		value := *sub.MarginVPct
		apply(templateFieldMarginV, func() { cfg.SubtitleStyle.MarginVPct = value })
	}
	if sub.Bold != nil {
		value := *sub.Bold
		apply(templateFieldBold, func() { cfg.SubtitleStyle.Bold = value })
	}

	if len(tpl.PublishDefaults.Platforms) > 0 {
		value := append([]string{}, tpl.PublishDefaults.Platforms...)
		apply(templateFieldPlatforms, func() { cfg.Platforms = value })
	}
	if tpl.PublishDefaults.Region != "" {
		value := tpl.PublishDefaults.Region
		apply(templateFieldRegion, func() { cfg.Region = value })
	}
	if tpl.PublishDefaults.Vertical != "" {
		value := tpl.PublishDefaults.Vertical
		apply(templateFieldVertical, func() { cfg.Vertical = value })
	}

	if len(tpl.Characters) > 0 {
		value := templateCharacterRefs(tpl)
		apply(templateFieldCharacters, func() { cfg.CharacterRefs = value })
	}

	// 用户改过但模板没定义的字段同样标 override，避免被误读成默认值
	for key, value := range overrides {
		if _, ok := sources[key]; !ok && strings.TrimSpace(value) != "" {
			sources[key] = TemplateSourceOverride
		}
	}

	if len(tpl.Characters) > 0 && len(cfg.CharacterRefs) == 0 {
		warnings = append(warnings, "模板声明了角色，但当前配置没有可用角色参考（图片不可读或已被移除）")
	}
	return cfg, sources, warnings
}

// templateCharacterRefs 把模板角色转成 RunConfig 的角色参考（外观块留在模板快照里）。
func templateCharacterRefs(tpl *VideoTemplate) []VideoCharacterRef {
	refs := make([]VideoCharacterRef, 0, len(tpl.Characters))
	for _, character := range tpl.Characters {
		refs = append(refs, VideoCharacterRef{
			RoleID:    character.RoleID,
			Name:      character.Name,
			Images:    append([]string{}, character.Images...),
			VoiceHint: character.VoiceHint,
		})
	}
	return refs
}

// interpolateTemplateText 用本次变量插值占位符，并回收缺失的变量名。
func interpolateTemplateText(text string, values map[string]string) (string, []string) {
	missing := make([]string, 0, 2)
	result := templateVarPattern.ReplaceAllStringFunc(text, func(match string) string {
		key := match[1 : len(match)-1]
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
		missing = append(missing, key)
		return ""
	})
	result = strings.Join(strings.Fields(result), " ")
	return strings.TrimSpace(result), uniqueStrings(missing)
}

// templateOpeningLine 选开场钩子：按使用次数轮换（§4 多样性护栏），并记录缺失变量。
func templateOpeningLine(tpl *VideoTemplate, values map[string]string, usage int) (line string, variants []string, missing []string) {
	if tpl == nil {
		return "", nil, nil
	}
	missingSet := make([]string, 0, 2)
	for _, variant := range tpl.Opening.Variants {
		interpolated, missingList := interpolateTemplateText(variant, values)
		missingSet = append(missingSet, missingList...)
		if interpolated != "" {
			variants = append(variants, interpolated)
		}
	}
	if len(variants) > 0 {
		if usage < 0 {
			usage = 0
		}
		line = variants[usage%len(variants)]
	}
	return line, variants, uniqueStrings(missingSet)
}

// templateVarValues 组装插值变量：主题 / 垂类 / 平台 / 时长 + 本次手填的业务变量。
func templateVarValues(cfg VideoRunConfig, ref VideoTemplateRef) map[string]string {
	platform := ""
	if len(cfg.Platforms) > 0 {
		platform = platformProfile(cfg.Platforms[0]).Label
	}
	values := map[string]string{
		"topic":    cfg.Topic,
		"vertical": cfg.Vertical,
		"duration": strconv.Itoa(cfg.DurationS),
		"platform": platform,
	}
	for key, value := range ref.OpeningVars {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}
	return values
}

// checkVideoTemplateCompliance 模板层合规前置（§6）：违禁词阻断，其余提示。
func checkVideoTemplateCompliance(tpl *VideoTemplate) (issues []string, warnings []string) {
	if tpl == nil {
		return nil, nil
	}
	texts := append([]string{}, tpl.Opening.Variants...)
	for _, key := range sortedMapKeys(tpl.Style.PromptSnippets) {
		texts = append(texts, tpl.Style.PromptSnippets[key])
	}
	for _, character := range tpl.Characters {
		texts = append(texts, character.AppearanceBlock)
	}

	issues = uniqueStrings(complianceHits(texts))
	if len(issues) > 0 {
		for index, issue := range issues {
			issues[index] = "模板" + issue
		}
	}

	warnings = make([]string, 0, 3)
	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, word := range templateFillerWords {
			if strings.Contains(lower, word) {
				warnings = append(warnings, fmt.Sprintf("模板含无增益空词「%s」：建议换成可度量的光线 / 焦段 / 质感描述", strings.TrimSpace(word)))
			}
		}
	}
	for _, character := range tpl.Characters {
		if len(character.Images) == 0 && character.AppearanceBlock == "" {
			warnings = append(warnings, fmt.Sprintf("角色「%s」既无参考图也无外观描述，跨段一致性无法保证", character.Name))
		}
		if len(character.Images) > 0 && character.AppearanceBlock == "" {
			warnings = append(warnings, fmt.Sprintf("角色「%s」缺少外观描述块，不支持主体参考的模型无法降级复用", character.Name))
		}
		for _, image := range character.Images {
			if strings.HasPrefix(strings.ToLower(image), "http") {
				warnings = append(warnings, fmt.Sprintf("角色「%s」使用远程定妆图：需确认肖像授权与公网可访问性", character.Name))
			}
		}
	}
	if len(tpl.Style.Tokens) == 0 && len(tpl.Style.PromptSnippets) == 0 {
		warnings = append(warnings, "模板未定义风格 token 与风格片段：换模型后调性可能漂移")
	}
	if len(tpl.Opening.Variants) == 0 {
		warnings = append(warnings, "模板没有开场钩子：shot#1 台词将由 Agent1 自由发挥")
	}
	return issues, uniqueStrings(warnings)
}

// buildVideoTemplatePreview 生成「样例开场白 + 风格片段 + 字幕/发布档」预览文案。
func buildVideoTemplatePreview(tpl *VideoTemplate, values map[string]string) VideoTemplatePreview {
	preview := VideoTemplatePreview{StyleTokens: map[string]string{}, Compliance: []string{}, Warnings: []string{}}
	if tpl == nil {
		return preview
	}
	issues, warnings := checkVideoTemplateCompliance(tpl)
	preview.Compliance = append(preview.Compliance, issues...)
	preview.Warnings = append(preview.Warnings, warnings...)

	line, variants, missing := templateOpeningLine(tpl, values, 0)
	preview.Opening = line
	preview.OpeningAll = variants
	if len(missing) > 0 {
		preview.Warnings = append(preview.Warnings, "开场白变量未提供："+strings.Join(missing, "、")+"（可用「模板变量」补充）")
	}

	preview.StyleSummary = templateStyleSummary(tpl)
	preview.StyleSnippet = templateStyleSnippet(tpl, "generic")
	for key, value := range tpl.Style.Tokens {
		preview.StyleTokens[key] = value
	}

	for _, character := range tpl.Characters {
		detail := fmt.Sprintf("%s（%s）", character.Name, character.RoleID)
		if character.AppearanceBlock != "" {
			detail += "：" + character.AppearanceBlock
		}
		if len(character.Images) > 0 {
			detail += fmt.Sprintf("（%d 张定妆图）", len(character.Images))
		}
		if character.VoiceHint != "" {
			detail += "；声音：" + character.VoiceHint
		}
		preview.Characters = append(preview.Characters, detail)
	}

	sub := tpl.SubtitleStyle
	parts := make([]string, 0, 4)
	if sub.FontName != "" {
		parts = append(parts, sub.FontName)
	}
	if sub.FontSizePct != nil {
		parts = append(parts, fmt.Sprintf("%.1f%% 画面高", *sub.FontSizePct))
	}
	if sub.PrimaryColor != "" {
		parts = append(parts, sub.PrimaryColor)
	}
	if sub.OutlineColor != "" || sub.OutlineWidth != nil {
		width := 0.0
		if sub.OutlineWidth != nil {
			width = *sub.OutlineWidth
		}
		parts = append(parts, fmt.Sprintf("描边 %s/%.1f", firstNonEmpty(sub.OutlineColor, "默认"), width))
	}
	if sub.Alignment >= 1 && sub.Alignment <= 9 {
		parts = append(parts, subtitleAlignmentLabel(sub.Alignment))
	}
	preview.Subtitle = strings.Join(parts, " · ")

	publishParts := make([]string, 0, 3)
	for _, platform := range tpl.PublishDefaults.Platforms {
		publishParts = append(publishParts, platformProfile(platform).Label)
	}
	if tpl.PublishDefaults.Region != "" {
		publishParts = append(publishParts, tpl.PublishDefaults.Region)
	}
	if tpl.PublishDefaults.Vertical != "" {
		publishParts = append(publishParts, tpl.PublishDefaults.Vertical)
	}
	preview.Publish = strings.Join(publishParts, " · ")

	defaultParts := make([]string, 0, 4)
	if tpl.Defaults.AspectRatio != "" {
		defaultParts = append(defaultParts, tpl.Defaults.AspectRatio)
	}
	if tpl.Defaults.DurationS > 0 {
		defaultParts = append(defaultParts, fmt.Sprintf("%ds", tpl.Defaults.DurationS))
	}
	if tpl.Defaults.Fps > 0 {
		defaultParts = append(defaultParts, fmt.Sprintf("%dfps", tpl.Defaults.Fps))
	}
	if rate := templateSpeechRate(tpl); rate > 0 {
		unit := "字"
		if isEnglishLanguage(tpl.Defaults.Language) {
			unit = "词"
		}
		defaultParts = append(defaultParts, fmt.Sprintf("%.1f %s/秒", rate, unit))
	}
	preview.Defaults = strings.Join(defaultParts, " · ")

	for _, key := range []string{"logo", "watermark", "bgm"} {
		var value string
		switch key {
		case "logo":
			value = tpl.Assets.Logo
		case "watermark":
			value = tpl.Assets.Watermark
		case "bgm":
			value = tpl.Assets.Bgm
		}
		if value != "" {
			preview.Assets = append(preview.Assets, key+"="+value)
		}
	}
	return preview
}

func subtitleAlignmentLabel(alignment int) string {
	switch alignment {
	case 1:
		return "左下"
	case 2:
		return "底部居中"
	case 3:
		return "右下"
	case 4:
		return "左侧居中"
	case 5:
		return "居中"
	case 6:
		return "右侧居中"
	case 7:
		return "左上"
	case 8:
		return "顶部居中"
	case 9:
		return "右上"
	default:
		return "底部居中"
	}
}
