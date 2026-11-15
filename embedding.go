package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const embeddingTimeout = 45 * time.Second

// embeddingEndpoint 复用文本提供商的 OpenAI 兼容 /embeddings 端点。
// 大多数厂商与 chat/completions 同根：<base>/embeddings 或 <base>/v1/embeddings。
func embeddingEndpoint(baseURL string) string {
	base := apiRoot(baseURL)
	if base == "" {
		return ""
	}
	if hasVersionSegment(base) {
		return base + "/embeddings"
	}
	return base + "/v1/embeddings"
}

// embedTexts 调用外部 embedding 接口；失败时由调用方降级为纯关键词检索。
func (a *App) embedTexts(provider Provider, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if resolveLLMDialect(provider) != DialectOpenAI {
		return nil, errors.New("当前文本提供商不是 OpenAI 兼容协议，暂不支持 embedding")
	}
	endpoint := embeddingEndpoint(provider.BaseURL)
	if endpoint == "" {
		return nil, errors.New("embedding 提供商缺少 Base URL")
	}
	payload, err := json.Marshal(map[string]any{
		"model": provider.Model,
		"input": inputs,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(provider.APIKey))
	client := &http.Client{Timeout: embeddingTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding 请求失败（HTTP %d）：%s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	var parsed struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析 embedding 响应失败: %w", err)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding 返回条数不匹配：期望 %d，实际 %d", len(inputs), len(parsed.Data))
	}
	vectors := make([][]float32, len(parsed.Data))
	for index, item := range parsed.Data {
		vector := make([]float32, len(item.Embedding))
		for i, value := range item.Embedding {
			vector[i] = float32(value)
		}
		vectors[index] = vector
	}
	return vectors, nil
}
