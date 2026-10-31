package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 「设置 - 提供商」的三个分类，与前端 ProviderCategory 保持一致。
const (
	CategoryText          = "text"
	CategoryMultimodal    = "multimodal"
	CategoryMultimodalGen = "multimodal_gen"
)

// Provider 是用户登记在本地的模型服务商配置。
type Provider struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Name     string `json:"name"`
	BaseURL  string `json:"baseUrl"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	// Dialect 是请求方言：auto（按域名推断）/ openai / anthropic / gemini。
	Dialect   string `json:"dialect"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func validCategory(category string) bool {
	switch category {
	case CategoryText, CategoryMultimodal, CategoryMultimodalGen:
		return true
	}
	return false
}

func newProviderID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

// ListProviders 返回全部提供商配置，按分类、创建时间排序。
func (a *App) ListProviders() ([]Provider, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}

	rows, err := a.db.Query(
		`SELECT id, category, name, base_url, api_key, model, dialect, created_at, updated_at
		 FROM providers ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("读取提供商列表失败: %w", err)
	}
	defer rows.Close()

	providers := make([]Provider, 0, 8)
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Category, &p.Name, &p.BaseURL, &p.APIKey, &p.Model, &p.Dialect, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("解析提供商记录失败: %w", err)
		}
		providers = append(providers, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历提供商记录失败: %w", err)
	}
	return providers, nil
}

// SaveProvider 新增（ID 为空）或更新一条提供商配置，返回落库后的完整记录。
func (a *App) SaveProvider(input Provider) (Provider, error) {
	if a.db == nil {
		return Provider{}, errors.New("数据库未初始化")
	}

	input.Category = strings.TrimSpace(input.Category)
	input.Name = strings.TrimSpace(input.Name)
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.APIKey = strings.TrimSpace(input.APIKey)
	input.Model = strings.TrimSpace(input.Model)
	input.Dialect = strings.ToLower(strings.TrimSpace(input.Dialect))

	// 允许直接粘贴具体接口地址（.../chat/completions、.../responses），落库前收敛到 API 根，
	// 否则后续请求会拼成 .../responses/v1/chat/completions 这类不存在的路径。
	input.BaseURL = apiRoot(input.BaseURL)

	if !validCategory(input.Category) {
		return Provider{}, errors.New("提供商分类不合法")
	}
	if input.Dialect == "" {
		input.Dialect = DialectAuto
	}
	if !validDialect(input.Dialect) {
		return Provider{}, errors.New("请求方言需为 auto / openai / anthropic / gemini 之一")
	}
	if input.BaseURL == "" {
		return Provider{}, errors.New("Base URL 不能为空")
	}
	parsed, err := url.Parse(input.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Provider{}, errors.New("Base URL 需为 http(s) 开头的完整地址")
	}
	if input.APIKey == "" {
		return Provider{}, errors.New("API Key 不能为空")
	}
	if input.Model == "" {
		return Provider{}, errors.New("模型名称不能为空")
	}
	if input.Name == "" {
		input.Name = parsed.Host
	}

	now := time.Now().Format(time.RFC3339)
	if input.ID == "" {
		input.ID = newProviderID()
		input.CreatedAt = now
		input.UpdatedAt = now
		_, err = a.db.Exec(
			`INSERT INTO providers (id, category, name, base_url, api_key, model, dialect, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			input.ID, input.Category, input.Name, input.BaseURL, input.APIKey, input.Model, input.Dialect, input.CreatedAt, input.UpdatedAt,
		)
		if err != nil {
			return Provider{}, fmt.Errorf("保存提供商失败: %w", err)
		}
		return input, nil
	}

	result, err := a.db.Exec(
		`UPDATE providers SET category = ?, name = ?, base_url = ?, api_key = ?, model = ?, dialect = ?, updated_at = ?
		 WHERE id = ?`,
		input.Category, input.Name, input.BaseURL, input.APIKey, input.Model, input.Dialect, now, input.ID,
	)
	if err != nil {
		return Provider{}, fmt.Errorf("更新提供商失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Provider{}, fmt.Errorf("更新提供商失败: %w", err)
	}
	if affected == 0 {
		return Provider{}, errors.New("要更新的提供商不存在")
	}

	saved, err := a.providerByID(input.ID)
	if err != nil {
		return Provider{}, err
	}
	return saved, nil
}

// DeleteProvider 删除指定提供商配置。
func (a *App) DeleteProvider(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("提供商 ID 不能为空")
	}
	if _, err := a.db.Exec(`DELETE FROM providers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除提供商失败: %w", err)
	}
	return nil
}

func (a *App) providerByID(id string) (Provider, error) {
	var p Provider
	err := a.db.QueryRow(
		`SELECT id, category, name, base_url, api_key, model, dialect, created_at, updated_at
		 FROM providers WHERE id = ?`, id,
	).Scan(&p.ID, &p.Category, &p.Name, &p.BaseURL, &p.APIKey, &p.Model, &p.Dialect, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, errors.New("提供商不存在")
	}
	if err != nil {
		return Provider{}, fmt.Errorf("读取提供商失败: %w", err)
	}
	return p, nil
}
