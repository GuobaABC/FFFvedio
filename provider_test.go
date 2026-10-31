package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestApp(t *testing.T) *App {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		t.Fatalf("初始化测试表失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &App{db: db}
}

func TestProviderLifecycle(t *testing.T) {
	app := newTestApp(t)

	created, err := app.SaveProvider(Provider{
		Category: CategoryText,
		BaseURL:  "https://api.deepseek.com",
		APIKey:   "sk-deepseek",
		Model:    "deepseek-chat",
	})
	if err != nil {
		t.Fatalf("新增提供商失败: %v", err)
	}
	if created.ID == "" || created.CreatedAt == "" {
		t.Fatalf("新增后缺少 ID 或创建时间: %+v", created)
	}
	// 名称留空时回退为 Base URL 的 host
	if created.Name != "api.deepseek.com" {
		t.Fatalf("名称回退错误，期望 api.deepseek.com，实际 %q", created.Name)
	}

	list, err := app.ListProviders()
	if err != nil {
		t.Fatalf("读取列表失败: %v", err)
	}
	if len(list) != 1 || list[0].Model != "deepseek-chat" {
		t.Fatalf("列表内容不符合预期: %+v", list)
	}

	updated, err := app.SaveProvider(Provider{
		ID:       created.ID,
		Category: CategoryMultimodal,
		Name:     "DeepSeek",
		BaseURL:  "https://api.deepseek.com/v1",
		APIKey:   "sk-deepseek-2",
		Model:    "deepseek-vl",
	})
	if err != nil {
		t.Fatalf("更新提供商失败: %v", err)
	}
	if updated.Category != CategoryMultimodal || updated.Model != "deepseek-vl" || updated.CreatedAt != created.CreatedAt {
		t.Fatalf("更新结果不符合预期: %+v", updated)
	}

	if err := app.DeleteProvider(created.ID); err != nil {
		t.Fatalf("删除提供商失败: %v", err)
	}
	list, err = app.ListProviders()
	if err != nil {
		t.Fatalf("删除后读取列表失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("删除后仍有记录: %+v", list)
	}

	if _, err := app.SaveProvider(Provider{Category: "unknown", BaseURL: "https://a.com", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("非法分类应返回错误")
	}
	if _, err := app.SaveProvider(Provider{Category: CategoryText, BaseURL: "ftp://a.com", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("非 http(s) 的 Base URL 应返回错误")
	}
}

func TestFetchModelsOpenAICompatible(t *testing.T) {
	var gotAuth string
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"gpt-5"},{"id":"gpt-4o"},{"id":"gpt-5"}]}`)
	}))
	defer server.Close()

	app := newTestApp(t)
	models, err := app.FetchModels(server.URL, "sk-test")
	if err != nil {
		t.Fatalf("获取模型列表失败: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("认证头错误: %q", gotAuth)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("候选端点回退失败，最终请求路径: %q", gotPath)
	}
	if strings.Join(models, ",") != "gpt-4o,gpt-5" {
		t.Fatalf("模型列表不符合预期: %v", models)
	}
}

func TestFetchModelsReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer server.Close()

	app := newTestApp(t)
	if _, err := app.FetchModels(server.URL, "sk-bad"); err == nil {
		t.Fatal("401 时应返回错误")
	} else if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("错误信息缺少状态码或响应片段: %v", err)
	}
}

func TestModelListHeaders(t *testing.T) {
	if got := modelListHeaders("https://api.anthropic.com", "k"); got["x-api-key"] != "k" || got["anthropic-version"] == "" {
		t.Fatalf("Anthropic 认证头错误: %v", got)
	}
	if got := modelListHeaders("https://generativelanguage.googleapis.com", "k"); got["x-goog-api-key"] != "k" {
		t.Fatalf("Gemini 认证头错误: %v", got)
	}
	if got := modelListHeaders("https://api.openai.com/v1", "k"); got["Authorization"] != "Bearer k" {
		t.Fatalf("OpenAI 认证头错误: %v", got)
	}
}

func TestParseModelListShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		expected string
	}{
		{"openai", `{"data":[{"id":"b"},{"id":"a"}]}`, "a,b"},
		{"gemini", `{"models":[{"name":"models/gemini-3-pro"},{"name":"models/gemini-3-flash"}]}`, "gemini-3-flash,gemini-3-pro"},
		{"array", `["wa","wx"]`, "wa,wx"},
	}

	for _, tc := range cases {
		models, err := parseModelList([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s 解析失败: %v", tc.name, err)
		}
		if strings.Join(models, ",") != tc.expected {
			t.Fatalf("%s 解析结果不符合预期: %v", tc.name, models)
		}
	}

	if _, err := parseModelList([]byte(`{"error":"boom"}`)); err == nil {
		t.Fatal("无法识别结构时应返回错误")
	}
}

func TestModelListCandidates(t *testing.T) {
	got := modelListCandidates("https://api.deepseek.com/")
	expected := []string{"https://api.deepseek.com/models", "https://api.deepseek.com/v1/models"}
	if strings.Join(got, "|") != strings.Join(expected, "|") {
		t.Fatalf("候选端点生成错误: %v", got)
	}

	got = modelListCandidates("https://dashscope.aliyuncs.com/compatible-mode/v1")
	if len(got) != 1 || got[0] != "https://dashscope.aliyuncs.com/compatible-mode/v1/models" {
		t.Fatalf("已含版本段时不应再拼 /v1: %v", got)
	}
}

// 用户常把完整接口地址当 Base URL 填入，应先剥掉操作段再拼 /models。
func TestModelListCandidatesTrimEndpointPath(t *testing.T) {
	cases := []struct {
		base     string
		expected string
	}{
		{"https://ark.cn-beijing.volces.com/api/v3/responses", "https://ark.cn-beijing.volces.com/api/v3/models"},
		{"https://ark.cn-beijing.volces.com/api/v3/chat/completions", "https://ark.cn-beijing.volces.com/api/v3/models"},
		{"https://api.openai.com/v1/responses/", "https://api.openai.com/v1/models"},
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1/models"},
		{"https://api.openai.com/v1/models", "https://api.openai.com/v1/models"},
	}

	for _, tc := range cases {
		got := modelListCandidates(tc.base)
		if len(got) == 0 || got[0] != tc.expected {
			t.Fatalf("%s 候选端点错误: %v", tc.base, got)
		}
	}
}

func TestModelListHintMentionsVendor(t *testing.T) {
	if hint := modelListHint("https://ark.cn-beijing.volces.com/api/v3"); !strings.Contains(hint, "火山方舟") {
		t.Fatalf("方舟应提示不提供 /models 接口: %q", hint)
	}
	if hint := modelListHint("https://api.openai.com/v1"); hint == "" {
		t.Fatal("未识别厂商时也应给出通用指引")
	}
}

// 对话接口同样不能因为 Base URL 填成具体接口而拼出 404 路径。
func TestChatCompletionsEndpointNormalizesBase(t *testing.T) {
	cases := map[string]string{
		"https://ark.cn-beijing.volces.com/api/v3":                  "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
		"https://ark.cn-beijing.volces.com/api/v3/responses":        "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
		"https://ark.cn-beijing.volces.com/api/v3/chat/completions": "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
		"https://api.openai.com":                                    "https://api.openai.com/v1/chat/completions",
	}

	for base, expected := range cases {
		if got := chatCompletionsEndpoint(base); got != expected {
			t.Fatalf("%s 拼出的对话接口错误: %q", base, got)
		}
	}
}

func TestSaveProviderNormalizesBaseURL(t *testing.T) {
	app := newTestApp(t)
	saved, err := app.SaveProvider(Provider{
		Category: CategoryText,
		Name:     "方舟",
		BaseURL:  "https://ark.cn-beijing.volces.com/api/v3/responses/",
		APIKey:   "ark-key",
		Model:    "doubao-seed-1-6-250615",
	})
	if err != nil {
		t.Fatalf("保存提供商失败: %v", err)
	}
	if saved.BaseURL != "https://ark.cn-beijing.volces.com/api/v3" {
		t.Fatalf("Base URL 应收敛到 API 根: %q", saved.BaseURL)
	}
}
