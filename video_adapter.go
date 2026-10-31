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

// VideoGenRequest 单段生成请求（模型无关）。
type VideoGenRequest struct {
	Prompt          string
	FirstFrame      string
	LastFrame       string
	CharacterImages []string
	DurationS       int
	AspectRatio     string
	Fps             int
	Seed            int64
	OutputPath      string
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

func adapterInfos() []VideoAdapterInfo {
	entries := []struct {
		id          string
		label       string
		description string
		adapter     videoAdapter
	}{
		{
			id:          "dashscope",
			label:       "DashScope 视频（万相 / 可灵）",
			description: "阿里云百炼异步任务接口，支持首帧/尾帧条件；首帧需公网 URL",
			adapter:     dashscopeAdapter{},
		},
		{
			id:          "ark",
			label:       "火山方舟 Seedance（豆包视频）",
			description: "方舟独立视频端点 /contents/generations/tasks，支持首帧/尾帧/参考图；Seedance 2.x 可音画同步",
			adapter:     arkAdapter{},
		},
		{
			id:          "runway",
			label:       "Runway Gen-4",
			description: "Runway image_to_video + 任务轮询；首帧需公网 URL",
			adapter:     runwayAdapter{},
		},
		{
			id:          "veo",
			label:       "Google Veo（Gemini API）",
			description: "Veo 长任务接口，原生音频路线，台词可直接写入提示词",
			adapter:     veoAdapter{},
		},
		{
			id:          "generic",
			label:       "通用 HTTP 适配器",
			description: "按通用 REST 约定提交/轮询视频任务，适配自建网关",
			adapter:     genericAdapter{},
		},
		{
			id:          "mock",
			label:       "本地联调占位（ffmpeg）",
			description: "用 ffmpeg 合成测试画面，仅用于验证流水线，不调用任何模型",
			adapter:     mockAdapter{},
		},
	}
	infos := make([]VideoAdapterInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, VideoAdapterInfo{
			ID:           entry.id,
			Label:        entry.label,
			Description:  entry.description,
			Capabilities: entry.adapter.Capabilities(),
		})
	}
	return infos
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

// adapterCapabilities 取适配器能力；部分适配器会按模型 ID 微调（如火山方舟仅 Seedance 2.x 有原生音频）。
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
		prompt += fmt.Sprintf("\n口播台词（只允许一人说话，保持口型在画面内）：“%s”", strings.TrimSpace(segment.Dialogue))
	}
	if caps.PromptStyle == "veo" {
		prompt += "\n限制：单镜头、不切镜，动作连续，保持人物与产品外观一致。"
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
	if err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil); err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       req.Fps,
		DurationS: float64(duration),
		Seed:      req.Seed,
		TaskID:    submitted.Output.TaskID,
		ModelVer:  gc.Model,
	}, nil
}

/* ------------------------------ 火山方舟 Seedance ------------------------------ */

// arkVideoPath 方舟视频任务的固定路径段（创建与轮询共用）。
const arkVideoPath = "/contents/generations/tasks"

// arkAdapter 对接火山方舟（豆包 Seedance）视频生成：
// 走独立异步端点 POST {base}/contents/generations/tasks 建任务，再 GET 同路径/{id} 轮询；
// chat 端点不吐视频（接口参考 §3.2），所以不能走通用 OpenAI 兼容适配器。
type arkAdapter struct{}

func (arkAdapter) Capabilities() VideoCapabilities { return arkCapabilities(true) }

// CapabilitiesFor 按模型微调：Seedance 1.0 不输出音轨，把台词写进提示词会出现「画面写字」副作用。
func (arkAdapter) CapabilitiesFor(model string) VideoCapabilities {
	return arkCapabilities(arkSupportsNativeAudio(model))
}

func arkCapabilities(nativeAudio bool) VideoCapabilities {
	return VideoCapabilities{
		I2VFirstFrame:      true,
		LastFrame:          true,
		CharacterReference: true,
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

// arkSupportsNativeAudio 仅 Seedance 2.x 系列支持音画同步输出。
func arkSupportsNativeAudio(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "seedance-2") || strings.Contains(lower, "seedance_2")
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

	content := []map[string]any{{"type": "text", "text": req.Prompt}}
	if req.FirstFrame != "" {
		ref, err := arkImageRef(ctx, req.FirstFrame)
		if err != nil {
			return VideoGenResult{}, fmt.Errorf("首帧图片不可用: %w", err)
		}
		if ref != "" {
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": ref}, "role": "first_frame",
			})
		}
	}
	if req.LastFrame != "" {
		ref, err := arkImageRef(ctx, req.LastFrame)
		if err != nil {
			return VideoGenResult{}, fmt.Errorf("尾帧图片不可用: %w", err)
		}
		if ref != "" {
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": ref}, "role": "last_frame",
			})
		}
	}
	for _, image := range req.CharacterImages {
		ref, err := arkImageRef(ctx, image)
		if err != nil {
			return VideoGenResult{}, fmt.Errorf("角色参考图不可用: %w", err)
		}
		if ref != "" {
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": ref}, "role": "reference_image",
			})
		}
	}

	caps := arkCapabilities(arkSupportsNativeAudio(model))
	duration := nearestAllowedDuration(float64(req.DurationS), caps.AllowedDurations)
	// 首帧/尾帧图片会把输出画幅钉在图片自己的比例上
	lockedByFrame := req.FirstFrame != "" || req.LastFrame != ""
	ratio, downgraded := arkRatio(req.AspectRatio, lockedByFrame)
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

	videoURL, err := pollVideoJob(ctx, endpoint+"/"+url.PathEscape(taskID), headers, parseArkTaskStatus)
	if err != nil {
		return VideoGenResult{}, fmt.Errorf("任务 %s: %w", taskID, err)
	}
	if err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil); err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       req.Fps,
		DurationS: float64(duration),
		Seed:      req.Seed,
		TaskID:    taskID,
		ModelVer:  model,
		Notes:     notes,
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
		I2VFirstFrame:       true,
		LastFrame:           true,
		CharacterReference:  true,
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
	if err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil); err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{
		VideoURI:  req.OutputPath,
		FPS:       req.Fps,
		DurationS: float64(nearestAllowedDuration(float64(req.DurationS), []int{5, 10})),
		Seed:      req.Seed,
		TaskID:    submitted.ID,
		ModelVer:  model,
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
	if err := downloadVideoFile(ctx, downloadURL, req.OutputPath, nil); err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(duration), Seed: req.Seed, TaskID: operation.Name, ModelVer: model}, nil
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
		payload["reference_images"] = req.CharacterImages
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
		if err := downloadVideoFile(ctx, immediate, req.OutputPath, nil); err != nil {
			return VideoGenResult{}, err
		}
		return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(req.DurationS), Seed: req.Seed, ModelVer: gc.Model}, nil
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
	if err := downloadVideoFile(ctx, videoURL, req.OutputPath, nil); err != nil {
		return VideoGenResult{}, err
	}
	return VideoGenResult{VideoURI: req.OutputPath, FPS: req.Fps, DurationS: float64(req.DurationS), Seed: req.Seed, TaskID: taskID, ModelVer: gc.Model}, nil
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
	var reader io.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("构造请求失败: %w", err)
		}
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	if payload != nil {
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
		return nil, fmt.Errorf("%s 返回 %d%s: %s", endpoint, resp.StatusCode, statusReason(resp.Status), apiErrorDetail(data))
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

func downloadVideoFile(ctx context.Context, rawURL, dest string, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("构造下载请求失败: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("下载生成结果失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("下载生成结果 %s 返回 %d%s: %s", rawURL, resp.StatusCode, statusReason(resp.Status), apiErrorDetail(body))
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

// readImageBytes 读取本地图片或下载远程图片，供需要内联图片的厂商使用。
func readImageBytes(ctx context.Context, ref string) ([]byte, string, error) {
	mimeType := imageMimeType(ref)
	if isRemoteRef(ref) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return nil, mimeType, fmt.Errorf("构造图片下载请求失败: %w", err)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return nil, mimeType, fmt.Errorf("下载首帧图片失败: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return nil, mimeType, fmt.Errorf("下载首帧图片 %s 返回 %d%s: %s", ref, resp.StatusCode, statusReason(resp.Status), apiErrorDetail(body))
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return nil, mimeType, fmt.Errorf("读取首帧图片失败: %w", err)
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
