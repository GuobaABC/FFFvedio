package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 方舟视频是独立端点，Base URL 的各种写法都要收敛到 /contents/generations/tasks。
func TestArkTaskEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://ark.cn-beijing.volces.com/api/v3":                            "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		"https://ark.cn-beijing.volces.com/api/v3/":                           "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		"https://ark.cn-beijing.volces.com/api/v3/chat/completions":           "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		"https://ark.cn-beijing.volces.com/api/plan/v3":                       "https://ark.cn-beijing.volces.com/api/plan/v3/contents/generations/tasks",
		"https://ark.cn-beijing.volces.com":                                   "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		"https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks": "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
	}

	for base, expected := range cases {
		if got := arkTaskEndpoint(base); got != expected {
			t.Fatalf("%s 拼出的视频端点错误:\n 得到 %q\n 期望 %q", base, got, expected)
		}
	}
}

// 火山方舟的提供商不能再走通用适配器（通用 /video/generations 必然 404）。
func TestInferAdapterPrefersArkForVolcano(t *testing.T) {
	cases := []struct {
		provider Provider
		expected string
	}{
		{Provider{ID: "p1", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Model: "doubao-seedance-2-5-260628"}, "ark"},
		{Provider{ID: "p2", Name: "火山方舟", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Model: "ep-20240620105426-sbtkf"}, "ark"},
		{Provider{ID: "p3", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "wan2.2-i2v-flash"}, "dashscope"},
		{Provider{ID: "p4", BaseURL: "https://api.dev.runwayml.com/v1", Model: "gen4_turbo"}, "runway"},
		{Provider{ID: "p5", BaseURL: "https://gateway.example.com/v1", Model: "my-video"}, "generic"},
	}

	for _, tc := range cases {
		if got := inferAdapterFromProvider(tc.provider); got != tc.expected {
			t.Fatalf("%s 适配器推断错误: 得到 %q 期望 %q", tc.provider.Model, got, tc.expected)
		}
	}
}

// Seedance 1.0 不出音轨，把台词写进提示词会让模型在画面里「写字」。
func TestArkCapabilitiesFollowModelGeneration(t *testing.T) {
	if caps := adapterCapabilities(arkAdapter{}, "doubao-seedance-1-0-pro-250528"); caps.NativeAudio {
		t.Fatal("Seedance 1.0 不应声明原生音频能力")
	}
	caps := adapterCapabilities(arkAdapter{}, "doubao-seedance-2-5-260628")
	if !caps.NativeAudio {
		t.Fatal("Seedance 2.x 应声明原生音频能力")
	}
	if !caps.LastFrame || !caps.CharacterReference {
		t.Fatalf("方舟应支持尾帧与参考图: %+v", caps)
	}
	if caps.MaxClipS != 15 {
		t.Fatalf("单段上限应为 15 秒: %d", caps.MaxClipS)
	}
	// 未实现 modelAwareAdapter 的适配器退回静态能力表
	if got := adapterCapabilities(mockAdapter{}, "anything"); got.MaxClipS == 0 {
		t.Fatal("静态适配器能力读取失败")
	}
}

func TestLooksLikeArkVideoModel(t *testing.T) {
	for _, model := range []string{"doubao-seedance-2-5-260628", "ep-20240620105426-sbtkf", "wan2.2-i2v-flash"} {
		if !looksLikeArkVideoModel(model) {
			t.Fatalf("%s 应被当成视频模型", model)
		}
	}
	for _, model := range []string{"", "doubao-seed-1-6-250615", "qwen-plus"} {
		if looksLikeArkVideoModel(model) {
			t.Fatalf("%q 不是视频模型，应提醒用户", model)
		}
	}
}

func TestGenericVideoCandidatesAvoidDoubleVersion(t *testing.T) {
	got := genericVideoCandidates("https://ark.cn-beijing.volces.com/api/v3")
	if len(got) != 1 || got[0] != "https://ark.cn-beijing.volces.com/api/v3/video/generations" {
		t.Fatalf("已含版本段时不应再拼 /v1: %v", got)
	}
	got = genericVideoCandidates("https://gateway.example.com")
	if len(got) != 2 || got[1] != "https://gateway.example.com/v1/video/generations" {
		t.Fatalf("缺版本段时应补 /v1 候选: %v", got)
	}
}

// 端到端校验方舟的请求体结构、任务轮询与结果下载。
func TestArkAdapterGenerateSubmitsAndPollsTask(t *testing.T) {
	var (
		gotAuth   string
		gotCreate map[string]any
		pollCount int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/contents/generations/tasks":
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotCreate)
			io.WriteString(w, `{"id":"cgt-20260620105426"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/contents/generations/tasks/cgt-20260620105426":
			pollCount++
			fmt.Fprintf(w, `{"id":"cgt-20260620105426","status":"succeeded","content":{"video_url":"http://%s/download.mp4"}}`, r.Host)
		case r.URL.Path == "/download.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("fake-video-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"InvalidEndpoint","message":"路径不存在"}}`)
		}
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "clips", "01-s1.mp4")
	result, err := arkAdapter{}.Generate(context.Background(), videoGenContext{
		Provider: Provider{
			BaseURL: server.URL + "/api/v3/chat/completions", // 用户误填对话接口，也应收敛到视频端点
			APIKey:  "ark-key",
			Model:   "doubao-seedance-2-5-260628",
		},
		Model: "doubao-seedance-2-5-260628",
	}, VideoGenRequest{
		Prompt:      "小猫对着镜头打哈欠",
		FirstFrame:  server.URL + "/first.png",
		DurationS:   7,
		AspectRatio: "9:16",
		Fps:         24,
		Seed:        11,
		OutputPath:  dest,
	})
	if err != nil {
		t.Fatalf("方舟生成流程失败: %v", err)
	}
	if gotAuth != "Bearer ark-key" {
		t.Fatalf("鉴权头错误: %q", gotAuth)
	}
	if pollCount == 0 {
		t.Fatal("未轮询任务状态")
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "fake-video-bytes" {
		t.Fatalf("视频未落地: %v %q", err, data)
	}
	if result.TaskID != "cgt-20260620105426" || result.VideoURI != dest {
		t.Fatalf("结果字段错误: %+v", result)
	}

	// 请求体结构需符合方舟约定：content 数组 + role 标注 + 顶层参数
	if gotCreate["model"] != "doubao-seedance-2-5-260628" {
		t.Fatalf("model 字段错误: %v", gotCreate["model"])
	}
	// 首帧任务画幅被锁定：官方约束 ratio 只能为 adaptive，传固定比例会被忽略
	if gotCreate["ratio"] != "adaptive" || gotCreate["watermark"] != false {
		t.Fatalf("参数错误: ratio=%v watermark=%v", gotCreate["ratio"], gotCreate["watermark"])
	}
	if len(result.Notes) == 0 || !strings.Contains(result.Notes[0], "adaptive") {
		t.Fatalf("画幅被服务商规则改写时应给出说明: %+v", result.Notes)
	}
	if gotCreate["generate_audio"] != true {
		t.Fatalf("Seedance 2.x 应请求音画同步: %v", gotCreate["generate_audio"])
	}
	// 方舟 duration 为整数秒，7 秒不该被档位吸成 6/8
	if gotCreate["duration"] != float64(7) {
		t.Fatalf("时长应保持请求值: %v", gotCreate["duration"])
	}
	content, ok := gotCreate["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content 应为文本 + 首帧两段: %v", gotCreate["content"])
	}
	first, _ := content[0].(map[string]any)
	second, _ := content[1].(map[string]any)
	if first["type"] != "text" || second["role"] != "first_frame" {
		t.Fatalf("content 结构错误: %v / %v", first, second)
	}
	imageURL, _ := second["image_url"].(map[string]any)
	if imageURL["url"] != server.URL+"/first.png" {
		t.Fatalf("首帧 URL 应透传: %v", second["image_url"])
	}
}

// 任务侧报错要把 error.code / error.message 带进日志，而不是只剩「返回 404」。
func TestArkAdapterReportsVendorError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"InvalidParameter.Duration","message":"duration 必须在 [4,15] 秒内"},"request_id":"req-9527"}`)
	}))
	defer server.Close()

	_, err := arkAdapter{}.Generate(context.Background(), videoGenContext{
		Provider: Provider{BaseURL: server.URL + "/api/v3", APIKey: "k", Model: "doubao-seedance-2-5-260628"},
		Model:    "doubao-seedance-2-5-260628",
	}, VideoGenRequest{Prompt: "p", DurationS: 5, AspectRatio: "16:9", OutputPath: filepath.Join(t.TempDir(), "a.mp4")})
	if err == nil {
		t.Fatal("服务商报错时应返回错误")
	}
	for _, want := range []string{"InvalidParameter.Duration", "duration 必须在", "req-9527", "400"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息缺少 %q，实际: %v", want, err)
		}
	}
}

func TestArkAdapterRequiresModelID(t *testing.T) {
	_, err := arkAdapter{}.Generate(context.Background(), videoGenContext{
		Provider: Provider{BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: "k"},
	}, VideoGenRequest{Prompt: "p", DurationS: 5, AspectRatio: "16:9"})
	if err == nil || !strings.Contains(err.Error(), "模型 ID") {
		t.Fatalf("缺少模型 ID 应给出可执行提示: %v", err)
	}
}

// 旧任务把适配器固定成「通用」时，遇到方舟这种专用端点应自动改道并告知。
func TestResolveVideoAdapterUpgradesGenericToArk(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{
		Category: CategoryMultimodalGen,
		Name:     "火山方舟",
		BaseURL:  "https://ark.cn-beijing.volces.com/api/v3",
		APIKey:   "ark-key",
		Model:    "doubao-seedance-2-5-260628",
	}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}

	id, adapter, provider, warnings, err := app.resolveVideoAdapter(VideoRunConfig{Adapter: "generic"})
	if err != nil {
		t.Fatalf("解析适配器失败: %v", err)
	}
	if id != "ark" || adapter == nil || provider.Model == "" {
		t.Fatalf("应改道到方舟适配器: id=%q adapter=%v provider=%+v", id, adapter, provider)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "自动") {
		t.Fatalf("改道应留下警告: %v", warnings)
	}

	// 填的确是 Seedance 模型时不应误报，否则警告会被用户当成噪声忽略
	_, _, _, warnings, err = app.resolveVideoAdapter(VideoRunConfig{Adapter: "ark", ProviderID: provider.ID})
	if err != nil {
		t.Fatalf("解析适配器失败: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("Seedance 模型不该被警告: %v", warnings)
	}
}

func TestApiErrorDetailCoversCommonShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		contains string
	}{
		{"方舟嵌套", `{"error":{"code":"NotFound","message":"task not found"}}`, "NotFound"},
		{"OpenAI 兼容", `{"error":{"type":"invalid_request_error","message":"模型不存在"}}`, "模型不存在"},
		{"error 为字符串", `{"error":"quota exceeded"}`, "quota exceeded"},
		{"顶层平铺", `{"code":"Throttling","message":"请求过于频繁"}`, "Throttling"},
		{"非 JSON", `<html>502 Bad Gateway</html>`, "502 Bad Gateway"},
		{"空响应", ``, "(服务商返回空响应体)"},
	}

	for _, tc := range cases {
		got := apiErrorDetail([]byte(tc.body))
		if !strings.Contains(got, tc.contains) {
			t.Fatalf("%s 报错提取失败: 得到 %q", tc.name, got)
		}
	}
}

// 长响应体按 rune 截断，中文不能被切成乱码。
func TestBodySnippetKeepsChineseReadable(t *testing.T) {
	long := strings.Repeat("错误", 1000)
	got := bodySnippet([]byte(long))
	if !strings.HasSuffix(got, "字符)") {
		t.Fatalf("超长响应应标注总长度: %q", got[len(got)-20:])
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("按字节截断产生了乱码")
	}
}

// 只有首帧/尾帧任务会被锁到 adaptive，文生视频与参考生视频仍按用户选的画幅提交。
func TestArkRatioOnlyDowngradesForFrameTasks(t *testing.T) {
	if ratio, downgraded := arkRatio("9:16", true); ratio != "adaptive" || !downgraded {
		t.Fatalf("首帧任务应改为 adaptive 并报告改写: %s / %v", ratio, downgraded)
	}
	if ratio, downgraded := arkRatio("9:16", false); ratio != "9:16" || downgraded {
		t.Fatalf("无首帧时应保留请求画幅: %s / %v", ratio, downgraded)
	}
	if ratio, downgraded := arkRatio("adaptive", false); ratio != "adaptive" || downgraded {
		t.Fatalf("adaptive 应原样透传: %s / %v", ratio, downgraded)
	}
	if ratio, _ := arkRatio("", false); ratio != "adaptive" {
		t.Fatalf("没填画幅时交给模型自选: %s", ratio)
	}
}
