package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func TestCanonicalizeURLStripsTracking(t *testing.T) {
	cases := map[string]string{
		"https://WWW.Bilibili.com/video/BV1xx411c7mD/?spm_id_from=333.999&vd_source=abc#reply": "https://www.bilibili.com/video/BV1xx411c7mD/",
		"//douyin.com/video/123?utm_source=a&share_medium=b":                                   "https://douyin.com/video/123",
		"www.example.com/watch?v=2&from=timeline":                                              "https://www.example.com/watch?v=2",
		"https://example.com:443/a?b=1&a=2":                                                    "https://example.com/a?a=2&b=1",
	}
	for input, expected := range cases {
		got, err := canonicalizeURL(input)
		if err != nil {
			t.Fatalf("%s 归一化失败: %v", input, err)
		}
		if got != expected {
			t.Fatalf("%s 归一化错误：期望 %s，实际 %s", input, expected, got)
		}
	}
	if _, err := canonicalizeURL("ftp://example.com/a"); err == nil {
		t.Fatal("非 http(s) 链接应报错")
	}
	if _, err := canonicalizeURL("随便一段文字"); err == nil {
		t.Fatal("不是链接应报错")
	}
}

func TestExtractURLsFromShareText(t *testing.T) {
	input := "看看这个 https://www.bilibili.com/video/BV1xx411c7mD?spm=1 ，还有 https://youtu.be/abc。"
	urls := extractURLs(input)
	if len(urls) != 2 {
		t.Fatalf("应抽出 2 条 URL，实际 %d：%v", len(urls), urls)
	}
	entries, problems := normalizeURLList(input+" https://www.bilibili.com/video/BV1xx411c7mD?spm=1", 10)
	if len(entries) != 2 {
		t.Fatalf("归一化后应去重为 2 条，实际 %d", len(entries))
	}
	if len(problems) != 0 {
		t.Fatalf("不应有解析问题: %v", problems)
	}
	if siteKeyFor(entries[0].Canonical) != "bilibili" {
		t.Fatalf("站点识别错误: %s", siteKeyFor(entries[0].Canonical))
	}
}

func TestRobotsParsingAndMatching(t *testing.T) {
	rules := parseRobots(`
User-agent: Googlebot
Disallow: /

User-agent: *
Disallow: /search
Allow: /search/about
Disallow: /*.pdf$
`)
	if !robotsMatch("/search?q=1", "/search?q=1") {
		t.Fatal("规则前缀匹配失败")
	}
	for _, rule := range rules.disallow {
		if robotsMatch(rule, "/video/1") {
			t.Fatalf("不在规则里的路径不应被禁止: %s", rule)
		}
	}
	if !robotsMatch("/*.pdf$", "/a/b.pdf") || robotsMatch("/*.pdf$", "/a/b.pdf?x=1") {
		t.Fatal("$ 结尾匹配语义错误")
	}
	if len(rules.allow) != 1 || len(rules.disallow) != 2 {
		t.Fatalf("robots 解析结果不符预期: %+v", rules)
	}
}

const crawlFixtureHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <title>凌晨四点的城市 - 某某视频</title>
  <link rel="canonical" href="https://example.com/video/1?utm_source=x">
  <meta property="og:title" content="凌晨四点的城市" />
  <meta property="og:description" content="环卫工人在凌晨四点开始一天的工作，镜头跟随他穿过安静的街道。" />
  <meta property="og:image" content="//cdn.example.com/cover/1.jpg" />
  <meta property="og:image:width" content="1280" />
  <meta property="og:image" content="https://cdn.example.com/cover/2.jpg" />
  <meta property="og:site_name" content="某某视频" />
  <script type="application/ld+json">
  {"@context":"https://schema.org","@type":"VideoObject","name":"凌晨四点的城市","description":"环卫工人在凌晨四点开始一天的工作。","thumbnailUrl":"https://cdn.example.com/cover/3.jpg","uploadDate":"2026-10-01T08:00:00+08:00","duration":"PT1M30S","author":{"@type":"Person","name":"阿力"}}
  </script>
</head>
<body><h1 class="video-title">凌晨四点的城市</h1><div id="app"><p>正文</p></div></body>
</html>`

func TestSelectorEngineParsesProfileRules(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(crawlFixtureHTML))
	if err != nil {
		t.Fatalf("解析 HTML 失败: %v", err)
	}
	if got := selectFirstValue(doc, []string{"meta[property='og:title']@content"}); got != "凌晨四点的城市" {
		t.Fatalf("meta 选择器取值错误: %q", got)
	}
	if got := selectFirstValue(doc, []string{"h1.video-title::text"}); got != "凌晨四点的城市" {
		t.Fatalf("类选择器取值错误: %q", got)
	}
	if got := selectFirstValue(doc, []string{"div#app > p::text"}); got != "正文" {
		t.Fatalf("子节点选择器取值错误: %q", got)
	}
	if !selectorHits(doc, "meta[property='og:description']@content", "环卫工人") {
		t.Fatal("dry-run 应命中简介片段")
	}
	if selectorHits(doc, "meta[property='og:title']@content", "不存在的标题") {
		t.Fatal("不匹配的文本不应通过 dry-run")
	}
	if _, err := parseSelector("div[unclosed"); err == nil {
		t.Fatal("非法选择器应报错")
	}
}

func TestStructuredExtractionPicksVideoObjectAndAbsoluteImage(t *testing.T) {
	page := crawlPage{
		URL:    "https://example.com/video/1",
		Status: 200,
		Body:   []byte(crawlFixtureHTML),
	}
	session, err := newCrawlExtractSession(&App{}, CrawlRunConfig{}, Provider{}, page, nil)
	if err != nil {
		t.Fatalf("创建抽取会话失败: %v", err)
	}
	candidate, err := session.fromStructured(context.Background())
	if err != nil {
		t.Fatalf("结构化抽取失败: %v", err)
	}
	if candidate.Title != "凌晨四点的城市" {
		t.Fatalf("标题抽取错误: %q", candidate.Title)
	}
	if !strings.HasPrefix(candidate.Thumbnail, "https://cdn.example.com/cover/") {
		t.Fatalf("封面应补全为绝对地址: %q", candidate.Thumbnail)
	}
	if candidate.Author != "阿力" {
		t.Fatalf("作者应来自 JSON-LD: %q", candidate.Author)
	}
	if candidate.DurationS != 90 {
		t.Fatalf("ISO 时长应解析为 90 秒，实际 %.1f", candidate.DurationS)
	}
}

func TestValidateCandidateReportsProblemsAndScores(t *testing.T) {
	good := crawlCandidate{
		Title:       "凌晨四点的城市",
		Description: "环卫工人在凌晨四点开始一天的工作，镜头跟随他穿过安静的街道。",
		Thumbnail:   "https://cdn.example.com/cover/1.jpg",
		Tier:        CrawlTierStructured,
		Confidence:  0.85,
	}
	score, problems := validateCandidate(good, "https://example.com/video/1")
	if len(problems) != 0 {
		t.Fatalf("完整三要素不应有问题: %v", problems)
	}
	if score < 0.7 || score > 1 {
		t.Fatalf("置信度打分超出预期: %.2f", score)
	}

	broken := crawlCandidate{Title: "短", Description: "", Thumbnail: "//cdn/cover.jpg", Tier: CrawlTierLLM}
	score, problems = validateCandidate(broken, "https://example.com/video/1")
	if score >= 0.6 {
		t.Fatalf("残缺候选的分不应达标: %.2f", score)
	}
	joined := strings.Join(problems, "；")
	for _, expect := range []string{"标题过短", "缺少简介", "封面不是 http(s) 绝对地址"} {
		if !strings.Contains(joined, expect) {
			t.Fatalf("缺少问题描述 %s: %v", expect, problems)
		}
	}
}

func TestMergeCandidatesPrefersAuthoritativeTier(t *testing.T) {
	merged := mergeCandidates([]crawlCandidate{
		{Title: "OG 标题", Description: "OG 简介", Tier: CrawlTierStructured},
		{Title: "oEmbed 标题", Thumbnail: "https://cdn/x.jpg", Tier: CrawlTierOEmbed},
		{Title: "LLM 标题", Author: "LLM 作者", Tier: CrawlTierLLM},
	})
	if merged.Title != "oEmbed 标题" {
		t.Fatalf("应优先采信 oEmbed 标题: %q", merged.Title)
	}
	if merged.Description != "OG 简介" || merged.Author != "LLM 作者" {
		t.Fatalf("缺失字段应被下层补齐: %+v", merged)
	}
	if len(merged.Variants) == 0 {
		t.Fatal("多源冲突应记录 variants")
	}
}

func TestCleanGenericTitleStripsSiteSuffix(t *testing.T) {
	cases := map[string]string{
		"凌晨四点的城市 - 某某视频":                     "凌晨四点的城市",
		"一杯咖啡唤醒清晨_哔哩哔哩 (゜-゜)つロ 干杯~-bilibili": "一杯咖啡唤醒清晨",
		"没有后缀的标题":                            "没有后缀的标题",
	}
	for input, expected := range cases {
		if got := cleanGenericTitle(input, "某某视频"); got != expected {
			t.Fatalf("%q 清洗后应为 %q，实际 %q", input, expected, got)
		}
	}
}

func TestCrawlTaskLifecycleWithLocalSite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(crawlFixtureHTML))
	}))
	defer server.Close()

	app := newTestApp(t)
	task, err := app.CreateCrawlTask(CrawlTaskInput{
		Title: "选题池A",
		Config: CrawlRunConfig{
			Input:         "看看这个 " + server.URL + "/video/1?utm_source=share ，很值得做一期。",
			Tags:          []string{"选题池A"},
			MaxUrls:       3,
			RespectRobots: true,
			IntervalMS:    200,
			TimeoutS:      5,
		},
	})
	if err != nil {
		t.Fatalf("创建爬取任务失败: %v", err)
	}
	if task.Status != CrawlStatusDraft || task.Stage != CrawlStageIntake {
		t.Fatalf("新建任务应停在待执行: %s/%s", task.Stage, task.Status)
	}
	if len(task.Run.Results) != 1 || task.Run.Results[0].State != CrawlStateCreated {
		t.Fatalf("URL 收录结果不符预期: %+v", task.Run.Results)
	}

	app.runCrawlPipeline(task.ID, false)

	stored, err := app.GetCrawlTask(task.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if stored.Status != CrawlStatusDone || stored.Stage != CrawlStageDone {
		t.Fatalf("任务应完成: %s/%s %s", stored.Stage, stored.Status, stored.Error)
	}
	if len(stored.Run.Items) != 1 {
		t.Fatalf("条目库视图应有 1 条: %+v", stored.Run.Items)
	}
	item := stored.Run.Items[0]
	if item.Title != "凌晨四点的城市" || item.Meta.ExtractTier != CrawlTierStructured {
		t.Fatalf("抽取结果不符预期: %+v", item)
	}
	// 任务上填的「选题池标签」得真的跟着条目走，否则这个输入框就是摆设
	if tags := strings.Join(item.Optional.Tags, ","); !strings.Contains(tags, "选题池A") {
		t.Fatalf("选题池标签未写入条目: %+v", item.Optional.Tags)
	}
	if stored.Run.Stats.Stored != 1 || stored.Run.Stats.TierHits[CrawlTierStructured] != 1 {
		t.Fatalf("统计不符预期: %+v", stored.Run.Stats)
	}

	// 二次执行命中缓存（30 天内）
	stored.Run.Results[0].State = CrawlStateCreated
	stored.Run.Results[0].Steps = nil
	_ = app.updateCrawlTaskRecord(stored)
	app.runCrawlPipeline(task.ID, false)
	cached, err := app.GetCrawlTask(task.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if cached.Run.Results[0].State != CrawlStateCached || !cached.Run.Results[0].Cached {
		t.Fatalf("二次执行应命中缓存: %+v", cached.Run.Results[0])
	}

	// 强制重爬忽略缓存
	app.runCrawlPipeline(task.ID, true)
	recrawled, err := app.GetCrawlTask(task.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if recrawled.Run.Results[0].State != CrawlStateStored {
		t.Fatalf("强制重爬应重新入库: %+v", recrawled.Run.Results[0])
	}

	items, err := app.ListCrawlItems(20)
	if err != nil || len(items) != 1 {
		t.Fatalf("素材库应有 1 条: %v %+v", err, items)
	}
	if tags := strings.Join(items[0].Optional.Tags, ","); !strings.Contains(tags, "选题池A") {
		t.Fatalf("素材库条目应带着选题池标签: %+v", items[0].Optional.Tags)
	}

	if err := app.DeleteCrawlTask(task.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	if list, err := app.ListCrawlTasks(); err != nil || len(list) != 0 {
		t.Fatalf("删除后任务列表应为空: %v %+v", err, list)
	}
}

func TestCrawlSiteProfileRoundTrip(t *testing.T) {
	app := newTestApp(t)
	profile := SiteProfile{
		SiteKey:        "example",
		HostPattern:    "example.com",
		Selectors:      map[string][]string{"title": []string{"meta[property='og:title']@content"}},
		RequiresRender: false,
		Enabled:        true,
		AutoGenerated:  true,
		UpdatedAt:      time.Now().Format(time.RFC3339),
	}
	if err := app.upsertSiteProfile(profile); err != nil {
		t.Fatalf("写入站点规则失败: %v", err)
	}
	loaded, err := app.siteProfileByKey("example")
	if err != nil || loaded == nil {
		t.Fatalf("读取站点规则失败: %v", err)
	}
	if loaded.Selectors["title"][0] != "meta[property='og:title']@content" || !loaded.Enabled {
		t.Fatalf("站点规则内容不符预期: %+v", loaded)
	}

	app.recordSiteProfileHit("example", true)
	app.recordSiteProfileHit("example", false)
	app.recordSiteProfileHit("example", false)
	app.recordSiteProfileHit("example", false)
	updated, _ := app.siteProfileByKey("example")
	if updated.HitSuccess != 1 || updated.HitFail != 3 {
		t.Fatalf("命中统计不符预期: %+v", updated)
	}

	profiles, err := app.ListSiteProfiles()
	if err != nil || len(profiles) != 1 {
		t.Fatalf("站点画像列表不符预期: %v %+v", err, profiles)
	}
	if err := app.DeleteSiteProfile("example"); err != nil {
		t.Fatalf("删除站点规则失败: %v", err)
	}
}

func TestCrawlTaskCreateRejectsInputWithoutURL(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.CreateCrawlTask(CrawlTaskInput{Config: CrawlRunConfig{Input: "只有文字没有链接"}}); err == nil {
		t.Fatal("没有链接时创建任务应报错")
	}
}

// 抓取策略上的勾选得真的作数：勾掉 L3 不能因为选了文本模型就被顶回开启，
// 关掉 robots 得留下提醒，间隔/超时没填那么回落默认档。
func TestNormalizeCrawlConfigKeepsUserChoices(t *testing.T) {
	cfg, warnings, err := normalizeCrawlConfig(CrawlRunConfig{
		Input:          "https://www.bilibili.com/video/BV1xx411c7mD",
		TextProviderID: " provider-1 ",
		UseLLM:         false,
	})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if cfg.UseLLM {
		t.Fatal("勾掉 L3 后不该被文本模型顶回开启")
	}
	if cfg.TextProviderID != "provider-1" {
		t.Fatalf("文本模型 ID 应去空格: %q", cfg.TextProviderID)
	}
	if cfg.IntervalMS != defaultCrawlIntervalMS || cfg.TimeoutS != defaultCrawlTimeoutS {
		t.Fatalf("间隔/超时应回落默认档: %d / %d", cfg.IntervalMS, cfg.TimeoutS)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "已关闭 L3") {
		t.Fatalf("应提示 L3 已按用户勾选关闭: %v", warnings)
	}
	if !strings.Contains(joined, "robots.txt") {
		t.Fatalf("未勾选 robots 应给出警告: %v", warnings)
	}

	// 勾上 L3 且不填模型 ID：交给流水线自动挑选，不改用户意图
	on, _, err := normalizeCrawlConfig(CrawlRunConfig{
		Input:  "https://www.bilibili.com/video/BV1xx411c7mD",
		UseLLM: true,
	})
	if err != nil || !on.UseLLM {
		t.Fatalf("勾选 L3 应保持开启: %v %+v", err, on)
	}
}
