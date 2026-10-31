package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 三档 UA 池（Chrome/Win、Chrome/Mac、Safari/iPhone），按 host 亲和性选择（§9）
var userAgentPool = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
}

// 移动页结构通常比 PC 页更稳定，这类站点固定用手机 UA（§5）
var mobileFirstHosts = []string{"douyin.com", "iesdouyin.com", "xiaohongshu.com", "kuaishou.com", "tiktok.com"}

// pickUserAgent 同一 host 永远拿同一个 UA，避免会话特征漂移。
func pickUserAgent(host string) string {
	host = strings.ToLower(host)
	for _, suffix := range mobileFirstHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return userAgentPool[2]
		}
	}
	sum := 0
	for _, char := range host {
		sum += int(char)
	}
	return userAgentPool[sum%2]
}

func acceptLanguageFor(agent string) string {
	if strings.Contains(agent, "iPhone") {
		return "zh-CN,zh-Hans;q=0.9"
	}
	return "zh-CN,zh;q=0.9,en;q=0.8"
}

// crawlError 携带失败分类，便于按类别退避（§8）。
type crawlError struct {
	Code   string
	Status int
	Err    error
}

func (e *crawlError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *crawlError) Unwrap() error { return e.Err }

func newCrawlError(code string, status int, format string, args ...any) *crawlError {
	return &crawlError{Code: code, Status: status, Err: fmt.Errorf(format, args...)}
}

// classifyHTTPStatus 把 HTTP 状态码映射到失败分类。
func classifyHTTPStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return CrawlErrLogin
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return CrawlErrBotBlocked
	case status == http.StatusNotFound || status == http.StatusGone:
		return CrawlErrDeadLink
	case status >= 500:
		return CrawlErrNetwork
	}
	return ""
}

// crawlPage 一次抓取的产物：最终 URL、状态码、原始 HTML。
type crawlPage struct {
	URL         string
	Status      int
	ContentType string
	Body        []byte
	Rendered    bool
	ElapsedMS   int64
}

// crawlFetcher 负责静态抓取：同 host 令牌桶限速 + robots.txt + 浏览器 UA。
// 无头浏览器兜底不在本实现内（需要 Playwright 运行时），因此 Rendered 恒为 false。
type crawlFetcher struct {
	client        *http.Client
	interval      time.Duration
	respectRobots bool

	mu        sync.Mutex
	lastHost  map[string]time.Time
	robots    map[string]*robotsRules
	robotsErr map[string]error
}

type robotsRules struct {
	disallow []string
	allow    []string
	fetched  time.Time
}

func newCrawlFetcher(cfg CrawlRunConfig) *crawlFetcher {
	timeout := time.Duration(cfg.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = defaultCrawlTimeoutS * time.Second
	}
	return &crawlFetcher{
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 6 {
					return errors.New("重定向次数过多")
				}
				return nil
			},
		},
		interval:      time.Duration(cfg.IntervalMS) * time.Millisecond,
		respectRobots: cfg.RespectRobots,
		lastHost:      make(map[string]time.Time),
		robots:        make(map[string]*robotsRules),
		robotsErr:     make(map[string]error),
	}
}

// waitHost 同 host 串行 + 间隔限速，防止瞬时打爆（§9）。
func (f *crawlFetcher) waitHost(ctx context.Context, host string) error {
	f.mu.Lock()
	last := f.lastHost[host]
	wait := f.interval - time.Since(last)
	if wait < 0 {
		wait = 0
	}
	f.lastHost[host] = time.Now().Add(wait)
	f.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// allowedByRobots robots.txt 优先；抓取失败时放行（fail-open），并把原因写进审计。
func (f *crawlFetcher) allowedByRobots(ctx context.Context, rawURL string) (bool, string) {
	if !f.respectRobots {
		return true, ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true, ""
	}
	host := strings.ToLower(parsed.Host)
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}

	f.mu.Lock()
	rules, cached := f.robots[host]
	fetchErr := f.robotsErr[host]
	f.mu.Unlock()

	if !cached {
		rules, fetchErr = f.loadRobots(ctx, parsed.Scheme+"://"+host)
		f.mu.Lock()
		f.robots[host] = rules
		f.robotsErr[host] = fetchErr
		f.mu.Unlock()
	}
	if fetchErr != nil || rules == nil {
		return true, ""
	}
	for _, allow := range rules.allow {
		if robotsMatch(allow, path) {
			return true, ""
		}
	}
	for _, disallow := range rules.disallow {
		if robotsMatch(disallow, path) {
			return false, "robots.txt 禁止抓取 " + path
		}
	}
	return true, ""
}

func (f *crawlFetcher) loadRobots(ctx context.Context, origin string) (*robotsRules, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pickUserAgent(hostOf(origin)))
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 没有 robots.txt 视为允许
		return &robotsRules{fetched: time.Now()}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, err
	}
	return parseRobots(string(body)), nil
}

// parseRobots 只关心 `User-agent: *` 分组，够覆盖常见站点。
func parseRobots(text string) *robotsRules {
	rules := &robotsRules{fetched: time.Now()}
	active := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		value := strings.TrimSpace(parts[1])
		switch key {
		case "user-agent":
			active = value == "*"
		case "disallow":
			if active && value != "" {
				rules.disallow = append(rules.disallow, value)
			}
		case "allow":
			if active && value != "" {
				rules.allow = append(rules.allow, value)
			}
		}
	}
	return rules
}

// robotsMatch 支持 * 通配与 $ 结尾；不追求完整 RFC 实现。
func robotsMatch(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	quoted := regexp.QuoteMeta(pattern)
	quoted = strings.ReplaceAll(quoted, `\*`, `.*`)
	if strings.HasSuffix(quoted, `\$`) {
		quoted = strings.TrimSuffix(quoted, `\$`) + "$"
	}
	matched, err := regexp.MatchString("^"+quoted, path)
	return err == nil && matched
}

// fetchPage 抓取静态 HTML；失败时返回带分类的 crawlError。
func (f *crawlFetcher) fetchPage(ctx context.Context, rawURL string) (crawlPage, error) {
	host := hostOf(rawURL)
	if allowed, reason := f.allowedByRobots(ctx, rawURL); !allowed {
		return crawlPage{}, newCrawlError(CrawlErrRobots, 0, "%s", reason)
	}
	if err := f.waitHost(ctx, host); err != nil {
		return crawlPage{}, newCrawlError(CrawlErrNetwork, 0, "等待限速失败: %v", err)
	}

	agent := pickUserAgent(host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return crawlPage{}, newCrawlError(CrawlErrUnsupported, 0, "构造请求失败: %v", err)
	}
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", acceptLanguageFor(agent))
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	started := time.Now()
	resp, err := f.client.Do(req)
	if err != nil {
		return crawlPage{}, newCrawlError(CrawlErrNetwork, 0, "请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	elapsed := time.Since(started).Milliseconds()
	if readErr != nil {
		return crawlPage{}, newCrawlError(CrawlErrNetwork, resp.StatusCode, "读取响应失败: %v", readErr)
	}
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if decoded, err := gunzip(body); err == nil {
			body = decoded
		}
	}

	if code := classifyHTTPStatus(resp.StatusCode); code != "" {
		return crawlPage{URL: resp.Request.URL.String(), Status: resp.StatusCode, ElapsedMS: elapsed},
			newCrawlError(code, resp.StatusCode, "HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return crawlPage{URL: resp.Request.URL.String(), Status: resp.StatusCode, ElapsedMS: elapsed},
			newCrawlError(CrawlErrNetwork, resp.StatusCode, "HTTP %d", resp.StatusCode)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "html") && !strings.Contains(contentType, "xml") &&
		!strings.Contains(contentType, "text/plain") && !strings.Contains(contentType, "json") {
		return crawlPage{URL: resp.Request.URL.String(), Status: resp.StatusCode, ContentType: contentType, ElapsedMS: elapsed},
			newCrawlError(CrawlErrParse, resp.StatusCode, "不是网页内容: %s", contentType)
	}

	text := string(body)
	if blockedReason := detectBlockPage(text); blockedReason != "" {
		return crawlPage{URL: resp.Request.URL.String(), Status: resp.StatusCode, ContentType: contentType, Body: body, ElapsedMS: elapsed},
			newCrawlError(CrawlErrBotBlocked, resp.StatusCode, "%s", blockedReason)
	}

	return crawlPage{
		URL:         resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: contentType,
		Body:        body,
		ElapsedMS:   elapsed,
	}, nil
}

func gunzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// detectBlockPage 状态码 200 也可能是风控页，命中关键词直接判 bot-blocked。
func detectBlockPage(html string) string {
	if len(html) == 0 {
		return ""
	}
	lower := strings.ToLower(html)
	needles := map[string]string{
		"请完成验证":                "命中站点风控页：需要完成人机验证",
		"访问过于频繁":               "命中站点风控页：访问频率受限",
		"captcha":              "命中验证码页",
		"verify you are human": "命中人机验证页",
		"登录后查看":                "需要登录后才能查看",
	}
	for needle, reason := range needles {
		if strings.Contains(lower, needle) {
			return reason
		}
	}
	return ""
}

// looksLikeSPAShell 判断静态 HTML 是否只是空壳，需要 JS 渲染（§5 第 3 条）。
func looksLikeSPAShell(html string) bool {
	if html == "" {
		return false
	}
	lower := strings.ToLower(html)
	shell := strings.Contains(lower, `<div id="app"`) || strings.Contains(lower, `<div id="root"`) ||
		strings.Contains(lower, `<app-root`)
	if !shell {
		return false
	}
	return len(stripHTMLTags(html)) < 400
}

// stripHTMLTags 粗略去标签，用于统计可见文本长度与喂给 LLM。
func stripHTMLTags(html string) string {
	var builder strings.Builder
	inTag := false
	for _, char := range html {
		switch char {
		case '<':
			inTag = true
		case '>':
			inTag = false
			builder.WriteRune(' ')
		default:
			if !inTag {
				builder.WriteRune(char)
			}
		}
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}
