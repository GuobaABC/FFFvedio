package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 文本模型的三套请求方言（见《模型 API 提供商接口参考》§2）：
//   - openai：OpenAI 兼容 /chat/completions，绝大多数国内厂商与聚合网关都走这套；
//   - anthropic：/v1/messages，x-api-key + anthropic-version + 顶层 system/max_tokens；
//   - gemini：/v1beta/models/{model}:generateContent，x-goog-api-key + parts/inline_data。
const (
	DialectAuto      = "auto"
	DialectOpenAI    = "openai"
	DialectAnthropic = "anthropic"
	DialectGemini    = "gemini"
)

// anthropicVersion 用官方稳定版本的 header 值；Anthropic 要求显式声明。
const anthropicVersion = "2023-06-01"

// 单张参考图的体积上限，避免把整段视频或超大截图塞进请求体。
const maxImageBytes = 12 << 20

func validDialect(dialect string) bool {
	switch dialect {
	case DialectAuto, DialectOpenAI, DialectAnthropic, DialectGemini:
		return true
	}
	return false
}

// resolveLLMDialect 优先使用用户显式配置，其次按域名推断（文档 §6.2：端点全部做成配置项）。
func resolveLLMDialect(provider Provider) string {
	if dialect := strings.ToLower(strings.TrimSpace(provider.Dialect)); validDialect(dialect) && dialect != DialectAuto {
		return dialect
	}
	base := strings.ToLower(provider.BaseURL)
	switch {
	case strings.Contains(base, "anthropic"):
		return DialectAnthropic
	case strings.Contains(base, "googleapis.com"), strings.Contains(base, "generativelanguage"), strings.Contains(base, "/v1beta"):
		return DialectGemini
	}
	return DialectOpenAI
}

// chatCompletion 走方言适配层，返回模型原始文本。
func (a *App) chatCompletion(ctx context.Context, provider Provider, system, user string) (string, error) {
	return a.chatCompletionWithImages(ctx, provider, system, user, nil)
}

// chatCompletionWithImages 支持把首帧图 / 角色定妆图作为多模态输入（文档 §5 的 B 类能力）。
// images 里的元素可以是本地路径、http(s) URL 或 data URI。
func (a *App) chatCompletionWithImages(ctx context.Context, provider Provider, system, user string, images []string) (string, error) {
	content, _, err := a.chatCompletionTraced(ctx, provider, system, user, images)
	return content, err
}

// chatCompletionTraced 额外返回“被跳过的参考图”这类降级提示，
// 避免图片没生效时日志里完全看不出来。
func (a *App) chatCompletionTraced(ctx context.Context, provider Provider, system, user string, images []string) (string, []string, error) {
	if strings.TrimSpace(provider.BaseURL) == "" {
		return "", nil, errors.New("文本模型缺少 Base URL")
	}
	if strings.TrimSpace(provider.Model) == "" {
		return "", nil, errors.New("文本模型缺少模型名称")
	}

	requestCtx, cancel := context.WithTimeout(ctx, agentRequestTimeout)
	defer cancel()

	switch resolveLLMDialect(provider) {
	case DialectAnthropic:
		return requestAnthropicMessages(requestCtx, provider, system, user, images)
	case DialectGemini:
		return requestGeminiContent(requestCtx, provider, system, user, images)
	default:
		return requestOpenAIChat(requestCtx, provider, system, user, images)
	}
}

// llmImagePayload 把一张参考图整理成「mime + base64 或 URL」，三套方言各自按需取用。
type llmImagePayload struct {
	MimeType string
	Base64   string
	URL      string
}

func loadLLMImage(source string) (llmImagePayload, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return llmImagePayload{}, errors.New("图片路径为空")
	}

	if strings.HasPrefix(strings.ToLower(source), "data:") {
		header, data, ok := strings.Cut(source[5:], ",")
		if !ok {
			return llmImagePayload{}, errors.New("data URI 格式不正确")
		}
		mimeType := strings.TrimSuffix(strings.SplitN(header, ";", 2)[0], ";base64")
		return llmImagePayload{MimeType: firstNonEmpty(mimeType, "image/png"), Base64: data}, nil
	}

	if parsed, err := url.Parse(source); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return llmImagePayload{URL: source, MimeType: "image/png"}, nil
	}

	info, err := os.Stat(source)
	if err != nil || info.IsDir() {
		return llmImagePayload{}, fmt.Errorf("参考图不可读：%s", source)
	}
	if info.Size() > maxImageBytes {
		return llmImagePayload{}, fmt.Errorf("参考图超过 %dMB：%s", maxImageBytes>>20, filepath.Base(source))
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return llmImagePayload{}, fmt.Errorf("读取参考图失败: %w", err)
	}

	mimeType := mime.TypeByExtension(strings.ToLower(path.Ext(source)))
	if !strings.HasPrefix(mimeType, "image/") {
		mimeType = "image/png"
	}
	return llmImagePayload{MimeType: mimeType, Base64: base64.StdEncoding.EncodeToString(data)}, nil
}

// collectLLMImages 读取所有参考图，单张失败只跳过该图，不阻断文本生成。
func collectLLMImages(images []string, warnings *[]string) []llmImagePayload {
	payloads := make([]llmImagePayload, 0, len(images))
	for _, source := range images {
		if strings.TrimSpace(source) == "" {
			continue
		}
		payload, err := loadLLMImage(source)
		if err != nil {
			if warnings != nil {
				*warnings = append(*warnings, err.Error())
			}
			continue
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func doJSONRequest(ctx context.Context, endpoint string, headers map[string]string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := (&http.Client{Timeout: agentRequestTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s 返回 %d%s: %s", endpoint, resp.StatusCode, statusReason(resp.Status), apiErrorDetail(data))
	}
	return data, nil
}

/* --------------------------- ① OpenAI 兼容 --------------------------- */

func requestOpenAIChat(ctx context.Context, provider Provider, system, user string, images []string) (string, []string, error) {
	warnings := make([]string, 0, 1)
	payloads := collectLLMImages(images, &warnings)

	userContent := any(user)
	if len(payloads) > 0 {
		blocks := make([]map[string]any, 0, len(payloads)+1)
		for _, image := range payloads {
			blocks = append(blocks, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": imageRefForOpenAI(image)},
			})
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": user})
		userContent = blocks
	}

	request := map[string]any{
		"model":       provider.Model,
		"temperature": 0.6,
		"stream":      false,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": userContent},
		},
	}

	data, err := doJSONRequest(ctx, chatCompletionsEndpoint(provider.BaseURL), map[string]string{
		"Authorization": "Bearer " + provider.APIKey,
	}, request)
	if err != nil {
		return "", warnings, fmt.Errorf("文本模型（OpenAI 兼容 %s）%w", provider.Model, err)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", warnings, fmt.Errorf("解析文本模型响应失败: %w（响应：%s）", err, bodySnippet(data))
	}
	if len(parsed.Choices) == 0 {
		return "", warnings, errors.New("文本模型没有返回任何结果：" + bodySnippet(data))
	}
	content := decodeOpenAIContent(parsed.Choices[0].Message.Content)
	if strings.TrimSpace(content) == "" {
		return "", warnings, errors.New("文本模型返回了空内容：" + bodySnippet(data))
	}
	return content, warnings, nil
}

func imageRefForOpenAI(image llmImagePayload) string {
	if image.Base64 != "" {
		return fmt.Sprintf("data:%s;base64,%s", image.MimeType, image.Base64)
	}
	return image.URL
}

// decodeOpenAIContent 兼容 content 为字符串或分段数组两种返回形态。
func decodeOpenAIContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

/* --------------------------- ② Anthropic Messages --------------------------- */

func anthropicEndpoint(baseURL string) string {
	base := apiRoot(baseURL)
	if base == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func requestAnthropicMessages(ctx context.Context, provider Provider, system, user string, images []string) (string, []string, error) {
	warnings := make([]string, 0, 1)
	payloads := collectLLMImages(images, &warnings)

	blocks := make([]map[string]any, 0, len(payloads)+1)
	for _, image := range payloads {
		if image.Base64 != "" {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": image.MimeType,
					"data":       image.Base64,
				},
			})
			continue
		}
		blocks = append(blocks, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "url", "url": image.URL},
		})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": user})

	request := map[string]any{
		"model":       provider.Model,
		"max_tokens":  4096,
		"temperature": 0.6,
		"system":      system,
		"messages": []map[string]any{
			{"role": "user", "content": blocks},
		},
	}

	data, err := doJSONRequest(ctx, anthropicEndpoint(provider.BaseURL), map[string]string{
		"x-api-key":         provider.APIKey,
		"anthropic-version": anthropicVersion,
	}, request)
	if err != nil {
		return "", warnings, fmt.Errorf("文本模型（Anthropic Messages %s）%w", provider.Model, err)
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", warnings, fmt.Errorf("解析文本模型响应失败: %w（响应：%s）", err, bodySnippet(data))
	}
	parts := make([]string, 0, len(parsed.Content))
	for _, block := range parsed.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	content := strings.Join(parts, "")
	if strings.TrimSpace(content) == "" {
		return "", warnings, errors.New("文本模型返回了空内容：" + bodySnippet(data))
	}
	return content, warnings, nil
}

/* --------------------------- ③ Google Gemini --------------------------- */

func geminiEndpoint(baseURL, model string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if strings.Contains(strings.ToLower(base), ":generatecontent") {
		return base
	}
	if !strings.HasSuffix(strings.ToLower(base), "/v1beta") && !strings.HasSuffix(strings.ToLower(base), "/v1") {
		base += "/v1beta"
	}
	return fmt.Sprintf("%s/models/%s:generateContent", base, url.PathEscape(model))
}

func requestGeminiContent(ctx context.Context, provider Provider, system, user string, images []string) (string, []string, error) {
	warnings := make([]string, 0, 1)
	payloads := collectLLMImages(images, &warnings)

	parts := make([]map[string]any, 0, len(payloads)+1)
	for _, image := range payloads {
		if image.Base64 != "" {
			parts = append(parts, map[string]any{
				"inline_data": map[string]any{"mime_type": image.MimeType, "data": image.Base64},
			})
			continue
		}
		parts = append(parts, map[string]any{
			"file_data": map[string]any{"file_uri": image.URL, "mime_type": image.MimeType},
		})
	}
	parts = append(parts, map[string]any{"text": user})

	request := map[string]any{
		"system_instruction": map[string]any{"parts": []map[string]any{{"text": system}}},
		"contents":           []map[string]any{{"role": "user", "parts": parts}},
		"generationConfig":   map[string]any{"temperature": 0.6},
	}

	data, err := doJSONRequest(ctx, geminiEndpoint(provider.BaseURL, provider.Model), map[string]string{
		"x-goog-api-key": provider.APIKey,
	}, request)
	if err != nil {
		return "", warnings, fmt.Errorf("文本模型（Gemini generateContent %s）%w", provider.Model, err)
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", warnings, fmt.Errorf("解析文本模型响应失败: %w（响应：%s）", err, bodySnippet(data))
	}
	texts := make([]string, 0, 4)
	for _, candidate := range parsed.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	content := strings.Join(texts, "")
	if strings.TrimSpace(content) == "" {
		return "", warnings, errors.New("文本模型返回了空内容：" + bodySnippet(data))
	}
	return content, warnings, nil
}
