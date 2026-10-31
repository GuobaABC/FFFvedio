package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// 可通过变量覆盖的第三方端点，测试时指向 httptest 服务。
var (
	crawlYouTubeOEmbedAPI = "https://www.youtube.com/oembed?format=json&url="
	crawlVimeoOEmbedAPI   = "https://vimeo.com/api/oembed.json?url="
	crawlBilibiliViewAPI  = "https://api.bilibili.com/x/web-interface/view?bvid="
)

// crawlCandidate 是抽取瀑布每一层的中间产物；拿到完整且合规的三要素就停止下钻（§6）。
type crawlCandidate struct {
	Title       string
	Description string
	Thumbnail   string
	Author      string
	PublishedAt string
	DurationS   float64
	Tags        []string
	Tier        string
	Confidence  float64
	Variants    []string
}

func (c crawlCandidate) complete() bool {
	return strings.TrimSpace(c.Title) != "" && strings.TrimSpace(c.Description) != "" && strings.TrimSpace(c.Thumbnail) != ""
}

func (c crawlCandidate) empty() bool {
	return c.Title == "" && c.Description == "" && c.Thumbnail == ""
}

var tierBaseScore = map[string]float64{
	CrawlTierOEmbed:     0.98,
	CrawlTierAdapter:    0.92,
	CrawlTierProfile:    0.9,
	CrawlTierStructured: 0.85,
	CrawlTierLLM:        0.72,
	CrawlTierRule:       0.8,
}

var tierPriority = map[string]int{
	CrawlTierOEmbed:     5,
	CrawlTierAdapter:    4,
	CrawlTierProfile:    3,
	CrawlTierStructured: 2,
	CrawlTierLLM:        1,
	CrawlTierRule:       6,
}

// crawlExtractSession 一次 URL 的抽取上下文：DOM + 站点画像 + 已尝试的层级。
type crawlExtractSession struct {
	app      *App
	cfg      CrawlRunConfig
	provider Provider
	page     crawlPage
	doc      *html.Node
	profile  *SiteProfile
	steps    []CrawlStep
	warnings []string
	llmUsed  bool
}

func newCrawlExtractSession(app *App, cfg CrawlRunConfig, provider Provider, page crawlPage, profile *SiteProfile) (*crawlExtractSession, error) {
	doc, err := html.Parse(strings.NewReader(string(page.Body)))
	if err != nil {
		return nil, newCrawlError(CrawlErrParse, page.Status, "HTML 解析失败: %v", err)
	}
	return &crawlExtractSession{
		app:      app,
		cfg:      cfg,
		provider: provider,
		page:     page,
		doc:      doc,
		profile:  profile,
	}, nil
}

// run 顺序执行 L0 → L1(画像/适配器) → L2 → L3，任一层拿到完整三要素即返回。
func (s *crawlExtractSession) run(ctx context.Context) (crawlCandidate, error) {
	type tierFn struct {
		name string
		fn   func(context.Context) (crawlCandidate, error)
	}
	tiers := []tierFn{
		{CrawlTierOEmbed, s.fromOEmbed},
		{CrawlTierProfile, s.fromProfile},
		{CrawlTierAdapter, s.fromAdapter},
		{CrawlTierStructured, s.fromStructured},
	}

	candidates := make([]crawlCandidate, 0, 4)
	for _, tier := range tiers {
		started := time.Now()
		candidate, err := tier.fn(ctx)
		elapsed := time.Since(started).Milliseconds()
		if err != nil {
			s.recordStep(tier.name, CrawlStateSkipped, "error", elapsed, err.Error())
			continue
		}
		if candidate.empty() {
			s.recordStep(tier.name, CrawlStateSkipped, "empty", elapsed, "该层没有拿到字段")
			continue
		}
		candidates = append(candidates, candidate)
		s.recordStep(tier.name, CrawlStateParsed, "ok", elapsed, describeCandidate(candidate))
		if merged := mergeCandidates(candidates); merged.complete() {
			return merged, nil
		}
	}

	// L3：LLM 零样本抽取（只在新域名首爬/规则失效时启用，§1 第 5 条）
	if s.cfg.UseLLM && s.provider.ID != "" {
		started := time.Now()
		candidate, err := s.fromLLM(ctx)
		elapsed := time.Since(started).Milliseconds()
		if err != nil {
			s.recordStep(CrawlTierLLM, CrawlStateNeedsLLM, CrawlErrLLM, elapsed, err.Error())
		} else if !candidate.empty() {
			s.llmUsed = true
			candidates = append(candidates, candidate)
			s.recordStep(CrawlTierLLM, CrawlStateLLMDone, "ok", elapsed, describeCandidate(candidate))
		}
	}

	merged := mergeCandidates(candidates)
	if merged.empty() {
		return crawlCandidate{}, newCrawlError(CrawlErrParse, s.page.Status, "所有抽取层都没有拿到有效字段")
	}
	return merged, nil
}

func describeCandidate(c crawlCandidate) string {
	parts := make([]string, 0, 3)
	if c.Title != "" {
		parts = append(parts, "标题 "+strconv.Itoa(runeLen(c.Title))+" 字")
	}
	if c.Description != "" {
		parts = append(parts, "简介 "+strconv.Itoa(runeLen(c.Description))+" 字")
	}
	if c.Thumbnail != "" {
		parts = append(parts, "封面 1 张")
	}
	return strings.Join(parts, " · ")
}

func (s *crawlExtractSession) recordStep(tier, state, status string, latencyMS int64, note string) {
	s.steps = append(s.steps, CrawlStep{
		Step:      "EXTRACT",
		State:     state,
		Tier:      tier,
		Status:    status,
		LatencyMS: latencyMS,
		Note:      note,
		At:        time.Now().Format(time.RFC3339),
	})
}

func (s *crawlExtractSession) warn(message string) {
	for _, existing := range s.warnings {
		if existing == message {
			return
		}
	}
	s.warnings = append(s.warnings, message)
}

/* ---------------- L0：oEmbed / 平台 API ---------------- */

func (s *crawlExtractSession) fromOEmbed(ctx context.Context) (crawlCandidate, error) {
	siteKey := siteKeyFor(s.page.URL)
	if siteKey == "" {
		siteKey = siteKeyFor(s.cfg.Input)
	}
	if !hasOEmbedChannel(siteKey) {
		return crawlCandidate{}, nil
	}
	switch siteKey {
	case "youtube":
		endpoint := crawlYouTubeOEmbedAPI + url.QueryEscape(s.page.URL)
		body, err := s.getJSON(ctx, endpoint)
		if err != nil {
			return crawlCandidate{}, err
		}
		var payload struct {
			Title        string `json:"title"`
			AuthorName   string `json:"author_name"`
			ThumbnailURL string `json:"thumbnail_url"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return crawlCandidate{}, err
		}
		return crawlCandidate{
			Title:       strings.TrimSpace(payload.Title),
			Description: s.youtubeDescription(),
			Thumbnail:   resolveURL(s.page.URL, payload.ThumbnailURL),
			Author:      payload.AuthorName,
			Tier:        CrawlTierOEmbed,
			Confidence:  tierBaseScore[CrawlTierOEmbed],
		}, nil

	case "vimeo":
		endpoint := crawlVimeoOEmbedAPI + url.QueryEscape(s.page.URL)
		body, err := s.getJSON(ctx, endpoint)
		if err != nil {
			return crawlCandidate{}, err
		}
		var payload struct {
			Title        string `json:"title"`
			Description  string `json:"description"`
			ThumbnailURL string `json:"thumbnail_url"`
			AuthorName   string `json:"author_name"`
			Duration     int    `json:"duration"`
			UploadDate   string `json:"upload_date"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return crawlCandidate{}, err
		}
		return crawlCandidate{
			Title:       strings.TrimSpace(payload.Title),
			Description: strings.TrimSpace(payload.Description),
			Thumbnail:   resolveURL(s.page.URL, payload.ThumbnailURL),
			Author:      payload.AuthorName,
			PublishedAt: payload.UploadDate,
			DurationS:   float64(payload.Duration),
			Tier:        CrawlTierOEmbed,
			Confidence:  tierBaseScore[CrawlTierOEmbed],
		}, nil

	case "bilibili":
		bvid := bilibiliBVID(s.page.URL)
		if bvid == "" {
			return crawlCandidate{}, nil
		}
		body, err := s.getJSON(ctx, crawlBilibiliViewAPI+bvid)
		if err != nil {
			return crawlCandidate{}, err
		}
		var payload struct {
			Code int `json:"code"`
			Data struct {
				Title    string `json:"title"`
				Desc     string `json:"desc"`
				Pic      string `json:"pic"`
				PubDate  int64  `json:"pubdate"`
				Duration int    `json:"duration"`
				Owner    struct {
					Name string `json:"name"`
				} `json:"owner"`
				Tname string `json:"tname"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return crawlCandidate{}, err
		}
		if payload.Code != 0 {
			return crawlCandidate{}, newCrawlError(CrawlErrParse, 0, "B站接口返回 code=%d", payload.Code)
		}
		published := ""
		if payload.Data.PubDate > 0 {
			published = time.Unix(payload.Data.PubDate, 0).Format(time.RFC3339)
		}
		return crawlCandidate{
			Title:       strings.TrimSpace(payload.Data.Title),
			Description: strings.TrimSpace(payload.Data.Desc),
			Thumbnail:   resolveURL(s.page.URL, payload.Data.Pic),
			Author:      payload.Data.Owner.Name,
			PublishedAt: published,
			DurationS:   float64(payload.Data.Duration),
			Tags:        nonEmptyList(payload.Data.Tname),
			Tier:        CrawlTierOEmbed,
			Confidence:  tierBaseScore[CrawlTierOEmbed],
		}, nil
	}
	return crawlCandidate{}, nil
}

func hasOEmbedChannel(siteKey string) bool {
	switch siteKey {
	case "youtube", "vimeo", "bilibili":
		return true
	}
	return false
}

func (s *crawlExtractSession) getJSON(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pickUserAgent(hostOf(endpoint)))
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return nil, newCrawlError(CrawlErrNetwork, 0, "oEmbed 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, newCrawlError(CrawlErrNetwork, resp.StatusCode, "oEmbed 响应读取失败: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newCrawlError(classifyHTTPStatus(resp.StatusCode), resp.StatusCode, "oEmbed 返回 %d", resp.StatusCode)
	}
	return body, nil
}

var bilibiliBVIDPattern = regexp.MustCompile(`BV[0-9A-Za-z]{10}`)

func bilibiliBVID(rawURL string) string {
	return bilibiliBVIDPattern.FindString(rawURL)
}

// youtubeDescription 从页面里补简介，oEmbed 不返回简介。
func (s *crawlExtractSession) youtubeDescription() string {
	if s.doc != nil {
		if meta := metaContent(s.doc, "property", "og:description"); meta != "" {
			return meta
		}
		if meta := metaContent(s.doc, "name", "description"); meta != "" {
			return meta
		}
	}
	return ""
}

/* ---------------- L1：站点画像（沉淀的规则） ---------------- */

func (s *crawlExtractSession) fromProfile(_ context.Context) (crawlCandidate, error) {
	if s.profile == nil || !s.profile.Enabled || len(s.profile.Selectors) == 0 {
		return crawlCandidate{}, nil
	}
	title := selectFirstValue(s.doc, s.profile.Selectors["title"])
	description := selectFirstValue(s.doc, s.profile.Selectors["description"])
	thumbnail := selectFirstValue(s.doc, s.profile.Selectors["thumbnail"])
	if title == "" && description == "" && thumbnail == "" {
		return crawlCandidate{}, newCrawlError(CrawlErrParse, 0, "站点画像规则未命中")
	}
	candidate := crawlCandidate{
		Title:       title,
		Description: description,
		Thumbnail:   resolveURL(s.page.URL, thumbnail),
		Tier:        CrawlTierProfile,
		Confidence:  tierBaseScore[CrawlTierProfile],
	}
	return applyCleaners(candidate, s.profile), nil
}

/* ---------------- L1：站点适配器 ---------------- */

func (s *crawlExtractSession) fromAdapter(_ context.Context) (crawlCandidate, error) {
	switch siteKeyFor(s.page.URL) {
	case "douyin":
		return s.fromDouyin(), nil
	case "bilibili":
		return s.fromBilibiliState(), nil
	case "youtube":
		return s.fromYouTubeState(), nil
	case "xiaohongshu":
		return s.fromXiaohongshuState(), nil
	}
	return crawlCandidate{}, nil
}

func (s *crawlExtractSession) fromDouyin() crawlCandidate {
	// 抖音把页面数据放在 RENDER_DATA / _ROUTER_DATA 里（URL 编码后的 JSON）
	for _, id := range []string{"RENDER_DATA", "_ROUTER_DATA"} {
		raw := scriptByID(s.doc, id)
		if raw == "" {
			continue
		}
		if decoded, err := url.QueryUnescape(raw); err == nil {
			raw = decoded
		}
		var payload any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		title := findJSONString(payload, []string{"desc", "title"}, 8)
		description := findJSONString(payload, []string{"desc"}, 8)
		cover := findJSONString(payload, []string{"url_list", "cover", "origin_cover"}, 8)
		if title == "" && description == "" && cover == "" {
			continue
		}
		return crawlCandidate{
			Title:       title,
			Description: description,
			Thumbnail:   resolveURL(s.page.URL, cover),
			Author:      findJSONString(payload, []string{"nickname", "unique_id"}, 9),
			Tier:        CrawlTierAdapter,
			Confidence:  tierBaseScore[CrawlTierAdapter],
		}
	}
	return crawlCandidate{}
}

func (s *crawlExtractSession) fromBilibiliState() crawlCandidate {
	raw := jsonAfterMarker(s.page.Body, "window.__INITIAL_STATE__=")
	if raw == "" {
		return crawlCandidate{}
	}
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return crawlCandidate{}
	}
	return crawlCandidate{
		Title:       findJSONString(payload, []string{"title"}, 6),
		Description: findJSONString(payload, []string{"desc"}, 6),
		Thumbnail:   resolveURL(s.page.URL, findJSONString(payload, []string{"pic"}, 6)),
		Author:      findJSONString(payload, []string{"name"}, 7),
		Tier:        CrawlTierAdapter,
		Confidence:  tierBaseScore[CrawlTierAdapter],
	}
}

func (s *crawlExtractSession) fromYouTubeState() crawlCandidate {
	raw := jsonAfterMarker(s.page.Body, "ytInitialPlayerResponse =")
	if raw == "" {
		raw = jsonAfterMarker(s.page.Body, "ytInitialPlayerResponse=")
	}
	if raw == "" {
		return crawlCandidate{}
	}
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return crawlCandidate{}
	}
	duration := findJSONString(payload, []string{"lengthSeconds"}, 7)
	seconds, _ := strconv.ParseFloat(strings.TrimSpace(duration), 64)
	return crawlCandidate{
		Title:       findJSONString(payload, []string{"title"}, 7),
		Description: findJSONString(payload, []string{"shortDescription"}, 7),
		Thumbnail:   resolveURL(s.page.URL, findJSONString(payload, []string{"thumbnails", "url"}, 9)),
		Author:      findJSONString(payload, []string{"author"}, 7),
		DurationS:   seconds,
		Tier:        CrawlTierAdapter,
		Confidence:  tierBaseScore[CrawlTierAdapter],
	}
}

func (s *crawlExtractSession) fromXiaohongshuState() crawlCandidate {
	raw := jsonAfterMarker(s.page.Body, "window.__INITIAL_STATE__=")
	if raw == "" {
		return crawlCandidate{}
	}
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return crawlCandidate{}
	}
	return crawlCandidate{
		Title:       findJSONString(payload, []string{"title"}, 9),
		Description: findJSONString(payload, []string{"desc"}, 9),
		Thumbnail:   resolveURL(s.page.URL, findJSONString(payload, []string{"urlDefault", "url"}, 9)),
		Author:      findJSONString(payload, []string{"nickname"}, 9),
		Tier:        CrawlTierAdapter,
		Confidence:  tierBaseScore[CrawlTierAdapter],
	}
}

/* ---------------- L2：结构化标记 ---------------- */

var videoObjectTypes = map[string]bool{
	"VideoObject": true, "Movie": true, "TVEpisode": true, "Clip": true,
}

func (s *crawlExtractSession) fromStructured(_ context.Context) (crawlCandidate, error) {
	candidate := crawlCandidate{Tier: CrawlTierStructured, Confidence: tierBaseScore[CrawlTierStructured]}

	// 1. JSON-LD：最像视频页的那条
	if ld := s.fromJSONLD(); !ld.empty() {
		candidate = ld
	}

	// 2. Open Graph
	if candidate.Title == "" {
		candidate.Title = metaContent(s.doc, "property", "og:title")
	}
	if candidate.Description == "" {
		candidate.Description = metaContent(s.doc, "property", "og:description")
	}
	if candidate.Thumbnail == "" {
		candidate.Thumbnail = s.bestOGImage()
	}
	if candidate.Author == "" {
		candidate.Author = metaContent(s.doc, "name", "author")
	}

	// 3. Twitter Card
	if candidate.Title == "" {
		candidate.Title = metaContent(s.doc, "name", "twitter:title")
	}
	if candidate.Description == "" {
		candidate.Description = metaContent(s.doc, "name", "twitter:description")
	}
	if candidate.Thumbnail == "" {
		candidate.Thumbnail = resolveURL(s.page.URL, metaContent(s.doc, "name", "twitter:image"))
	}

	// 4. 传统 title + description
	if candidate.Title == "" {
		if titleNode := findFirstNode(s.doc, "title"); titleNode != nil {
			candidate.Title = nodeText(titleNode)
		}
	}
	if candidate.Description == "" {
		candidate.Description = metaContent(s.doc, "name", "description")
	}
	// 5. h1 兜底
	if candidate.Title == "" {
		for _, node := range findAllNodes(s.doc, "h1") {
			if text := nodeText(node); runeLen(text) >= 4 {
				candidate.Title = text
				break
			}
		}
	}

	candidate.Title = cleanGenericTitle(candidate.Title, s.siteName())
	candidate.Description = strings.TrimSpace(candidate.Description)
	candidate.Thumbnail = resolveURL(s.page.URL, candidate.Thumbnail)
	if candidate.empty() {
		return crawlCandidate{}, newCrawlError(CrawlErrParse, 0, "页面没有结构化标记")
	}
	siteName := s.siteName()
	if siteName != "" && strings.TrimSpace(candidate.Title) == strings.TrimSpace(siteName) {
		candidate.Title = ""
	}
	return candidate, nil
}

func (s *crawlExtractSession) fromJSONLD() crawlCandidate {
	for _, node := range findAllNodes(s.doc, "script") {
		if !strings.EqualFold(attrOf(node, "type"), "application/ld+json") {
			continue
		}
		raw := strings.TrimSpace(rawText(node))
		if raw == "" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		for _, object := range collectJSONObjects(payload) {
			kind, _ := object["@type"].(string)
			if !videoObjectTypes[kind] {
				continue
			}
			candidate := crawlCandidate{Tier: CrawlTierStructured, Confidence: tierBaseScore[CrawlTierStructured]}
			candidate.Title = stringField(object, "name")
			candidate.Description = stringField(object, "description")
			candidate.Thumbnail = resolveURL(s.page.URL, thumbnailField(object))
			candidate.Author = authorField(object)
			candidate.PublishedAt = stringField(object, "uploadDate")
			candidate.DurationS = durationField(object, "duration")
			candidate.Tags = stringListField(object, "keywords")
			if !candidate.empty() {
				return candidate
			}
		}
	}
	return crawlCandidate{}
}

// bestOGImage 有多个 og:image 时按 og:image:width 取最大（§6 L2 注意坑）。
func (s *crawlExtractSession) bestOGImage() string {
	best := ""
	bestWidth := -1
	for _, node := range findAllNodes(s.doc, "meta") {
		if !strings.EqualFold(attrOf(node, "property"), "og:image") {
			continue
		}
		value := resolveURL(s.page.URL, attrOf(node, "content"))
		if value == "" {
			continue
		}
		width := 0
		if raw := attrOf(node, "data-width"); raw != "" {
			width, _ = strconv.Atoi(raw)
		}
		if best == "" || width > bestWidth {
			best = value
			bestWidth = width
		}
	}
	if best == "" {
		best = resolveURL(s.page.URL, metaContent(s.doc, "property", "og:image"))
	}
	return best
}

func (s *crawlExtractSession) siteName() string {
	if name := metaContent(s.doc, "property", "og:site_name"); name != "" {
		return name
	}
	if s.profile != nil && s.profile.SiteKey != "" {
		return s.profile.SiteKey
	}
	return siteKeyFor(s.page.URL)
}

/* ---------------- L3：LLM 零样本抽取 ---------------- */

func (s *crawlExtractSession) fromLLM(ctx context.Context) (crawlCandidate, error) {
	candidate, err := s.app.llmExtractVideoItem(ctx, s.provider, s.page, s.doc)
	if err != nil {
		return crawlCandidate{}, err
	}
	candidate.Thumbnail = resolveURL(s.page.URL, candidate.Thumbnail)
	candidate.Title = cleanGenericTitle(candidate.Title, s.siteName())
	candidate.Tier = CrawlTierLLM
	if candidate.Confidence <= 0 {
		candidate.Confidence = tierBaseScore[CrawlTierLLM]
	}
	if candidate.Confidence > 0.95 {
		candidate.Confidence = 0.95
	}
	return candidate, nil
}

/* ---------------- 合并、清洗、校验 ---------------- */

// mergeCandidates 多源冲突按层级权威度取值（oEmbed > 适配器 > 画像 > 结构化 > LLM，§7）。
func mergeCandidates(list []crawlCandidate) crawlCandidate {
	if len(list) == 0 {
		return crawlCandidate{}
	}
	ordered := append([]crawlCandidate{}, list...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return tierPriority[ordered[i].Tier] > tierPriority[ordered[j].Tier]
	})

	merged := ordered[0]
	merged.Variants = nil
	for _, candidate := range ordered[1:] {
		if candidate.Title != "" && candidate.Title != merged.Title {
			merged.Variants = append(merged.Variants, candidate.Tier+"："+candidate.Title)
		}
		if merged.Title == "" {
			merged.Title = candidate.Title
		}
		if merged.Description == "" {
			merged.Description = candidate.Description
		}
		if merged.Thumbnail == "" {
			merged.Thumbnail = candidate.Thumbnail
		}
		if merged.Author == "" {
			merged.Author = candidate.Author
		}
		if merged.PublishedAt == "" {
			merged.PublishedAt = candidate.PublishedAt
		}
		if merged.DurationS == 0 {
			merged.DurationS = candidate.DurationS
		}
		if len(merged.Tags) == 0 {
			merged.Tags = candidate.Tags
		}
	}
	return merged
}

var genericTitleSuffix = regexp.MustCompile(`(?i)\s*[-_|–—·]+\s*(哔哩哔哩|bilibili|抖音|快手|小红书|视频号|youtube|tiktok|vimeo|微博|西瓜视频).*$`)

// cleanGenericTitle 剥掉“标题 - 站名”式污染，正则只做尾部清洗（§6 L4）。
func cleanGenericTitle(title, siteName string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return ""
	}
	if siteName != "" {
		suffix := " - " + siteName
		if strings.HasSuffix(strings.ToLower(title), strings.ToLower(suffix)) {
			title = strings.TrimSpace(title[:len(title)-len(suffix)])
		}
	}
	if cleaned := genericTitleSuffix.ReplaceAllString(title, ""); runeLen(cleaned) >= 4 {
		title = cleaned
	}
	return strings.TrimSpace(title)
}

// applyCleaners 只允许正则做清洗，不允许做抽取。
func applyCleaners(candidate crawlCandidate, profile *SiteProfile) crawlCandidate {
	if profile == nil {
		return candidate
	}
	for _, cleaner := range profile.Cleaners {
		pattern, err := regexp.Compile(cleaner.Regex)
		if err != nil {
			continue
		}
		switch cleaner.Field {
		case "title":
			candidate.Title = strings.TrimSpace(pattern.ReplaceAllString(candidate.Title, cleaner.Replace))
		case "description":
			candidate.Description = strings.TrimSpace(pattern.ReplaceAllString(candidate.Description, cleaner.Replace))
		case "thumbnail":
			candidate.Thumbnail = strings.TrimSpace(pattern.ReplaceAllString(candidate.Thumbnail, cleaner.Replace))
		}
	}
	return candidate
}

// validateCandidate 字段完整性 + 合理性启发式 + 置信度打分（§7）。
func validateCandidate(candidate crawlCandidate, pageURL string) (float64, []string) {
	problems := make([]string, 0, 4)

	title := strings.TrimSpace(candidate.Title)
	switch {
	case title == "":
		problems = append(problems, "缺少标题")
	case runeLen(title) < 2:
		problems = append(problems, "标题过短")
	case runeLen(title) > 200:
		problems = append(problems, "标题过长")
	case strings.ContainsAny(title, "<>"):
		problems = append(problems, "标题里还是模板占位符")
	case strings.Contains(title, "{{"):
		problems = append(problems, "标题里还是模板占位符")
	}

	description := strings.TrimSpace(candidate.Description)
	if description == "" {
		problems = append(problems, "缺少简介")
	} else if runeLen(description) < 6 {
		problems = append(problems, "简介过短")
	}

	thumbnail := strings.TrimSpace(candidate.Thumbnail)
	if thumbnail == "" {
		problems = append(problems, "缺少封面")
	} else if !isAbsoluteHTTP(thumbnail) {
		problems = append(problems, "封面不是 http(s) 绝对地址")
	} else if !looksLikeImageURL(thumbnail) {
		problems = append(problems, "封面地址不像图片")
	}

	// 跨源一致度：封面域名与页面同源/同 CDN 才算可信
	consistent := 0.0
	if thumbnail != "" && isAbsoluteHTTP(thumbnail) {
		thumbnailHost := hostOf(thumbnail)
		pageHost := hostOf(pageURL)
		if thumbnailHost != "" && (thumbnailHost == pageHost || strings.HasSuffix(pageHost, thumbnailHost) || strings.HasSuffix(thumbnailHost, baseDomain(pageHost))) {
			consistent = 1
		} else if thumbnailHost != "" {
			consistent = 0.4
		}
	}

	complete := 0.0
	if title != "" {
		complete += 1.0 / 3
	}
	if description != "" {
		complete += 1.0 / 3
	}
	if thumbnail != "" {
		complete += 1.0 / 3
	}

	base := candidate.Confidence
	if base <= 0 {
		base = tierBaseScore[candidate.Tier]
	}
	score := 0.6*base + 0.2*complete + 0.2*consistent
	if score > 1 {
		score = 1
	}
	return round2(score), problems
}

func isAbsoluteHTTP(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

var imageExtPattern = regexp.MustCompile(`(?i)\.(jpg|jpeg|png|webp|gif|avif|bmp)(\?|$)`)

func looksLikeImageURL(raw string) bool {
	if imageExtPattern.MatchString(raw) {
		return true
	}
	// CDN 图片常常没有扩展名，此时只要不是明显的网页地址就放行
	pathPart := raw
	if parsed, err := url.Parse(raw); err == nil {
		pathPart = parsed.Path
	}
	return path.Ext(pathPart) == ""
}

func baseDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// crawlImageMimeType 封面 MIME 只能靠 URL 路径推断，不额外发请求。
func crawlImageMimeType(rawURL string) string {
	pathPart := rawURL
	if parsed, err := url.Parse(rawURL); err == nil {
		pathPart = parsed.Path
	}
	switch strings.ToLower(path.Ext(pathPart)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".avif":
		return "image/avif"
	}
	return "image/png"
}

// resolveURL 相对路径 / 协议相对地址补全（§6 L2 注意坑）。
func resolveURL(baseURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if isAbsoluteHTTP(raw) {
		return raw
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return raw
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(reference).String()
}

/* ---------------- DOM / JSON 小工具 ---------------- */

func metaContent(doc *html.Node, key, value string) string {
	if doc == nil {
		return ""
	}
	for _, node := range findAllNodes(doc, "meta") {
		if strings.EqualFold(attrOf(node, key), value) {
			return strings.TrimSpace(attrOf(node, "content"))
		}
	}
	return ""
}

func scriptByID(doc *html.Node, id string) string {
	if doc == nil {
		return ""
	}
	for _, node := range findAllNodes(doc, "script") {
		if attrOf(node, "id") == id {
			return strings.TrimSpace(rawText(node))
		}
	}
	return ""
}

// jsonAfterMarker 从 `window.__INITIAL_STATE__=` 之类的标记后截取一个平衡的 JSON 对象。
func jsonAfterMarker(body []byte, marker string) string {
	text := string(body)
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	rest := strings.TrimSpace(text[index+len(marker):])
	return balancedJSON(rest)
}

func balancedJSON(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		char := text[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return text[start : index+1]
			}
		}
	}
	return ""
}

// findJSONString 在任意 JSON 结构里按 key 优先顺序做广度搜索（适配器 best-effort）。
func findJSONString(root any, keys []string, maxDepth int) string {
	for _, key := range keys {
		if value := findJSONStringByKey(root, key, maxDepth); value != "" {
			return value
		}
	}
	return ""
}

func findJSONStringByKey(root any, key string, maxDepth int) string {
	type frame struct {
		value any
		depth int
	}
	queue := []frame{{root, 0}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth > maxDepth {
			continue
		}
		switch typed := current.value.(type) {
		case map[string]any:
			if raw, ok := typed[key]; ok {
				switch value := raw.(type) {
				case string:
					if strings.TrimSpace(value) != "" {
						return strings.TrimSpace(value)
					}
				case []any:
					for _, item := range value {
						if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
							return strings.TrimSpace(text)
						}
					}
				}
			}
			for _, child := range typed {
				queue = append(queue, frame{child, current.depth + 1})
			}
		case []any:
			for _, child := range typed {
				queue = append(queue, frame{child, current.depth + 1})
			}
		}
	}
	return ""
}

func collectJSONObjects(root any) []map[string]any {
	out := make([]map[string]any, 0, 4)
	type frame struct {
		value any
		depth int
	}
	queue := []frame{{root, 0}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth > 6 {
			continue
		}
		switch typed := current.value.(type) {
		case map[string]any:
			out = append(out, typed)
			if graph, ok := typed["@graph"]; ok {
				queue = append(queue, frame{graph, current.depth + 1})
			}
			for key, child := range typed {
				if key == "@graph" {
					continue
				}
				queue = append(queue, frame{child, current.depth + 1})
			}
		case []any:
			for _, child := range typed {
				queue = append(queue, frame{child, current.depth + 1})
			}
		}
	}
	return out
}

func stringField(object map[string]any, key string) string {
	if raw, ok := object[key]; ok {
		switch value := raw.(type) {
		case string:
			return strings.TrimSpace(value)
		case []any:
			for _, item := range value {
				if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
		}
	}
	return ""
}

func stringListField(object map[string]any, key string) []string {
	raw, ok := object[key]
	if !ok {
		return nil
	}
	out := make([]string, 0, 4)
	switch value := raw.(type) {
	case string:
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
	}
	return out
}

// thumbnailField JSON-LD 的 thumbnailUrl 可能是字符串，也可能是 ImageObject。
func thumbnailField(object map[string]any) string {
	raw, ok := object["thumbnailUrl"]
	if !ok {
		raw = object["thumbnail"]
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		return stringField(value, "url")
	case []any:
		for _, item := range value {
			switch typed := item.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					return strings.TrimSpace(typed)
				}
			case map[string]any:
				if url := stringField(typed, "url"); url != "" {
					return url
				}
			}
		}
	}
	return ""
}

func authorField(object map[string]any) string {
	raw, ok := object["author"]
	if !ok {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		return stringField(value, "name")
	case []any:
		for _, item := range value {
			if typed, ok := item.(map[string]any); ok {
				if name := stringField(typed, "name"); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// durationField 支持 ISO-8601（PT1M30S）与秒数两种写法。
func durationField(object map[string]any, key string) float64 {
	raw, ok := object[key]
	if !ok {
		return 0
	}
	text, _ := raw.(string)
	if text == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
		return seconds
	}
	return parseISODuration(text)
}

func parseISODuration(value string) float64 {
	pattern := regexp.MustCompile(`(?i)^P(?:(\d+)D)?T?(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$`)
	matches := pattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(matches) == 0 {
		return 0
	}
	toFloat := func(input string) float64 {
		if input == "" {
			return 0
		}
		parsed, _ := strconv.ParseFloat(input, 64)
		return parsed
	}
	return toFloat(matches[1])*86400 + toFloat(matches[2])*3600 + toFloat(matches[3])*60 + toFloat(matches[4])
}
