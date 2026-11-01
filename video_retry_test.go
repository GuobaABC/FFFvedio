package main

// 视频链路瞬时故障重试的回归测试：TLS 握手超时这类抖动必须自动恢复并留痕，
// 而建任务的 5xx 绝不能重试（服务端可能已排队成功，重复下单就是双倍扣点）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkVideoRetry 把退避压到毫秒级，别让单测真的等上几秒。
func shrinkVideoRetry(t *testing.T) {
	t.Helper()
	old := videoRetryBaseDelay
	videoRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { videoRetryBaseDelay = old })
}

func TestVideoShouldRetryClassifiesErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"方舟成片下载的 TLS 握手超时", errors.New(`Get "https://ark CDN/video.mp4": net/http: TLS handshake timeout`), true},
		// 用户现场撞到的原文：对象存储拉 mp4 时握手超时，必须归入可重试
		{"TOS 拉 mp4 握手超时（现场报错）", errors.New(`Get "https://ark-acg-cn-beijing.tos-cn-beijing.volces.com/doubao-seedance-2-0-mini/20260620105426.mp4": net/http: TLS handshake timeout`), true},
		{"连接被拒", errors.New(`Get "https://ark-acg-cn-beijing.tos-cn-beijing.volces.com/a.mp4": dial tcp: connect: connection refused`), true},
		{"空闲连接被提前关掉（裸 EOF）", &url.Error{Op: "Get", URL: "https://tos.example.com/a.mp4", Err: io.EOF}, true},
		{"轮询时连接被重置", errors.New("GET https://ark/tasks/x 请求失败: read tcp 127.0.0.1:1: connection reset by peer"), true},
		{"下载中途断流", fmt.Errorf("写入视频文件失败: %w", io.ErrUnexpectedEOF), true},
		{"客户端超时", errors.New("Get \"https://ark/tasks/x\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), true},
		{"GET 撞上 503", &httpStatusError{Method: http.MethodGet, Code: 503}, true},
		{"429 限流", &httpStatusError{Method: http.MethodPost, Code: http.StatusTooManyRequests}, true},
		{"POST 撞上 500 不能重试", &httpStatusError{Method: http.MethodPost, Code: 500}, false},
		{"参数错误不该重试", &httpStatusError{Method: http.MethodPost, Code: 400, Detail: "duration 必须在 4~15"}, false},
		{"签名 URL 过期不该重试", &httpStatusError{Method: http.MethodGet, Code: http.StatusForbidden}, false},
		{"用户取消", fmt.Errorf("下载生成结果失败: %w", context.Canceled), false},
		{"解析响应失败不是网络问题", errors.New("解析任务状态失败: invalid character 'x'"), false},
	}
	for _, item := range cases {
		if got := videoShouldRetry(ctx, item.err); got != item.want {
			t.Errorf("%s：重试判定应为 %v，实际 %v（%v）", item.name, item.want, got, item.err)
		}
	}

	// 整段渲染预算已耗尽时，再抖动的错误也不该继续重试
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if videoShouldRetry(canceled, &httpStatusError{Method: http.MethodGet, Code: 503}) {
		t.Fatal("ctx 已结束就不该再重试")
	}
}

func TestVideoRetryDelayBounds(t *testing.T) {
	old := videoRetryBaseDelay
	videoRetryBaseDelay = time.Second
	t.Cleanup(func() { videoRetryBaseDelay = old })

	// 退避窗口：第 n 次落在 [base*2^(n-1)/2, base*2^(n-1)]，且不超过上限
	windows := []struct {
		attempt     int
		min, maxDur time.Duration
	}{
		{1, 500 * time.Millisecond, time.Second},
		{2, time.Second, 2 * time.Second},
		{3, 2 * time.Second, 4 * time.Second},
		{9, videoRetryMaxDelay / 2, videoRetryMaxDelay},
	}
	for _, window := range windows {
		delay := videoRetryDelay(window.attempt)
		if delay < window.min || delay > window.maxDur {
			t.Fatalf("第 %d 次退避 %v 不在 [%v, %v] 内", window.attempt, delay, window.min, window.maxDur)
		}
	}
}

func TestVideoRetryStopsWhenContextDone(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	attempts, err := videoRetry(canceled, func() error {
		calls++
		return errors.New("不该被执行")
	})
	if calls != 0 {
		t.Fatalf("ctx 已结束时不应发起请求，实际 %d 次", calls)
	}
	if attempts != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("应直接交回 ctx 错误: attempts=%d err=%v", attempts, err)
	}
}

// 下载遇到 5xx / TLS 抖动要重试到成功，并把「重试到第几次」回报成用户可见的说明。
func TestDownloadVideoFileRetriesTransientFailure(t *testing.T) {
	shrinkVideoRetry(t)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := atomic.AddInt32(&hits, 1)
		if round <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"cdn busy"}}`)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		io.WriteString(w, "real-video")
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "clips", "01-s1.mp4")
	attempts, err := downloadVideoFile(context.Background(), server.URL+"/video.mp4", dest, nil)
	if err != nil {
		t.Fatalf("瞬时故障应自动恢复: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("应在第 3 次成功: %d", attempts)
	}
	if data, readErr := os.ReadFile(dest); readErr != nil || string(data) != "real-video" {
		t.Fatalf("成片内容错误: %v %q", readErr, data)
	}
	notes := videoRetryNote("成片下载", attempts)
	if len(notes) != 1 || !strings.Contains(notes[0], "第 3 次") || !strings.Contains(notes[0], "无需重新生成") {
		t.Fatalf("重试成功要留痕: %v", notes)
	}
	if videoRetryNote("成片下载", 1) != nil {
		t.Fatal("首次成功不该产生说明")
	}
}

// 传输层直接断掉（握手抖动、连接被重置这类，不带 HTTP 状态码）也必须重试到拿到完整成片。
func TestDownloadVideoFileRetriesTransportFailure(t *testing.T) {
	shrinkVideoRetry(t)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := atomic.AddInt32(&hits, 1)
		if round <= 2 {
			// 不写任何响应头直接掉下线：客户端只能报 EOF，没有状态码可参考
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatalf("无法断开连接: %v", err)
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		io.WriteString(w, "tos-video")
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "clips", "02-s2.mp4")
	attempts, err := downloadVideoFile(context.Background(), server.URL+"/doubao-seedance-2-0-mini/x.mp4", dest, nil)
	if err != nil {
		t.Fatalf("传输层抖动应自动恢复: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("应在第 3 次成功: %d", attempts)
	}
	if data, readErr := os.ReadFile(dest); readErr != nil || string(data) != "tos-video" {
		t.Fatalf("重试后成片内容错误: %v %q", readErr, data)
	}
}

// 重试判定只看「错在哪」，不看「下载的是哪条 URL」：
// 带签名查询参数、目录名不同、甚至没有 .mp4 后缀都要能同样恢复。
func TestDownloadVideoFileRetryIgnoresURLShape(t *testing.T) {
	shrinkVideoRetry(t)

	urls := []string{
		"/doubao-seedance-2-0-mini/20260620105426.mp4?X-Tos-Algorithm=TOS4-HMAC-SHA256&X-Tos-Expires=86400&X-Tos-Signature=deadbeef",
		"/ark-acg-cn-beijing/doubao-seedance-2-5-260628/other-task.mp4",
		"/storage/video/3f2a1b", // 完全没后缀的签名直链
	}
	for index, path := range urls {
		var hits int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&hits, 1) == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Fatalf("无法断开连接: %v", err)
				}
				conn.Close()
				return
			}
			io.WriteString(w, "video-body")
		}))
		dest := filepath.Join(t.TempDir(), fmt.Sprintf("%02d.mp4", index+1))
		attempts, err := downloadVideoFile(context.Background(), server.URL+path, dest, nil)
		server.Close()
		if err != nil {
			t.Fatalf("%s 应重试恢复: %v", path, err)
		}
		if attempts != 2 {
			t.Fatalf("%s 应在第 2 次成功: %d", path, attempts)
		}
		if data, readErr := os.ReadFile(dest); readErr != nil || string(data) != "video-body" {
			t.Fatalf("%s 重试后内容错误: %v %q", path, readErr, data)
		}
	}
}

// 4xx 是服务商明确拒绝，重试只会拖慢反馈；错误文本要保住厂商原话并补上重试次数。
func TestDownloadVideoFileDoesNotRetryClientError(t *testing.T) {
	shrinkVideoRetry(t)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"signature expired"}}`)
	}))
	defer server.Close()

	attempts, err := downloadVideoFile(context.Background(), server.URL+"/video.mp4", filepath.Join(t.TempDir(), "a.mp4"), nil)
	if err == nil {
		t.Fatal("403 应报错")
	}
	if attempts != 1 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("4xx 不该重试: attempts=%d hits=%d", attempts, hits)
	}
	if !strings.Contains(err.Error(), "signature expired") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("错误文本丢失厂商原话: %v", err)
	}
}

// 重试满仍失败：告知已重试次数，且不能留下半截文件骗过后续校验。
func TestDownloadVideoFileGivesUpWithClearError(t *testing.T) {
	shrinkVideoRetry(t)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "cdn down")
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "clips", "b.mp4")
	attempts, err := downloadVideoFile(context.Background(), server.URL+"/video.mp4", dest, nil)
	if err == nil {
		t.Fatal("持续 5xx 应最终失败")
	}
	if attempts != videoDownloadAttempts || atomic.LoadInt32(&hits) != int32(videoDownloadAttempts) {
		t.Fatalf("应尝试 %d 次: attempts=%d hits=%d", videoDownloadAttempts, attempts, hits)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("已重试 %d 次", videoDownloadAttempts-1)) {
		t.Fatalf("错误应说明重试次数: %v", err)
	}
}

// 响应体被截断（连接提前关闭）时，半截 mp4 必须清掉，重试从干净文件开始。
func TestDownloadVideoFileCleansTruncatedFile(t *testing.T) {
	shrinkVideoRetry(t)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "only-a-part")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatalf("无法断开连接: %v", err)
		}
		conn.Close()
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "clips", "c.mp4")
	if _, err := downloadVideoFile(context.Background(), server.URL+"/video.mp4", dest, nil); err == nil {
		t.Fatal("截断下载应报错")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("半截文件不该留在磁盘上: %v", statErr)
	}
	if atomic.LoadInt32(&hits) != int32(videoDownloadAttempts) {
		t.Fatalf("截断属瞬时抖动，应重试满 %d 次: %d", videoDownloadAttempts, hits)
	}
}

// 建任务 POST 撞 5xx 不重试（可能已扣点），轮询 GET 撞 5xx 必须重试。
func TestVideoRequestRetriesGetButNotPost(t *testing.T) {
	shrinkVideoRetry(t)

	var postHits, getHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			atomic.AddInt32(&postHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"gateway error"}}`)
		default:
			if atomic.AddInt32(&getHits, 1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				io.WriteString(w, "bad gateway")
				return
			}
			io.WriteString(w, `{"status":"succeeded"}`)
		}
	}))
	defer server.Close()

	if _, err := videoRequest(context.Background(), videoHTTPClient(), http.MethodPost, server.URL, nil, map[string]any{"a": 1}); err == nil {
		t.Fatal("POST 500 应报错")
	} else if !strings.Contains(err.Error(), "gateway error") {
		t.Fatalf("POST 错误应保留厂商原话: %v", err)
	}
	if got := atomic.LoadInt32(&postHits); got != 1 {
		t.Fatalf("POST 撞 5xx 不能重试，否则重复下单: %d 次", got)
	}

	body, err := videoRequest(context.Background(), videoHTTPClient(), http.MethodGet, server.URL, nil, nil)
	if err != nil {
		t.Fatalf("轮询 GET 的瞬时 5xx 应重试成功: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil || parsed["status"] != "succeeded" {
		t.Fatalf("重试后应拿到正常响应: %v %s", err, body)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET 应重试一次: %d 次", got)
	}
}

// 端到端：方舟任务已成功、只有成片下载抖动时，重试次数要作为 Note 冒泡到任务日志。
func TestArkAdapterReportsDownloadRetryNote(t *testing.T) {
	shrinkVideoRetry(t)

	var downloadHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/contents/generations/tasks":
			io.WriteString(w, `{"id":"cgt-retry-1"}`)
		case strings.Contains(r.URL.Path, "/contents/generations/tasks/"):
			fmt.Fprintf(w, `{"id":"cgt-retry-1","status":"succeeded","content":{"video_url":"http://%s/v.mp4"}}`, r.Host)
		case r.URL.Path == "/v.mp4":
			if atomic.AddInt32(&downloadHits, 1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, "busy")
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "bytes")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := arkAdapter{}.Generate(context.Background(), videoGenContext{
		Provider: Provider{BaseURL: server.URL + "/api/v3", APIKey: "k"},
		Model:    "doubao-seedance-2-5-260628",
	}, VideoGenRequest{Prompt: "小猫打哈欠", DurationS: 5, AspectRatio: "16:9", OutputPath: filepath.Join(t.TempDir(), "01.mp4")})
	if err != nil {
		t.Fatalf("下载抖动应自动恢复: %v", err)
	}
	if atomic.LoadInt32(&downloadHits) != 2 {
		t.Fatalf("应重试一次下载: %d", downloadHits)
	}
	found := false
	for _, note := range result.Notes {
		if strings.Contains(note, "成片下载重试到第 2 次") {
			found = true
		}
	}
	if !found {
		t.Fatalf("重试要写进 Notes 让任务日志可见: %+v", result.Notes)
	}
}

/* ---------------------- 直链本身失效：换新签名重下 ---------------------- */

func TestIsStaleSignedURLError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"签名过期", &httpStatusError{Code: http.StatusForbidden, Detail: "signature expired"}, true},
		{"鉴权失败", &httpStatusError{Code: http.StatusUnauthorized}, true},
		{"对象还不可见", &httpStatusError{Code: http.StatusNotFound, Detail: "NoSuchKey"}, true},
		{"参数非法是我们自己的错，换链接也没用", &httpStatusError{Code: http.StatusBadRequest}, false},
		{"服务端 5xx 属于瞬时抖动，走下载重试而不是回查", &httpStatusError{Code: http.StatusServiceUnavailable}, false},
	}
	for _, tc := range cases {
		if got := isStaleSignedURLError(tc.err); got != tc.want {
			t.Fatalf("%s: 判定为 %v，期望 %v", tc.name, got, tc.want)
		}
	}
	// 没有状态码的传输层错误不属于直链失效（那是 downloadVideoFile 自己重试的范畴）
	if isStaleSignedURLError(errors.New(`Get "https://tos.example.com/v.mp4": net/http: TLS handshake timeout`)) {
		t.Fatal("TLS 握手超时不是直链失效")
	}
}

// 成片直链是带签名的临时地址：403 签名过期时重发同一条 URL 永远没用，
// 必须回查任务（GET、幂等、不二次计费）拿一条新鲜链接重下。
func TestDownloadGeneratedVideoRefreshesSignedURL(t *testing.T) {
	shrinkVideoRetry(t)

	var downloadHits, refreshHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloadHits, 1)
		if r.URL.Query().Get("sig") == "expired" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":"SignatureExpired","message":"signature expired"}`)
			return
		}
		io.WriteString(w, "fresh-body")
	}))
	defer server.Close()

	refresh := func(context.Context) (string, error) {
		atomic.AddInt32(&refreshHits, 1)
		return server.URL + "/v.mp4?sig=fresh", nil
	}
	dest := filepath.Join(t.TempDir(), "clips", "01.mp4")
	attempts, err := downloadGeneratedVideo(context.Background(), server.URL+"/v.mp4?sig=expired", dest, nil, refresh)
	if err != nil {
		t.Fatalf("签名过期应换新链接恢复: %v", err)
	}
	if attempts != 2 || atomic.LoadInt32(&refreshHits) != 1 || atomic.LoadInt32(&downloadHits) != 2 {
		t.Fatalf("应旧链 1 次 + 新链 1 次，回查 1 次: attempts=%d refresh=%d download=%d", attempts, refreshHits, downloadHits)
	}
	if data, readErr := os.ReadFile(dest); readErr != nil || string(data) != "fresh-body" {
		t.Fatalf("换新链接后内容错误: %v %q", readErr, data)
	}
}

// 回查拿不到新链接时，必须把原始下载错误原样交出，不能用排障信息更少的错替掉它。
func TestDownloadGeneratedVideoKeepsOriginalErrorWhenRefreshFails(t *testing.T) {
	shrinkVideoRetry(t)

	var downloadHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloadHits, 1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"signature expired"}`)
	}))
	defer server.Close()

	attempts, err := downloadGeneratedVideo(context.Background(), server.URL+"/v.mp4", filepath.Join(t.TempDir(), "a.mp4"), nil,
		func(context.Context) (string, error) { return "", errors.New("任务已被清理") })
	if err == nil {
		t.Fatal("拿不到新链接应失败")
	}
	if attempts != 1 || atomic.LoadInt32(&downloadHits) != 1 {
		t.Fatalf("不应无限循环: attempts=%d hits=%d", attempts, downloadHits)
	}
	if !strings.Contains(err.Error(), "signature expired") {
		t.Fatalf("要保留下载侧的厂商原话: %v", err)
	}
}

// 5xx 重试耗尽不是直链失效，不能误走回查分支（白耗一次请求）。
func TestDownloadGeneratedVideoDoesNotRefreshOnServerError(t *testing.T) {
	shrinkVideoRetry(t)

	var refreshHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "busy")
	}))
	defer server.Close()

	_, err := downloadGeneratedVideo(context.Background(), server.URL+"/v.mp4", filepath.Join(t.TempDir(), "a.mp4"), nil,
		func(context.Context) (string, error) {
			atomic.AddInt32(&refreshHits, 1)
			return server.URL + "/v2.mp4", nil
		})
	if err == nil {
		t.Fatal("持续 5xx 应失败")
	}
	if atomic.LoadInt32(&refreshHits) != 0 {
		t.Fatalf("5xx 不该回查直链: %d 次", refreshHits)
	}
}

// 端到端：方舟任务已成功但直链签名过期，回查轮询接口拿新鲜链接后重试下载要能跑通。
func TestArkAdapterRefreshesExpiredVideoURL(t *testing.T) {
	shrinkVideoRetry(t)

	var pollHits, downloadHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/contents/generations/tasks":
			io.WriteString(w, `{"id":"cgt-expired-1"}`)
		case strings.Contains(r.URL.Path, "/contents/generations/tasks/"):
			// 首次轮询给的链接已过期，回查给新鲜链接
			if atomic.AddInt32(&pollHits, 1) == 1 {
				fmt.Fprintf(w, `{"id":"cgt-expired-1","status":"succeeded","content":{"video_url":"http://%s/v.mp4?sig=expired"}}`, r.Host)
				return
			}
			fmt.Fprintf(w, `{"id":"cgt-expired-1","status":"succeeded","content":{"video_url":"http://%s/v.mp4?sig=fresh"}}`, r.Host)
		case r.URL.Path == "/v.mp4":
			atomic.AddInt32(&downloadHits, 1)
			if r.URL.Query().Get("sig") == "expired" {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"code":"SignatureExpired"}`)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "seedance-video")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "01.mp4")
	result, err := arkAdapter{}.Generate(context.Background(), videoGenContext{
		Provider: Provider{BaseURL: server.URL + "/api/v3", APIKey: "k"},
		Model:    "doubao-seedance-2-0-mini",
	}, VideoGenRequest{Prompt: "小猫打哈欠", DurationS: 5, AspectRatio: "16:9", OutputPath: dest})
	if err != nil {
		t.Fatalf("签名过期应回查新链接重下: %v", err)
	}
	if pollHits < 2 || downloadHits != 2 {
		t.Fatalf("应轮询 %d 次（含回查）且下载 2 次", pollHits)
	}
	if data, readErr := os.ReadFile(result.VideoURI); readErr != nil || string(data) != "seedance-video" {
		t.Fatalf("成片内容错误: %v %q", readErr, data)
	}
}
