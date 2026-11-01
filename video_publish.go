package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// 本文件实现《视频生成工具·Agent4 发布元数据设计》：
// 平台档位（PlatformProfile）→ Agent4 发布元数据（标题/标签/发布时段/封面）→ gate③ 确认。

/* ------------------------------------------------------------------
   平台无关适配层（§10.5）
   ------------------------------------------------------------------ */

// PlatformPeakHour 平台默认高峰时段；Days 取 all|weekday|weekend。
type PlatformPeakHour struct {
	Days   string  `json:"days"`
	Start  string  `json:"start"` // HH:MM
	End    string  `json:"end"`
	Weight float64 `json:"weight"`
	Label  string  `json:"label"`
}

// PlatformProfile 发布侧适配器：新增平台只加一份配置，不改 Agent4 逻辑。
type PlatformProfile struct {
	ID                string             `json:"id"`
	Label             string             `json:"label"`
	TitleMaxLen       int                `json:"titleMaxLen"`
	TitleVisibleLen   int                `json:"titleVisibleLen"`
	HashtagMax        int                `json:"hashtagMax"`
	KeywordMax        int                `json:"keywordMax"`
	DescriptionMax    int                `json:"descriptionMax"`
	PeakHours         []PlatformPeakHour `json:"peakHours"`
	SupportsScheduled bool               `json:"supportsScheduled"`
	ReviewLatencyMin  int                `json:"reviewLatencyMin"`
	AspectRatios      []string           `json:"aspectRatios"`
	KeywordSlot       string             `json:"keywordSlot"` // tags|description
}

func platformProfiles() []PlatformProfile {
	return []PlatformProfile{
		{
			ID: "douyin", Label: "抖音", TitleMaxLen: 30, TitleVisibleLen: 15,
			HashtagMax: 5, KeywordMax: 5, DescriptionMax: 120,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "11:30", End: "13:30", Weight: 0.8, Label: "工作日午休"},
				{Days: "all", Start: "18:30", End: "22:00", Weight: 0.95, Label: "晚间高峰"},
				{Days: "weekend", Start: "10:00", End: "12:00", Weight: 0.85, Label: "周末上午"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 30,
			AspectRatios: []string{"9:16", "1:1", "16:9"}, KeywordSlot: "tags",
		},
		{
			ID: "channels", Label: "视频号", TitleMaxLen: 40, TitleVisibleLen: 20,
			HashtagMax: 4, KeywordMax: 5, DescriptionMax: 1000,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "07:00", End: "09:00", Weight: 0.8, Label: "早通勤社交"},
				{Days: "all", Start: "18:00", End: "21:30", Weight: 0.9, Label: "晚间社交高峰"},
				{Days: "all", Start: "21:30", End: "23:00", Weight: 0.75, Label: "睡前时段"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 30,
			AspectRatios: []string{"9:16", "16:9", "1:1"}, KeywordSlot: "tags",
		},
		{
			ID: "xiaohongshu", Label: "小红书", TitleMaxLen: 20, TitleVisibleLen: 20,
			HashtagMax: 10, KeywordMax: 6, DescriptionMax: 1000,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "12:00", End: "14:00", Weight: 0.8, Label: "午间浏览"},
				{Days: "all", Start: "18:00", End: "22:00", Weight: 0.9, Label: "晚间高峰"},
				{Days: "weekend", Start: "09:00", End: "11:00", Weight: 0.85, Label: "周末上午"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 20,
			AspectRatios: []string{"3:4", "9:16", "1:1"}, KeywordSlot: "tags",
		},
		{
			ID: "bilibili", Label: "B站", TitleMaxLen: 80, TitleVisibleLen: 30,
			HashtagMax: 15, KeywordMax: 8, DescriptionMax: 2000,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "18:00", End: "23:00", Weight: 0.9, Label: "晚间长时段"},
				{Days: "weekend", Start: "10:00", End: "12:00", Weight: 0.8, Label: "周末上午"},
				{Days: "weekend", Start: "14:00", End: "17:00", Weight: 0.85, Label: "周末午后"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 30,
			AspectRatios: []string{"16:9"}, KeywordSlot: "tags",
		},
		{
			ID: "kuaishou", Label: "快手", TitleMaxLen: 30, TitleVisibleLen: 15,
			HashtagMax: 5, KeywordMax: 4, DescriptionMax: 120,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "11:30", End: "13:00", Weight: 0.75, Label: "午休"},
				{Days: "all", Start: "19:00", End: "22:30", Weight: 0.95, Label: "晚间高峰"},
				{Days: "weekend", Start: "09:00", End: "11:00", Weight: 0.8, Label: "周末早间"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 30,
			AspectRatios: []string{"9:16", "16:9"}, KeywordSlot: "tags",
		},
		{
			ID: "youtube_shorts", Label: "YouTube Shorts", TitleMaxLen: 60, TitleVisibleLen: 30,
			HashtagMax: 3, KeywordMax: 8, DescriptionMax: 5000,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "19:00", End: "22:00", Weight: 0.9, Label: "晚间高峰"},
				{Days: "weekend", Start: "10:00", End: "13:00", Weight: 0.85, Label: "周末上午"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 60,
			AspectRatios: []string{"9:16"}, KeywordSlot: "description",
		},
		{
			ID: "tiktok", Label: "TikTok", TitleMaxLen: 100, TitleVisibleLen: 40,
			HashtagMax: 5, KeywordMax: 6, DescriptionMax: 2200,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "12:00", End: "13:30", Weight: 0.8, Label: "午休"},
				{Days: "all", Start: "19:00", End: "23:00", Weight: 0.95, Label: "晚间高峰"},
				{Days: "weekend", Start: "10:00", End: "12:00", Weight: 0.85, Label: "周末上午"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 60,
			AspectRatios: []string{"9:16"}, KeywordSlot: "tags",
		},
		{
			ID: "generic", Label: "通用默认档", TitleMaxLen: 40, TitleVisibleLen: 20,
			HashtagMax: 5, KeywordMax: 5, DescriptionMax: 500,
			PeakHours: []PlatformPeakHour{
				{Days: "all", Start: "11:30", End: "13:30", Weight: 0.8, Label: "午休时段"},
				{Days: "all", Start: "18:30", End: "22:00", Weight: 0.9, Label: "晚间高峰"},
				{Days: "weekend", Start: "10:00", End: "12:00", Weight: 0.85, Label: "周末上午"},
			},
			SupportsScheduled: true, ReviewLatencyMin: 30,
			AspectRatios: []string{"16:9", "9:16", "1:1"}, KeywordSlot: "tags",
		},
	}
}

func platformProfile(id string) PlatformProfile {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, profile := range platformProfiles() {
		if profile.ID == id {
			return profile
		}
	}
	return platformProfileOrGeneric()
}

func platformProfileOrGeneric() PlatformProfile {
	for _, profile := range platformProfiles() {
		if profile.ID == "generic" {
			return profile
		}
	}
	return PlatformProfile{ID: "generic", Label: "通用默认档", TitleMaxLen: 40, HashtagMax: 5, KeywordMax: 5, DescriptionMax: 500}
}

// normalizePlatforms 归一化多选平台：只保留已知 ID、去重、最多 4 个。
func normalizePlatforms(list []string) []string {
	known := map[string]bool{}
	for _, profile := range platformProfiles() {
		known[profile.ID] = true
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		id := strings.ToLower(strings.TrimSpace(item))
		if !known[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func primaryPlatform(cfg VideoRunConfig) string {
	if len(cfg.Platforms) > 0 {
		return cfg.Platforms[0]
	}
	return "generic"
}

// normalizeRegion 受众时区；空值与非法值回落到 Asia/Shanghai。
func normalizeRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(region); err != nil {
		return "Asia/Shanghai"
	}
	return region
}

// verticalPeakHints 垂类对高峰档的偏移修正（§10.4 C）。
func verticalPeakHints(vertical string) []PlatformPeakHour {
	switch strings.TrimSpace(vertical) {
	case "美食":
		return []PlatformPeakHour{
			{Days: "all", Start: "10:30", End: "12:00", Weight: 0.92, Label: "饭点前"},
			{Days: "all", Start: "16:30", End: "18:30", Weight: 0.92, Label: "晚饭前"},
		}
	case "游戏":
		return []PlatformPeakHour{
			{Days: "all", Start: "21:00", End: "23:30", Weight: 0.93, Label: "游戏深夜档"},
		}
	case "母婴":
		return []PlatformPeakHour{
			{Days: "all", Start: "08:00", End: "10:00", Weight: 0.88, Label: "育儿早间"},
			{Days: "all", Start: "13:00", End: "15:00", Weight: 0.82, Label: "午后育儿"},
		}
	case "知识", "职场":
		return []PlatformPeakHour{
			{Days: weekdayOnly, Start: "07:00", End: "09:00", Weight: 0.88, Label: "通勤时段"},
			{Days: "all", Start: "12:00", End: "13:30", Weight: 0.85, Label: "午休充电"},
		}
	case "健身":
		return []PlatformPeakHour{
			{Days: "all", Start: "06:30", End: "08:00", Weight: 0.85, Label: "晨练"},
			{Days: "all", Start: "20:00", End: "22:00", Weight: 0.88, Label: "晚间运动"},
		}
	}
	return nil
}

const weekdayOnly = "weekday"

/* ------------------------------------------------------------------
   输出契约（§10.3）
   ------------------------------------------------------------------ */

type PublishTitle struct {
	Primary          string   `json:"primary"`
	Alternatives     []string `json:"alternatives"`
	AlternativeHooks []string `json:"alternativeHooks"`
	CharCount        int      `json:"charCount"`
	Limit            int      `json:"limit"`
	VisibleLimit     int      `json:"visibleLimit"`
	HookType         string   `json:"hookType"`
	Compliance       []string `json:"compliance"`
}

type PublishTagSource struct {
	FromPromptSlots []string `json:"fromPromptSlots"`
	FromTopic       []string `json:"fromTopic"`
	FromVertical    []string `json:"fromVertical"`
}

type PublishTagCounts struct {
	Hashtag int `json:"hashtag"`
	Keyword int `json:"keyword"`
}

type PublishTags struct {
	Hashtags []string         `json:"hashtags"`
	Keywords []string         `json:"keywords"`
	Counts   PublishTagCounts `json:"counts"`
	Source   PublishTagSource `json:"source"`
}

type PublishTimeCandidate struct {
	Slot   string  `json:"slot"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score"`
}

type PublishTime struct {
	Candidates []PublishTimeCandidate `json:"candidates"`
	Timezone   string                 `json:"timezone"`
	BufferNote string                 `json:"bufferNote"`
}

type PublishCover struct {
	FrameTS     float64 `json:"frameTs"`
	TextOverlay string  `json:"textOverlay"`
	Ratio       string  `json:"ratio"`
}

// PublishMeta Agent4 输出契约；与 renders/compose 解耦，可独立重算与确认。
type PublishMeta struct {
	Platform      string       `json:"platform"`
	PlatformLabel string       `json:"platformLabel"`
	Platforms     []string     `json:"platforms"`
	Title         PublishTitle `json:"title"`
	Tags          PublishTags  `json:"tags"`
	PublishTime   PublishTime  `json:"publishTime"`
	Cover         PublishCover `json:"cover"`
	Description   string       `json:"description"`
	Category      string       `json:"category"`
	Source        string       `json:"source"` // llm|local
	Note          string       `json:"note"`
	GeneratedAt   string       `json:"generatedAt"`
}

// VideoPublishEdit gate③ 的局部修改：只改文案与时段，不动视频。
type VideoPublishEdit struct {
	Title       string   `json:"title"`
	Hashtags    []string `json:"hashtags"`
	Keywords    []string `json:"keywords"`
	Description string   `json:"description"`
	Slot        string   `json:"slot"`
	CoverTS     *float64 `json:"coverTs"`
}

/* ------------------------------------------------------------------
   Agent4 生成
   ------------------------------------------------------------------ */

// generatePublishMeta 有文本模型时走 LLM，失败或无模型回退本地规则；
// 发布时段始终用本地时区/平台档位计算，避免模型凭空编日期。
func (a *App) generatePublishMeta(ctx context.Context, cfg VideoRunConfig, script *VideoScript, prompts []VideoPromptItem, feedback string) (*PublishMeta, []string) {
	warnings := make([]string, 0, 3)
	profile := platformProfile(primaryPlatform(cfg))
	if len(cfg.Platforms) == 0 {
		warnings = append(warnings, "未选择发布平台：按「通用默认档」生成标题字数与高峰时段")
	}

	meta := localPublishMeta(cfg, script, prompts, profile)

	provider := a.pickTextProvider(cfg)
	if provider.ID != "" {
		if llm, err := a.llmPublishMeta(ctx, provider, cfg, script, prompts, profile, feedback); err == nil && strings.TrimSpace(llm.Title.Primary) != "" {
			meta.Title.Primary = strings.TrimSpace(llm.Title.Primary)
			meta.Title.Alternatives = trimList(llm.Title.Alternatives, 3)
			meta.Title.HookType = firstNonEmpty(strings.TrimSpace(llm.Title.HookType), meta.Title.HookType)
			meta.Tags.Hashtags = normalizeHashtags(llm.Tags.Hashtags, profile)
			meta.Tags.Keywords = trimList(llm.Tags.Keywords, profile.KeywordMax)
			meta.Description = firstNonEmpty(strings.TrimSpace(llm.Description), meta.Description)
			meta.Category = firstNonEmpty(strings.TrimSpace(llm.Category), meta.Category)
			if strings.TrimSpace(llm.Cover.TextOverlay) != "" {
				meta.Cover.TextOverlay = strings.TrimSpace(llm.Cover.TextOverlay)
			}
			meta.Source = "llm"
		} else if err != nil {
			warnings = append(warnings, fmt.Sprintf("文本模型生成发布元数据失败，已回退本地规则：%v", err))
		}
	} else {
		warnings = append(warnings, "未配置文本模型，发布元数据由本地规则生成")
	}
	// 没走成 LLM 就等于本轮意见无人执行，要写进日志而不是默默回退本地文案
	if meta.Source != "llm" {
		if warning := feedbackDroppedWarning(feedback); warning != "" {
			warnings = append(warnings, warning)
		}
	}

	finalizePublishMeta(meta, cfg, profile)
	return meta, warnings
}

// finalizePublishMeta 统一按平台档位夹取字数与数量，并回填合规提示。
func finalizePublishMeta(meta *PublishMeta, cfg VideoRunConfig, profile PlatformProfile) {
	meta.Platform = profile.ID
	meta.PlatformLabel = profile.Label
	meta.Platforms = append([]string{}, cfg.Platforms...)
	if len(meta.Platforms) == 0 {
		meta.Platforms = []string{profile.ID}
	}

	meta.Title.Limit = profile.TitleMaxLen
	meta.Title.VisibleLimit = profile.TitleVisibleLen
	meta.Title.Primary = clampTitle(meta.Title.Primary, profile.TitleMaxLen)
	alternatives := make([]string, 0, 3)
	for _, item := range meta.Title.Alternatives {
		text := clampTitle(item, profile.TitleMaxLen)
		if text == "" || text == meta.Title.Primary {
			continue
		}
		alternatives = append(alternatives, text)
		if len(alternatives) >= 3 {
			break
		}
	}
	meta.Title.Alternatives = alternatives
	meta.Title.CharCount = runeLen(meta.Title.Primary)
	meta.Title.Compliance = complianceHits(append(append([]string{}, meta.Title.Alternatives...), meta.Title.Primary))
	if len(meta.Title.Compliance) > 0 {
		meta.Note = "标题命中合规红线，请替换后再发布：" + strings.Join(meta.Title.Compliance, "；")
	}

	meta.Tags.Hashtags = normalizeHashtags(meta.Tags.Hashtags, profile)
	meta.Tags.Keywords = trimmedKeywords(meta.Tags.Keywords, meta.Tags.Hashtags, profile.KeywordMax)
	meta.Tags.Counts = PublishTagCounts{Hashtag: len(meta.Tags.Hashtags), Keyword: len(meta.Tags.Keywords)}

	meta.PublishTime = buildPublishTime(profile, cfg, time.Now())
	meta.Description = truncateRunes(meta.Description, profile.DescriptionMax)
	meta.Cover.Ratio = cfg.AspectRatio
	meta.Cover.TextOverlay = truncateRunes(meta.Cover.TextOverlay, 14)
	if meta.Cover.FrameTS <= 0 {
		meta.Cover.FrameTS = round2(mathMin(1.2, float64(cfg.DurationS)*0.15))
	}
	if meta.Category == "" {
		meta.Category = firstNonEmpty(strings.TrimSpace(cfg.Vertical), "通用")
	}
	meta.GeneratedAt = time.Now().Format(time.RFC3339)
}

// localPublishMeta 无文本模型时的确定性兜底：标签、标题、时段全部可解释。
func localPublishMeta(cfg VideoRunConfig, script *VideoScript, prompts []VideoPromptItem, profile PlatformProfile) *PublishMeta {
	phrases := topicPhrases(cfg)
	roles, scenes, styles := promptSlots(script, cfg)

	titlePrimary, alternatives, hookType := localPublishTitle(cfg, script, profile)

	hashtags := make([]string, 0, profile.HashtagMax)
	addTag := func(value string) {
		value = strings.TrimSpace(strings.TrimPrefix(value, "#"))
		if value == "" || runeLen(value) > 12 {
			return
		}
		for _, existing := range hashtags {
			if existing == value {
				return
			}
		}
		hashtags = append(hashtags, value)
	}
	if cfg.Vertical != "" {
		addTag(cfg.Vertical)
	}
	for _, phrase := range phrases {
		addTag(phrase)
	}
	for _, role := range roles {
		addTag(role)
	}
	for _, style := range styles {
		addTag(style)
	}
	addTag(cfg.VideoParams.Style)

	keywords := make([]string, 0, profile.KeywordMax)
	keywordSeen := map[string]bool{}
	addKeyword := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || keywordSeen[value] {
			return
		}
		keywordSeen[value] = true
		keywords = append(keywords, value)
	}
	for _, phrase := range phrases {
		addKeyword(phrase)
	}
	for _, scene := range scenes {
		addKeyword(scene)
	}
	for _, role := range roles {
		addKeyword(role)
	}
	for _, style := range styles {
		addKeyword(style)
	}
	if cfg.Vertical != "" {
		addKeyword(cfg.Vertical)
	}
	if len(keywords) == 0 {
		addKeyword(truncateRunes(cfg.Topic, 12))
	}

	description := localDescription(cfg, script, hashtags, profile)

	meta := &PublishMeta{
		Platform:      profile.ID,
		PlatformLabel: profile.Label,
		Platforms:     append([]string{}, cfg.Platforms...),
		Title: PublishTitle{
			Primary:      titlePrimary,
			Alternatives: alternatives,
			HookType:     hookType,
		},
		Tags: PublishTags{
			Hashtags: hashtags,
			Keywords: keywords,
			Source: PublishTagSource{
				FromPromptSlots: promptSlotLabels(script, cfg),
				FromTopic:       phrases,
				FromVertical:    nonEmptyList(cfg.Vertical),
			},
		},
		Description: description,
		Category:    firstNonEmpty(strings.TrimSpace(cfg.Vertical), "通用"),
		Cover: PublishCover{
			FrameTS:     round2(mathMin(1.2, float64(cfg.DurationS)*0.15)),
			TextOverlay: truncateRunes(titlePrimary, 14),
			Ratio:       cfg.AspectRatio,
		},
		Source: "local",
	}
	if len(prompts) == 0 {
		meta.Note = "缺少提示词：标签仅依据主题与垂类推导"
	}
	return meta
}

func localPublishTitle(cfg VideoRunConfig, script *VideoScript, profile PlatformProfile) (string, []string, string) {
	keyword := firstNonEmpty(firstOf(topicPhrases(cfg)), truncateRunes(cfg.Topic, 10))
	shots := 1
	if script != nil && len(script.Shots) > 0 {
		shots = len(script.Shots)
	}

	type candidate struct {
		text string
		hook string
	}
	candidates := make([]candidate, 0, 4)
	if isEnglishLanguage(cfg.Language) {
		candidates = append(candidates,
			candidate{fmt.Sprintf("%d shots to nail %s", shots, keyword), "数字清单"},
			candidate{fmt.Sprintf("The %s detail most people miss", keyword), "反常识"},
			candidate{fmt.Sprintf("What really happens with %s", keyword), "悬念"},
			candidate{cfg.Topic, "主题直述"},
		)
	} else {
		candidates = append(candidates,
			candidate{fmt.Sprintf("%d个镜头讲清%s", shots, keyword), "数字清单"},
			candidate{fmt.Sprintf("%s：第%d步最容易被忽略", keyword, clampInt(2, 1, shots)), "反常识"},
			candidate{fmt.Sprintf("别急着上手，%s的关键细节", keyword), "悬念"},
			candidate{cfg.Topic, "主题直述"},
		)
	}

	primary := ""
	hookType := "主题直述"
	alternatives := make([]string, 0, 3)
	for _, item := range candidates {
		text := clampTitle(item.text, profile.TitleMaxLen)
		if text == "" {
			continue
		}
		if primary == "" {
			primary = text
			hookType = item.hook
			continue
		}
		if text == primary || len(alternatives) >= 3 {
			continue
		}
		alternatives = append(alternatives, text)
	}
	if primary == "" {
		primary = clampTitle(cfg.Topic, profile.TitleMaxLen)
	}
	return primary, alternatives, hookType
}

func localDescription(cfg VideoRunConfig, script *VideoScript, hashtags []string, profile PlatformProfile) string {
	base := ""
	if script != nil && strings.TrimSpace(script.Logline) != "" {
		base = strings.TrimSpace(script.Logline)
	}
	if base == "" {
		base = cfg.Topic
	}
	desc := base
	if len(hashtags) > 0 {
		tags := make([]string, 0, 3)
		for _, tag := range hashtags {
			if len(tags) >= 3 {
				break
			}
			tags = append(tags, "#"+tag)
		}
		if profile.KeywordSlot == "description" {
			desc = base + "\n\n关键词：" + strings.Join(tags, " ")
		} else {
			desc = base + " " + strings.Join(tags, " ")
		}
	}
	if profile.SupportsScheduled {
		desc += "\n\n内容为 AI 辅助创作，画面与文案已人工确认。"
	}
	return truncateRunes(desc, profile.DescriptionMax)
}

func promptSlots(script *VideoScript, cfg VideoRunConfig) (roles, scenes, styles []string) {
	seen := map[string]bool{}
	if script != nil {
		for _, shot := range script.Shots {
			for _, role := range shot.Roles {
				role = strings.TrimSpace(role)
				if role != "" && !seen["r:"+role] {
					seen["r:"+role] = true
					roles = append(roles, role)
				}
			}
			for _, phrase := range splitPhrases(shot.SceneIntent) {
				if !seen["s:"+phrase] {
					seen["s:"+phrase] = true
					scenes = append(scenes, phrase)
				}
			}
		}
	}
	if style := strings.TrimSpace(cfg.VideoParams.Style); style != "" {
		styles = append(styles, style)
	}
	if mood := strings.TrimSpace(cfg.VideoParams.Mood); mood != "" {
		styles = append(styles, mood)
	}
	return roles, scenes, styles
}

func promptSlotLabels(script *VideoScript, cfg VideoRunConfig) []string {
	roles, scenes, _ := promptSlots(script, cfg)
	labels := make([]string, 0, 4)
	if len(roles) > 0 {
		labels = append(labels, "主体")
	}
	if len(scenes) > 0 {
		labels = append(labels, "场景")
	}
	if strings.TrimSpace(cfg.VideoParams.Style) != "" {
		labels = append(labels, "风格")
	}
	if strings.TrimSpace(cfg.VideoParams.Mood) != "" {
		labels = append(labels, "情绪")
	}
	return labels
}

// buildPublishTime 按平台档位 + 垂类修正 + 受众时区给出 Top-N 时段（§10.4 C）。
func buildPublishTime(profile PlatformProfile, cfg VideoRunConfig, now time.Time) PublishTime {
	tz := normalizeRegion(cfg.Region)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	local := now.In(loc)

	windows := append([]PlatformPeakHour{}, profile.PeakHours...)
	windows = append(windows, verticalPeakHints(cfg.Vertical)...)

	type entry struct {
		at     time.Time
		reason string
		score  float64
	}
	entries := make([]entry, 0, len(windows)*3)
	earliest := local.Add(time.Duration(profile.ReviewLatencyMin+30) * time.Minute)

	for day := 0; day <= 7; day++ {
		date := local.AddDate(0, 0, day)
		weekday := date.Weekday()
		isWeekend := weekday == time.Saturday || weekday == time.Sunday
		for _, window := range windows {
			switch window.Days {
			case weekdayOnly:
				if isWeekend {
					continue
				}
			case "weekend":
				if !isWeekend {
					continue
				}
			}
			start, ok := parseClock(window.Start)
			if !ok {
				continue
			}
			at := time.Date(date.Year(), date.Month(), date.Day(), start.Hour(), start.Minute(), 0, 0, loc)
			if at.Before(earliest) {
				continue
			}
			entries = append(entries, entry{at: at, reason: window.Label, score: window.Weight})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score == entries[j].score {
			return entries[i].at.Before(entries[j].at)
		}
		return entries[i].score > entries[j].score
	})

	candidates := make([]PublishTimeCandidate, 0, 3)
	for _, item := range entries {
		tooClose := false
		for _, chosen := range candidates {
			// 同一时段（±90min）或同一钟点只保留一个，保证 Top-N 覆盖不同时段
			if sameWindow(chosen.Slot, item.at) || sameClock(chosen.Slot, item.at) {
				tooClose = true
				break
			}
		}
		if tooClose {
			continue
		}
		candidates = append(candidates, PublishTimeCandidate{
			Slot:   item.at.Format("2006-01-02 15:04"),
			Reason: fmt.Sprintf("%s（受众本地时间 %s）", item.reason, tz),
			Score:  item.score,
		})
		if len(candidates) >= 3 {
			break
		}
	}
	if len(candidates) == 0 {
		fallback := local.AddDate(0, 0, 1)
		fallback = time.Date(fallback.Year(), fallback.Month(), fallback.Day(), 19, 30, 0, 0, loc)
		candidates = append(candidates, PublishTimeCandidate{
			Slot:   fallback.Format("2006-01-02 15:04"),
			Reason: fmt.Sprintf("默认晚间档（受众本地时间 %s）", tz),
			Score:  0.6,
		})
	}

	buffer := fmt.Sprintf("预留审核/转码延迟 %d–%dmin，建议定时发布而非即时", profile.ReviewLatencyMin, profile.ReviewLatencyMin+30)
	if !profile.SupportsScheduled {
		buffer = "该平台不支持定时发布：建议提前 " + fmt.Sprintf("%d", profile.ReviewLatencyMin) + " 分钟准备，准点手动发布"
	}
	return PublishTime{Candidates: candidates, Timezone: tz, BufferNote: buffer}
}

func sameWindow(slot string, at time.Time) bool {
	parsed, err := time.ParseInLocation("2006-01-02 15:04", slot, at.Location())
	if err != nil {
		return false
	}
	diff := parsed.Sub(at)
	if diff < 0 {
		diff = -diff
	}
	return diff < 90*time.Minute
}

func sameClock(slot string, at time.Time) bool {
	parsed, err := time.ParseInLocation("2006-01-02 15:04", slot, at.Location())
	if err != nil {
		return false
	}
	return parsed.Format("15:04") == at.Format("15:04")
}

func parseClock(value string) (time.Time, bool) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

/* ------------------------------------------------------------------
   LLM 分支
   ------------------------------------------------------------------ */

type llmPublishPayload struct {
	Title struct {
		Primary      string   `json:"primary"`
		Alternatives []string `json:"alternatives"`
		HookType     string   `json:"hook_type"`
	} `json:"title"`
	Tags struct {
		Hashtags []string `json:"hashtags"`
		Keywords []string `json:"keywords"`
	} `json:"tags"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Cover       struct {
		TextOverlay string `json:"text_overlay"`
	} `json:"cover"`
}

func (a *App) llmPublishMeta(ctx context.Context, provider Provider, cfg VideoRunConfig, script *VideoScript, prompts []VideoPromptItem, profile PlatformProfile, feedback string) (*PublishMeta, error) {
	system := "你是短视频平台的发布运营。只输出严格 JSON，不要 Markdown 代码块，不要解释。" +
		"标题必须关键词前置、带一个可验证的钩子（数字清单/反常识/悬念），不得夸大或违规。" +
		"标签要能追溯到画面提示词或主题，不要蹭无关热点。"
	user := buildPublishPrompt(cfg, script, prompts, profile, feedback)
	content, err := a.chatCompletion(ctx, provider, system, user)
	if err != nil {
		return nil, err
	}
	raw := extractJSONBlock(content)
	payload := llmPublishPayload{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("解析发布元数据 JSON 失败: %w", err)
	}
	meta := &PublishMeta{}
	meta.Title.Primary = payload.Title.Primary
	meta.Title.Alternatives = payload.Title.Alternatives
	meta.Title.HookType = payload.Title.HookType
	meta.Tags.Hashtags = payload.Tags.Hashtags
	meta.Tags.Keywords = payload.Tags.Keywords
	meta.Description = payload.Description
	meta.Category = payload.Category
	meta.Cover.TextOverlay = payload.Cover.TextOverlay
	meta.Source = "llm"
	return meta, nil
}

func buildPublishPrompt(cfg VideoRunConfig, script *VideoScript, prompts []VideoPromptItem, profile PlatformProfile, feedback string) string {
	var builder strings.Builder
	builder.WriteString("平台：" + profile.Label + "（标题最多 " + fmt.Sprintf("%d", profile.TitleMaxLen) +
		" 字，首屏可见约 " + fmt.Sprintf("%d", profile.TitleVisibleLen) + " 字；话题最多 " +
		fmt.Sprintf("%d", profile.HashtagMax) + " 个；搜索关键词最多 " + fmt.Sprintf("%d", profile.KeywordMax) + " 个）\n")
	builder.WriteString("受众时区：" + normalizeRegion(cfg.Region) + "；垂类：" + firstNonEmpty(cfg.Vertical, "通用") + "；语言：" + cfg.Language + "\n")
	builder.WriteString("主题：" + cfg.Topic + "\n")
	if script != nil {
		builder.WriteString("一句话概述：" + script.Logline + "\n")
		builder.WriteString("分镜：\n")
		for _, shot := range script.Shots {
			builder.WriteString(fmt.Sprintf("- %s [%.1fs-%.1fs] 角色=%s 场景=%s 情绪=%s 台词=%s\n",
				shot.ID, shot.TimeRange[0], shot.TimeRange[1],
				strings.Join(shot.Roles, "/"), shot.SceneIntent, shot.Mood, shot.Dialogue))
		}
	}
	if len(prompts) > 0 {
		builder.WriteString("画面提示词（标签主要来源）：\n")
		for _, item := range prompts {
			builder.WriteString("- " + truncateRunes(item.VisualPrompt, 160) + "\n")
		}
	}
	// 关卡③上敲的意见（“标签太多”“换个更晚的时段”）必须让模型看见，拼在输出格式前一条
	builder.WriteString(feedbackDirective(feedback))
	builder.WriteString("\n输出 JSON：{\"title\":{\"primary\":\"\",\"alternatives\":[\"\",\"\"],\"hook_type\":\"\"}," +
		"\"tags\":{\"hashtags\":[\"\"],\"keywords\":[\"\"]},\"description\":\"\",\"category\":\"\",\"cover\":{\"text_overlay\":\"\"}}")
	return builder.String()
}

/* ------------------------------------------------------------------
   小工具
   ------------------------------------------------------------------ */

var publishBannedWords = []string{"最好", "最强", "第一", "顶级", "国家级", "百分百", "100%", "永久有效", "绝对", "稳赚", "躺赚", "治愈", "疗效"}

func complianceHits(titles []string) []string {
	hits := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, title := range titles {
		for _, word := range publishBannedWords {
			if strings.Contains(title, word) && !seen[word] {
				seen[word] = true
				hits = append(hits, "命中敏感词「"+word+"」")
			}
		}
	}
	return hits
}

func topicPhrases(cfg VideoRunConfig) []string {
	phrases := splitPhrases(cfg.Topic)
	if len(phrases) == 0 {
		return nonEmptyList(truncateRunes(cfg.Topic, 12))
	}
	if len(phrases) > 4 {
		phrases = phrases[:4]
	}
	return phrases
}

// splitPhrases 按标点/空白切分，保留 2–12 字（英文 3 字母以上）的短语。
func splitPhrases(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("，。！？；、：,.!?;:|/\\-—~`\"'“”‘’()（）[]【】《》<>", r)
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		length := runeLen(field)
		if length == 0 || length > 12 {
			continue
		}
		out = append(out, field)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func normalizeHashtags(list []string, profile PlatformProfile) []string {
	out := make([]string, 0, profile.HashtagMax)
	seen := map[string]bool{}
	for _, item := range list {
		tag := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), "#"))
		if tag == "" || seen[tag] || runeLen(tag) > 12 {
			continue
		}
		seen[tag] = true
		if profile.KeywordSlot == "description" {
			out = append(out, "#"+tag)
		} else {
			out = append(out, "#"+tag)
		}
		if len(out) >= profile.HashtagMax {
			break
		}
	}
	return out
}

func trimmedKeywords(list []string, hashtags []string, max int) []string {
	blocked := map[string]bool{}
	for _, tag := range hashtags {
		blocked[strings.TrimPrefix(tag, "#")] = true
	}
	out := make([]string, 0, max)
	seen := map[string]bool{}
	for _, item := range list {
		word := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), "#"))
		if word == "" || seen[word] || blocked[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
		if len(out) >= max {
			break
		}
	}
	return out
}

func trimList(list []string, max int) []string {
	out := make([]string, 0, max)
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
		if len(out) >= max {
			break
		}
	}
	return out
}

func nonEmptyList(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func firstOf(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

func clampTitle(text string, limit int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	return truncateRunes(text, limit)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if limit <= 0 || len(runes) <= limit {
		return string(runes)
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func runeLen(text string) int {
	return len([]rune(text))
}

func mathMin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
