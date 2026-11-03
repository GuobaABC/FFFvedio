package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 热点爬取流水线的阶段（《视频主题爬虫·架构与最佳实践设计》§2）：
// 收录归一化 → 抓取路由 → 抽取瀑布 → 校验合并 → 沉淀存储。
// 单条 URL 的生命周期（§8）记录在 CrawlResult.State 上，任务阶段只表示整体进度。
const (
	CrawlStageIntake   = "INTAKE"
	CrawlStageFetch    = "FETCH"
	CrawlStageExtract  = "EXTRACT"
	CrawlStageValidate = "VALIDATE"
	CrawlStageDone     = "DONE"
)

// 任务状态：爬虫没有人工确认关卡，waiting 仅在“有 URL 需人工处理”时短暂出现。
const (
	CrawlStatusDraft   = "draft"
	CrawlStatusRunning = "running"
	CrawlStatusDone    = "done"
	CrawlStatusFailed  = "failed"
)

// 单条 URL 的状态机（§8）
const (
	CrawlStateCreated   = "CREATED"
	CrawlStateRouted    = "ROUTED"
	CrawlStateFetched   = "FETCHED"
	CrawlStateParsed    = "PARSED"
	CrawlStateValidated = "VALIDATED"
	CrawlStateStored    = "STORED"
	CrawlStateCached    = "CACHED"
	CrawlStateNeedsLLM  = "NEEDS_LLM"
	CrawlStateLLMDone   = "LLM_EXTRACTED"
	CrawlStateRuleNew   = "RULE_PROPOSED"
	CrawlStateFailed    = "FAILED_RETRY"
	CrawlStateBlocked   = "BLOCKED"
	CrawlStateSkipped   = "SKIPPED"
)

// 失败分类：不同类别不同退避策略（§8）
const (
	CrawlErrNetwork     = "network"
	CrawlErrParse       = "parse"
	CrawlErrValidate    = "validate"
	CrawlErrBotBlocked  = "bot-blocked"
	CrawlErrLogin       = "login-required"
	CrawlErrDeadLink    = "dead-link"
	CrawlErrRobots      = "robots-disallowed"
	CrawlErrUnsupported = "unsupported-url"
	CrawlErrLLM         = "llm-failed"
)

// 抽取层级：_meta.extract_tier 记录这份数据从哪一层抽出来的（§3.1）
const (
	CrawlTierOEmbed     = "L0-oEmbed"
	CrawlTierAdapter    = "L1-adapter"
	CrawlTierProfile    = "L1-profile"
	CrawlTierStructured = "L2-structured"
	CrawlTierLLM        = "L3-llm"
	CrawlTierRule       = "L4-rule"
)

const (
	defaultCrawlMaxUrls    = 10
	maxCrawlMaxUrls        = 50
	defaultCrawlIntervalMS = 1500
	defaultCrawlTimeoutS   = 20
	defaultCrawlCacheHours = 30 * 24
)

// CrawlRunConfig 输入契约：一次给全、入口归一化。
type CrawlRunConfig struct {
	// Input 是用户原始输入：可以是一条 URL、多条 URL，或含 URL 的混排文本（分享文案）
	Input string   `json:"input"`
	Tags  []string `json:"tags"`
	Note  string   `json:"note"`
	// MaxUrls 单任务最多收录多少条 URL
	MaxUrls int `json:"maxUrls"`
	// UseLLM 新域名首爬时启用 L3 零样本抽取，并把结果反推成站点规则（L4）；
	// 表单默认勾选，勾掉后即使指定了文本模型也不调用模型
	UseLLM bool `json:"useLlm"`
	// TextProviderID 指定 L3/L4 使用的文本模型；为空则自动挑选
	TextProviderID string `json:"textProviderId"`
	// RespectRobots 遵守 robots.txt（表单默认勾选；取消勾选后不检查，归一化会给出警告）
	RespectRobots bool `json:"respectRobots"`
	// IntervalMS 同 host 抓取间隔，默认 1.5s
	IntervalMS int `json:"intervalMs"`
	// TimeoutS 单次请求超时
	TimeoutS int `json:"timeoutS"`
}

// VideoThumbnail 只记录封面 URL，不落盘原图（避免二次分发风险，§5）
type VideoThumbnail struct {
	URL         string `json:"url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ContentType string `json:"contentType"`
}

// VideoItemOptional 可选字段口袋：扩展不动核心三字段（§11）
type VideoItemOptional struct {
	Author      string   `json:"author"`
	PublishedAt string   `json:"publishedAt"`
	DurationSec float64  `json:"durationSec"`
	Tags        []string `json:"tags"`
}

// VideoItemMeta 抽取元信息
type VideoItemMeta struct {
	ExtractedAt string   `json:"extractedAt"`
	ExtractTier string   `json:"extractTier"`
	Confidence  float64  `json:"confidence"`
	HTTPStatus  int      `json:"httpStatus"`
	Rendered    bool     `json:"rendered"`
	LatencyMS   int64    `json:"latencyMs"`
	RawRef      string   `json:"rawRef"`
	Variants    []string `json:"variants"`
	SiteProfile bool     `json:"siteProfile"`
}

// VideoItem 抽取产物，喂给下游剧本 Agent（§3.1）
type VideoItem struct {
	ID           string            `json:"id"`
	CanonicalURL string            `json:"canonicalUrl"`
	OriginalURL  string            `json:"originalUrl"`
	SiteKey      string            `json:"siteKey"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Thumbnail    VideoThumbnail    `json:"thumbnail"`
	Optional     VideoItemOptional `json:"optional"`
	Meta         VideoItemMeta     `json:"meta"`
	CreatedAt    string            `json:"createdAt"`
	UpdatedAt    string            `json:"updatedAt"`
}

// CrawlStep 审计步骤：每一步都落 {step, tier, latency, status, err_code}（§8）
type CrawlStep struct {
	Step      string `json:"step"`
	State     string `json:"state"`
	Tier      string `json:"tier"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	Note      string `json:"note"`
	At        string `json:"at"`
}

// CrawlResult 单条 URL 的处理结果，任务视图直接消费
type CrawlResult struct {
	OriginalURL  string      `json:"originalUrl"`
	CanonicalURL string      `json:"canonicalUrl"`
	SiteKey      string      `json:"siteKey"`
	State        string      `json:"state"`
	Tier         string      `json:"tier"`
	HTTPStatus   int         `json:"httpStatus"`
	Rendered     bool        `json:"rendered"`
	Confidence   float64     `json:"confidence"`
	ErrorCode    string      `json:"errorCode"`
	Message      string      `json:"message"`
	LatencyMS    int64       `json:"latencyMs"`
	ItemID       string      `json:"itemId"`
	Cached       bool        `json:"cached"`
	Steps        []CrawlStep `json:"steps"`
}

// CrawlStats 任务级统计（前端头部 chips 与 Tier 命中率）
type CrawlStats struct {
	Total      int            `json:"total"`
	Stored     int            `json:"stored"`
	Cached     int            `json:"cached"`
	Failed     int            `json:"failed"`
	Blocked    int            `json:"blocked"`
	LLMCalls   int            `json:"llmCalls"`
	TierHits   map[string]int `json:"tierHits"`
	DurationMS int64          `json:"durationMs"`
}

// CrawlRunContext 任务上下文：结果、条目库视图、日志与统计
type CrawlRunContext struct {
	Results    []CrawlResult `json:"results"`
	Items      []VideoItem   `json:"items"`
	Stats      CrawlStats    `json:"stats"`
	Warnings   []string      `json:"warnings"`
	Logs       []string      `json:"logs"`
	LastRunAt  string        `json:"lastRunAt"`
	FinishedAt string        `json:"finishedAt"`
}

// CrawlTask 以任务为视图的核心实体
type CrawlTask struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Stage     string          `json:"stage"`
	Status    string          `json:"status"`
	Error     string          `json:"error"`
	Config    CrawlRunConfig  `json:"config"`
	Run       CrawlRunContext `json:"run"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
}

// CrawlTaskInput 新建任务入参
type CrawlTaskInput struct {
	Title  string         `json:"title"`
	Config CrawlRunConfig `json:"config"`
}

// SiteCleaner 正则只做尾部清洗，不做主抽取（§6 L4）
type SiteCleaner struct {
	Field   string `json:"field"`
	Regex   string `json:"regex"`
	Replace string `json:"replace"`
}

// SiteProfile 站点画像 · 沉淀物（§3.2）
type SiteProfile struct {
	SiteKey        string              `json:"siteKey"`
	HostPattern    string              `json:"hostPattern"`
	RequiresRender bool                `json:"requiresRender"`
	Selectors      map[string][]string `json:"selectors"`
	Cleaners       []SiteCleaner       `json:"cleaners"`
	HitSuccess     int                 `json:"hitSuccess"`
	HitFail        int                 `json:"hitFail"`
	LastVerifiedAt string              `json:"lastVerifiedAt"`
	Enabled        bool                `json:"enabled"`
	AutoGenerated  bool                `json:"autoGenerated"`
	ReviewStatus   string              `json:"reviewStatus"`
	UpdatedAt      string              `json:"updatedAt"`
}

// CrawlEnvironment 能力探测：前端表单据此提示「有无文本模型」「支持哪些站」
type CrawlEnvironment struct {
	LLMAvailable   bool     `json:"llmAvailable"`
	TextModel      string   `json:"textModel"`
	BrowserSupport bool     `json:"browserSupport"`
	SupportedSites []string `json:"supportedSites"`
	Notes          []string `json:"notes"`
}

// normalizeCrawlConfig 归一化用户输入，并给出可解释的警告。
func normalizeCrawlConfig(cfg CrawlRunConfig) (CrawlRunConfig, []string, error) {
	cfg.Input = strings.TrimSpace(cfg.Input)
	if cfg.Input == "" {
		return cfg, nil, errors.New("请先粘贴要爬取的链接，或包含链接的分享文案")
	}

	cfg.Note = strings.TrimSpace(cfg.Note)
	cfg.Tags = normalizeCrawlTags(cfg.Tags)

	if cfg.MaxUrls <= 0 {
		cfg.MaxUrls = defaultCrawlMaxUrls
	}
	if cfg.MaxUrls > maxCrawlMaxUrls {
		cfg.MaxUrls = maxCrawlMaxUrls
	}
	// L3 零样本抽取以用户勾选为准：选了文本模型不等于想跑 L3，
	// 勾掉了又被模型顶回开启，这个复选框就是个摆设。
	cfg.TextProviderID = strings.TrimSpace(cfg.TextProviderID)
	if cfg.IntervalMS <= 0 {
		cfg.IntervalMS = defaultCrawlIntervalMS
	}
	cfg.IntervalMS = clampInt(cfg.IntervalMS, 200, 10000)
	if cfg.TimeoutS <= 0 {
		cfg.TimeoutS = defaultCrawlTimeoutS
	}
	cfg.TimeoutS = clampInt(cfg.TimeoutS, 5, 60)

	warnings := make([]string, 0, 2)
	if !cfg.RespectRobots {
		warnings = append(warnings, "已关闭 robots.txt 检查：仅在你确认目标站允许抓取时使用")
	}
	if !cfg.UseLLM && cfg.TextProviderID != "" {
		warnings = append(warnings, "已关闭 L3 零样本抽取：新域名只走 L0/L1/L2，指定的文本模型不会被调用")
	}
	return cfg, warnings, nil
}

// normalizeCrawlTags 标签用于「选题池」批量重跑，去重并去掉多余空白。
func normalizeCrawlTags(list []string) []string {
	out := make([]string, 0, 6)
	seen := make(map[string]struct{}, len(list))
	for _, item := range list {
		item = strings.TrimSpace(strings.TrimPrefix(item, "#"))
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// mergeCrawlTaskTags 把任务上填的「选题池标签」并进条目抽到的标签：用户标的排前面，
// 去重后随条目入库，素材库与「去生成」才带得走这几个标签。
func mergeCrawlTaskTags(taskTags, itemTags []string) []string {
	merged := make([]string, 0, len(taskTags)+len(itemTags))
	merged = append(merged, taskTags...)
	merged = append(merged, itemTags...)
	return normalizeCrawlTags(merged)
}

// crawlTagsCarried 任务上填的标签是否已经都在条目里了：都在就不必再写一次库。
func crawlTagsCarried(taskTags, itemTags []string) bool {
	for _, tag := range normalizeCrawlTags(taskTags) {
		found := false
		for _, existing := range itemTags {
			if existing == tag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func newCrawlTaskID() string {
	return "crawl_" + time.Now().Format("20060102150405") + "_" + randomHex(4)
}

// crawlResultByCanonical 便于按 canonical URL 做幂等去重。
func crawlResultByCanonical(results []CrawlResult, canonical string) (int, bool) {
	for i, item := range results {
		if item.CanonicalURL == canonical {
			return i, true
		}
	}
	return -1, false
}

// upsertCrawlItem 条目库按 canonical URL 幂等：同一条视频在多个任务里只留一份。
func upsertCrawlItem(items []VideoItem, item VideoItem) []VideoItem {
	for i, existing := range items {
		if existing.ID == item.ID {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

func crawlStageLabel(stage string) string {
	switch stage {
	case CrawlStageIntake:
		return "URL 收录"
	case CrawlStageFetch:
		return "抓取页面"
	case CrawlStageExtract:
		return "抽取瀑布"
	case CrawlStageValidate:
		return "校验合并"
	case CrawlStageDone:
		return "完成"
	}
	return stage
}

func crawlSummary(run CrawlRunContext) string {
	return fmt.Sprintf("共 %d 条，入库 %d 条（缓存 %d），失败 %d 条，拦截 %d 条",
		run.Stats.Total, run.Stats.Stored, run.Stats.Cached, run.Stats.Failed, run.Stats.Blocked)
}
