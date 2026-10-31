package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const modelListTimeout = 20 * time.Second

// versionSuffixes 用于判断 Base URL 是否已包含版本段，
// 例如 DeepSeek 填 https://api.deepseek.com、通义填 .../compatible-mode/v1。
var versionSuffixes = []string{"/v1", "/v1beta", "/v2", "/v3", "/v4", "/api/v3", "/api/v4"}

// endpointSuffixes 是用户容易误当 Base URL 填入的具体接口路径，
// 例如火山方舟 https://ark.cn-beijing.volces.com/api/v3/responses。
// 先剥掉操作段再拼 /models，避免请求到 .../responses/models 这类不存在的路径。
var endpointSuffixes = []string{
	"/chat/completions",
	"/completions",
	"/responses",
	"/embeddings",
	"/images/generations",
	"/contents/generations/tasks",
	"/video/generations",
	"/audio/speech",
	"/messages",
	"/models",
}

// hasVersionSegment 判断 Base URL 是否已经带到版本段，避免再拼一次 /v1 造出 .../v3/v1/xxx。
func hasVersionSegment(base string) bool {
	lower := strings.ToLower(strings.TrimRight(base, "/"))
	for _, suffix := range versionSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// apiRoot 去掉接口操作段与尾部斜杠，得到可继续拼接子路径的 API 根。
func apiRoot(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range endpointSuffixes {
		if !strings.HasSuffix(strings.ToLower(base), suffix) {
			continue
		}
		trimmed := strings.TrimRight(base[:len(base)-len(suffix)], "/")
		// 剥离后仍需保留 scheme://host，否则说明原地址本身就是异常输入
		if index := strings.Index(trimmed, "://"); index >= 0 && len(trimmed) > index+3 {
			return trimmed
		}
	}
	return base
}

// modelListCandidates 给出候选模型列表端点。
// 绝大多数厂商 OpenAI 兼容（<base>/models 或 <base>/v1/models），逐个尝试取第一个可用的。
func modelListCandidates(baseURL string) []string {
	base := apiRoot(baseURL)
	if base == "" {
		return nil
	}

	candidates := []string{base + "/models"}
	if !hasVersionSegment(base) {
		candidates = append(candidates, base+"/v1/models")
	}
	return candidates
}

// modelListHeaders 按厂商差异选择认证方式：
// 主流 Authorization: Bearer；Anthropic 用 x-api-key；Google Gemini 用 x-goog-api-key。
func modelListHeaders(baseURL, apiKey string) map[string]string {
	host := strings.ToLower(baseURL)
	headers := map[string]string{"Accept": "application/json"}

	switch {
	case strings.Contains(host, "anthropic"):
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case strings.Contains(host, "googleapis.com"), strings.Contains(host, "generativelanguage"):
		headers["x-goog-api-key"] = apiKey
	default:
		headers["Authorization"] = "Bearer " + apiKey
	}
	return headers
}

// FetchModels 使用给定的 Base URL 与 API Key 请求服务商的模型列表。
// 前端「获取模型」按钮调用它：真正的请求由 Go 侧发出，避免浏览器 CORS 限制与密钥出现在页面网络请求中。
func (a *App) FetchModels(baseURL, apiKey string) ([]string, error) {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)

	if baseURL == "" {
		return nil, errors.New("请先填写 Base URL")
	}
	if apiKey == "" {
		return nil, errors.New("请先填写 API Key")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("Base URL 需为 http(s) 开头的完整地址")
	}

	candidates := modelListCandidates(baseURL)
	headers := modelListHeaders(baseURL, apiKey)
	client := &http.Client{Timeout: modelListTimeout}

	failures := make([]string, 0, len(candidates))
	for _, endpoint := range candidates {
		models, err := requestModelList(client, endpoint, headers)
		if err == nil && len(models) > 0 {
			return models, nil
		}
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		failures = append(failures, endpoint+" 返回空模型列表")
	}

	return nil, fmt.Errorf("获取模型列表失败：%s；%s", strings.Join(failures, "；"), modelListHint(baseURL))
}

// modelListHint 针对不提供 OpenAI 兼容 /models 的服务商给出可执行建议。
func modelListHint(baseURL string) string {
	host := strings.ToLower(baseURL)
	switch {
	case strings.Contains(host, "volces.com"):
		return "火山方舟不提供 /models 接口：Base URL 填 https://ark.cn-beijing.volces.com/api/v3，模型名直接填控制台里的模型 ID（如 doubao-seed-1-6-250615）或推理接入点 ID（ep-xxxxxxxx）"
	case strings.Contains(host, "dashscope.aliyuncs.com"):
		return "通义建议 Base URL 填 https://dashscope.aliyuncs.com/compatible-mode/v1，模型名如 qwen-plus"
	case strings.Contains(host, "bigmodel.cn"):
		return "智谱建议 Base URL 填 https://open.bigmodel.cn/api/paas/v4，模型名如 glm-4-plus"
	}
	return "请确认 Base URL 填到版本段（如 https://api.openai.com/v1）；若服务商不提供模型列表接口，直接手动填写文档中的模型 ID"
}

func requestModelList(client *http.Client, endpoint string, headers map[string]string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%s 请求构造失败: %w", endpoint, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s 请求失败: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s 读取响应失败: %w", endpoint, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d%s: %s", endpoint, resp.StatusCode, statusReason(resp.Status), apiErrorDetail(body))
	}

	models, err := parseModelList(body)
	if err != nil {
		return nil, fmt.Errorf("%s 响应解析失败: %w", endpoint, err)
	}
	return models, nil
}

// parseModelList 兼容三种常见结构：
// OpenAI / Anthropic: {"data":[{"id":"..."}]}
// Google Gemini:      {"models":[{"name":"models/..."}]}
// 简易网关:            ["model-a","model-b"] 或 [{"id":"model-a"}]
func parseModelList(body []byte) ([]string, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, errors.New("响应内容为空")
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"models"`
	}

	names := make([]string, 0, 16)
	if err := json.Unmarshal(body, &payload); err == nil {
		for _, item := range payload.Data {
			names = append(names, item.ID)
		}
		for _, item := range payload.Models {
			if item.Name != "" {
				names = append(names, item.Name)
			} else {
				names = append(names, item.ID)
			}
		}
	}

	if len(names) == 0 {
		var rawList []json.RawMessage
		if err := json.Unmarshal(body, &rawList); err != nil {
			return nil, errors.New("无法识别的模型列表结构")
		}
		for _, raw := range rawList {
			var asString string
			if err := json.Unmarshal(raw, &asString); err == nil {
				names = append(names, asString)
				continue
			}
			var asObject struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &asObject); err == nil {
				if asObject.ID != "" {
					names = append(names, asObject.ID)
				} else {
					names = append(names, asObject.Name)
				}
			}
		}
	}

	cleaned := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimPrefix(strings.TrimSpace(name), "models/")
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		cleaned = append(cleaned, name)
	}
	if len(cleaned) == 0 {
		return nil, errors.New("响应中没有可用模型")
	}

	sort.Strings(cleaned)
	return cleaned, nil
}

// bodySnippet 把响应体压成一行文本。仅在超过 1200 字符时才截断，
// 且按 rune 切，避免中文报错被切成乱码尾巴。
func bodySnippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if text == "" {
		return "(服务商返回空响应体)"
	}
	runes := []rune(text)
	const limit = 1200
	if len(runes) > limit {
		return string(runes[:limit]) + fmt.Sprintf("…(响应体共 %d 字符)", len(runes))
	}
	return text
}

// statusReason 取 HTTP 状态文本里除去数字码的部分（"404 Not Found" -> " Not Found"），
// 让日志能区分 404「路径不存在」与 404「资源已过期」这类同码不同因的情况。
func statusReason(status string) string {
	reason := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(status), "0123456789"))
	if reason == "" {
		return ""
	}
	return " " + reason
}

// apiErrorDetail 生成可直接定位问题的报错文本：
// 先取服务商的结构化错误字段（error.message / error.code / message / msg / code），
// 拿不到再退回原始响应体，避免日志里只剩「返回 404」这种无从下手的信息。
func apiErrorDetail(body []byte) string {
	parsed := extractAPIError(body)
	if parsed != "" {
		return parsed
	}
	return bodySnippet(body)
}

// extractAPIError 兼容各家错误结构，无结构化字段时返回空串。
func extractAPIError(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var payload struct {
		Error json.RawMessage `json:"error"`
		// 方舟/通义等网关也会把错误平铺在顶层
		Code      string `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		ErrorCode string `json:"error_code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	fields := make([]string, 0, 4)
	if nested := nestedErrorFields(payload.Error); len(nested) > 0 {
		fields = append(fields, nested...)
	}
	for _, value := range []string{payload.ErrorCode, payload.Code, payload.Message, payload.Msg} {
		if value = strings.TrimSpace(value); value != "" {
			fields = append(fields, value)
		}
	}
	if len(fields) == 0 {
		return ""
	}
	if id := strings.TrimSpace(payload.RequestID); id != "" {
		fields = append(fields, "request_id="+id)
	}
	return strings.Join(uniqueStrings(fields), " | ")
}

// nestedErrorFields 处理 error 既可能是对象也可能是字符串的两种写法。
func nestedErrorFields(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil
		}
		return []string{strings.TrimSpace(asString)}
	}
	var asObject struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
	}
	if err := json.Unmarshal(raw, &asObject); err != nil {
		return nil
	}
	fields := make([]string, 0, 4)
	for _, value := range []string{asObject.Code, asObject.Type, asObject.Message, asObject.Param} {
		if value = strings.TrimSpace(value); value != "" {
			fields = append(fields, value)
		}
	}
	return fields
}
