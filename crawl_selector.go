package main

import (
	"errors"
	"strings"

	"golang.org/x/net/html"
)

// 站点画像里的选择器只覆盖「够用」的子集：
//
//	meta[property='og:title']@content
//	h1.video-title::text
//	#app .play-info .title::text
//	div.cover > img@src
//
// 这是有意的：把复杂度限制在可 dry-run、可回归的范围内。
type selectorAttr struct {
	name  string
	value string
}

type selectorStep struct {
	tag     string
	id      string
	classes []string
	attrs   []selectorAttr
}

type parsedSelector struct {
	steps       []selectorStep
	combinators []string // 与 steps 同长，首项为空
	attr        string
	wantText    bool
}

func parseSelector(raw string) (parsedSelector, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return parsedSelector{}, errors.New("选择器为空")
	}
	parsed := parsedSelector{combinators: []string{""}}

	if idx := strings.LastIndex(value, "::text"); idx >= 0 {
		parsed.wantText = true
		value = strings.TrimSpace(value[:idx])
	} else if idx := strings.LastIndex(value, "@"); idx >= 0 {
		parsed.attr = strings.TrimSpace(value[idx+1:])
		value = strings.TrimSpace(value[:idx])
	} else {
		parsed.wantText = true
	}
	if parsed.attr == "" && !parsed.wantText {
		parsed.wantText = true
	}

	tokens := tokenizeSelector(value)
	if len(tokens) == 0 {
		return parsedSelector{}, errors.New("选择器缺少节点")
	}
	for _, token := range tokens {
		if token == ">" {
			parsed.combinators[len(parsed.combinators)-1] = "child"
			continue
		}
		step, err := parseSelectorStep(token)
		if err != nil {
			return parsedSelector{}, err
		}
		parsed.steps = append(parsed.steps, step)
		parsed.combinators = append(parsed.combinators, "")
	}
	// combinators 长度 = steps+1，最后一项无用
	parsed.combinators = parsed.combinators[:len(parsed.steps)]
	return parsed, nil
}

func tokenizeSelector(value string) []string {
	tokens := make([]string, 0, 4)
	var builder strings.Builder
	depth := 0
	var quote rune
	flush := func() {
		if builder.Len() > 0 {
			tokens = append(tokens, builder.String())
			builder.Reset()
		}
	}
	for _, char := range value {
		if quote != 0 {
			builder.WriteRune(char)
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			builder.WriteRune(char)
		case '[':
			depth++
			builder.WriteRune(char)
		case ']':
			if depth > 0 {
				depth--
			}
			builder.WriteRune(char)
		case ' ', '\t', '\n':
			if depth == 0 {
				flush()
				continue
			}
			builder.WriteRune(char)
		case '>':
			if depth == 0 {
				flush()
				tokens = append(tokens, ">")
				continue
			}
			builder.WriteRune(char)
		default:
			builder.WriteRune(char)
		}
	}
	flush()
	return tokens
}

func parseSelectorStep(token string) (selectorStep, error) {
	step := selectorStep{}
	index := 0
	// tag
	for index < len(token) && token[index] != '.' && token[index] != '#' && token[index] != '[' {
		index++
	}
	step.tag = strings.ToLower(token[:index])
	if step.tag == "" {
		step.tag = "*"
	}

	for index < len(token) {
		switch token[index] {
		case '.':
			index++
			start := index
			for index < len(token) && token[index] != '.' && token[index] != '#' && token[index] != '[' {
				index++
			}
			if start == index {
				return step, errors.New("类名为空")
			}
			step.classes = append(step.classes, token[start:index])
		case '#':
			index++
			start := index
			for index < len(token) && token[index] != '.' && token[index] != '#' && token[index] != '[' {
				index++
			}
			if start == index {
				return step, errors.New("id 为空")
			}
			step.id = token[start:index]
		case '[':
			end := strings.Index(token[index:], "]")
			if end < 0 {
				return step, errors.New("属性选择器未闭合")
			}
			body := token[index+1 : index+end]
			index += end + 1
			name, value := body, ""
			if eq := strings.Index(body, "="); eq >= 0 {
				name = body[:eq]
				value = strings.Trim(body[eq+1:], ` "'`)
			}
			step.attrs = append(step.attrs, selectorAttr{name: strings.ToLower(strings.TrimSpace(name)), value: value})
		default:
			return step, errors.New("无法识别的选择器片段: " + token)
		}
	}
	return step, nil
}

func (s selectorStep) matches(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	if s.tag != "*" && s.tag != "" && strings.ToLower(node.Data) != s.tag {
		return false
	}
	if s.id != "" && attrOf(node, "id") != s.id {
		return false
	}
	if len(s.classes) > 0 {
		classText := attrOf(node, "class")
		for _, class := range s.classes {
			if !hasClass(classText, class) {
				return false
			}
		}
	}
	for _, attr := range s.attrs {
		actual := attrOf(node, attr.name)
		if attr.value == "" {
			if actual == "" {
				return false
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(actual), attr.value) {
			return false
		}
	}
	return true
}

func hasClass(classText, class string) bool {
	for _, item := range strings.Fields(classText) {
		if item == class {
			return true
		}
	}
	return false
}

// apply 在文档上执行选择器，返回命中的元素节点。
func (p parsedSelector) apply(root *html.Node) []*html.Node {
	current := []*html.Node{root}
	for index, step := range p.steps {
		combinator := "desc"
		if index > 0 {
			combinator = p.combinators[index-1]
		}
		next := make([]*html.Node, 0, 4)
		for _, node := range current {
			if combinator == "child" {
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					if step.matches(child) {
						next = append(next, child)
					}
				}
				continue
			}
			walkElements(node, func(candidate *html.Node) {
				if step.matches(candidate) {
					next = append(next, candidate)
				}
			})
		}
		if len(next) == 0 {
			return nil
		}
		current = next
	}
	return current
}

// value 取值：@attr 取属性，否则取文本。
func (p parsedSelector) value(node *html.Node) string {
	if p.wantText || p.attr == "" || p.attr == "text" {
		return strings.TrimSpace(nodeText(node))
	}
	return strings.TrimSpace(attrOf(node, p.attr))
}

// selectFirstValue 按顺序尝试多个选择器，返回第一个非空值。
func selectFirstValue(root *html.Node, selectors []string) string {
	for _, raw := range selectors {
		parsed, err := parseSelector(raw)
		if err != nil {
			continue
		}
		for _, node := range parsed.apply(root) {
			if value := parsed.value(node); value != "" {
				return value
			}
		}
	}
	return ""
}

// selectorHits 用于 L4 dry-run：选择器必须真实命中预期文本。
func selectorHits(root *html.Node, raw, expected string) bool {
	parsed, err := parseSelector(raw)
	if err != nil {
		return false
	}
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return false
	}
	for _, node := range parsed.apply(root) {
		actual := parsed.value(node)
		if actual == "" {
			continue
		}
		if actual == expected || strings.Contains(actual, expected) || strings.Contains(expected, actual) {
			return true
		}
	}
	return false
}

func walkElements(node *html.Node, visit func(*html.Node)) {
	if node == nil {
		return
	}
	if node.Type == html.ElementNode {
		visit(node)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkElements(child, visit)
	}
}

func nodeText(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current == nil {
			return
		}
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
			builder.WriteRune(' ')
		}
		if current.Type == html.ElementNode && (current.Data == "script" || current.Data == "style") {
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(builder.String()), " ")
}

// rawText 取元素内的原始文本（script/style 也返回内容，且保留空白），
// JSON-LD 与内联状态必须用原始文本，不能走 nodeText 的折叠逻辑。
func rawText(node *html.Node) string {
	if node == nil {
		return ""
	}
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current == nil {
			return
		}
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func attrOf(node *html.Node, name string) string {
	if node == nil {
		return ""
	}
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, name) {
			return attr.Val
		}
	}
	return ""
}

func findFirstNode(root *html.Node, tag string) *html.Node {
	var found *html.Node
	walkElements(root, func(node *html.Node) {
		if found != nil {
			return
		}
		if strings.EqualFold(node.Data, tag) {
			found = node
		}
	})
	return found
}

func findAllNodes(root *html.Node, tag string) []*html.Node {
	out := make([]*html.Node, 0, 8)
	walkElements(root, func(node *html.Node) {
		if strings.EqualFold(node.Data, tag) {
			out = append(out, node)
		}
	})
	return out
}
