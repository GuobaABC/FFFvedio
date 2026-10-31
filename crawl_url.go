package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// urlPattern 从分享文案里抠出 URL（允许中文标点作为分隔符）
var urlPattern = regexp.MustCompile(`(?:https?://|//)[^\s"'<>()\[\]{}，。；、！？【】《》‘’“”]+`)

// trackingParams 易变的追踪参数，同一支视频带与不带必须视为同一实体（§4.1）
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true, "utm_term": true, "utm_content": true,
	"spm": true, "spm_id_from": true, "from": true, "from_source": true, "from_spmid": true,
	"refer": true, "referer": true, "referrer": true, "vd_source": true, "is_from_tq": true,
	"tt_from": true, "timestamp": true, "signature": true, "_r": true, "previous_page": true,
	"app": true, "scene": true, "clicktime": true, "enterid": true, "sessionid": true,
	"wfr": true, "share_source": true, "share_medium": true, "share_plat": true, "share_tag": true,
	"unique_k": true, "si": true, "feature": true, "pp": true, "t": true,
}

// shortLinkHosts 短链需要追 302 才能拿到真正的详情页（§4.1 第 5 步）
var shortLinkHosts = map[string]bool{
	"t.cn": true, "url.cn": true, "v.douyin.com": true, "b23.tv": true,
	"youtu.be": true, "yt.be": true, "bit.ly": true, "tinyurl.com": true, "xhslink.com": true,
}

// siteKeyBySuffix 已知站点：加站点只需加一行（与抽取适配器解耦）
var siteKeyBySuffix = []struct {
	suffix string
	key    string
}{
	{"bilibili.com", "bilibili"},
	{"b23.tv", "bilibili"},
	{"youtube.com", "youtube"},
	{"youtu.be", "youtube"},
	{"douyin.com", "douyin"},
	{"iesdouyin.com", "douyin"},
	{"xiaohongshu.com", "xiaohongshu"},
	{"xhslink.com", "xiaohongshu"},
	{"channels.weixin.qq.com", "channels"},
	{"mp.weixin.qq.com", "channels"},
	{"vimeo.com", "vimeo"},
	{"kuaishou.com", "kuaishou"},
	{"tiktok.com", "tiktok"},
	{"weibo.com", "weibo"},
	{"ixigua.com", "ixigua"},
	{"haokan.baidu.com", "haokan"},
}

// extractURLs 从任意文本里抽取 URL，并做去重前的轻清洗。
func extractURLs(input string) []string {
	matches := urlPattern.FindAllString(input, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		cleaned := strings.TrimSpace(match)
		// 去掉句末标点与括号，避免把“链接）”带进去
		cleaned = strings.TrimRight(cleaned, ".,;:!?)]}'\"”’。，；：！？")
		if cleaned == "" {
			continue
		}
		out = append(out, cleaned)
	}
	return out
}

// canonicalizeURL 归一化：小写 host、去默认端口、剥离追踪参数、去 fragment、补 scheme（§4.1）。
func canonicalizeURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("链接为空")
	}
	if strings.HasPrefix(value, "//") {
		value = "https:" + value
	}
	if !strings.Contains(value, "://") {
		// 用户常直接粘 www.xxx.com/watch?v=1
		if !looksLikeDomain(value) {
			return "", fmt.Errorf("不是可识别的链接: %s", raw)
		}
		value = "https://" + strings.TrimPrefix(value, "//")
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("链接解析失败: %s", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("仅支持 http(s) 链接: %s", raw)
	}

	host := strings.ToLower(parsed.Hostname())
	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
	}
	// 去默认端口，保留非默认端口
	port := parsed.Port()
	if port == "80" || port == "443" {
		port = ""
	}
	hostPort := host
	if port != "" {
		hostPort = host + ":" + port
	}

	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}

	query := canonicalQuery(parsed.Query())
	canonical := parsed.Scheme + "://" + hostPort + path
	if query != "" {
		canonical += "?" + query
	}
	return canonical, nil
}

// canonicalQuery 去掉追踪参数并按 key 排序，保证参数顺序不影响幂等 ID。
func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		lower := strings.ToLower(key)
		if trackingParams[lower] || strings.HasPrefix(lower, "utm_") || strings.HasPrefix(lower, "share_") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make(url.Values, len(keys))
	for _, key := range keys {
		list := append([]string{}, values[key]...)
		sort.Strings(list)
		out[key] = list
	}
	return out.Encode()
}

func looksLikeDomain(value string) bool {
	candidate := value
	if idx := strings.IndexAny(candidate, "/?#"); idx >= 0 {
		candidate = candidate[:idx]
	}
	if strings.Contains(candidate, " ") || !strings.Contains(candidate, ".") {
		return false
	}
	return true
}

// crawlURLInput 保留了用户的原始链接与归一化后的 canonical URL
// （original_url 用于贴回前端，canonical_url 用于幂等 / 缓存 / 去重）。
type crawlURLInput struct {
	Raw       string
	Canonical string
}

// normalizeURLList 归一化并去重，最多保留 max 条。
func normalizeURLList(input string, max int) ([]crawlURLInput, []string) {
	rawList := extractURLs(input)
	entries := make([]crawlURLInput, 0, len(rawList))
	problems := make([]string, 0)
	seen := make(map[string]struct{}, len(rawList))
	for _, raw := range rawList {
		canonical, err := canonicalizeURL(raw)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		entries = append(entries, crawlURLInput{Raw: raw, Canonical: canonical})
		if max > 0 && len(entries) >= max {
			break
		}
	}
	return entries, problems
}

func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// siteKeyFor 站点标识：未知站点回落到 host 本身，后续由 L3 抽规则。
func siteKeyFor(rawURL string) string {
	host := hostOf(rawURL)
	if host == "" {
		return "unknown"
	}
	for _, entry := range siteKeyBySuffix {
		if host == entry.suffix || strings.HasSuffix(host, "."+entry.suffix) {
			return entry.key
		}
	}
	return strings.TrimPrefix(host, "www.")
}

func isShortLink(rawURL string) bool {
	return shortLinkHosts[hostOf(rawURL)]
}

func videoItemIDFor(canonicalURL string) string {
	sum := sha1.Sum([]byte(canonicalURL))
	return hex.EncodeToString(sum[:])[:16]
}

// expandShortLink 追 302 拿到详情页；抖音分享链接常需要二次跳转（§12 第 1 条）。
func expandShortLink(ctx context.Context, client *http.Client, rawURL string) (string, string) {
	if !isShortLink(rawURL) {
		return rawURL, ""
	}
	requestCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodHead, rawURL, nil)
	if err != nil {
		return rawURL, ""
	}
	req.Header.Set("User-Agent", pickUserAgent(hostOf(rawURL)))

	resp, err := client.Do(req)
	if err == nil {
		final := resp.Request.URL.String()
		resp.Body.Close()
		if resp.StatusCode < 400 && final != rawURL {
			if canonical, err := canonicalizeURL(final); err == nil {
				return canonical, "短链展开: " + final
			}
		}
	}

	// HEAD 被拒时用 GET 再试一次，只读第一段响应就关掉
	req, err = http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return rawURL, ""
	}
	req.Header.Set("User-Agent", pickUserAgent(hostOf(rawURL)))
	resp, err = client.Do(req)
	if err != nil {
		return rawURL, ""
	}
	final := resp.Request.URL.String()
	resp.Body.Close()
	if canonical, err := canonicalizeURL(final); err == nil && canonical != rawURL {
		return canonical, "短链展开: " + final
	}
	return rawURL, ""
}
