package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// 视频生成流水线的阶段（与《视频生成工具·架构与最佳实践设计》§2 一致，
// 并在合成之后接入 Agent4 发布元数据与 gate③）。
// 人工确认关卡本身也是一个阶段，处于该阶段时任务状态为 waiting。
// 剪辑与字幕排在视频生成之后：先拿到真实片段，生成默认剪辑计划、预览画布与字幕时间轴，
// 再让用户在同一页里调片段与字幕；确认后合成阶段只负责烧录，不再重排时间轴。
const (
	VideoStageDraftScript   = "DRAFT_SCRIPT"
	VideoStageConfirmScript = "CONFIRM_SCRIPT"
	VideoStageBuildPrompt   = "BUILD_PROMPT"
	VideoStageConfirmPrompt = "CONFIRM_PROMPT"
	VideoStageRender        = "RENDER_VIDEO"
	VideoStageBuildEdit     = "BUILD_EDIT"
	VideoStageConfirmEdit   = "CONFIRM_EDIT"
	VideoStageCompose       = "COMPOSE"
	VideoStageBuildMeta     = "BUILD_PUBLISH_META"
	VideoStageConfirmMeta   = "CONFIRM_META"
	VideoStageDone          = "DONE"
)

// legacyVideoGateSubs v0.1.1 及更早版本的剪辑关卡键；读取任务时迁移到 VideoGateEdit。
const legacyVideoGateSubs = "gate_subs"

// legacyVideoStages 把历史任务里存过的旧阶段名映射到当前阶段名：
// v0.0.5 之前提示词与字幕挤在同一阶段（BUILD_PROMPT_SUBS），现在拆成两段；
// v0.1.2 把字幕阶段并入剪辑（BUILD_SUBS / CONFIRM_SUBS）。
var legacyVideoStages = map[string]string{
	"BUILD_PROMPT_SUBS": VideoStageBuildPrompt,
	"BUILD_SUBS":        VideoStageBuildEdit,
	"CONFIRM_SUBS":      VideoStageConfirmEdit,
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

// 确认关卡：分镜 → 提示词 → 剪辑与字幕 → 发布元数据（剪辑关卡始终出现，字幕可以关闭）
const (
	VideoGateScript = "gate1"
	VideoGatePrompt = "gate2"
	VideoGateEdit   = "gate_edit"
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

// VideoVoiceProfile 角色音色档案：需求 5 先把结构存下来，需求 10 接入 TTS 后可直接复用。
// 未配置时为零值指针，JSON 里不出现，旧任务不受影响。
type VideoVoiceProfile struct {
	VoiceID string  `json:"voiceId"`
	Rate    float64 `json:"rate"`   // 语速倍率（1 = 正常）
	Pitch   float64 `json:"pitch"`  // 音高偏移（半音）
	Volume  float64 `json:"volume"` // 音量（0-2，1 = 原始音量）
	Emotion string  `json:"emotion"`
}

// VideoCharacter 角色库条目：跨任务复用的角色档案。
// 任务创建时会把档案「快照」进 VideoCharacterRef，因此角色库后续修改不会改写旧任务。
type VideoCharacter struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	RoleID          string             `json:"roleId"`
	AppearanceBlock string             `json:"appearanceBlock"`
	Persona         string             `json:"persona"`
	NegativePrompt  string             `json:"negativePrompt"`
	VoiceHint       string             `json:"voiceHint"`
	VoiceProfile    *VideoVoiceProfile `json:"voiceProfile"`
	Perspective     string             `json:"perspective"`
	Images          []string           `json:"images"`
	Tags            []string           `json:"tags"`
	Note            string             `json:"note"`
	CreatedAt       string             `json:"createdAt"`
	UpdatedAt       string             `json:"updatedAt"`
}

// VideoCharacterRef 多角色参考图：每个角色一张或多张定妆图，用于跨段保持外观一致。
// 角色档案字段（AppearanceBlock / Persona / NegativePrompt / Perspective / VoiceProfile）
// 允许任务级覆盖；为空时由 CreateVideoTask 从绑定的角色库条目补全并快照。
type VideoCharacterRef struct {
	RoleID string `json:"roleId"`
	Name   string `json:"name"`
	// CharacterID 绑定的角色库条目；旧任务没有该字段，仍按纯角色参考图方式工作。
	CharacterID string   `json:"characterId,omitempty"`
	Images      []string `json:"images"`
	// ImageSource 标记 Images 的来源，决定提交顺序与提示词里的措辞权重
	ImageSource     string             `json:"imageSource"`
	AppearanceBlock string             `json:"appearanceBlock,omitempty"`
	Persona         string             `json:"persona,omitempty"`
	NegativePrompt  string             `json:"negativePrompt,omitempty"`
	Perspective     string             `json:"perspective,omitempty"`
	VoiceHint       string             `json:"voiceHint"`
	VoiceProfile    *VideoVoiceProfile `json:"voiceProfile,omitempty"`
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
	Subtitles        bool                `json:"subtitles"` // 是否生成并烧录字幕（0.1.2 起新任务默认开启）
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

// VideoDialogueLine 一句台词及其说话人。一段可以有多个说话人，
// 因此台词不能只用一个 speaker 字段承载；Dialogue / Speaker 保留为单说话人兼容字段。
type VideoDialogueLine struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// VideoShot Agent1 的分镜数组元素。
type VideoShot struct {
	ID          string     `json:"id"`
	TimeRange   [2]float64 `json:"timeRange"`
	Roles       []string   `json:"roles"`
	SceneIntent string     `json:"sceneIntent"`
	// Dialogue 所有台词按行拼接后的兼容字段；多说话人时以 DialogueLines 为准。
	Dialogue string `json:"dialogue"`
	// Speaker 只有单一说话人时才保留（role_id 或 VideoSpeakerNarration）；无台词或多人时为空。
	Speaker       string              `json:"speaker"`
	DialogueLines []VideoDialogueLine `json:"dialogueLines,omitempty"`
	Camera        string              `json:"camera"`
	Mood          string              `json:"mood"`
	AudioCue      string              `json:"audioCue"`
	DurationS     float64             `json:"durationS"`
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
	ShotID       string `json:"shotId"`
	VisualPrompt string `json:"visualPrompt"`
	// Dialogue 所有台词按行拼接后的兼容字段；多说话人时以 DialogueLines 为准。
	Dialogue string `json:"dialogue"`
	// Speaker 只有单一说话人时才保留；多说话人时为空。
	Speaker         string              `json:"speaker"`
	DialogueLines   []VideoDialogueLine `json:"dialogueLines,omitempty"`
	Start           float64             `json:"start"`
	End             float64             `json:"end"`
	CharCount       int                 `json:"charCount"`
	EstSpeechS      float64             `json:"estSpeechS"`
	Fits            bool                `json:"fits"`
	RenderDurationS int                 `json:"renderDurationS"`
	Note            string              `json:"note"`
}

// VideoSegment 渲染段：分镜按模型时长档位切分后的最小生成单元。
type VideoSegment struct {
	ID     string `json:"id"`
	ShotID string `json:"shotId"`
	Index  int    `json:"index"`
	Prompt string `json:"prompt"`
	// Dialogue 所有台词按行拼接后的兼容字段；多说话人时以 DialogueLines 为准。
	Dialogue string `json:"dialogue"`
	// Speaker 只有单一说话人时才保留；多说话人时为空。
	Speaker string `json:"speaker"`
	// SpeakerName 写进提示词的显示名；多说话人时为“甲、乙”这类可读标签。
	SpeakerName   string              `json:"speakerName"`
	DialogueLines []VideoDialogueLine `json:"dialogueLines,omitempty"`
	Start         float64             `json:"start"`
	End           float64             `json:"end"`
	DurationS     int                 `json:"durationS"`
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
// 位置与字号支持逐条覆盖：字幕确认关卡里用户可以为某一句单独挪位置、放大字号（ASS numpad 1-9）。
type VideoSubtitleCue struct {
	Index int     `json:"index"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	// Anchor ≤0 表示沿用全局样式的对齐；其余为 ASS numpad 编码（1 左下 … 9 右上）
	Anchor int `json:"anchor,omitempty"`
	// MarginVPct 距对齐边的垂直偏移（画面高度百分比），≤0 表示沿用全局样式
	MarginVPct float64 `json:"marginVpct,omitempty"`
	// FontSizePct 字号（画面高度百分比），≤0 表示沿用全局样式
	FontSizePct float64 `json:"fontSizePct,omitempty"`
	// SegmentID 自动生成的字幕归属片段；剪辑重排/裁剪时按它做段内重映射
	SegmentID string `json:"segmentId,omitempty"`
	// OffsetS 相对片段起点的偏移（秒），由保存时按当前剪辑计划重算
	OffsetS float64 `json:"offsetS,omitempty"`
}

// VideoSubtitles 字幕产物（SRT + ASS）。
// Confirmed 标记用户是否在字幕关卡里确认过：确认后合成阶段只烧录，不再按测时重排。
type VideoSubtitles struct {
	SRTPath         string             `json:"srtPath"`
	ASSPath         string             `json:"assPath"`
	SRTUrl          string             `json:"srtUrl"`
	ASSUrl          string             `json:"assUrl"`
	TimingTablePath string             `json:"timingTablePath"`
	TimingTableURL  string             `json:"timingTableUrl"`
	TimingSource    string             `json:"timingSource"` // estimate|measured|tts|aligned
	Cues            []VideoSubtitleCue `json:"cues"`
	// Confirmed 用户在 CONFIRM_EDIT 里确认过时间轴与位置，合成时以它为准
	Confirmed bool   `json:"confirmed"`
	EditedAt  string `json:"editedAt,omitempty"`
}

// VideoEditClip 剪辑计划里的一个片段：顺序、启停与入出点。
// 顺序只在 none/overlap 续接模式下允许调整；last_frame 必须保持渲染顺序。
type VideoEditClip struct {
	SegmentID       string  `json:"segmentId"`
	ShotID          string  `json:"shotId,omitempty"`
	Order           int     `json:"order"`           // 0-based，越小越靠前
	Enabled         bool    `json:"enabled"`         // false 表示该段不入片
	InPoint         float64 `json:"inPoint"`         // 相对源片段起点，秒
	OutPoint        float64 `json:"outPoint"`        // 相对源片段起点，秒；0 表示到片尾
	SourceDurationS float64 `json:"sourceDurationS"` // 只读：probe 出来的源时长
}

// VideoEditPlan 用户可编辑的剪辑计划；Revision 只在片段顺序/启停/入出点变化时 +1。
type VideoEditPlan struct {
	Clips     []VideoEditClip `json:"clips"`
	Revision  int             `json:"revision"`
	UpdatedAt string          `json:"updatedAt"`
}

// VideoSubtitleIssue 字幕校验问题：error 级禁止确认合成，warning 只提示。
type VideoSubtitleIssue struct {
	Index   int    `json:"index"`
	Level   string `json:"level"` // error|warning
	Code    string `json:"code"`
	Message string `json:"message"`
}

// VideoEditEdit 剪辑页保存/校验的整表提交：片段计划 + 字幕草稿。
type VideoEditEdit struct {
	Clips []VideoEditClip    `json:"clips"`
	Cues  []VideoSubtitleCue `json:"cues"`
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

// VideoComposePrep 合成前的剪辑预处理产物：选段 → 冻帧裁剪 → 拼接 → 画幅归一。
// BUILD_EDIT 先把它算出来，字幕时间轴才能按真实成片测量；
// 合成阶段直接复用，避免二次裁切/二次拼接，也避免用户调好的字幕被重排。
type VideoComposePrep struct {
	MergedPath        string            `json:"mergedPath"` // 拼接后（未归一画幅）的素材
	CanvasPath        string            `json:"canvasPath"` // 归一画幅后的画布，字幕预览与烧录都用它
	CanvasUrl         string            `json:"canvasUrl"`  // 前端预览地址
	CanvasWidth       int               `json:"canvasWidth"`
	CanvasHeight      int               `json:"canvasHeight"`
	ComposedBySegment map[string]string `json:"composedBySegment"` // 段 ID → 实际入片文件（裁剪后的）
	SegmentIDs        []string          `json:"segmentIds"`        // 实际入片段顺序，用于判断缓存是否过期
	EditRevision      int               `json:"editRevision"`      // 与 Edit.Revision 对齐；不等就重建
	DurationS         float64           `json:"durationS"`         // 画布实测总时长，前端时间轴上限
	HasFFmpeg         bool              `json:"hasFFmpeg"`
	UpdatedAt         string            `json:"updatedAt"`
}

// VideoRunContext 贯穿全链路的 RunContext（§7）。
type VideoRunContext struct {
	Adapter              string               `json:"adapter"`
	Script               *VideoScript         `json:"script"`
	Prompts              []VideoPromptItem    `json:"prompts"`
	Plan                 *VideoPlan           `json:"plan"`
	Edit                 *VideoEditPlan       `json:"edit,omitempty"`
	Subtitles            *VideoSubtitles      `json:"subtitles"`
	SubtitleIssues       []VideoSubtitleIssue `json:"subtitleIssues,omitempty"`
	Renders              []VideoRender        `json:"renders"`
	Compose              *VideoComposePrep    `json:"compose"`
	Output               *VideoOutput         `json:"output"`
	PublishMeta          *PublishMeta         `json:"publishMeta"`
	Template             *VideoTemplateRun    `json:"template"`      // 任务锁定的模板快照（可复现、可审计）
	ConfigSources        map[string]string    `json:"configSources"` // 每个字段的来源：template / override / default
	Gates                map[string]string    `json:"gates"`
	Warnings             []string             `json:"warnings"`
	Logs                 []string             `json:"logs"`
	Feedback             string               `json:"feedback"`
	ReviewPendingSegment string               `json:"reviewPendingSegment"` // 逐段确认关卡：等待用户确认的片段 ID
	// ReviewPendingPromptSegment 逐段确认的「生成前」关卡：下一段尚未消耗额度，等待用户确认提示词。
	ReviewPendingPromptSegment string `json:"reviewPendingPromptSegment"`
	// ApprovedSegmentPrompts 已确认过提示词、可以直接生成的片段 ID：
	// 只放行用户明确确认过的段，避免每次续跑都重复弹确认。
	ApprovedSegmentPrompts []string `json:"approvedSegmentPrompts,omitempty"`
}

// VideoSegmentRegen 单段重生成的入参：修改该段提示词/台词后只重渲这一段的下游。
type VideoSegmentRegen struct {
	SegmentID    string `json:"segmentId"`
	VisualPrompt string `json:"visualPrompt"`
	// Dialogue / Speaker 单说话人兼容字段；多说话人用 DialogueLines。
	Dialogue      string              `json:"dialogue"`
	Speaker       string              `json:"speaker"`
	DialogueLines []VideoDialogueLine `json:"dialogueLines,omitempty"`
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

// VideoPromptEdit 确认门②的局部修改（只改提示词、台词与本段时长，不动分镜结构）。
type VideoPromptEdit struct {
	ShotID       string `json:"shotId"`
	VisualPrompt string `json:"visualPrompt"`
	// Dialogue / Speaker 单说话人兼容字段；多说话人用 DialogueLines。
	Dialogue      string              `json:"dialogue"`
	Speaker       string              `json:"speaker"` // 留空表示不改台词归属
	DialogueLines []VideoDialogueLine `json:"dialogueLines,omitempty"`
	// DurationS 本段目标时长（秒）；≤0 表示沿用原时长。
	// 改时长会重排后续镜头的时间轴，渲染时仍会按模型档位切分并对差额给出警告。
	DurationS float64 `json:"durationS"`
}

// VideoSubtitleEdit 字幕关卡的整表提交：时间轴、文本、位置、字号由用户在这一步定稿。
// 整表覆盖而不是逐条 diff：字幕条数少，用户增删一条也该重排 index，整体提交最不容易出错。
type VideoSubtitleEdit struct {
	Cues []VideoSubtitleCue `json:"cues"`
}

// VideoGateEdits 确认关卡提交的局部修改；按阶段决定生效分支。
type VideoGateEdits struct {
	Shots     []VideoShot        `json:"shots"`
	Prompts   []VideoPromptEdit  `json:"prompts"`
	Meta      *VideoPublishEdit  `json:"meta"`
	Subtitles *VideoSubtitleEdit `json:"subtitles"`
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
	if _, ok := run.Gates[VideoGateEdit]; !ok {
		// 旧任务的 gate_subs 状态迁移过来：已批准的剪辑/字幕不该因为改名又变回待确认
		state := videoGatePending
		if legacy, ok := run.Gates[legacyVideoGateSubs]; ok && strings.TrimSpace(legacy) != "" {
			state = legacy
		}
		run.Gates[VideoGateEdit] = state
	}
	if _, ok := run.Gates[VideoGateMeta]; !ok {
		run.Gates[VideoGateMeta] = videoGatePending
	}
}

func defaultVideoSubtitleStyle() VideoSubtitleStyle {
	return VideoSubtitleStyle{
		FontName:     "Noto Sans SC",
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
		style.FontName = "Noto Sans SC"
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
		ref.CharacterID = strings.TrimSpace(ref.CharacterID)
		ref.AppearanceBlock = strings.TrimSpace(ref.AppearanceBlock)
		ref.Persona = strings.TrimSpace(ref.Persona)
		ref.NegativePrompt = strings.TrimSpace(ref.NegativePrompt)
		ref.Perspective = strings.TrimSpace(ref.Perspective)
		ref.VoiceHint = strings.TrimSpace(ref.VoiceHint)
		ref.VoiceProfile = normalizeVideoVoiceProfile(ref.VoiceProfile)
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
	// role_id 是台词归属的唯一锚点：重复标识会让“谁在说”永远绑不到确定的人。
	// 新建任务时直接拦截；旧任务的数据在加载时不会走这里，不受影响。
	seenRoleIDs := make(map[string]string, len(refs))
	for _, ref := range refs {
		key := normalizeRoleKey(ref.RoleID)
		if key == "" {
			continue
		}
		if previous, ok := seenRoleIDs[key]; ok {
			return cfg, warnings, fmt.Errorf("角色标识 %q 重复（%s 与 %s）：每个角色的 role_id 必须唯一", ref.RoleID, previous, ref.Name)
		}
		seenRoleIDs[key] = ref.Name
	}

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

// normalizeVideoVoiceProfile 音色档案做宽松归一：空值保持为空，数值夹到可用区间。
// 需求 5 只负责保存与展示，真正的 TTS 参数在需求 10 接入后才会被渲染层消费。
func normalizeVideoVoiceProfile(profile *VideoVoiceProfile) *VideoVoiceProfile {
	if profile == nil {
		return nil
	}
	normalized := *profile
	normalized.VoiceID = strings.TrimSpace(normalized.VoiceID)
	normalized.Emotion = strings.TrimSpace(normalized.Emotion)
	if normalized.Rate != 0 {
		normalized.Rate = round2(clampFloat(normalized.Rate, 0.5, 2))
	}
	if normalized.Pitch != 0 {
		normalized.Pitch = round2(clampFloat(normalized.Pitch, -12, 12))
	}
	if normalized.Volume != 0 {
		normalized.Volume = round2(clampFloat(normalized.Volume, 0, 2))
	}
	if normalized.VoiceID == "" && normalized.Rate == 0 && normalized.Pitch == 0 &&
		normalized.Volume == 0 && normalized.Emotion == "" {
		return nil
	}
	return &normalized
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

// speechPauseBufferS 每句台词的起音/收尾缓冲：估算与裁剪必须用同一个口径，
// 否则会出现「按语速刚好写满，却被判定超时」的自相矛盾。
const speechPauseBufferS = 0.3

// speechUnitBudget 本段台词可用字数/词数：先扣掉停顿缓冲，再按语速换算。
// 中文按字、英文按词，与 speechUnitCount 的统计口径一致。
func speechUnitBudget(duration, rate float64, lang string) int {
	if rate <= 0 {
		rate = defaultSpeechRate(lang)
	}
	available := duration - speechPauseBufferS
	if available <= 0 {
		return 0
	}
	return int(math.Floor(available * rate))
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
	return round2(float64(units)/rate + speechPauseBufferS)
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
// 旧实现先平均再逐段就近取档，余数会被整段丢掉（21s → 3×7 → 3×5 = 15s）；
// 这里改成在“段数 × 允许档位”的组合里找总和最接近目标的一组，同样差额优先段数更少。
func splitShotDurations(duration float64, allowed []int, maxClip float64) []int {
	allowed = normalizeAllowedDurations(allowed)
	if duration <= 0 {
		duration = float64(allowed[0])
	}
	if maxClip <= 0 {
		maxClip = float64(allowed[len(allowed)-1])
	}
	// 单段上限必须压过档位：宁可拆更多段，也不提交模型不接受的时长。
	fitted := make([]int, 0, len(allowed))
	for _, value := range allowed {
		if float64(value) <= maxClip {
			fitted = append(fitted, value)
		}
	}
	if len(fitted) > 0 {
		allowed = fitted
	} else {
		// 档位全都超过单段上限：退回上限本身，至少保证不提交模型不接受的时长。
		capacity := int(math.Floor(maxClip))
		if capacity < 1 {
			capacity = 1
		}
		allowed = []int{capacity}
	}
	partsMin := int(math.Ceil(duration / maxClip))
	if partsMin < 1 {
		partsMin = 1
	}
	if partsMin > 12 {
		partsMin = 12
	}
	partsMax := int(math.Ceil(duration / float64(allowed[0])))
	if partsMax < partsMin {
		partsMax = partsMin
	}
	if partsMax > 12 {
		partsMax = 12
	}

	best := []int(nil)
	bestDiff := math.Inf(1)
	for parts := partsMin; parts <= partsMax; parts++ {
		combo := closestDurationCombo(duration, parts, allowed)
		if len(combo) == 0 {
			continue
		}
		sum := 0
		for _, part := range combo {
			sum += part
		}
		diff := math.Abs(float64(sum) - duration)
		// 差额相同时保留段数更少的方案：少一段就少一次生成成本。
		if diff < bestDiff-1e-9 {
			best = combo
			bestDiff = diff
		}
	}
	if len(best) == 0 {
		return []int{nearestAllowedDuration(duration, allowed)}
	}
	return best
}

// normalizeAllowedDurations 去重、升序，并过滤掉非正数档位。
func normalizeAllowedDurations(allowed []int) []int {
	if len(allowed) == 0 {
		allowed = defaultVideoCapabilities().AllowedDurations
	}
	cleaned := make([]int, 0, len(allowed))
	seen := make(map[int]bool, len(allowed))
	for _, value := range allowed {
		if value <= 0 || seen[value] {
			continue
		}
		seen[value] = true
		cleaned = append(cleaned, value)
	}
	if len(cleaned) == 0 {
		return []int{10}
	}
	sort.Ints(cleaned)
	return cleaned
}

// closestDurationCombo 在恰好 parts 段、每段取 allowed 档位的约束下，
// 找一个总和最接近 target 的组合（差额相同取总时长更短的，控制成本）。
func closestDurationCombo(target float64, parts int, allowed []int) []int {
	if parts <= 0 || len(allowed) == 0 {
		return nil
	}
	maxAllowed := allowed[len(allowed)-1]
	limit := int(math.Ceil(target)) + maxAllowed
	if maxSum := parts * maxAllowed; limit > maxSum {
		limit = maxSum
	}
	if limit < allowed[0] {
		return nil
	}

	reachable := make([][]bool, parts+1)
	previous := make([][]int, parts+1)
	for i := range reachable {
		reachable[i] = make([]bool, limit+1)
		previous[i] = make([]int, limit+1)
	}
	reachable[0][0] = true
	for count := 1; count <= parts; count++ {
		for sum := 0; sum <= limit; sum++ {
			if !reachable[count-1][sum] {
				continue
			}
			for _, value := range allowed {
				next := sum + value
				if next > limit || reachable[count][next] {
					continue
				}
				reachable[count][next] = true
				previous[count][next] = sum
			}
		}
	}

	bestSum := -1
	bestDiff := math.Inf(1)
	for sum := 0; sum <= limit; sum++ {
		if !reachable[parts][sum] {
			continue
		}
		diff := math.Abs(float64(sum) - target)
		if diff < bestDiff-1e-9 || (math.Abs(diff-bestDiff) <= 1e-9 && sum < bestSum) {
			bestSum = sum
			bestDiff = diff
		}
	}
	if bestSum < 0 {
		return nil
	}

	combo := make([]int, parts)
	sum := bestSum
	for count := parts; count > 0; count-- {
		prev := previous[count][sum]
		combo[count-1] = sum - prev
		sum = prev
	}
	// 统一按长段在前输出，计划与日志更稳定，也避免同一输入每次排列不同。
	sort.Sort(sort.Reverse(sort.IntSlice(combo)))
	return combo
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

// normalizeDialogueLines 统一台词行：优先结构化 dialogueLines；
// 旧任务只有 dialogue / speaker 时自动升级成一行，保证多说话人改造对旧数据无感。
func normalizeDialogueLines(lines []VideoDialogueLine, dialogue, speaker string) []VideoDialogueLine {
	out := make([]VideoDialogueLine, 0, len(lines)+1)
	for _, line := range lines {
		text := strings.TrimSpace(strings.ReplaceAll(line.Text, "\r\n", "\n"))
		if text == "" {
			continue
		}
		out = append(out, VideoDialogueLine{Speaker: strings.TrimSpace(line.Speaker), Text: text})
	}
	if len(out) == 0 {
		if text := strings.TrimSpace(dialogue); text != "" {
			out = append(out, VideoDialogueLine{Speaker: strings.TrimSpace(speaker), Text: text})
		}
	}
	return out
}

// dialogueText 把多行台词拼回一个字符串，供字幕、字数与时长估算使用。
func dialogueText(lines []VideoDialogueLine) string {
	texts := make([]string, 0, len(lines))
	for _, line := range lines {
		if text := strings.TrimSpace(line.Text); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// dialogueSpeakerList 按出现顺序列出不重复的说话人（含 narration）。
func dialogueSpeakerList(lines []VideoDialogueLine) []string {
	out := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		speaker := strings.TrimSpace(line.Speaker)
		if speaker == "" || seen[speaker] {
			continue
		}
		seen[speaker] = true
		out = append(out, speaker)
	}
	return out
}

// dialogueSpeakerSummary 单一说话人时返回 role_id；多人或未定时返回空，兼容旧字段。
func dialogueSpeakerSummary(lines []VideoDialogueLine) string {
	speakers := dialogueSpeakerList(lines)
	if len(speakers) == 1 {
		return speakers[0]
	}
	return ""
}

// dialogueSpeakerName 把本段所有说话人翻成可读标签：旁白 / 角色名，多个用“、”连接。
func dialogueSpeakerName(cfg VideoRunConfig, lines []VideoDialogueLine) string {
	speakers := dialogueSpeakerList(lines)
	labels := make([]string, 0, len(speakers))
	for _, speaker := range speakers {
		if label := speakerLabel(cfg, speaker); label != "" {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, "、")
}

// dialogueHasCharacterSpeaker 本段是否有角色开口（旁白不算，旁白不需要画面里有人对口型）。
func dialogueHasCharacterSpeaker(lines []VideoDialogueLine) bool {
	for _, speaker := range dialogueSpeakerList(lines) {
		if speaker != VideoSpeakerNarration {
			return true
		}
	}
	return false
}

// dialogueSpeakerSequence 按台词顺序输出说话人标签（连续重复的合并），
// 用于提示词里描述“谁先说、谁后说”。旁白也参与排序。
func dialogueSpeakerSequence(cfg VideoRunConfig, lines []VideoDialogueLine) []string {
	out := make([]string, 0, len(lines))
	last := ""
	for _, line := range lines {
		label := speakerLabel(cfg, line.Speaker)
		if label == "" || label == last {
			continue
		}
		out = append(out, label)
		last = label
	}
	return out
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
				// 台词只在首段下发；多说话人按行携带，归属不再被压成单值。
				segment.DialogueLines = normalizeDialogueLines(item.DialogueLines, item.Dialogue, item.Speaker)
				segment.Dialogue = dialogueText(segment.DialogueLines)
				segment.Speaker = dialogueSpeakerSummary(segment.DialogueLines)
				segment.SpeakerName = dialogueSpeakerName(cfg, segment.DialogueLines)
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
	SegmentID  string
	Start      float64
	Duration   float64
	Text       string
	FillWindow bool // 片段自带配音时语速由模型掌控，只在声学对齐路径下允许铺满说话窗口
}

// plannedSubtitleSpans 把预算的分镜时间轴当作字幕窗口（无实测数据时的兜底）。
func plannedSubtitleSpans(plan VideoPlan) []SubtitleSpan {
	spans := make([]SubtitleSpan, 0, len(plan.Segments))
	for _, segment := range plan.Segments {
		spans = append(spans, SubtitleSpan{
			SegmentID: segment.ID,
			Start:     segment.Start,
			Duration:  segment.End - segment.Start,
			Text:      segment.Dialogue,
		})
	}
	return spans
}

// buildSubtitleCues 按预算时间轴铺字幕（无实测数据时的兜底）。
func buildSubtitleCues(plan VideoPlan, cfg VideoRunConfig) []VideoSubtitleCue {
	return buildSubtitleCuesFromSpans(cfg, plannedSubtitleSpans(plan))
}

// subtitleCueMinS 单条字幕最短停留：更短的话人眼根本来不及读。
const subtitleCueMinS = 0.4

// subtitleMaxCues 单次提交的字幕条数上限：0.1.2 从 200 提高到 1000，
// 长视频/密集对白不再静默丢条；仍超过才截断并明确告警。
const subtitleMaxCues = 1000

// subtitleReadMaxUnits 每秒最多读多少字/词，超过只提醒不阻断。
const subtitleReadMaxUnits = 8.0

// normalizeSubtitleCues 字幕关卡提交的整表整理：去空行、消解重叠、夹取越界、重排序号。
// 用户改时间就按用户的时间走（不重排到台词上）；无法保留的条目不静默处理，逐条返回问题。
func normalizeSubtitleCues(cues []VideoSubtitleCue, total float64, cfg VideoRunConfig) ([]VideoSubtitleCue, []VideoSubtitleIssue) {
	issues := make([]VideoSubtitleIssue, 0, 4)
	if len(cues) > subtitleMaxCues {
		cues = cues[:subtitleMaxCues]
		issues = append(issues, VideoSubtitleIssue{Level: "warning", Code: "cue_limit",
			Message: fmt.Sprintf("字幕最多 %d 条，超出的部分已丢弃", subtitleMaxCues)})
	}
	out := make([]VideoSubtitleCue, 0, len(cues))
	for position, cue := range cues {
		index := cue.Index
		if index <= 0 {
			index = position + 1
		}
		text := strings.TrimSpace(strings.ReplaceAll(cue.Text, "\r\n", "\n"))
		if text == "" {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "empty_text",
				Message: fmt.Sprintf("第 %d 条字幕文本为空，已丢弃", index)})
			continue
		}
		wrapped := wrapSubtitleText(text, cfg.Language)
		if wrapped != text {
			text = wrapped
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "warning", Code: "wrapped",
				Message: fmt.Sprintf("第 %d 条字幕过长，已自动换行", index)})
		}
		start := round2(math.Max(0, cue.Start))
		end := round2(cue.End)
		if total > 0 && start >= total {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "out_of_range",
				Message: fmt.Sprintf("第 %d 条字幕起始时间 %.1fs 超出成片长度 %.1fs，已丢弃", index, start, total)})
			continue
		}
		if total > 0 && end > total {
			end = round2(total)
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "warning", Code: "clamped_end",
				Message: fmt.Sprintf("第 %d 条字幕结束时间超出成片长度，已夹到 %.1fs", index, total)})
		}
		if end-start < subtitleCueMinS {
			end = round2(start + subtitleCueMinS)
			if total > 0 && end > total {
				end = round2(total)
			}
			if end-start < subtitleCueMinS {
				issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "too_short",
					Message: fmt.Sprintf("第 %d 条字幕无法满足最短 %.1fs，已丢弃", index, subtitleCueMinS)})
				continue
			}
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "warning", Code: "extended",
				Message: fmt.Sprintf("第 %d 条字幕过短，已延长到 %.1fs", index, subtitleCueMinS)})
		}
		if cue.Anchor < 1 || cue.Anchor > 9 {
			cue.Anchor = 0
		}
		cue.MarginVPct = clampFloat(round2(cue.MarginVPct), 0, 90)
		cue.FontSizePct = clampFloat(round2(cue.FontSizePct), 0, 30)
		out = append(out, VideoSubtitleCue{
			Start: start, End: end, Text: text,
			Anchor: cue.Anchor, MarginVPct: cue.MarginVPct, FontSizePct: cue.FontSizePct,
			SegmentID: strings.TrimSpace(cue.SegmentID), OffsetS: round2(math.Max(0, cue.OffsetS)),
		})
	}

	// 按起始时间排序后再消解重叠：用户拖动过的顺序才是最终时间顺序
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start == out[j].Start {
			return out[i].End < out[j].End
		}
		return out[i].Start < out[j].Start
	})
	for index := 1; index < len(out); index++ {
		prev := &out[index-1]
		current := &out[index]
		if current.Start >= prev.End {
			continue
		}
		// 优先收前一条的尾巴；收完不足最短停留就把后一条整体顺延
		if nextEnd := round2(current.Start - 0.05); nextEnd-prev.Start >= subtitleCueMinS {
			prev.End = nextEnd
			continue
		}
		shifted := round2(prev.End + 0.05)
		if total > 0 && shifted >= total {
			shifted = round2(math.Max(prev.End, total-subtitleCueMinS))
		}
		current.Start = shifted
		if current.End-current.Start < subtitleCueMinS {
			current.End = round2(current.Start + subtitleCueMinS)
			if total > 0 && current.End > total {
				current.End = round2(total)
			}
		}
		issues = append(issues, VideoSubtitleIssue{Index: index + 1, Level: "warning", Code: "overlap",
			Message: fmt.Sprintf("第 %d 条与上一条重叠，已自动顺延", index+1)})
	}
	// 顺延可能把后段推出成片，最后统一夹取一次
	if total > 0 {
		filtered := out[:0]
		for _, cue := range out {
			if cue.Start >= total {
				issues = append(issues, VideoSubtitleIssue{Index: cue.Index, Level: "error", Code: "out_of_range",
					Message: fmt.Sprintf("第 %d 条字幕起始时间超出成片长度，已丢弃", cue.Index)})
				continue
			}
			if cue.End > total {
				cue.End = round2(total)
			}
			filtered = append(filtered, cue)
		}
		out = filtered
	}
	for index := range out {
		out[index].Index = index + 1
	}
	issues = append(issues, ValidateVideoSubtitles(out, total, cfg)...)
	return out, dedupeSubtitleIssues(issues)
}

// ValidateVideoSubtitles 字幕规则校验：error 级禁止确认合成，warning 只提示。
// 前端用同一套口径做实时校验，保存/确认时以后端结果为准。
func ValidateVideoSubtitles(cues []VideoSubtitleCue, total float64, cfg VideoRunConfig) []VideoSubtitleIssue {
	issues := make([]VideoSubtitleIssue, 0, 4)
	if len(cues) > subtitleMaxCues {
		issues = append(issues, VideoSubtitleIssue{Level: "warning", Code: "cue_limit",
			Message: fmt.Sprintf("字幕条数 %d 超过上限 %d，将只保留前 %d 条", len(cues), subtitleMaxCues, subtitleMaxCues)})
	}
	limit := subtitleLineLimit(cfg.Language)
	for position, cue := range cues {
		index := cue.Index
		if index <= 0 {
			index = position + 1
		}
		text := strings.TrimSpace(cue.Text)
		if text == "" {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "empty_text",
				Message: fmt.Sprintf("第 %d 条字幕文本为空", index)})
			continue
		}
		if cue.Start < 0 || (total > 0 && cue.Start >= total) {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "out_of_range",
				Message: fmt.Sprintf("第 %d 条字幕起始时间越界", index)})
		}
		if total > 0 && cue.End > total+0.01 {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "out_of_range",
				Message: fmt.Sprintf("第 %d 条字幕结束时间 %.1fs 超出成片长度 %.1fs", index, cue.End, total)})
		}
		if cue.End-cue.Start < subtitleCueMinS-0.001 {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "error", Code: "too_short",
				Message: fmt.Sprintf("第 %d 条字幕短于 %.1fs", index, subtitleCueMinS)})
		}
		units := speechUnitCount(text, cfg.Language)
		duration := cue.End - cue.Start
		if duration > 0 && float64(units)/duration > subtitleReadMaxUnits {
			issues = append(issues, VideoSubtitleIssue{Index: index, Level: "warning", Code: "reading_speed",
				Message: fmt.Sprintf("第 %d 条字幕语速过快（%.1f 字/秒），建议拆分或延长", index, float64(units)/duration)})
		}
		for _, line := range strings.Split(text, "\n") {
			if runeLen(strings.TrimSpace(line)) > limit {
				issues = append(issues, VideoSubtitleIssue{Index: index, Level: "warning", Code: "line_too_long",
					Message: fmt.Sprintf("第 %d 条字幕有单行超过 %d 字，建议换行", index, limit)})
				break
			}
		}
	}
	if len(cues) > 0 && !subtitleFontAvailable(cfg.SubtitleStyle.FontName) {
		issues = append(issues, VideoSubtitleIssue{Level: "warning", Code: "font_unavailable",
			Message: fmt.Sprintf("字幕字体「%s」在本机可能不存在，建议改用内置 Noto Sans SC", firstNonEmpty(cfg.SubtitleStyle.FontName, "Noto Sans SC"))})
	}
	return dedupeSubtitleIssues(issues)
}

// dedupeSubtitleIssues 去掉重复的同一问题（规范化与校验会各报一次）。
func dedupeSubtitleIssues(issues []VideoSubtitleIssue) []VideoSubtitleIssue {
	if len(issues) < 2 {
		return issues
	}
	out := make([]VideoSubtitleIssue, 0, len(issues))
	seen := make(map[string]bool, len(issues))
	for _, issue := range issues {
		key := fmt.Sprintf("%d|%s|%s", issue.Index, issue.Level, issue.Code)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, issue)
	}
	return out
}

// hasSubtitleErrors 是否有阻止确认合成的错误级问题。
func hasSubtitleErrors(issues []VideoSubtitleIssue) bool {
	for _, issue := range issues {
		if issue.Level == "error" {
			return true
		}
	}
	return false
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
	// 台词比窗口短就按自然语速收尾（别在静音处挂着字幕），比窗口长就铺满整段。
	// FillWindow 只服务于声学对齐路径；估算路径短句不再被拉满整段。
	spoken := math.Min(math.Max(estimate, 0.6), window)

	parts := splitDialogueSentences(text, cfg.Language)
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
		start := round2(cursor)
		end := round2(display)
		if end <= start {
			// 窗口被压到极短时不丢条：并入上一条字幕（保持时间不倒退）
			if len(cues) > 0 && cues[len(cues)-1].End >= start {
				last := &cues[len(cues)-1]
				last.Text = strings.TrimSpace(last.Text + "\n" + wrapSubtitleText(part, cfg.Language))
				if end > last.End {
					last.End = end
				}
			}
		} else {
			cues = append(cues, VideoSubtitleCue{
				Index:     len(cues) + 1,
				Start:     start,
				End:       end,
				Text:      wrapSubtitleText(part, cfg.Language),
				SegmentID: span.SegmentID,
				OffsetS:   round2(start - span.Start),
			})
		}
		cursor = bound
	}
	return cues
}

// splitDialogueSentences 按句末标点/换行把台词拆成字幕行，过短的碎片并回上一句，
// 避免“嗯”“然后”这类词单独闪一条字幕；单句超长时再按逗号/空格与长度二次切分，绝不丢尾。
func splitDialogueSentences(text, lang string) []string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return nil
	}
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

	merged := make([]string, 0, len(raw))
	for _, piece := range raw {
		if len([]rune(piece)) < 4 && len(merged) > 0 {
			merged[len(merged)-1] = merged[len(merged)-1] + "，" + piece
			continue
		}
		merged = append(merged, piece)
	}
	parts := make([]string, 0, len(merged))
	for _, piece := range merged {
		parts = append(parts, splitLongSubtitlePart(piece, lang)...)
	}
	if len(parts) == 0 && strings.TrimSpace(text) != "" {
		parts = append(parts, strings.TrimSpace(text))
	}
	return parts
}

// subtitleLineLimit 单行字数上限：中文 18 字、英文 42 字符。
func subtitleLineLimit(lang string) int {
	if isEnglishLanguage(lang) {
		return 42
	}
	return 18
}

// subtitleMaxLines 一条字幕最多显示三行；更长的句子会先拆成多条 cue。
const subtitleMaxLines = 3

// splitLongSubtitlePart 把一段可能很长的台词拆成若干条（每条 ≤3 行），保证所有字符都有归属。
func splitLongSubtitlePart(part, lang string) []string {
	part = strings.TrimSpace(part)
	if part == "" {
		return nil
	}
	lines := subtitleLines(part, lang)
	maxLines := subtitleLineLimit(lang) * subtitleMaxLines
	if len([]rune(part)) <= maxLines {
		return []string{strings.Join(lines, "\n")}
	}
	out := make([]string, 0, len(lines)/subtitleMaxLines+1)
	for index := 0; index < len(lines); index += subtitleMaxLines {
		end := index + subtitleMaxLines
		if end > len(lines) {
			end = len(lines)
		}
		out = append(out, strings.Join(lines[index:end], "\n"))
	}
	return out
}

// subtitleLines 把一段文本按语言习惯拆成视觉行：中文按字数，英文优先在空格处断行。
func subtitleLines(text, lang string) []string {
	limit := subtitleLineLimit(lang)
	paragraphs := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]string, 0, 4)
	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		if isEnglishLanguage(lang) {
			lines = append(lines, wrapEnglishParagraph(paragraph, limit)...)
			continue
		}
		runes := []rune(paragraph)
		for len(runes) > limit {
			lines = append(lines, string(runes[:limit]))
			runes = runes[limit:]
		}
		if len(runes) > 0 {
			lines = append(lines, string(runes))
		}
	}
	if len(lines) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return lines
}

// wrapEnglishParagraph 英文按单词断行，单词本身超长时硬切，任何字符都不丢。
func wrapEnglishParagraph(text string, limit int) []string {
	words := strings.Fields(text)
	lines := make([]string, 0, 2)
	current := ""
	for _, word := range words {
		for len([]rune(word)) > limit {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}
			runes := []rune(word)
			lines = append(lines, string(runes[:limit]))
			word = string(runes[limit:])
		}
		if current == "" {
			current = word
			continue
		}
		if len([]rune(current))+1+len([]rune(word)) <= limit {
			current += " " + word
			continue
		}
		lines = append(lines, current)
		current = word
	}
	if current != "" {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		lines = append(lines, text)
	}
	return lines
}

// wrapSubtitleText 建议断行：返回全部行，不再只保留前两行。
func wrapSubtitleText(text, lang string) string {
	return strings.Join(subtitleLines(text, lang), "\n")
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
