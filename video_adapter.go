package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VideoCapabilities 适配器能力声明（§5.1）：上层只提交抽象能力，厂商差异封在实现里。
type VideoCapabilities struct {
	I2VFirstFrame       bool     `json:"i2vFirstFrame"`
	LastFrame           bool     `json:"lastFrame"`
	CharacterReference  bool     `json:"characterReference"`
	NativeAudio         bool     `json:"nativeAudio"`
	AllowedDurations    []int    `json:"allowedDurations"`
	MaxClipS            int      `json:"maxClipS"`
	AllowedAspectRatios []string `json:"allowedAspectRatios"`
	MaxResolution       string   `json:"maxResolution"`
	PromptStyle         string   `json:"promptStyle"`
	CreditsPerClip      float64  `json:"creditsPerClip"`
}

// VideoCharacterImage 一张角色参考图：带角色身份，适配器才能在提示词里按厂商要求的序号把它绑定到角色。
// Source 区分用户上传与模板生成图：前者优先，超上限时先丢后者。
type VideoCharacterImage struct {
	RoleID string
	Name   string
	URL    string
	Source string
}

// VideoGenRequest 单段生成请求（模型无关）。
type VideoGenRequest struct {
	Prompt          string
	FirstFrame      string
	LastFrame       string
	CharacterImages []VideoCharacterImage
	// StrictFirstFrame 本段有首帧时必须以 first_frame 角色提交（画面硬从这张图开始），
	// 不与角色参考图混提。方舟一类“任务类型互斥”的通道靠它做取舍。
	StrictFirstFrame bool
	DurationS        int
	AspectRatio      string
	Fps              int
	Seed             int64
	OutputPath       string
}

// VideoGenResult 单段生成结果。
type VideoGenResult struct {
	VideoURI  string
	FPS       int
	DurationS float64
	Seed      int64
	TaskID    string
	ModelVer  string
	Notes     []string // 服务商实际行为与请求不一致时的说明，由流水线归入任务日志
}

// videoGenContext 适配器运行时需要的上下文（提供商配置 + 工作目录）。
type videoGenContext struct {
	Provider  Provider
	Model     string
	AdapterID string
	WorkDir   string
}

// videoAdapter 统一适配器接口：generate(request) -> {video_uri, fps, duration, seed}
type videoAdapter interface {
	Capabilities() VideoCapabilities
	Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error)
}

// modelAwareAdapter 可选接口：能力随模型 ID 变化的适配器实现它（文档 §6.1：不把模型名硬编码进配置）。
type modelAwareAdapter interface {
	CapabilitiesFor(model string) VideoCapabilities
}

// findAdapterLabel 取适配器中文名，供日志与提示复用。
func findAdapterLabel(id string) string {
	_, info, ok := findVideoAdapter(id)
	if !ok {
		return id
	}
	return info.Label
}

// looksLikeArkVideoModel 方舟的文本模型与视频模型共用一个提供商配置，
// 把 doubao-seed 当视频模型提交只会拿到一句服务商报错，提前拦一下。
// 推理接入点（ep-xxxx）无法从名字判断，当作合法处理。
func looksLikeArkVideoModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return false
	}
	if strings.HasPrefix(lower, "ep-") {
		return true
	}
	for _, keyword := range []string{"seedance", "i2v", "t2v", "video", "veo", "kling"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// 适配成熟度：目前只有字节跳动火山方舟 Seedance 按官方文档逐能力核对过（全链路适配），
// 其余通道只打通了基础的生视频任务，参考图 / 音画同步等能力尚未逐项验收。
const (
	AdapterMaturityPrimary = "primary" // 主力适配
	AdapterMaturityPartial = "partial" // 基础可用，未完整适配
	AdapterMaturityLocal   = "local"   // 本地联调，不计费
)

func adapterInfos() []VideoAdapterInfo {
	entries := []struct {
		id          string
		label       string
		description string
		maturity    string
		matNote     string
		adapter     videoAdapter
	}{
		{
			id:          "dashscope",
			label:       "DashScope 视频（万相 / 可灵）",
			description: "阿里云百炼异步任务接口，支持首帧/尾帧条件；首帧需公网 URL",
			maturity:    AdapterMaturityPartial,
			matNote:     "首帧 / 尾帧主链路可用；角色参考图与音画同步未适配验收（已有参考图生视频接口，但未逐项对齐官方参数）",
			adapter:     dashscopeAdapter{},
		},
		{
			id:          "ark",
			label:       "火山方舟 Seedance（豆包视频）",
			description: "方舟独立视频端点 /contents/generations/tasks，支持首帧/尾帧/参考图；Seedance 2.x 可音画同步",
			maturity:    AdapterMaturityPrimary,
			matNote:     "主力适配：任务类型互斥、图片N引用、参考图上限、画幅锁定、能力按模型代际都已按官方文档实现并测试",
			adapter:     arkAdapter{},
		},
		{
			id:          "runway",
			label:       "Runway Gen-4",
			description: "Runway image_to_video + 任务轮询；首帧需公网 URL",
			maturity:    AdapterMaturityPartial,
			matNote:     "基础生视频可用；没有角色参考图能力，跨段一致性只靠提示词里的外观描述",
			adapter:     runwayAdapter{},
		},
		{
			id:          "veo",
			label:       "Google Veo（Gemini API）",
			description: "Veo 长任务接口，原生音频路线，台词可直接写入提示词",
			maturity:    AdapterMaturityPartial,
			matNote:     "任务提交 / 轮询可用；参考图与画幅行为未逐参数核对",
			adapter:     veoAdapter{},
		},
		{
			id:          "generic",
			label:       "通用 HTTP 适配器",
			description: "按通用 REST 约定提交/轮询视频任务，适配自建网关",
			maturity:    AdapterMaturityPartial,
			matNote:     "网关实参结构各家不一致，未针对具体厂商适配，能力声明按通用约定从宽处理",
			adapter:     genericAdapter{},
		},
		{
			id:          "mock",
			label:       "本地联调占位（ffmpeg）",
			description: "用 ffmpeg 合成测试画面，仅用于验证流水线，不调用任何模型",
			maturity:    AdapterMaturityLocal,
			matNote:     "不计费、不计入消耗统计，只用来跑通流水线",
			adapter:     mockAdapter{},
		},
	}
	infos := make([]VideoAdapterInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, VideoAdapterInfo{
			ID:            entry.id,
			Label:         entry.label,
			Description:   entry.description,
			Maturity:      entry.maturity,
			MaturityLabel: adapterMaturityLabel(entry.maturity),
			MaturityNote:  entry.matNote,
			Capabilities:  entry.adapter.Capabilities(),
		})
	}
	return infos
}

func adapterMaturityLabel(maturity string) string {
	switch maturity {
	case AdapterMaturityPrimary:
		return "主力适配"
	case AdapterMaturityLocal:
		return "本地联调（不计费）"
	default:
		return "基础可用 · 未完整适配"
	}
}

// adapterMaturityRows 大屏的适配声明：把“哪些通道真验收过、哪些只是能跑”直接告诉用户。
func adapterMaturityRows() []adapterMaturityRow {
	infos := adapterInfos()
	rows := make([]adapterMaturityRow, 0, len(infos))
	// 主力适配排最前，声明的结论一眼就能看到
	for _, pass := range []string{AdapterMaturityPrimary, AdapterMaturityPartial, AdapterMaturityLocal} {
		for _, info := range infos {
			if info.Maturity != pass {
				continue
			}
			rows = append(rows, adapterMaturityRow{
				ID:            info.ID,
				Label:         info.Label,
				Description:   info.Description,
				Maturity:      info.Maturity,
				MaturityLabel: info.MaturityLabel,
				Primary:       info.Maturity == AdapterMaturityPrimary,
				Note:          info.MaturityNote,
			})
		}
	}
	return rows
}

func findVideoAdapter(id string) (videoAdapter, VideoAdapterInfo, bool) {
	for _, info := range adapterInfos() {
		if info.ID == id {
			for _, entry := range []struct {
				id      string
				adapter videoAdapter
			}{
				{"dashscope", dashscopeAdapter{}},
				{"ark", arkAdapter{}},
				{"runway", runwayAdapter{}},
				{"veo", veoAdapter{}},
				{"generic", genericAdapter{}},
				{"mock", mockAdapter{}},
			} {
				if entry.id == id {
					return entry.adapter, info, true
				}
			}
		}
	}
	return nil, VideoAdapterInfo{}, false
}

// inferAdapterFromProvider 根据已登记的提供商推断适配器，避免用户必须手选。
func inferAdapterFromProvider(provider Provider) string {
	haystack := strings.ToLower(provider.Model + " " + provider.BaseURL + " " + provider.Name)
	switch {
	case strings.Contains(haystack, "kling"), strings.Contains(haystack, "wan"),
		strings.Contains(haystack, "dashscope"), strings.Contains(haystack, "aliyuncs"):
		return "dashscope"
	// 方舟的视频能力是独立端点，走通用 /video/generations 只会拿到 404
	case strings.Contains(haystack, "seedance"), strings.Contains(haystack, "volces"),
		strings.Contains(haystack, "ark.cn"), strings.Contains(haystack, "方舟"),
		strings.Contains(haystack, "doubao"):
		return "ark"
	case strings.Contains(haystack, "runway"):
		return "runway"
	case strings.Contains(haystack, "veo"), strings.Contains(haystack, "gemini"),
		strings.Contains(haystack, "generativelanguage"):
		return "veo"
	case provider.ID != "":
		return "generic"
	}
	return "none"
}

// adapterCapabilities 取适配器能力；部分适配器会按模型 ID 微调（如火山方舟仅 Seedance 2.x 有原生音频与角色参考图）。
func adapterCapabilities(adapter videoAdapter, model string) VideoCapabilities {
	if adapter == nil {
		return defaultVideoCapabilities()
	}
	if aware, ok := adapter.(modelAwareAdapter); ok {
		return aware.CapabilitiesFor(model)
	}
	return adapter.Capabilities()
}

// resolveVideoAdapter 解析任务实际使用的适配器与提供商。
func (a *App) resolveVideoAdapter(cfg VideoRunConfig) (string, videoAdapter, Provider, []string, error) {
	warnings := make([]string, 0, 2)
	providers, err := a.ListProviders()
	if err != nil {
		return "", nil, Provider{}, warnings, err
	}

	provider := Provider{}
	if cfg.ProviderID != "" {
		for _, p := range providers {
			if p.ID == cfg.ProviderID {
				provider = p
				break
			}
		}
	}
	if provider.ID == "" {
		for _, p := range providers {
			if p.Category == CategoryMultimodalGen {
				provider = p
				break
			}
		}
	}

	id := strings.ToLower(strings.TrimSpace(cfg.Adapter))
	if id == "" || id == "auto" {
		id = inferAdapterFromProvider(provider)
	} else if id == "generic" {
		// 旧任务可能已把适配器固定成通用；提供商明显属于专用端点时自动改道，避免反复 404
		if dedicated := inferAdapterFromProvider(provider); dedicated != "" && dedicated != "generic" && dedicated != "none" {
			warnings = append(warnings, fmt.Sprintf("该提供商的视频能力走专用端点，已自动从「通用 HTTP 适配器」切换到「%s」", findAdapterLabel(dedicated)))
			id = dedicated
		}
	}
	if id == "none" {
		warnings = append(warnings, "未配置多模态生成模型：请在「系统设置 - 提供商」登记，或在任务里选「本地联调占位」")
		return "none", nil, provider, warnings, nil
	}
	if id == "ark" && !looksLikeArkVideoModel(provider.Model) {
		warnings = append(warnings, fmt.Sprintf("火山方舟视频任务需要 Seedance 系列模型 ID（如 doubao-seedance-2-5-260628），当前填的是「%s」，大概率会被服务商拒绝", firstNonEmpty(provider.Model, "未填写")))
	}

	adapter, info, ok := findVideoAdapter(id)
	if !ok {
		return id, nil, provider, warnings, fmt.Errorf("未知的视频适配器：%s", cfg.Adapter)
	}
	if id != "mock" && provider.ID == "" {
		warnings = append(warnings, fmt.Sprintf("适配器「%s」没有绑定提供商配置，渲染会直接失败", info.Label))
	}
	return id, adapter, provider, warnings, nil
}

// GetVideoEnvironment 前端在新建任务页展示 ffmpeg 状态、适配器档位与工作目录。
func (a *App) GetVideoEnvironment() VideoEnvironment {
	env := VideoEnvironment{Adapters: adapterInfos()}
	if path, ok := ffmpegPath(); ok {
		env.FFmpegAvailable = true
		env.FFmpegPath = path
	}
	if _, ok := ffprobePath(); ok {
		env.FFprobeAvailable = true
	}
	if root, err := videoWorkRoot(); err == nil {
		env.WorkRoot = root
	}
	return env
}

// composeRenderPrompt 按厂商提示词风格拼装最终提示词。
// 只有原生音频模型才把台词写进提示词（无音频模型会出现"画面写字"的副作用）。
func composeRenderPrompt(segment VideoSegment, caps VideoCapabilities) string {
	prompt := strings.TrimSpace(segment.Prompt)
	if caps.NativeAudio && strings.TrimSpace(segment.Dialogue) != "" {
		prompt += fmt.Sprintf("\n口播台词（只允许一个主体说话，保持口型在画面内）：“%s”", strings.TrimSpace(segment.Dialogue))
	}
	if caps.PromptStyle == "veo" {
		prompt += "\n限制：单镜头、不切镜，动作连续，保持主体与产品外观一致。"
	}
	return prompt
}

/* ------------------------------ 本地联调适配器 ------------------------------ */

type mockAdapter struct{}

func (mockAdapter) Capabilities() VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:       false,
		LastFrame:           true,
		CharacterReference:  false,
		NativeAudio:         false,
		AllowedDurations:    []int{3, 5, 8, 10, 15},
		MaxClipS:            15,
		AllowedAspectRatios: []string{"16:9", "9:16", "1:1"},
		MaxResolution:       "1080P",
		PromptStyle:         "kling",
		CreditsPerClip:      0,
	}
}

func (mockAdapter) Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error) {
	ffmpeg, ok := ffmpegPath()
	if !ok {
		return VideoGenResult{}, errors.New("本地联调占位需要 ffmpeg：请安装 ffmpeg 并加入 PATH，或在任务里改用真实模型")
	}
	width, height := aspectResolution(req.AspectRatio)
	fps := clampInt(req.Fps, 12, 60)
	duration := clampInt(req.DurationS, 1, 60)
	source := fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", width, height, fps)
	args := []string{
		"-y", "-f", "lavfi", "-i", source,
		"-t", fmt.Sprintf("%d", duration),
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "veryfast",
		req.OutputPath,
	}
	if err := runCommand(ctx, ffmpeg, args...); err != nil {
		return VideoGenResult{}, fmt.Errorf("ffmpeg 合成测试片段失败: %w", err)
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       fps,
		DurationS: float64(duration),
		Seed:      req.Seed,
		ModelVer:  "mock-ffmpeg",
	}, nil
}

/* ------------------------------ DashScope（万相 / 可灵） ------------------------------ */

type dashscopeAdapter struct{}

func (dashscopeAdapter) Capabilities() VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:       true,
		LastFrame:           true,
		CharacterReference:  false,
		NativeAudio:         false,
		AllowedDurations:    []int{5, 10},
		MaxClipS:            10,
		AllowedAspectRatios: []string{"16:9", "9:16", "1:1"},
		MaxResolution:       "1080P",
		PromptStyle:         "kling",
		CreditsPerClip:      0,
	}
}

func (dashscopeAdapter) Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error) {
	if gc.Provider.APIKey == "" {
		return VideoGenResult{}, errors.New("DashScope 适配器缺少 API Key")
	}
	imageURL, err := requireRemoteImage(req.FirstFrame, "首帧图片")
	if err != nil {
		return VideoGenResult{}, err
	}
	base := originOf(gc.Provider.BaseURL)
	if base == "" {
		base = "https://dashscope.aliyuncs.com"
	}
	headers := map[string]string{
		"Authorization":     "Bearer " + gc.Provider.APIKey,
		"X-DashScope-Async": "enable",
	}

	input := map[string]any{"prompt": req.Prompt, "img_url": imageURL}
	if req.LastFrame != "" {
		if lastFrame, err := requireRemoteImage(req.LastFrame, "尾帧图片"); err == nil {
			input["last_frame_url"] = lastFrame
		}
	}
	duration := nearestAllowedDuration(float64(req.DurationS), []int{5, 10})
	width, height := aspectResolution(req.AspectRatio)
	payload := map[string]any{
		"model": gc.Model,
		"input": input,
		"parameters": map[string]any{
			"size":          fmt.Sprintf("%d*%d", width, height),
			"duration":      duration,
			"prompt_extend": true,
			"watermark":     false,
		},
	}

	client := videoHTTPClient()
	data, err := videoRequest(ctx, client, http.MethodPost,
		base+"/api/v1/services/aigc/video-generation/video-synthesis", headers, payload)
	if err != nil {
		return VideoGenResult{}, err
	}
	var submitted struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &submitted); err != nil || submitted.Output.TaskID == "" {
		return VideoGenResult{}, fmt.Errorf("提交 DashScope 任务失败: %s", apiErrorDetail(data))
	}

	videoURL, err := pollVideoJob(ctx, base+"/api/v1/tasks/"+submitted.Output.TaskID, headers,
		func(body []byte) (string, string, error) {
			var payload struct {
				Output struct {
					TaskStatus string `json:"task_status"`
					VideoURL   string `json:"video_url"`
				} `json:"output"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return "", "", err
			}
			return payload.Output.TaskStatus, payload.Output.VideoURL, nil
		})
	if err != nil {
		return VideoGenResult{}, err
	}
	attempts, err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil)
	if err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       req.Fps,
		DurationS: float64(duration),
		Seed:      req.Seed,
		TaskID:    submitted.Output.TaskID,
		ModelVer:  gc.Model,
		Notes:     videoRetryNote("成片下载", attempts),
	}, nil
}

/* ------------------------------ 火山方舟 Seedance ------------------------------ */

// arkVideoPath 方舟视频任务的固定路径段（创建与轮询共用）。
const arkVideoPath = "/contents/generations/tasks"

// arkAdapter 对接火山方舟（豆包 Seedance）视频生成：
// 走独立异步端点 POST {base}/contents/generations/tasks 建任务，再 GET 同路径/{id} 轮询；
// chat 端点不吐视频（接口参考 §3.2），所以不能走通用 OpenAI 兼容适配器。
type arkAdapter struct{}

func (arkAdapter) Capabilities() VideoCapabilities { return arkCapabilities(true, true) }

// CapabilitiesFor 按模型微调：Seedance 1.0 不输出音轨，把台词写进提示词会出现「画面写字」副作用；
// 角色参考图（全模态参考）也只有 2.x 一代才有，声明错了上层会以为图已生效。
func (arkAdapter) CapabilitiesFor(model string) VideoCapabilities {
	return arkCapabilities(arkSupportsNativeAudio(model), arkSupportsReferenceImages(model))
}

func arkCapabilities(nativeAudio, referenceImages bool) VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:      true,
		LastFrame:          true,
		CharacterReference: referenceImages,
		NativeAudio:        nativeAudio,
		// duration 是整数秒而非离散档位，给满 [4,15] 避免拆段时被档位吸走。
		AllowedDurations:    []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		MaxClipS:            15,
		AllowedAspectRatios: []string{"21:9", "16:9", "9:16", "1:1", "4:3", "3:4"},
		MaxResolution:       "1080P",
		PromptStyle:         "kling",
		CreditsPerClip:      0,
	}
}

// arkIsSeedance2 判定 Seedance 2.x（2.0 系列与 2.5）：音画同步、全模态参考都是这一代才有的能力。
func arkIsSeedance2(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "seedance-2") || strings.Contains(lower, "seedance_2")
}

func arkSupportsNativeAudio(model string) bool { return arkIsSeedance2(model) }

// arkSupportsReferenceImages 官方能力表里 role=reference_image 只列了 Seedance 2.5 与 2.0 系列，
// 1.0 pro / 1.0 pro fast / 1.5 pro 只有文生、首帧、首尾帧三种任务类型。
func arkSupportsReferenceImages(model string) bool { return arkIsSeedance2(model) }

// arkReferenceImageLimit 全模态参考的图片数量上限：2.5 为 30 张，2.0 系列为 9 张（首帧图也计入）。
func arkReferenceImageLimit(model string) int {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "2-5") || strings.Contains(lower, "2.5") {
		return 30
	}
	return 9
}

// arkTaskEndpoint 归一化 Base URL 后拼视频端点。
// 兼容 .../api/v3、Agent Plan 专属入口 .../api/plan/v3，以及只填到域名的写法。
func arkTaskEndpoint(baseURL string) string {
	base := apiRoot(baseURL)
	if base == "" {
		return ""
	}
	if !strings.Contains(strings.ToLower(base), "/v3") {
		if strings.HasSuffix(strings.ToLower(base), "/api") {
			base += "/v3"
		} else {
			base += "/api/v3"
		}
	}
	return base + arkVideoPath
}

// arkImageRef 把首帧/尾帧/参考图转成方舟接受的 url 字段：
// 公网 URL 与素材 ID 直接透传，本地文件转 data URI（文档要求格式名小写）。
func arkImageRef(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	if isRemoteRef(ref) || strings.HasPrefix(strings.ToLower(ref), "data:") {
		return ref, nil
	}
	if !fileExists(ref) {
		return "", fmt.Errorf("文件不存在：%s", ref)
	}
	raw, mimeType, err := readImageBytes(ctx, ref)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("data:%s;base64,%s", strings.ToLower(mimeType), base64.StdEncoding.EncodeToString(raw)), nil
}

// arkRatio 决出提交给方舟的 ratio。
// 文档硬约束：首帧 / 首尾帧任务锁定输出画幅跟随首帧图，ratio 只能为 adaptive，
// 传固定比例会被忽略（部分入口还会直接 400 InvalidParameter.TaskTypeConstraint）。
func arkRatio(aspect string, lockedByFrame bool) (string, bool) {
	if lockedByFrame {
		return "adaptive", strings.TrimSpace(aspect) != "" && !strings.EqualFold(strings.TrimSpace(aspect), "adaptive")
	}
	for _, allowed := range []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"} {
		if strings.EqualFold(strings.TrimSpace(aspect), allowed) {
			return allowed, false
		}
	}
	return "adaptive", false
}

// arkImageEntry 一张待提交给方舟的图片：role 与其在提示词里的称呼。
type arkImageEntry struct {
	ref    string
	role   string
	source string // 出错信息里的称呼：首帧图片 / 尾帧图片 / 角色参考图
	label  string // 参与「图片N」绑定时写的说明文字
}

// arkMediaPlan 组装好的图片列表、提示词素材引用语句、画幅锁定与给用户看的说明。
type arkMediaPlan struct {
	Images        []map[string]any
	Binding       string
	LockedByFrame bool
	Notes         []string
}

// dedupeCharacterImages 同一张定妆图被多个角色引用时只提交一次，免得白占参考图额度。
// 同一张图同时挂在用户上传与模板生成下时，保留用户那一份身份（提示词措辞要用它）。
func dedupeCharacterImages(in []VideoCharacterImage) []VideoCharacterImage {
	seen := make(map[string]int, len(in))
	out := make([]VideoCharacterImage, 0, len(in))
	for _, item := range in {
		url := strings.TrimSpace(item.URL)
		if url == "" {
			continue
		}
		if index, ok := seen[url]; ok {
			if item.Source == CharacterImageSourceUser && out[index].Source != CharacterImageSourceUser {
				out[index].Source = item.Source
			}
			continue
		}
		seen[url] = len(out)
		out = append(out, item)
	}
	return out
}

// planArkMedia 按官方“任务类型互斥”规则组装图片：
// 图生视频-首帧、图生视频-首尾帧、全模态参考生视频三种场景不可混用，
// 之前把 first_frame 和 reference_image 一起塞进 content，参考图会被模型直接无视。
// 默认严格首帧：只要有首帧（多为用户输入或尾帧资产带入的那张图）就走「图生视频-首帧」
// 任务，画面硬从这张图开始，角色参考图让位；关掉严格首帧后才走全模态参考：
// 首帧图也当参考图提交，再按文档要求用「图片N」（N = 该图在 content 中 image_url 的顺序）把它钉成首帧。
func planArkMedia(ctx context.Context, req VideoGenRequest, supportsReference bool, maxImages int) (arkMediaPlan, error) {
	var plan arkMediaPlan
	// 先去重再把用户上传的图顶到前面：超上限截断时先丢模板生成图，不依赖调用方的入参顺序
	refs := orderCharacterImages(dedupeCharacterImages(req.CharacterImages))
	firstFrame := strings.TrimSpace(req.FirstFrame)
	lastFrame := strings.TrimSpace(req.LastFrame)
	entries := make([]arkImageEntry, 0, len(refs)+2)
	// 严格首帧与参考模式互斥：有首帧要锁时不能同时吃角色图，反之亦然
	referenceMode := supportsReference && len(refs) > 0 && !(req.StrictFirstFrame && firstFrame != "")

	switch {
	case referenceMode:
		if firstFrame != "" {
			entries = append(entries, arkImageEntry{ref: firstFrame, role: "reference_image", source: "首帧图片", label: "本段首帧画面（画面必须从这张图开始）"})
		}
		for _, ref := range refs {
			entries = append(entries, arkImageEntry{
				ref:    strings.TrimSpace(ref.URL),
				role:   "reference_image",
				source: "角色参考图",
				label:  arkCharacterLabel(ref),
			})
		}
		if lastFrame != "" {
			entries = append(entries, arkImageEntry{ref: lastFrame, role: "reference_image", source: "尾帧图片", label: "本段尾帧画面（画面结束于这张图）"})
		}
		if maxImages > 0 && len(entries) > maxImages {
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"角色参考图超出模型上限（全模态参考最多 %d 张，首帧图也计入），已只提交前 %d 张", maxImages, maxImages))
			entries = entries[:maxImages]
		}
	default:
		if firstFrame != "" {
			entries = append(entries, arkImageEntry{ref: firstFrame, role: "first_frame", source: "首帧图片"})
		}
		if lastFrame != "" {
			entries = append(entries, arkImageEntry{ref: lastFrame, role: "last_frame", source: "尾帧图片"})
		}
		switch {
		case len(refs) == 0:
		case !supportsReference:
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"当前方舟模型不支持角色参考图（role=reference_image 仅 Seedance 2.0 系列 / 2.5 支持），已忽略 %d 张角色图，跨段一致性改用提示词里的外观锚点", len(refs)))
		default:
			// 严格首帧优先：官方任务类型互斥，锁帧与角色参考图二选一，帧赢
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"严格首帧已开启：本段按「图生视频-首帧」任务提交，画面从首帧图开始；任务类型互斥，%d 张角色参考图本轮不提交（形象由首帧画面与提示词里的外观锚点保持）。需要同时喂角色图请关闭「严格首帧」。", len(refs)))
		}
	}

	plan.LockedByFrame = firstFrame != "" || lastFrame != ""
	bindings := make([]string, 0, len(entries))
	for _, entry := range entries {
		ref, err := arkImageRef(ctx, entry.ref)
		if err != nil {
			return plan, fmt.Errorf("%s不可用: %w", entry.source, err)
		}
		if ref == "" {
			continue
		}
		plan.Images = append(plan.Images, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": ref}, "role": entry.role,
		})
		if entry.label != "" {
			bindings = append(bindings, fmt.Sprintf("图片%d 是%s", len(plan.Images), entry.label))
		}
	}
	if len(bindings) > 0 {
		plan.Binding = "素材引用（序号按提交的图片顺序，不可改动）：" + strings.Join(bindings, "；") +
			"。画面里出现的角色必须与其特征参考图完全一致（体型、毛色、服饰、道具），不得自行改写形象。"
		if hasUserUploadedCharacterImage(refs) {
			plan.Binding += "标注为“用户上传的定妆图”的那张是该角色的形象基准，与其他描述冲突时以它为准。"
		}
	}
	return plan, nil
}

// arkCharacterLabel 提示词里对一张角色图的称呼。用户上传的定妆图要把来源写明白：
// 模型收到多张图时需要一条裁决规则，不然会把几张图的特征平均到一起。
func arkCharacterLabel(ref VideoCharacterImage) string {
	name := firstNonEmpty(ref.Name, ref.RoleID)
	if ref.Source == CharacterImageSourceUser {
		return fmt.Sprintf("角色「%s」的形象参考图（用户上传的定妆图，以此图为准）", name)
	}
	return fmt.Sprintf("角色「%s」的形象参考图", name)
}

func hasUserUploadedCharacterImage(refs []VideoCharacterImage) bool {
	for _, ref := range refs {
		if ref.Source == CharacterImageSourceUser {
			return true
		}
	}
	return false
}

func (arkAdapter) Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error) {
	if strings.TrimSpace(gc.Provider.APIKey) == "" {
		return VideoGenResult{}, errors.New("火山方舟适配器缺少 API Key")
	}
	model := strings.TrimSpace(gc.Model)
	if model == "" {
		return VideoGenResult{}, errors.New("火山方舟适配器需要 Seedance 模型 ID（如火山方舟控制台里的 doubao-seedance-2-5-260628）")
	}
	endpoint := arkTaskEndpoint(gc.Provider.BaseURL)
	if endpoint == "" {
		return VideoGenResult{}, errors.New("火山方舟适配器需要 Base URL（如 https://ark.cn-beijing.volces.com/api/v3）")
	}

	caps := arkCapabilities(arkSupportsNativeAudio(model), arkSupportsReferenceImages(model))
	media, err := planArkMedia(ctx, req, caps.CharacterReference, arkReferenceImageLimit(model))
	if err != nil {
		return VideoGenResult{}, err
	}
	prompt := strings.TrimSpace(req.Prompt)
	if media.Binding != "" {
		prompt += "\n" + media.Binding
	}
	content := []map[string]any{{"type": "text", "text": prompt}}
	content = append(content, media.Images...)

	duration := nearestAllowedDuration(float64(req.DurationS), caps.AllowedDurations)
	// 首帧/尾帧图片会把输出画幅钉在图片自己的比例上
	ratio, downgraded := arkRatio(req.AspectRatio, media.LockedByFrame)
	payload := map[string]any{
		"model":     model,
		"content":   content,
		"ratio":     ratio,
		"duration":  duration,
		"watermark": false,
	}
	if caps.NativeAudio {
		payload["generate_audio"] = true
	}
	if req.Seed > 0 {
		payload["seed"] = req.Seed
	}
	var notes []string
	notes = append(notes, media.Notes...)
	if downgraded {
		notes = append(notes, fmt.Sprintf(
			"方舟首帧/首尾帧任务的输出画幅跟随首帧图（ratio 已按官方约束改为 adaptive），任务选的 %s 将在合成阶段统一画幅",
			strings.TrimSpace(req.AspectRatio),
		))
	}

	client := videoHTTPClient()
	headers := map[string]string{"Authorization": "Bearer " + gc.Provider.APIKey}
	data, err := videoRequest(ctx, client, http.MethodPost, endpoint, headers, payload)
	if err != nil {
		return VideoGenResult{}, fmt.Errorf("提交火山方舟视频任务失败: %w", err)
	}
	var submitted struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(data, &submitted); err != nil {
		return VideoGenResult{}, fmt.Errorf("解析火山方舟任务响应失败: %w（响应：%s）", err, bodySnippet(data))
	}
	taskID := firstNonEmpty(submitted.ID, submitted.TaskID)
	if taskID == "" {
		return VideoGenResult{}, fmt.Errorf("火山方舟没有返回任务 ID: %s", apiErrorDetail(data))
	}
	pollURL := endpoint + "/" + url.PathEscape(taskID)

	videoURL, err := pollVideoJob(ctx, pollURL, headers, parseArkTaskStatus)
	if err != nil {
		return VideoGenResult{}, fmt.Errorf("任务 %s: %w", taskID, err)
	}
	// 直链签名过期/对象暂时不可见时，回查任务拿新鲜链接重下
	attempts, err := downloadGeneratedVideo(ctx, videoURL, req.OutputPath, nil, func(refreshCtx context.Context) (string, error) {
		return refreshJobResult(refreshCtx, pollURL, headers, parseArkTaskStatus)
	})
	if err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       req.Fps,
		DurationS: float64(duration),
		Seed:      req.Seed,
		TaskID:    taskID,
		ModelVer:  model,
		Notes:     append(notes, videoRetryNote("成片下载", attempts)...),
	}, nil
}

// parseArkTaskStatus 解析查询任务响应：状态在 status，视频在 content.video_url，失败原因在 error。
func parseArkTaskStatus(body []byte) (string, string, error) {
	var payload struct {
		Status  string `json:"status"`
		Content struct {
			VideoURL string `json:"video_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("响应结构无法解析: %w（响应：%s）", err, bodySnippet(body))
	}
	return payload.Status, payload.Content.VideoURL, nil
}

/* ------------------------------ Runway ------------------------------ */

type runwayAdapter struct{}

func (runwayAdapter) Capabilities() VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame: true,
		LastFrame:     true,
		// image_to_video 只接 promptImage 一张图，没有角色参考字段；声明成 true 会让上层误以为参考图已生效
		CharacterReference:  false,
		NativeAudio:         false,
		AllowedDurations:    []int{5, 10},
		MaxClipS:            10,
		AllowedAspectRatios: []string{"16:9", "9:16", "1:1"},
		MaxResolution:       "1080P",
		PromptStyle:         "runway",
		CreditsPerClip:      0,
	}
}

func (runwayAdapter) Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error) {
	if gc.Provider.APIKey == "" {
		return VideoGenResult{}, errors.New("Runway 适配器缺少 API Key")
	}
	imageURL, err := requireRemoteImage(req.FirstFrame, "首帧图片")
	if err != nil {
		return VideoGenResult{}, err
	}
	base := originOf(gc.Provider.BaseURL)
	if base == "" {
		base = "https://api.dev.runwayml.com"
	}
	headers := map[string]string{
		"Authorization":    "Bearer " + gc.Provider.APIKey,
		"X-Runway-Version": "2024-11-06",
	}
	model := firstNonEmpty(gc.Model, "gen4_turbo")
	ratio := "1280:720"
	switch req.AspectRatio {
	case "9:16":
		ratio = "720:1280"
	case "1:1":
		ratio = "960:960"
	}
	payload := map[string]any{
		"model":       model,
		"promptImage": imageURL,
		"promptText":  req.Prompt,
		"duration":    nearestAllowedDuration(float64(req.DurationS), []int{5, 10}),
		"ratio":       ratio,
	}
	if req.Seed > 0 {
		payload["seed"] = req.Seed
	}

	client := videoHTTPClient()
	data, err := videoRequest(ctx, client, http.MethodPost, base+"/v1/image_to_video", headers, payload)
	if err != nil {
		return VideoGenResult{}, err
	}
	var submitted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &submitted); err != nil || submitted.ID == "" {
		return VideoGenResult{}, fmt.Errorf("提交 Runway 任务失败: %s", apiErrorDetail(data))
	}

	videoURL, err := pollVideoJob(ctx, base+"/v1/tasks/"+submitted.ID, headers,
		func(body []byte) (string, string, error) {
			var payload struct {
				Status string   `json:"status"`
				Output []string `json:"output"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return "", "", err
			}
			result := ""
			if len(payload.Output) > 0 {
				result = payload.Output[0]
			}
			return payload.Status, result, nil
		})
	if err != nil {
		return VideoGenResult{}, err
	}
	attempts, err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil)
	if err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       req.Fps,
		DurationS: float64(nearestAllowedDuration(float64(req.DurationS), []int{5, 10})),
		Seed:      req.Seed,
		TaskID:    submitted.ID,
		ModelVer:  model,
		Notes:     videoRetryNote("成片下载", attempts),
	}, nil
}

/* ------------------------------ Google Veo ------------------------------ */

type veoAdapter struct{}

func (veoAdapter) Capabilities() VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:       true,
		LastFrame:           false,
		CharacterReference:  false,
		NativeAudio:         true,
		AllowedDurations:    []int{4, 6, 8},
		MaxClipS:            8,
		AllowedAspectRatios: []string{"16:9", "9:16"},
		MaxResolution:       "1080P",
		PromptStyle:         "veo",
		CreditsPerClip:      0,
	}
}

func (veoAdapter) Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error) {
	if gc.Provider.APIKey == "" {
		return VideoGenResult{}, errors.New("Veo 适配器缺少 API Key")
	}
	base := originOf(gc.Provider.BaseURL)
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	model := firstNonEmpty(gc.Model, "veo-3.1-generate-preview")
	apiKey := url.QueryEscape(gc.Provider.APIKey)

	instance := map[string]any{"prompt": req.Prompt}
	if req.FirstFrame != "" {
		imageBytes, mimeType, err := readImageBytes(ctx, req.FirstFrame)
		if err != nil {
			return VideoGenResult{}, err
		}
		instance["image"] = map[string]any{
			"bytesBase64Encoded": base64.StdEncoding.EncodeToString(imageBytes),
			"mimeType":           mimeType,
		}
	}
	duration := nearestAllowedDuration(float64(req.DurationS), []int{4, 6, 8})
	payload := map[string]any{
		"instances": []any{instance},
		"parameters": map[string]any{
			"aspectRatio":     req.AspectRatio,
			"durationSeconds": duration,
			"sampleCount":     1,
		},
	}

	client := videoHTTPClient()
	headers := map[string]string{"x-goog-api-key": gc.Provider.APIKey}
	submitURL := fmt.Sprintf("%s/v1beta/models/%s:predictLongRunning?key=%s", base, model, apiKey)
	data, err := videoRequest(ctx, client, http.MethodPost, submitURL, headers, payload)
	if err != nil {
		return VideoGenResult{}, err
	}
	var operation struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &operation); err != nil || operation.Name == "" {
		return VideoGenResult{}, fmt.Errorf("提交 Veo 任务失败: %s", apiErrorDetail(data))
	}

	videoURL := ""
	for attempt := 0; attempt < 120; attempt++ {
		if err := ctx.Err(); err != nil {
			return VideoGenResult{}, err
		}
		body, err := videoRequest(ctx, client, http.MethodGet,
			fmt.Sprintf("%s/v1beta/%s?key=%s", base, operation.Name, apiKey), headers, nil)
		if err != nil {
			return VideoGenResult{}, err
		}
		var status struct {
			Done  bool `json:"done"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Response struct {
				GenerateVideoResponse struct {
					GeneratedSamples []struct {
						Video struct {
							URI                string `json:"uri"`
							BytesBase64Encoded string `json:"bytesBase64Encoded"`
						} `json:"video"`
					} `json:"generatedSamples"`
				} `json:"generateVideoResponse"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &status); err != nil {
			return VideoGenResult{}, fmt.Errorf("解析 Veo 任务状态失败: %w", err)
		}
		if status.Error.Message != "" {
			return VideoGenResult{}, fmt.Errorf("Veo 生成失败: %s", status.Error.Message)
		}
		if status.Done {
			samples := status.Response.GenerateVideoResponse.GeneratedSamples
			if len(samples) == 0 {
				return VideoGenResult{}, errors.New("Veo 生成完成但没有返回视频")
			}
			video := samples[0].Video
			if video.BytesBase64Encoded != "" {
				raw, err := base64.StdEncoding.DecodeString(video.BytesBase64Encoded)
				if err != nil {
					return VideoGenResult{}, fmt.Errorf("解码 Veo 视频失败: %w", err)
				}
				if err := writeBinaryFile(req.OutputPath, raw); err != nil {
					return VideoGenResult{}, err
				}
				return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(duration), Seed: req.Seed, TaskID: operation.Name, ModelVer: model}, nil
			}
			videoURL = video.URI
			break
		}
		select {
		case <-ctx.Done():
			return VideoGenResult{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if videoURL == "" {
		return VideoGenResult{}, errors.New("等待 Veo 生成结果超时")
	}
	downloadURL := videoURL
	if !strings.Contains(downloadURL, "key=") {
		separator := "?"
		if strings.Contains(downloadURL, "?") {
			separator = "&"
		}
		downloadURL += separator + "key=" + apiKey
	}
	attempts, err := downloadVideoFile(ctx, downloadURL, req.OutputPath, nil)
	if err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(duration), Seed: req.Seed, TaskID: operation.Name, ModelVer: model, Notes: videoRetryNote("成片下载", attempts)}, nil
}

/* ------------------------------ 通用 HTTP 适配器 ------------------------------ */

type genericAdapter struct{}

func (genericAdapter) Capabilities() VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:       true,
		LastFrame:           true,
		CharacterReference:  true,
		NativeAudio:         false,
		AllowedDurations:    []int{5, 10},
		MaxClipS:            10,
		AllowedAspectRatios: []string{"16:9", "9:16", "1:1"},
		MaxResolution:       "1080P",
		PromptStyle:         "generic",
		CreditsPerClip:      0,
	}
}

func (genericAdapter) Generate(ctx context.Context, gc videoGenContext, req VideoGenRequest) (VideoGenResult, error) {
	if gc.Provider.APIKey == "" || gc.Provider.BaseURL == "" {
		return VideoGenResult{}, errors.New("通用适配器需要提供商 Base URL 与 API Key")
	}
	base := apiRoot(gc.Provider.BaseURL)
	headers := map[string]string{"Authorization": "Bearer " + gc.Provider.APIKey}

	imageRef := req.FirstFrame
	if imageRef != "" && !isRemoteRef(imageRef) {
		raw, mimeType, err := readImageBytes(ctx, imageRef)
		if err != nil {
			return VideoGenResult{}, err
		}
		imageRef = fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(raw))
	}
	payload := map[string]any{
		"model":        gc.Model,
		"prompt":       req.Prompt,
		"image":        imageRef,
		"duration":     nearestAllowedDuration(float64(req.DurationS), []int{5, 10}),
		"aspect_ratio": req.AspectRatio,
		"seed":         req.Seed,
	}
	if req.LastFrame != "" {
		payload["last_frame"] = req.LastFrame
	}
	if len(req.CharacterImages) > 0 {
		urls := make([]string, 0, len(req.CharacterImages))
		for _, image := range req.CharacterImages {
			if strings.TrimSpace(image.URL) != "" {
				urls = append(urls, strings.TrimSpace(image.URL))
			}
		}
		if len(urls) > 0 {
			payload["reference_images"] = urls
		}
	}

	client := videoHTTPClient()
	candidates := genericVideoCandidates(base)
	var (
		bodyData []byte
		pollBase string
		failures []string
	)
	for _, endpoint := range candidates {
		data, err := videoRequest(ctx, client, http.MethodPost, endpoint, headers, payload)
		if err == nil {
			bodyData = data
			pollBase = endpoint
			break
		}
		failures = append(failures, err.Error())
	}
	if bodyData == nil {
		return VideoGenResult{}, fmt.Errorf("通用适配器 %d 个候选端点均失败：%s%s", len(failures),
			strings.Join(failures, "；"), genericAdapterHint(gc))
	}

	var submitted struct {
		ID       string `json:"id"`
		TaskID   string `json:"task_id"`
		Status   string `json:"status"`
		URL      string `json:"url"`
		VideoURL string `json:"video_url"`
		Data     []struct {
			URL string `json:"url"`
		} `json:"data"`
		Output struct {
			URL string `json:"url"`
		} `json:"output"`
	}
	if err := json.Unmarshal(bodyData, &submitted); err != nil {
		return VideoGenResult{}, fmt.Errorf("解析通用适配器响应失败: %w", err)
	}
	immediate := firstNonEmpty(submitted.URL, submitted.VideoURL, submitted.Output.URL)
	if immediate == "" && len(submitted.Data) > 0 {
		immediate = submitted.Data[0].URL
	}
	if immediate != "" {
		attempts, err := downloadVideoFile(ctx, immediate, req.OutputPath, nil)
		if err != nil {
			return VideoGenResult{}, err
		}
		return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(req.DurationS), Seed: req.Seed, ModelVer: gc.Model, Notes: videoRetryNote("成片下载", attempts)}, nil
	}

	taskID := firstNonEmpty(submitted.TaskID, submitted.ID)
	if taskID == "" {
		return VideoGenResult{}, fmt.Errorf("通用适配器未返回任务 ID 或视频地址: %s", apiErrorDetail(bodyData))
	}
	videoURL, err := pollVideoJob(ctx, pollBase+"/"+taskID, headers,
		func(body []byte) (string, string, error) {
			var payload struct {
				Status   string `json:"status"`
				State    string `json:"state"`
				URL      string `json:"url"`
				VideoURL string `json:"video_url"`
				Data     []struct {
					URL string `json:"url"`
				} `json:"data"`
				Output struct {
					URL string `json:"url"`
				} `json:"output"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return "", "", err
			}
			status := firstNonEmpty(payload.Status, payload.State)
			result := firstNonEmpty(payload.URL, payload.VideoURL, payload.Output.URL)
			if result == "" && len(payload.Data) > 0 {
				result = payload.Data[0].URL
			}
			return status, result, nil
		})
	if err != nil {
		return VideoGenResult{}, err
	}
	attempts, err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil)
	if err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(req.DurationS), Seed: req.Seed, TaskID: taskID, ModelVer: gc.Model, Notes: videoRetryNote("成片下载", attempts)}, nil
}

// genericVideoCandidates 给出通用 REST 视频端点候选。
// Base URL 已到版本段时不再叠加 /v1，避免造出 .../api/v3/v1/video/generations 这种不存在的路径。
func genericVideoCandidates(base string) []string {
	if base == "" {
		return nil
	}
	candidates := []string{base + "/video/generations"}
	if !hasVersionSegment(base) {
		candidates = append(candidates, base+"/v1/video/generations")
	}
	return candidates
}

// genericAdapterHint 在通用适配器全军覆没时给出可执行的改道建议。
func genericAdapterHint(gc videoGenContext) string {
	haystack := strings.ToLower(gc.Provider.BaseURL + " " + gc.Provider.Name + " " + gc.Model)
	switch {
	case strings.Contains(haystack, "volces"), strings.Contains(haystack, "ark.cn"), strings.Contains(haystack, "seedance"):
		return "；火山方舟的视频模型（Seedance）不走通用端点，请在任务里选择「火山方舟 Seedance」适配器"
	case strings.Contains(haystack, "dashscope"), strings.Contains(haystack, "aliyuncs"):
		return "；阿里云百炼视频请选「DashScope 视频」适配器"
	case strings.Contains(haystack, "runway"):
		return "；Runway 请选「Runway Gen-4」适配器（image_to_video 端点）"
	}
	return "；若服务商有自己的视频任务端点，请在任务里改选对应适配器，或把 Base URL 填到版本段（如 https://host/v1）"
}

/* ------------------------------ HTTP 工具 ------------------------------ */

func videoHTTPClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}

func videoRequest(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, payload any) ([]byte, error) {
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("构造请求失败: %w", err)
		}
		body = raw
	}
	// 序列化一次、每次重试重建 reader：重试靠轮询建任务接口也适用，不能让第二次 attempt 拿到空体
	var data []byte
	_, err := videoRetry(ctx, func() error {
		var callErr error
		data, callErr = doVideoRequestOnce(ctx, client, method, endpoint, headers, body)
		return callErr
	})
	return data, err
}

// doVideoRequestOnce 单次请求。非 2xx 包成 httpStatusError，让重试策略能按方法+状态码区分对待。
func doVideoRequestOnce(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s 请求失败: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpStatusError{
			Method: method, URL: endpoint, Code: resp.StatusCode, Status: resp.Status, Detail: apiErrorDetail(data),
		}
	}
	return data, nil
}

// pollVideoJob 提交→轮询的通用异步任务模型（§5.2）。
func pollVideoJob(ctx context.Context, endpoint string, headers map[string]string, parse func([]byte) (string, string, error)) (string, error) {
	client := videoHTTPClient()
	deadline := time.Now().Add(30 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return "", errors.New("等待生成结果超时（30 分钟）")
		}
		body, err := videoRequest(ctx, client, http.MethodGet, endpoint, headers, nil)
		if err != nil {
			return "", err
		}
		status, result, err := parse(body)
		if err != nil {
			return "", fmt.Errorf("解析任务状态失败: %w", err)
		}
		switch strings.ToUpper(strings.TrimSpace(status)) {
		case "SUCCEEDED", "SUCCESS", "COMPLETED", "DONE", "FINISHED":
			if result == "" {
				return "", fmt.Errorf("生成成功但没有返回视频地址: %s", bodySnippet(body))
			}
			return result, nil
		case "FAILED", "FAILURE", "ERROR", "CANCELED", "CANCELLED", "EXPIRED", "TIMEOUT":
			return "", fmt.Errorf("模型侧生成失败(%s): %s", strings.TrimSpace(status), apiErrorDetail(body))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// downloadVideoFile 下载成片并落盘。方舟成片存在对象存储上（如
// ark-acg-*.tos-cn-beijing.volces.com），Get 报「net/http: TLS handshake timeout」这类抖动
// 会重建 TLS 会话按退避重试（重试次数回传给上层写进任务日志）；
// 每次重试重建文件，不会拼上坏数据。
func downloadVideoFile(ctx context.Context, rawURL, dest string, headers map[string]string) (int, error) {
	return videoRetryWith(ctx, videoDownloadAttempts, func() error {
		return downloadVideoOnce(ctx, rawURL, dest, headers)
	})
}

// videoURLRefreshRounds 直链本身失效时最多回查几次任务状态换新签名。
const videoURLRefreshRounds = 2

// downloadGeneratedVideo 下载成片，并在「链接本身不再可用」时换一条新链接再来一轮。
// 厂商给的视频地址是带签名的临时直链：403 多为签名过期/签名不匹配，404 多为对象刚写入还不可见，
// 这两种错重发同一条 URL 一定继续吃闭门羹（downloadVideoFile 故意不重试 4xx）。
// 而回查任务状态是 GET、幂等、不二次计费，能拿到一条新鲜签名链接。
// refresh 为 nil 时等同于 downloadVideoFile。
func downloadGeneratedVideo(ctx context.Context, rawURL, dest string, headers map[string]string, refresh func(context.Context) (string, error)) (int, error) {
	current := rawURL
	total := 0
	for round := 0; ; round++ {
		attempts, err := downloadVideoFile(ctx, current, dest, headers)
		total += attempts
		if err == nil {
			return total, nil
		}
		if refresh == nil || round >= videoURLRefreshRounds || !isStaleSignedURLError(err) {
			return total, err
		}
		fresh, refreshErr := refresh(ctx)
		if refreshErr != nil || strings.TrimSpace(fresh) == "" || fresh == current {
			// 换不到新链接就把原始下载错误交回去，别用排障信息更少的错替掉它
			return total, err
		}
		current = fresh
	}
}

// isStaleSignedURLError 直链失效特征：鉴权不过（401/403）或对象暂时不可见（404）。
// 其余 4xx（如 400 参数非法）说明是我们自己请求写错了，换链接也没用。
func isStaleSignedURLError(err error) bool {
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.Code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// refreshJobResult 单次回查任务状态取视频地址；轮询接口本身幂等，重查不产生费用。
func refreshJobResult(ctx context.Context, endpoint string, headers map[string]string, parse func([]byte) (string, string, error)) (string, error) {
	body, err := videoRequest(ctx, videoHTTPClient(), http.MethodGet, endpoint, headers, nil)
	if err != nil {
		return "", err
	}
	_, result, err := parse(body)
	if err != nil {
		return "", fmt.Errorf("回查视频地址失败: %w", err)
	}
	if strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("回查任务没有返回视频地址: %s", bodySnippet(body))
	}
	return result, nil
}

func downloadVideoOnce(ctx context.Context, rawURL, dest string, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("构造下载请求失败: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := videoResilientClient(30 * time.Minute).Do(req)
	if err != nil {
		return fmt.Errorf("下载生成结果失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("下载生成结果失败: %w", &httpStatusError{
			Method: http.MethodGet, URL: rawURL, Code: resp.StatusCode, Status: resp.Status, Detail: apiErrorDetail(body),
		})
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("创建片段目录失败: %w", err)
	}
	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("创建视频文件失败: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(file, resp.Body); err != nil {
		// 半截 mp4 留着除了干扰下一轮排障没有用处：先关句柄再删（Windows 上开着删不掉），重试从干净状态开始
		_ = file.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("写入视频文件失败: %w", err)
	}
	return nil
}

func writeBinaryFile(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return nil
}

// fetchRemoteImage 单次远程图片拉取。非 2xx 包成 httpStatusError，交给重试策略按状态码判定：
// 签名 URL 过期这类 4xx 不重试，第一时间报错比干等四轮有用。
func fetchRemoteImage(ctx context.Context, ref string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("构造图片下载请求失败: %w", err)
	}
	resp, err := videoResilientClient(60 * time.Second).Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载远程图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("下载远程图片失败: %w", &httpStatusError{
			Method: http.MethodGet, URL: ref, Code: resp.StatusCode, Status: resp.Status, Detail: apiErrorDetail(body),
		})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取远程图片失败: %w", err)
	}
	return data, nil
}

// readImageBytes 读取本地图片或下载远程图片，供需要内联图片的厂商使用。
// 远程拉取同样走瞬时故障重试：握手超时死在提交前，跟死在下载一样亏。
func readImageBytes(ctx context.Context, ref string) ([]byte, string, error) {
	mimeType := imageMimeType(ref)
	if isRemoteRef(ref) {
		var data []byte
		// 提交前的内联图，重试次数不必回报用户，失败时错误文本里会带上「已重试 N 次」
		_, err := videoRetry(ctx, func() error {
			var fetchErr error
			data, fetchErr = fetchRemoteImage(ctx, ref)
			return fetchErr
		})
		if err != nil {
			return nil, mimeType, err
		}
		return data, mimeType, nil
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, mimeType, fmt.Errorf("读取首帧图片失败: %w", err)
	}
	return data, mimeType, nil
}

func imageMimeType(ref string) string {
	ext := strings.ToLower(filepath.Ext(ref))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func requireRemoteImage(ref, label string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("%s 为空：该适配器要求先提供首帧图片 URL", label)
	}
	if !isRemoteRef(ref) {
		return "", fmt.Errorf("%s 需为公网可访问的图片 URL（当前是本地文件：%s）", label, ref)
	}
	return ref, nil
}

func trimAPIBase(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// originOf 去掉路径，只保留 scheme://host，适配器自己拼版本路径。
func originOf(baseURL string) string {
	base := trimAPIBase(baseURL)
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return base
	}
	return parsed.Scheme + "://" + parsed.Host
}
