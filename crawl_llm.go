package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

const crawlExtractSystemPrompt = `你是网页信息抽取器。用户会给出一个视频详情页的静态 HTML 片段与候选元数据。
只输出一个 JSON 对象，不要 Markdown 代码块，不要解释：
{"title":"视频标题","description":"视频简介 1-3 句","thumbnail":"封面图绝对 URL","author":"作者/账号，未知填空字符串","confidence":0.0}
硬性约束：
1. title 不要包含站点名、分隔符后缀，不要编造；
2. description 是视频简介，不是作者账号简介，不要写“本文共 X 字”这类占位；
3. thumbnail 必须是 http(s) 绝对地址，不要头像、logo、广告图；
4. 字段确实找不到时填空字符串，confidence 反映你的把握（0-1）。`

const crawlSelectorSystemPrompt = `你是网页规则编译器。用户会给出页面 HTML 与已经确认的字段值。
请输出能在该 HTML 中命中这些字段的 CSS 选择器，用于下次同站快速抽取。
只输出 JSON：
{"title":["sel1","sel2"],"description":["sel1","sel2"],"thumbnail":["sel1","sel2"]}
选择器语法（只允许这几种）：
- meta[property='og:title']@content   取属性
- h1.video-title::text                取文本
- #app .play-info > .title::text      支持后代(空格)与直接子节点(>)、.class、#id
每个字段给 1-3 个 fallback；不要写正则，不要写伪类，不要输出解释。`

// llmExtractVideoItem L3：文本模型零样本抽取（新域名首爬 / 规则失效时启用）。
func (a *App) llmExtractVideoItem(ctx context.Context, provider Provider, page crawlPage, doc *html.Node) (crawlCandidate, error) {
	if provider.ID == "" {
		return crawlCandidate{}, errors.New("没有可用的文本模型")
	}
	user := buildCrawlExtractPrompt(page, doc)
	raw, err := a.chatCompletion(ctx, provider, crawlExtractSystemPrompt, user)
	if err != nil {
		return crawlCandidate{}, err
	}
	var payload struct {
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Thumbnail   string  `json:"thumbnail"`
		Author      string  `json:"author"`
		Confidence  float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return crawlCandidate{}, fmt.Errorf("LLM 返回的不是合法 JSON: %w", err)
	}
	return crawlCandidate{
		Title:       strings.TrimSpace(payload.Title),
		Description: strings.TrimSpace(payload.Description),
		Thumbnail:   strings.TrimSpace(payload.Thumbnail),
		Author:      strings.TrimSpace(payload.Author),
		Tier:        CrawlTierLLM,
		Confidence:  payload.Confidence,
	}, nil
}

// buildCrawlExtractPrompt 把页面压缩成「标题候选 + 元数据候选 + 可见正文片段」。
// 说明：本实现走文本模型，不包含整页截图（多模态输入属于后续扩展层）。
func buildCrawlExtractPrompt(page crawlPage, doc *html.Node) string {
	var builder strings.Builder
	builder.WriteString("页面 URL: ")
	builder.WriteString(page.URL)
	builder.WriteString("\n")

	if doc != nil {
		if titleNode := findFirstNode(doc, "title"); titleNode != nil {
			builder.WriteString("<title>")
			builder.WriteString(truncateRunes(nodeText(titleNode), 200))
			builder.WriteString("</title>\n")
		}
		builder.WriteString("元数据候选:\n")
		count := 0
		for _, node := range findAllNodes(doc, "meta") {
			if count >= 40 {
				break
			}
			key := attrOf(node, "property")
			if key == "" {
				key = attrOf(node, "name")
			}
			value := strings.TrimSpace(attrOf(node, "content"))
			if key == "" || value == "" {
				continue
			}
			builder.WriteString("- ")
			builder.WriteString(key)
			builder.WriteString(": ")
			builder.WriteString(truncateRunes(value, 200))
			builder.WriteString("\n")
			count++
		}
		builder.WriteString("标题候选:\n")
		headings := 0
		for _, tag := range []string{"h1", "h2", "h3"} {
			for _, node := range findAllNodes(doc, tag) {
				if headings >= 12 {
					break
				}
				text := nodeText(node)
				if text == "" {
					continue
				}
				builder.WriteString("- ")
				builder.WriteString(truncateRunes(text, 160))
				builder.WriteString("\n")
				headings++
			}
		}
	}

	bodyText := visibleText(page.Body)
	builder.WriteString("正文片段:\n")
	builder.WriteString(truncateRunes(bodyText, 3000))
	builder.WriteString("\n\n请按要求输出 JSON。")
	return builder.String()
}

// visibleText 去标签、去脚本样式后的可见文本，用于喂模型。
func visibleText(body []byte) string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return stripHTMLTags(string(body))
	}
	bodyNode := findFirstNode(doc, "body")
	if bodyNode == nil {
		bodyNode = doc
	}
	return truncateRunes(stripHTMLTags(nodeText(bodyNode)), 6000)
}

// llmProposeSiteSelectors L4：把 L3 的结果反推成可复用选择器（Agent 只在首爬/失效时被唤醒）。
func (a *App) llmProposeSiteSelectors(ctx context.Context, provider Provider, page crawlPage, doc *html.Node, candidate crawlCandidate) (map[string][]string, error) {
	if provider.ID == "" {
		return nil, errors.New("没有可用的文本模型")
	}
	if doc == nil {
		return nil, errors.New("缺少可解析的 DOM")
	}

	var builder strings.Builder
	builder.WriteString("页面 URL: ")
	builder.WriteString(page.URL)
	builder.WriteString("\n已确认字段:\n- title: ")
	builder.WriteString(candidate.Title)
	builder.WriteString("\n- description: ")
	builder.WriteString(truncateRunes(candidate.Description, 300))
	builder.WriteString("\n- thumbnail: ")
	builder.WriteString(candidate.Thumbnail)
	builder.WriteString("\n\nHTML 片段:\n")
	builder.WriteString(truncateRunes(compactHTML(page.Body), 12000))
	builder.WriteString("\n\n请输出选择器 JSON。")

	raw, err := a.chatCompletion(ctx, provider, crawlSelectorSystemPrompt, builder.String())
	if err != nil {
		return nil, err
	}
	var payload map[string][]string
	if err := json.Unmarshal([]byte(extractJSONBlock(raw)), &payload); err != nil {
		return nil, fmt.Errorf("规则编译结果不是合法 JSON: %w", err)
	}

	// 本地 dry-run：选择器必须真实命中刚才确认的字段，否则不入库（§6 L4）
	validated := make(map[string][]string, 3)
	expected := map[string]string{
		"title":       candidate.Title,
		"description": candidate.Description,
		"thumbnail":   candidate.Thumbnail,
	}
	for field, selectors := range payload {
		expect, ok := expected[field]
		if !ok || strings.TrimSpace(expect) == "" {
			continue
		}
		for _, selector := range selectors {
			selector = strings.TrimSpace(selector)
			if selector == "" || len(validated[field]) >= 3 {
				continue
			}
			if selectorHits(doc, selector, expect) {
				validated[field] = append(validated[field], selector)
			}
		}
	}
	if len(validated["title"]) == 0 {
		return nil, errors.New("规则 dry-run 未通过：标题选择器没有命中预期文本")
	}
	return validated, nil
}

// compactHTML 压掉多余空白，控制提示词体积。
func compactHTML(body []byte) string {
	text := string(body)
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(strings.Join(out, "\n")) > 20000 {
			break
		}
	}
	return strings.Join(out, "\n")
}
