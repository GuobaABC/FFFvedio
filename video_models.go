package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// 视频生成流水线的阶段（与《视频生成工具·架构与最佳实践设计》§2 一致，
// 并在合成之后接入 Agent4 发布元数据与 gate③）。
// 人工确认关卡本身也是一个阶段，处于该阶段时任务状态为 waiting。
// 字幕排在 gate② 之后：台词要等用户在提示词关卡改完定稿，再铺字幕时间轴。
const (
	VideoStageDraftScript   = "DRAFT_SCRIPT"
	VideoStageConfirmScript = "CONFIRM_SCRIPT"
	VideoStageBuildPrompt   = "BUILD_PROMPT"
	VideoStageConfirmPrompt = "CONFIRM_PROMPT"
	VideoStageBuildSubs     = "BUILD_SUBS"
	VideoStageRender        = "RENDER_VIDEO"
	VideoStageCompose       = "COMPOSE"
	VideoStageBuildMeta     = "BUILD_PUBLISH_META"
	VideoStageConfirmMeta   = "CONFIRM_META"
	VideoStageDone          = "DONE"
)

// legacyVideoStages 把历史任务里存过的旧阶段名映射到当前阶段名：
// v0.0.5 之前提示词与字幕挤在同一阶段（BUILD_PROMPT_SUBS），现在拆成两段。
var legacyVideoStages = map[string]string{
	"BUILD_PROMPT_SUBS": VideoStageBuildPrompt,
}

func normalizeVideoStage(stage string) string {
	stage = strings.TrimSpace(stage)
	if mapped, ok := legacyVideoStages[stage]; ok {
		return mapped
	}
	return stage
}

// 任务状态
const (
	VideoStatusDraft   = "draft"   // 已创建，等待执行
	VideoStatusRunning = "running" // 自动化阶段执行中
	VideoStatusWaiting = "waiting" // 停在人工确认关卡
	VideoStatusDone    = "done"
	VideoStatusFailed  = "failed"
)

// 三道确认关卡：分镜 → 提示词 → 发布元数据
const (
	VideoGateScript = "gate1"
	VideoGatePrompt = "gate2"
	VideoGateMeta   = "gate3"
)

const (
	videoGatePending  = "pending"
	videoGateApproved = "approved"
)

// 角色参考图来源：用户上传的定妆图优先于模板里的生成图。
// 同一角色只吃一份来源，两边的图混着提交会被模型平均出第三个形象。
const (
	CharacterImageSourceUser     = "user"     // 用户在新建任务里选的图
	CharacterImageSourceTemplate = "template" // 模板自带的定妆图（多为生成图）
)

// VideoCharacterRef 多角色参考图：每个角色一张或多张定妆图，用于跨段保持外观一致。
type VideoCharacterRef struct {
	RoleID string   `json:"roleId"`
	Name   string   `json:"name"`
	Images []string `json:"images"`
	// ImageSource 标记 Images 的来源，决定提交顺序与提示词里的措辞权重
	ImageSource string `json:"imageSource"`
	VoiceHint   string `json:"voiceHint"`
}

// VideoSubtitleStyle 字幕样式，全部来自用户输入（§6）。
type VideoSubtitleStyle struct {
	FontName     string  `json:"fontName"`
	FontSizePct  float64 `json:"fontSizePct"`  // 相对画面高度百分比
	PrimaryColor string  `json:"primaryColor"` // #RRGGBB
	OutlineColor string  `json:"outlineColor"` // #RRGGBB
	OutlineWidth float64 `json:"outlineWidth"`
	Alignment    int     `json:"alignment"` // ASS numpad 1-9
	MarginVPct   float64 `json:"marginVpct"`
	Bold         bool    `json:"bold"`
}

// VideoParams 风格/运镜/情绪等结构化枚举 + 自由文本。
type VideoParams struct {
	Style          string `json:"style"`
	Camera         string `json:"camera"`
	Mood           string `json:"mood"`
	ExtraPrompt    string `json:"extraPrompt"`
	NegativePrompt string `json:"negativePrompt"`
	Seed           int64  `json:"seed"`
}

// VideoRunConfig 输入契约（§1）：一次给全、入口归一化。
type VideoRunConfig struct {
	Topic string `json:"topic"`
	// ScriptMode 分镜来源：auto（Agent1 自动生成）/ manual（用户逐段填写）
	ScriptMode string `json:"scriptMode"`
	// ManualShots 手动分段：scriptMode=manual 时直接当分镜用，段数与段时长以用户输入为准
	ManualShots      []VideoShot         `json:"manualShots"`
	FirstFrame       string              `json:"firstFrame"` // 本地路径或 URL
	CharacterRefs    []VideoCharacterRef `json:"characterRefs"`
	DurationS        int                 `json:"durationS"`
	Fps              int                 `json:"fps"`
	AspectRatio      string              `json:"aspectRatio"`
	ContinuationMode string              `json:"continuationMode"` // none|last_frame|overlap
	SpeechRate       float64             `json:"speechRate"`       // 字/秒 或 词/秒
	Language         string              `json:"language"`
	Subtitles        bool                `json:"subtitles"` // 是否生成并烧录字幕（默认关闭）
	SegmentReview    *bool               `json:"segmentReview"`
	// StrictFirstFrame 严格首帧：本段有首帧（多为用户输入或从尾帧资产带入的那张图）时，
	// 必须以「图生视频-首帧」任务提交、画面从这张图开始，不为角色参考图让位（nil 默认开启）。
	StrictFirstFrame *bool              `json:"strictFirstFrame"`
	KeepLastFrame    bool               `json:"keepLastFrame"` // 最后一段尾帧存入资产库
	VideoParams      VideoParams        `json:"videoParams"`
	SubtitleStyle    VideoSubtitleStyle `json:"subtitleStyle"`
	Adapter          string             `json:"adapter"`        // auto|mock|dashscope|runway|veo|generic
	ProviderID       string             `json:"providerId"`     // multimodal_gen 提供商
	TextProviderID   string             `json:"textProviderId"` // Agent1/Agent2 使用的文本模型
	LocalFallback    *bool              `json:"localFallback"`  // 无文本模型时用本地规则（默认 true）
	Platforms        []string           `json:"platforms"`      // Agent4 发布平台（可多选，首个为主平台）
	Region           string             `json:"region"`         // 受众时区，默认 Asia/Shanghai
	Vertical         string             `json:"vertical"`       // 垂类：美食/游戏/母婴/知识…
	TemplateRef      *VideoTemplateRef  `json:"templateRef"`    // 选中的模板（只预填默认值，不绕过 gate）
}

// segmentReviewEnabled 逐段确认默认开启（与 LocalFallback 同样的 *bool 缺省语义），
// 取消勾选后恢复“一次串行渲染全部片段”的全自动模式。
func (cfg VideoRunConfig) segmentReviewEnabled() bool {
	return cfg.SegmentReview == nil || *cfg.SegmentReview
}

// localFallbackEnabled 本地兜底默认开启；用户显式关掉后，没有文本模型就直接失败，
// 不再拿本地模板顶替——参数写了“不要兜底”就得真的不兜底。
func (cfg VideoRunConfig) localFallbackEnabled() bool {
	return cfg.LocalFallback == nil || *cfg.LocalFallback
}

// 分镜来源：auto 由 Agent1 生成，manual 直接用用户逐段填写的内容。
// 手动模式只接管 Agent1，提示词、确认关卡、字幕、渲染、合成全链路完全复用。
const (
	VideoScriptModeAuto   = "auto"
	VideoScriptModeManual = "manual"
)

// manualScriptEnabled 是否走到手动分段分支。
func (cfg VideoRunConfig) manualScriptEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(cfg.ScriptMode), VideoScriptModeManual)
}

func normalizeVideoScriptMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), VideoScriptModeManual) {
		return VideoScriptModeManual
	}
	return VideoScriptModeAuto
}

// strictFirstFrameEnabled 严格首帧默认开启：用户既然给了首帧/尾帧，新视频就得从这张画面起。
// 关掉它才会让首帧图与角色参考图一起走方舟「全模态参考」（官方两类任务互斥，
// 参考模式下锁帧只能靠提示词里的「图片1 是本段首帧画面」这种软约定）。
func (cfg VideoRunConfig) strictFirstFrameEnabled() bool {
	return cfg.StrictFirstFrame == nil || *cfg.StrictFirstFrame
}

// VideoSpeakerNarration 旁白（画外音）：台词由镜头外的解说承担，画面里不需要有人对口型。
// 除它以外，说话人必须是角色表里的 role_id——“谁在说”必须有确定归宿，
// 否则渲染层不知道给谁拍口型、字幕与配音也会含糊。
const VideoSpeakerNarration = "narration"

// VideoShot Agent1 的分镜数组元素。
type VideoShot struct {
	ID          string     `json:"id"`
	TimeRange   [2]float64 `json:"timeRange"`
	Roles       []string   `json:"roles"`
	SceneIntent string     `json:"sceneIntent"`
	Dialogue    string     `json:"dialogue"`
	// Speaker 这句台词谁说：role_id，或 VideoSpeakerNarration 表示旁白；无台词时为空
	Speaker   string  `json:"speaker"`
	Camera    string  `json:"camera"`
	Mood      string  `json:"mood"`
	AudioCue  string  `json:"audioCue"`
	DurationS float64 `json:"durationS"`
}

// VideoScript Agent1 输出契约。
type VideoScript struct {
	Logline  string      `json:"logline"`
	Language string      `json:"language"`
	Source   string      `json:"source"` // llm|local
	Shots    []VideoShot `json:"shots"`
}

// VideoPromptItem Agent2 输出：画面提示词 + 台词 + 时长预算校验。
type VideoPromptItem struct {
	ShotID          string  `json:"shotId"`
	VisualPrompt    string  `json:"visualPrompt"`
	Dialogue        string  `json:"dialogue"`
	Speaker         string  `json:"speaker"` // 台词归属：role_id 或 VideoSpeakerNarration
	Start           float64 `json:"start"`
	End             float64 `json:"end"`
	CharCount       int     `json:"charCount"`
	EstSpeechS      float64 `json:"estSpeechS"`
	Fits            bool    `json:"fits"`
	RenderDurationS int     `json:"renderDurationS"`
	Note            string  `json:"note"`
}

// VideoSegment 渲染段：分镜按模型时长档位切分后的最小生成单元。
type VideoSegment struct {
	ID       string `json:"id"`
	ShotID   string `json:"shotId"`
	Index    int    `json:"index"`
	Prompt   string `json:"prompt"`
	Dialogue string `json:"dialogue"`
	// Speaker 台词归属（role_id / narration）；SpeakerName 是写进提示词的显示名（旁白 / 角色名）
	Speaker     string  `json:"speaker"`
	SpeakerName string  `json:"speakerName"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	DurationS   int     `json:"durationS"`
}

// VideoPlan 成本/拼接计划（确认门②前展示段数×单价 + 串行/并行）。
type VideoPlan struct {
	Adapter    string         `json:"adapter"`
	ClipCount  int            `json:"clipCount"`
	Serial     bool           `json:"serial"`
	EstCredits float64        `json:"estCredits"`
	TotalS     float64        `json:"totalS"`
	Segments   []VideoSegment `json:"segments"`
	Warnings   []string       `json:"warnings"`
}

// VideoSubtitleCue 时间轴字幕行。
type VideoSubtitleCue struct {
	Index int     `json:"index"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// VideoSubtitles 字幕产物（SRT + ASS）。
type VideoSubtitles struct {
	SRTPath         string             `json:"srtPath"`
	ASSPath         string             `json:"assPath"`
	SRTUrl          string             `json:"srtUrl"`
	ASSUrl          string             `json:"assUrl"`
	TimingTablePath string             `json:"timingTablePath"`
	TimingTableURL  string             `json:"timingTableUrl"`
	TimingSource    string             `json:"timingSource"` // estimate|measured|tts|aligned
	Cues            []VideoSubtitleCue `json:"cues"`
}

// VideoRender 单段渲染记录（可断点续跑）。
type VideoRender struct {
	SegmentID  string  `json:"segmentId"`
	ShotID     string  `json:"shotId"`
	TaskID     string  `json:"taskId"`
	VideoURI   string  `json:"videoUri"`
	MediaURL   string  `json:"mediaUrl"`
	Status     string  `json:"status"` // pending|running|succeeded|failed|skipped
	Note       string  `json:"note"`
	Seed       int64   `json:"seed"`
	ModelVer   string  `json:"modelVer"`
	DurationS  float64 `json:"durationS"`
	FirstFrame string  `json:"firstFrame"`
	UpdatedAt  string  `json:"updatedAt"`
}

// VideoOutput 合成产物。
type VideoOutput struct {
	MergedPath    string `json:"mergedPath"`
	FinalPath     string `json:"finalPath"`
	MediaURL      string `json:"mediaUrl"`
	BurnSubtitles bool   `json:"burnSubtitles"`
	Note          string `json:"note"`
	FinishedAt    string `json:"finishedAt"`
}

// VideoRunContext 贯穿全链路的 RunContext（§7）。
type VideoRunContext struct {
	Adapter              string            `json:"adapter"`
	Script               *VideoScript      `json:"script"`
	Prompts              []VideoPromptItem `json:"prompts"`
	Plan                 *VideoPlan        `json:"plan"`
	Subtitles            *VideoSubtitles   `json:"subtitles"`
	Renders              []VideoRender     `json:"renders"`
	Output               *VideoOutput      `json:"output"`
	PublishMeta          *PublishMeta      `json:"publishMeta"`
	Template             *VideoTemplateRun `json:"template"`      // 任务锁定的模板快照（可复现、可审计）
	ConfigSources        map[string]string `json:"configSources"` // 每个字段的来源：template / override / default
	Gates                map[string]string `json:"gates"`
	Warnings             []string          `json:"warnings"`
	Logs                 []string          `json:"logs"`
	Feedback             string            `json:"feedback"`
	ReviewPendingSegment string            `json:"reviewPendingSegment"` // 逐段确认关卡：等待用户确认的片段 ID
}

// VideoSegmentRegen 单段重生成的入参：修改该段提示词/台词后只重渲这一段的下游。
type VideoSegmentRegen struct {
	SegmentID    string `json:"segmentId"`
	VisualPrompt string `json:"visualPrompt"`
	Dialogue     string `json:"dialogue"`
}

// TailFrameAsset 尾帧资产：任务最后一段的尾帧，供其他任务作为首帧续写。
type TailFrameAsset struct {
	ID        string `json:"id"`
	TaskID    string `json:"taskId"`
	TaskTitle string `json:"taskTitle"`
	SegmentID string `json:"segmentId"`
	ProjectID string `json:"projectId"` // 跟随来源任务的归属项目；任务被删后归未归类
	ImagePath string `json:"imagePath"`
	MediaURL  string `json:"mediaUrl"`
	CreatedAt string `json:"createdAt"`
}

// VideoProject 项目：任务的归组容器，项目视图按它看任务与尾帧。
type VideoProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	TaskCount int    `json:"taskCount"` // 以下两个统计字段仅列表查询时填充
	TailCount int    `json:"tailCount"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// VideoTask 以任务为视图的核心实体。
type VideoTask struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Stage      string          `json:"stage"`
	Status     string          `json:"status"`
	Error      string          `json:"error"`
	TemplateID string          `json:"templateId"`
	ProjectID  string          `json:"projectId"`
	Config     VideoRunConfig  `json:"config"`
	Run        VideoRunContext `json:"run"`
	CreatedAt  string          `json:"createdAt"`
	UpdatedAt  string          `json:"updatedAt"`
}

// VideoTaskInput 新建任务入参。
type VideoTaskInput struct {
	Title     string         `json:"title"`
	ProjectID string         `json:"projectId"`
	Config    VideoRunConfig `json:"config"`
}

// VideoPromptEdit 确认门②的局部修改（只改提示词与台词，不动分镜结构）。
type VideoPromptEdit struct {
	ShotID       string `json:"shotId"`
	VisualPrompt string `json:"visualPrompt"`
	Dialogue     string `json:"dialogue"`
	Speaker      string `json:"speaker"` // 留空表示不改台词归属
}

// VideoGateEdits 确认关卡提交的局部修改；按阶段决定生效分支。
type VideoGateEdits struct {
	Shots   []VideoShot       `json:"shots"`
	Prompts []VideoPromptEdit `json:"prompts"`
	Meta    *VideoPublishEdit `json:"meta"`
}

// VideoAdapterInfo 适配器能力声明，供前端展示档位/上限/成本。
// Maturity 说明适配成熟度：目前只有字节跳动火山方舟 Seedance 是主力适配，
// 其余通道只保证了基础生视频链路，不要把「能跑」当成「每个能力都验收过」。
type VideoAdapterInfo struct {
	ID            string            `json:"id"`
	Label         string            `json:"label"`
	Description   string            `json:"description"`
	Maturity      string            `json:"maturity"`
	MaturityLabel string            `json:"maturityLabel"`
	MaturityNote  string            `json:"maturityNote"`
	Capabilities  VideoCapabilities `json:"capabilities"`
}

// VideoEnvironment 运行环境（ffmpeg 可用性、可选适配器、工作目录）。
type VideoEnvironment struct {
	FFmpegAvailable  bool               `json:"ffmpegAvailable"`
	FFmpegPath       string             `json:"ffmpegPath"`
	FFprobeAvailable bool               `json:"ffprobeAvailable"`
	WorkRoot         string             `json:"workRoot"`
	Adapters         []VideoAdapterInfo `json:"adapters"`
}

func ensureVideoGates(run *VideoRunContext) {
	if run.Gates == nil {
		run.Gates = map[string]string{}
	}
	if _, ok := run.Gates[VideoGateScript]; !ok {
		run.Gates[VideoGateScript] = videoGatePending
	}
	if _, ok := run.Gates[VideoGatePrompt]; !ok {
		run.Gates[VideoGatePrompt] = videoGatePending
	}
	if _, ok := run.Gates[VideoGateMeta]; !ok {
		run.Gates[VideoGateMeta] = videoGatePending
	}
}

func defaultVideoSubtitleStyle() VideoSubtitleStyle {
	return VideoSubtitleStyle{
		FontName:     "思源黑体",
		FontSizePct:  4.2,
		PrimaryColor: "#FFFFFF",
		OutlineColor: "#000000",
		OutlineWidth: 2,
		Alignment:    2, // 底部居中
		MarginVPct:   5,
		Bold:         false,
	}
}

func defaultVideoParams() VideoParams {
	return VideoParams{Style: "写实电影感", Camera: "", Mood: ""}
}

func defaultVideoCapabilities() VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:       true,
		LastFrame:           false,
		CharacterReference:  false,
		NativeAudio:         false,
		AllowedDurations:    []int{5, 10},
		MaxClipS:            10,
		AllowedAspectRatios: []string{"16:9", "9:16", "1:1"},
		MaxResolution:       "720P",
		PromptStyle:         "kling",
		CreditsPerClip:      0,
	}
}

func normalizeLanguage(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if strings.HasPrefix(l, "en") {
		return "en-US"
	}
	return "zh-CN"
}

func isEnglishLanguage(lang string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "en")
}

func defaultSpeechRate(lang string) float64 {
	if isEnglishLanguage(lang) {
		return 2.6
	}
	return 4.0
}

func clampFloat(v, min, max float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return min
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func isRemoteRef(ref string) bool {
	lower := strings.ToLower(strings.TrimSpace(ref))
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "data:")
}

// normalizeHexColor 统一为 #RRGGBB，非法输入回退默认值。
func normalizeHexColor(value, fallback string) string {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "#")
	if len(v) == 3 {
		v = string([]byte{v[0], v[0], v[1], v[1], v[2], v[2]})
	}
	if len(v) != 6 {
		v = strings.TrimPrefix(fallback, "#")
	}
	for _, r := range v {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return "#" + strings.ToUpper(strings.TrimPrefix(fallback, "#"))
		}
	}
	return "#" + strings.ToUpper(v)
}

// normalizeVideoConfig 入口归一化与校验：全流程稳定性的第一道闸门（§1）。
func normalizeVideoConfig(cfg VideoRunConfig) (VideoRunConfig, []string, error) {
	warnings := make([]string, 0, 6)

	cfg.Topic = strings.TrimSpace(cfg.Topic)
	if cfg.Topic == "" {
		return cfg, warnings, errors.New("请填写视频主题")
	}
	cfg.FirstFrame = strings.TrimSpace(cfg.FirstFrame)
	cfg.Language = normalizeLanguage(cfg.Language)

	if cfg.DurationS <= 0 {
		cfg.DurationS = 10
	}
	cfg.DurationS = clampInt(cfg.DurationS, 2, 600)
	if cfg.Fps <= 0 {
		cfg.Fps = 24
	}
	cfg.Fps = clampInt(cfg.Fps, 12, 60)

	switch cfg.AspectRatio {
	case "16:9", "9:16", "1:1":
	default:
		cfg.AspectRatio = "16:9"
	}

	switch cfg.ContinuationMode {
	case "none", "last_frame", "overlap":
	default:
		cfg.ContinuationMode = "last_frame"
	}

	if cfg.SpeechRate <= 0 {
		cfg.SpeechRate = defaultSpeechRate(cfg.Language)
	}
	cfg.SpeechRate = round2(clampFloat(cfg.SpeechRate, 1, 10))

	// 字幕样式
	style := cfg.SubtitleStyle
	if strings.TrimSpace(style.FontName) == "" {
		style.FontName = "思源黑体"
	}
	if style.FontSizePct <= 0 {
		style.FontSizePct = 4.2
	}
	style.FontSizePct = round2(clampFloat(style.FontSizePct, 1.2, 12))
	style.PrimaryColor = normalizeHexColor(style.PrimaryColor, "#FFFFFF")
	style.OutlineColor = normalizeHexColor(style.OutlineColor, "#000000")
	if style.OutlineWidth < 0 {
		style.OutlineWidth = 0
	}
	style.OutlineWidth = round2(clampFloat(style.OutlineWidth, 0, 8))
	if style.Alignment < 1 || style.Alignment > 9 {
		style.Alignment = 2
	}
	if style.MarginVPct <= 0 {
		style.MarginVPct = 5
	}
	style.MarginVPct = round2(clampFloat(style.MarginVPct, 0, 30))
	cfg.SubtitleStyle = style

	// 视频参数
	cfg.VideoParams.Style = strings.TrimSpace(cfg.VideoParams.Style)
	cfg.VideoParams.Camera = strings.TrimSpace(cfg.VideoParams.Camera)
	cfg.VideoParams.Mood = strings.TrimSpace(cfg.VideoParams.Mood)
	cfg.VideoParams.ExtraPrompt = strings.TrimSpace(cfg.VideoParams.ExtraPrompt)
	cfg.VideoParams.NegativePrompt = strings.TrimSpace(cfg.VideoParams.NegativePrompt)
	if cfg.VideoParams.Seed < 0 {
		cfg.VideoParams.Seed = 0
	}

	cfg.Adapter = strings.ToLower(strings.TrimSpace(cfg.Adapter))
	cfg.ProviderID = strings.TrimSpace(cfg.ProviderID)
	cfg.TextProviderID = strings.TrimSpace(cfg.TextProviderID)
	cfg.Platforms = normalizePlatforms(cfg.Platforms)
	cfg.Region = normalizeRegion(cfg.Region)
	cfg.Vertical = strings.TrimSpace(cfg.Vertical)
	if cfg.SegmentReview == nil {
		enabled := true
		cfg.SegmentReview = &enabled
	}
	if cfg.StrictFirstFrame == nil {
		enabled := true
		cfg.StrictFirstFrame = &enabled
	}
	if cfg.LocalFallback == nil {
		enabled := true
		cfg.LocalFallback = &enabled
	}

	// 首帧：允许本地路径与 URL，本地路径必须可读
	if cfg.FirstFrame != "" && !isRemoteRef(cfg.FirstFrame) {
		info, err := os.Stat(cfg.FirstFrame)
		if err != nil {
			return cfg, warnings, fmt.Errorf("首帧图片不可读: %v", err)
		}
		if info.IsDir() {
			return cfg, warnings, errors.New("首帧需为图片文件，而不是目录")
		}
	}

	// 角色参考图：不可读的图片降级为警告，避免整条任务被阻塞
	refs := make([]VideoCharacterRef, 0, len(cfg.CharacterRefs))
	for index, ref := range cfg.CharacterRefs {
		ref.RoleID = strings.TrimSpace(ref.RoleID)
		ref.Name = strings.TrimSpace(ref.Name)
		ref.VoiceHint = strings.TrimSpace(ref.VoiceHint)
		ref.ImageSource = strings.ToLower(strings.TrimSpace(ref.ImageSource))
		if ref.Name == "" && ref.RoleID == "" && len(ref.Images) == 0 {
			continue
		}
		if ref.RoleID == "" {
			ref.RoleID = fmt.Sprintf("char_%d", index+1)
		}
		if ref.Name == "" {
			ref.Name = ref.RoleID
		}
		images := make([]string, 0, len(ref.Images))
		for _, image := range ref.Images {
			image = strings.TrimSpace(image)
			if image == "" {
				continue
			}
			if !isRemoteRef(image) {
				if info, err := os.Stat(image); err != nil || info.IsDir() {
					warnings = append(warnings, fmt.Sprintf("角色「%s」的参考图不可读，已忽略：%s", ref.Name, image))
					continue
				}
			}
			images = append(images, image)
		}
		ref.Images = images
		// 没标来源的（手填 / 老任务）都是用户自己给的图，按用户上传处理并享受优先权；
		// 图全被剔掉时清掉标记，免得留着一个没有图的“优先”身份。
		if ref.ImageSource == "" {
			ref.ImageSource = CharacterImageSourceUser
		}
		if len(images) == 0 {
			ref.ImageSource = ""
		}
		refs = append(refs, ref)
	}
	cfg.CharacterRefs = refs

	// 分镜来源：手动分段就地归一（段时长保真），目标时长以各段之和为准
	cfg.ScriptMode = normalizeVideoScriptMode(cfg.ScriptMode)
	if cfg.manualScriptEnabled() {
		shots, shotWarnings, shotErr := normalizeManualShots(cfg)
		if shotErr != nil {
			return cfg, warnings, shotErr
		}
		cfg.ManualShots = shots
		cfg.DurationS = int(math.Round(manualTotalSeconds(shots)))
		warnings = append(warnings, shotWarnings...)
	} else {
		// 自动模式不保留手填分段，免得以后被误当成权威来源
		cfg.ManualShots = nil
	}

	return cfg, warnings, nil
}

// speechUnitCount 中文按字符、英文按词统计（去空格）。
func speechUnitCount(text, lang string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	if isEnglishLanguage(lang) {
		return len(strings.Fields(text))
	}
	count := 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if unicode.IsPunct(r) {
			continue
		}
		count++
	}
	return count
}

// estimateSpeechSeconds 台词时长模型（§4.2）：字数 / 语速 + 每句停顿缓冲。
func estimateSpeechSeconds(text, lang string, rate float64) float64 {
	units := speechUnitCount(text, lang)
	if units == 0 {
		return 0
	}
	if rate <= 0 {
		rate = defaultSpeechRate(lang)
	}
	return round2(float64(units)/rate + 0.3)
}

// nearestAllowedDuration 把任意时长夹取到模型支持的离散档位，避免厂商侧直接拒绝。
func nearestAllowedDuration(duration float64, allowed []int) int {
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}
	best := allowed[0]
	bestDiff := math.Abs(float64(best) - duration)
	for _, candidate := range allowed[1:] {
		diff := math.Abs(float64(candidate) - duration)
		if diff < bestDiff || (diff == bestDiff && candidate < best) {
			best = candidate
			bestDiff = diff
		}
	}
	return best
}

func maxAllowedDuration(allowed []int) int {
	max := 0
	for _, v := range allowed {
		if v > max {
			max = v
		}
	}
	return max
}

// splitShotDurations 超过单段上限时按档位切分（§5.3）。
func splitShotDurations(duration float64, allowed []int, maxClip float64) []int {
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}
	if duration <= 0 {
		duration = float64(allowed[0])
	}
	if maxClip <= 0 {
		maxClip = float64(maxAllowedDuration(allowed))
	}
	parts := int(math.Ceil(duration / maxClip))
	if parts < 1 {
		parts = 1
	}
	if parts > 12 {
		parts = 12
	}
	per := duration / float64(parts)
	out := make([]int, 0, parts)
	for i := 0; i < parts; i++ {
		out = append(out, nearestAllowedDuration(per, allowed))
	}
	return out
}

// speakerLabel 把台词归属翻译成提示词里能读的名字：旁白就叫旁白，角色用角色名。
func speakerLabel(cfg VideoRunConfig, speaker string) string {
	speaker = strings.TrimSpace(speaker)
	switch speaker {
	case "":
		return ""
	case VideoSpeakerNarration:
		return "旁白"
	}
	return firstNonEmpty(characterDisplayName(cfg, speaker), speaker)
}

// buildVideoPlan 依据分镜提示词与适配器能力生成拼接/成本计划。
func buildVideoPlan(cfg VideoRunConfig, prompts []VideoPromptItem, caps VideoCapabilities, adapterID string) VideoPlan {
	plan := VideoPlan{Adapter: adapterID, Segments: make([]VideoSegment, 0, len(prompts))}
	if len(prompts) == 0 {
		plan.Warnings = append(plan.Warnings, "没有可渲染的分镜提示词")
		return plan
	}
	allowed := caps.AllowedDurations
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}
	maxClip := float64(caps.MaxClipS)
	if maxClip <= 0 {
		maxClip = float64(maxAllowedDuration(allowed))
	}

	cursor := 0.0
	for _, item := range prompts {
		duration := item.End - item.Start
		if duration <= 0.4 {
			duration = float64(cfg.DurationS) / float64(len(prompts))
		}
		parts := splitShotDurations(duration, allowed, maxClip)
		for index, part := range parts {
			segment := VideoSegment{
				ID:        fmt.Sprintf("%s-%d", item.ShotID, index+1),
				ShotID:    item.ShotID,
				Index:     len(plan.Segments),
				Prompt:    item.VisualPrompt,
				Start:     round2(cursor),
				End:       round2(cursor + float64(part)),
				DurationS: part,
			}
			if index == 0 {
				segment.Dialogue = item.Dialogue
				// 台词只在首段下发，归属跟着一起下：下游才能决定是给谁拍口型，还是按旁白处理
				segment.Speaker = item.Speaker
				segment.SpeakerName = speakerLabel(cfg, item.Speaker)
			}
			plan.Segments = append(plan.Segments, segment)
			cursor += float64(part)
		}
	}

	plan.ClipCount = len(plan.Segments)
	plan.TotalS = round2(cursor)
	plan.Serial = cfg.ContinuationMode == "last_frame" && caps.LastFrame
	plan.EstCredits = round2(float64(plan.ClipCount) * caps.CreditsPerClip)

	if diff := math.Abs(plan.TotalS - float64(cfg.DurationS)); diff >= 1 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"模型时长档位 %v 让实际总时长变为 %.1fs（目标 %ds），时间轴按实际时长生成",
			allowed, plan.TotalS, cfg.DurationS,
		))
	}
	if cfg.ContinuationMode == "last_frame" && !caps.LastFrame {
		plan.Warnings = append(plan.Warnings, "该模型不支持尾帧条件，衔接已降级为硬切/交叉淡化")
	}
	if cfg.ContinuationMode == "overlap" {
		plan.Warnings = append(plan.Warnings, "overlap 衔接在合成阶段按硬切处理（xfade 交叉淡化待接入）")
	}
	if caps.CreditsPerClip <= 0 {
		plan.Warnings = append(plan.Warnings, "该适配器未声明单段单价，成本预估仅统计段数")
	}
	return plan
}

// SubtitleSpan 一段台词在成片时间轴上实际占据的窗口。
// 合成阶段会用片段真实时长来填它：模型经常把 5s 档位渲染成 5.04s，
// 逐段累加后计划时间轴整体漂移，字幕就会和画面对不上。
type SubtitleSpan struct {
	Start      float64
	Duration   float64
	Text       string
	FillWindow bool // 片段自带配音时语速由模型掌控，台词铺满整段而不是按字速估算
}

// plannedSubtitleSpans 把预算的分镜时间轴当作字幕窗口（无实测数据时的兜底）。
func plannedSubtitleSpans(plan VideoPlan) []SubtitleSpan {
	spans := make([]SubtitleSpan, 0, len(plan.Segments))
	for _, segment := range plan.Segments {
		spans = append(spans, SubtitleSpan{
			Start:    segment.Start,
			Duration: segment.End - segment.Start,
			Text:     segment.Dialogue,
		})
	}
	return spans
}

// buildSubtitleCues 渲染前按预算时间轴生成字幕（估算值，供确认门预览与 SRT 下载）。
func buildSubtitleCues(plan VideoPlan, cfg VideoRunConfig) []VideoSubtitleCue {
	return buildSubtitleCuesFromSpans(cfg, plannedSubtitleSpans(plan))
}

// buildSubtitleCuesFromSpans 把台词铺进每一段的真实时间窗：
// 一句一条，按字数分配停留时长，保证单调、不重叠、不越界，
// 让字幕切换节奏跟得上对白节奏，而不是一整块台词挂满整段。
func buildSubtitleCuesFromSpans(cfg VideoRunConfig, spans []SubtitleSpan) []VideoSubtitleCue {
	cues := make([]VideoSubtitleCue, 0, len(spans))
	for _, span := range spans {
		cues = appendEstimatedCues(cues, cfg, span)
	}
	return cues
}

// appendEstimatedCues 按字速估算把一段台词铺进它的时间窗（声学对齐无活动区间时也退回这里）。
func appendEstimatedCues(cues []VideoSubtitleCue, cfg VideoRunConfig, span SubtitleSpan) []VideoSubtitleCue {
	text := strings.TrimSpace(span.Text)
	if text == "" || span.Duration <= 0 {
		return cues
	}
	window := span.Duration - 0.2 - 0.15 // 段尾留白：避免字幕压住切镜瞬间
	if window < 0.3 {
		window = math.Max(0.15, span.Duration-0.2)
	}
	if window <= 0 {
		return cues
	}
	estimate := estimateSpeechSeconds(text, cfg.Language, cfg.SpeechRate)
	// 台词比窗口短就按自然语速收尾（别在静音处挂着字幕），比窗口长就铺满整段
	spoken := math.Min(math.Max(estimate, 0.6), window)
	if span.FillWindow {
		spoken = window
	}

	parts := splitDialogueSentences(text)
	if len(parts) == 0 {
		return cues
	}
	weights := make([]float64, len(parts))
	total := 0.0
	for index, part := range parts {
		weights[index] = math.Max(1, float64(len([]rune(part))))
		total += weights[index]
	}
	// 每条至少停留 0.9s，再等比缩回窗口内，总和仍等于 spoken
	slots := make([]float64, len(parts))
	sum := 0.0
	for index, weight := range weights {
		slots[index] = math.Max(0.9, spoken*weight/total)
		sum += slots[index]
	}
	if scale := spoken / sum; scale < 1 {
		for index := range slots {
			slots[index] *= scale
		}
	}

	const lead = 0.2 // 段首留白：起音瞬间不上字幕
	cursor := span.Start + lead
	limit := span.Start + lead + spoken
	for index, part := range parts {
		bound := cursor + slots[index]
		if bound > limit {
			bound = limit
		}
		if index == len(parts)-1 {
			bound = limit // 末句吃掉取整误差，避免字幕提前消失
		}
		// 句间留 0.08s 空隙，读起来才有“换句”的感觉
		display := bound - 0.08
		if index == len(parts)-1 {
			display = bound
		}
		if display > cursor {
			cues = append(cues, VideoSubtitleCue{
				Index: len(cues) + 1,
				Start: round2(cursor),
				End:   round2(display),
				Text:  wrapSubtitleText(part, cfg.Language),
			})
		}
		cursor = bound
	}
	return cues
}

// splitDialogueSentences 按句末标点/换行把台词拆成字幕行，过短的碎片并回上一句，
// 避免“嗯”“然后”这类词单独闪一条字幕。
func splitDialogueSentences(text string) []string {
	ender := func(r rune) bool {
		switch r {
		case '。', '！', '？', '…', '；', '!', '?', ';', '\n', '\r':
			return true
		}
		return false
	}
	raw := make([]string, 0, 4)
	start := 0
	runes := []rune(text)
	for index, r := range runes {
		if !ender(r) {
			continue
		}
		if piece := strings.TrimSpace(string(runes[start:index])); piece != "" {
			raw = append(raw, piece)
		}
		start = index + 1
	}
	if tail := strings.TrimSpace(string(runes[start:])); tail != "" {
		raw = append(raw, tail)
	}

	parts := make([]string, 0, len(raw))
	for _, piece := range raw {
		if len(piece) > 0 && len([]rune(piece)) < 4 && len(parts) > 0 {
			parts[len(parts)-1] = parts[len(parts)-1] + "，" + piece
			continue
		}
		parts = append(parts, piece)
	}
	if len(parts) == 0 && strings.TrimSpace(text) != "" {
		parts = append(parts, strings.TrimSpace(text))
	}
	return parts
}

// wrapSubtitleText 建议断行：中文每行 ≤18 字，英文每行 ≤42 字符。
func wrapSubtitleText(text, lang string) string {
	limit := 18
	if isEnglishLanguage(lang) {
		limit = 42
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	lines := make([]string, 0, 3)
	for len(runes) > 0 {
		cut := limit
		if len(runes) < cut {
			cut = len(runes)
		}
		lines = append(lines, string(runes[:cut]))
		runes = runes[cut:]
		if len(lines) == 2 {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// aspectResolution 与 ASS PlayRes 对应的基准分辨率（§6.3）。
func aspectResolution(aspect string) (int, int) {
	switch aspect {
	case "9:16":
		return 720, 1280
	case "1:1":
		return 960, 960
	default:
		return 1280, 720
	}
}

// ratioValue 把 "9:16" 这类画幅字符串算成宽/高比值，无法解析时返回 false。
func ratioValue(aspect string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(aspect), ":")
	if len(parts) != 2 {
		return 0, false
	}
	width, widthErr := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	height, heightErr := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return 0, false
	}
	return width / height, true
}

// targetCanvas 把源画面换算到目标画幅的具体像素：以源短边为基准，
// 避免把 720p 素材拉成 1080p 徒增体积，也避免横转竖时把画面缩得太小。
// 源画幅已与目标一致（差 2% 以内）时返回 false，省一次重编码。
func targetCanvas(aspect string, srcWidth, srcHeight int) (int, int, bool) {
	target, ok := ratioValue(aspect)
	if !ok || srcWidth <= 0 || srcHeight <= 0 {
		return 0, 0, false
	}
	source := float64(srcWidth) / float64(srcHeight)
	if math.Abs(source-target)/target < 0.02 {
		return srcWidth, srcHeight, false
	}
	eveng := func(value float64) int {
		rounded := int(math.Round(value))
		if rounded%2 != 0 {
			rounded--
		}
		if rounded < 2 {
			rounded = 2
		}
		return rounded
	}
	short := math.Min(float64(srcWidth), float64(srcHeight))
	switch {
	case target > 1:
		return eveng(short * target), eveng(short), true
	case target < 1:
		return eveng(short), eveng(short / target), true
	default:
		side := eveng(short)
		return side, side, true
	}
}

// defaultVideoTitle 用主题生成任务标题。
func defaultVideoTitle(topic string) string {
	runes := []rune(strings.TrimSpace(topic))
	if len(runes) > 24 {
		return string(runes[:24]) + "…"
	}
	if len(runes) == 0 {
		return "未命名视频任务"
	}
	return string(runes)
}

func newVideoTaskID() string {
	return fmt.Sprintf("vt-%d-%s", time.Now().UnixNano(), randomHex(4))
}

func videoMediaURL(taskID, rel string) string {
	return "/media/" + taskID + "/" + strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
}
