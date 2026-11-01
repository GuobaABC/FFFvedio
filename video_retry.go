package main

// 视频生成链路的瞬时故障重试。
//
// 典型现场：成片下载时报「net/http: TLS handshake timeout」，或轮询任务状态时连接被重置。
// 这类抖动跟模型、提示词都无关，而额度在建任务时就已经扣掉了 —— 一次握手超时就让整段白跑，
// 用户得从头再等几分钟。所以下载、轮询、内联图片拉取都按指数退避重试，
// 并把「重试到第几次才成功」回报成任务日志里的一条说明，不让恢复过程静默。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// videoRetryAttempts 接口请求（建任务 / 轮询 / 内联图）：首次 + 3 次重试。
	videoRetryAttempts = 4
	// videoDownloadAttempts 成片下载多给几次：任务已经生成成功、额度已扣，
	// 对象存储（如火方舟的 ark-acg-*.tos-cn-beijing.volces.com）抖一下就没视频太亏。
	videoDownloadAttempts = 6
	// videoRetryMaxDelay 退避上限，避免网络长时间不通时干等。
	videoRetryMaxDelay = 15 * time.Second
)

// 退避基数写成变量，方便单测把等待时间压到毫秒级。
var videoRetryBaseDelay = time.Second

// httpStatusError 带上 HTTP 状态码与请求方法：重试策略要靠它区分「传输抖动」和「服务商明确拒绝」。
// 文案沿用改造前的格式（「%s 返回 %d%s: %s」），别让既有日志检索习惯失效。
type httpStatusError struct {
	Method string
	URL    string
	Code   int
	Status string // resp.Status 原文，如 "503 Service Unavailable"
	Detail string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s 返回 %d%s: %s", e.URL, e.Code, statusReason(e.Status), e.Detail)
}

// videoRetryDelay 第 attempt 次尝试失败后（attempt 从 1 起）该等多久：指数退避 + 抖动。
// 抖动是必要的：多段同时失败时不约好节奏，重试会一起撞回同一个故障窗口。
func videoRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := videoRetryBaseDelay
	for i := 1; i < attempt && delay < videoRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > videoRetryMaxDelay {
		delay = videoRetryMaxDelay
	}
	half := delay / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// isTransientTransportError 传输层错误：请求根本没进到业务处理，重试绝对安全。
func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	// 用户取消与整段渲染预算耗尽，重试没有意义
	if errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, hint := range []string{
		"tls handshake timeout", "tls handshake", "server rejected handshake", "remote error",
		"connection reset", "connection refused", "connection closed", "connection aborted",
		"forcibly closed", "wsarecv", "wsasend", "broken pipe",
		"unexpected eof", "i/o timeout", "timeout awaiting response", "dial tcp", "dns",
		"no route to host", "network is unreachable", "temporary failure in name resolution",
		"use of closed network connection", "context deadline exceeded (client.timeout",
	} {
		if strings.Contains(message, hint) {
			return true
		}
	}
	return false
}

// retryableStatus 状态码是否值得重试。
// 429 对写请求也安全（服务商还没接单）；5xx 只在幂等请求上重试 ——
// 建任务的 POST 撞上 5xx 时服务端可能已经排队成功，盲重试就是重复下单、双倍扣点。
func retryableStatus(method string, code int) bool {
	if code == http.StatusTooManyRequests {
		return true
	}
	if method == http.MethodGet || method == http.MethodHead {
		return code >= http.StatusInternalServerError
	}
	return false
}

// videoShouldRetry 综合判断一次失败要不要再来。
func videoShouldRetry(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if isTransientTransportError(err) {
		return true
	}
	var status *httpStatusError
	if errors.As(err, &status) {
		return retryableStatus(status.Method, status.Code)
	}
	return false
}

// sleepVideoRetry 退避等待；ctx 结束时立刻返回，不再重试。
func sleepVideoRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// videoResilientTransport 拉大文件/远程图片专用的传输。
// DisableKeepAlives：重试时重建全新 TLS 会话，不复用可能已经坏掉的空闲连接；
// TLSHandshakeTimeout：握手卡住时 20s 内就失败进下一轮，而不是靠总超时干等；
// ResponseHeaderTimeout：连接通了但服务端不回头，也尽早转重试。
var videoResilientTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   20 * time.Second,
	ResponseHeaderTimeout: 3 * time.Minute,
	ExpectContinueTimeout: 1 * time.Second,
	DisableKeepAlives:     true,
}

// videoResilientClient 带上面传输的客户端；整体超时由调用方按内容大小给（成片宽松、内联图紧凑）。
func videoResilientClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: videoResilientTransport}
}

// videoRetry 用默认次数执行重试。
func videoRetry(ctx context.Context, do func() error) (int, error) {
	return videoRetryWith(ctx, videoRetryAttempts, do)
}

// videoRetryWith 执行 do，最多 attempts 次，返回实际尝试次数与最后一次的错误。
// 失败时把厂商原话原样交出，只在重试过之后追加一句尝试次数，不能因为重试抹掉可定位的错误细节。
func videoRetryWith(ctx context.Context, attempts int, do func() error) (int, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return attempt - 1, lastErr
			}
			return attempt, err
		}
		err := do()
		if err == nil {
			return attempt, nil
		}
		lastErr = err
		if attempt == attempts || !videoShouldRetry(ctx, err) {
			if attempt > 1 {
				return attempt, fmt.Errorf("%w（已重试 %d 次仍失败）", err, attempt-1)
			}
			return attempt, err
		}
		if waitErr := sleepVideoRetry(ctx, videoRetryDelay(attempt)); waitErr != nil {
			return attempt, lastErr
		}
	}
	return attempts, lastErr
}

// videoRetryNote 重试后才成功时给任务日志留一句：静默恢复只会让用户以为模型慢得离谱。
func videoRetryNote(what string, attempts int) []string {
	if attempts <= 1 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s重试到第 %d 次才成功（TLS 握手超时这类瞬时网络抖动），已自动恢复，无需重新生成", what, attempts)}
}
