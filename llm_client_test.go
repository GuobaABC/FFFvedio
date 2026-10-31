package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveLLMDialect(t *testing.T) {
	cases := []struct {
		provider Provider
		expected string
	}{
		{Provider{BaseURL: "https://api.anthropic.com"}, DialectAnthropic},
		{Provider{BaseURL: "https://generativelanguage.googleapis.com"}, DialectGemini},
		{Provider{BaseURL: "https://ark.cn-beijing.volces.com/api/v3"}, DialectOpenAI},
		// 显式配置优先于域名推断
		{Provider{BaseURL: "https://api.anthropic.com", Dialect: DialectOpenAI}, DialectOpenAI},
		{Provider{BaseURL: "https://example.com", Dialect: DialectGemini}, DialectGemini},
	}

	for _, tc := range cases {
		if got := resolveLLMDialect(tc.provider); got != tc.expected {
			t.Fatalf("%s 方言判定错误: %q", tc.provider.BaseURL, got)
		}
	}
}

func TestDialectEndpoints(t *testing.T) {
	if got := anthropicEndpoint("https://api.anthropic.com"); got != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("Anthropic 端点错误: %q", got)
	}
	if got := anthropicEndpoint("https://api.anthropic.com/v1/messages/"); got != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("Anthropic 端点应幂等: %q", got)
	}
	if got := geminiEndpoint("https://generativelanguage.googleapis.com", "gemini-2.5-pro"); got != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("Gemini 端点错误: %q", got)
	}
}

func TestChatCompletionAnthropicDialect(t *testing.T) {
	var gotKey, gotVersion string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"{\"logline\":\"ok\"}"}]}`)
	}))
	defer server.Close()

	app := newTestApp(t)
	got, err := app.chatCompletion(t.Context(), Provider{
		BaseURL: server.URL,
		APIKey:  "sk-ant",
		Model:   "claude-sonnet",
		Dialect: DialectAnthropic,
	}, "system", "user")
	if err != nil {
		t.Fatalf("Anthropic 方言调用失败: %v", err)
	}
	if got != `{"logline":"ok"}` {
		t.Fatalf("Anthropic 响应解析错误: %q", got)
	}
	if gotKey != "sk-ant" || gotVersion == "" {
		t.Fatalf("Anthropic 鉴权头错误: key=%q version=%q", gotKey, gotVersion)
	}
	if _, ok := gotBody["system"]; !ok {
		t.Fatal("Anthropic 请求应把 system 放在顶层字段")
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Fatal("Anthropic 请求必须带 max_tokens")
	}
}

func TestChatCompletionGeminiDialect(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"gemini-"}]}},{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	defer server.Close()

	app := newTestApp(t)
	got, err := app.chatCompletion(t.Context(), Provider{
		BaseURL: server.URL,
		APIKey:  "g-key",
		Model:   "gemini-2.5-pro",
		Dialect: DialectGemini,
	}, "system", "user")
	if err != nil {
		t.Fatalf("Gemini 方言调用失败: %v", err)
	}
	if got != "gemini-ok" {
		t.Fatalf("Gemini 响应应拼接所有 candidate：%q", got)
	}
	if gotKey != "g-key" {
		t.Fatalf("Gemini 鉴权头错误: %q", gotKey)
	}
	if !strings.Contains(gotPath, "/v1beta/models/gemini-2.5-pro:generateContent") {
		t.Fatalf("Gemini 端点错误: %q", gotPath)
	}
	if _, ok := gotBody["system_instruction"]; !ok {
		t.Fatal("Gemini 请求应带 system_instruction")
	}
}

func TestOpenAIContentSegmentsAndImages(t *testing.T) {
	if got := decodeOpenAIContent(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)); got != "ab" {
		t.Fatalf("分段 content 解析错误: %q", got)
	}
	if got := decodeOpenAIContent(json.RawMessage(`"plain"`)); got != "plain" {
		t.Fatalf("字符串 content 解析错误: %q", got)
	}

	payload, err := loadLLMImage("data:image/png;base64,AAAA")
	if err != nil || payload.MimeType != "image/png" || payload.Base64 != "AAAA" {
		t.Fatalf("data URI 解析错误: %+v %v", payload, err)
	}
	if payload, err := loadLLMImage("https://example.com/a.jpg"); err != nil || payload.URL == "" {
		t.Fatalf("远程图片应保留 URL: %+v %v", payload, err)
	}
	if _, err := loadLLMImage("D:/not-exists/a.png"); err == nil {
		t.Fatal("不存在的本地图片应返回错误")
	}
}
