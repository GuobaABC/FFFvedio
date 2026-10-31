package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const crawlTaskColumns = `id, title, stage, status, error, config_json, context_json, created_at, updated_at`

func scanCrawlTask(scanner interface{ Scan(dest ...any) error }) (CrawlTask, error) {
	var task CrawlTask
	var configJSON, contextJSON string
	if err := scanner.Scan(
		&task.ID, &task.Title, &task.Stage, &task.Status, &task.Error,
		&configJSON, &contextJSON, &task.CreatedAt, &task.UpdatedAt,
	); err != nil {
		return task, err
	}
	if err := json.Unmarshal([]byte(configJSON), &task.Config); err != nil {
		return task, fmt.Errorf("解析爬取任务配置失败: %w", err)
	}
	if err := json.Unmarshal([]byte(contextJSON), &task.Run); err != nil {
		return task, fmt.Errorf("解析爬取任务产物失败: %w", err)
	}
	if task.Run.Stats.TierHits == nil {
		task.Run.Stats.TierHits = make(map[string]int, 4)
	}
	return task, nil
}

// ListCrawlTasks 返回全部爬取任务（最新的在前），「爬取结果」直接消费。
func (a *App) ListCrawlTasks() ([]CrawlTask, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	rows, err := a.db.Query(`SELECT ` + crawlTaskColumns + ` FROM crawl_tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("读取爬取任务列表失败: %w", err)
	}
	defer rows.Close()

	tasks := make([]CrawlTask, 0, 8)
	for rows.Next() {
		task, err := scanCrawlTask(rows)
		if err != nil {
			return nil, fmt.Errorf("解析爬取任务失败: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历爬取任务失败: %w", err)
	}
	return tasks, nil
}

// GetCrawlTask 读取单个任务，前端轮询执行进度时调用。
func (a *App) GetCrawlTask(id string) (CrawlTask, error) {
	return a.crawlTaskByID(id)
}

func (a *App) crawlTaskByID(id string) (CrawlTask, error) {
	if a.db == nil {
		return CrawlTask{}, errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return CrawlTask{}, errors.New("任务 ID 不能为空")
	}
	row := a.db.QueryRow(`SELECT `+crawlTaskColumns+` FROM crawl_tasks WHERE id = ?`, id)
	task, err := scanCrawlTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CrawlTask{}, errors.New("爬取任务不存在")
	}
	if err != nil {
		return CrawlTask{}, fmt.Errorf("读取爬取任务失败: %w", err)
	}
	return task, nil
}

func (a *App) insertCrawlTask(task CrawlTask) error {
	configJSON, err := encodeVideoJSON(task.Config)
	if err != nil {
		return err
	}
	contextJSON, err := encodeVideoJSON(task.Run)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(
		`INSERT INTO crawl_tasks (`+crawlTaskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.Title, task.Stage, task.Status, task.Error, configJSON, contextJSON, task.CreatedAt, task.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("创建爬取任务失败: %w", err)
	}
	return nil
}

// updateCrawlTaskRecord 持久化阶段/状态/产物，前端靠它看到渐进式进度。
func (a *App) updateCrawlTaskRecord(task CrawlTask) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	configJSON, err := encodeVideoJSON(task.Config)
	if err != nil {
		return err
	}
	contextJSON, err := encodeVideoJSON(task.Run)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(
		`UPDATE crawl_tasks SET title = ?, stage = ?, status = ?, error = ?, config_json = ?, context_json = ?, updated_at = ?
		 WHERE id = ?`,
		task.Title, task.Stage, task.Status, task.Error, configJSON, contextJSON, task.UpdatedAt, task.ID,
	)
	if err != nil {
		return fmt.Errorf("更新爬取任务失败: %w", err)
	}
	return nil
}

// DeleteCrawlTask 只删任务记录，条目库与站点画像保留（越跑越准）。
func (a *App) DeleteCrawlTask(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	if a.isCrawlTaskRunning(id) {
		return errors.New("任务正在执行，无法删除")
	}
	result, err := a.db.Exec(`DELETE FROM crawl_tasks WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("删除爬取任务失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除爬取任务失败: %w", err)
	}
	if affected == 0 {
		return errors.New("要删除的爬取任务不存在")
	}
	return nil
}

/* ---------------- 条目库：video_item ---------------- */

func (a *App) saveCrawlItem(item VideoItem) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	payload, err := encodeVideoJSON(item)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(
		`INSERT INTO crawl_items (id, site_key, canonical_url, original_url, title, description, thumbnail_url, payload_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   original_url = excluded.original_url,
		   title = excluded.title,
		   description = excluded.description,
		   thumbnail_url = excluded.thumbnail_url,
		   payload_json = excluded.payload_json,
		   updated_at = excluded.updated_at`,
		item.ID, item.SiteKey, item.CanonicalURL, item.OriginalURL, item.Title, item.Description,
		item.Thumbnail.URL, payload, item.CreatedAt, item.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("保存爬取条目失败: %w", err)
	}
	return nil
}

// cachedCrawlItem 同 URL 命中缓存时直接复用（默认 30 天，§12 第 8 条）。
func (a *App) cachedCrawlItem(itemID string, maxAge time.Duration) (VideoItem, bool) {
	if a.db == nil || itemID == "" {
		return VideoItem{}, false
	}
	var payload, updatedAt string
	err := a.db.QueryRow(`SELECT payload_json, updated_at FROM crawl_items WHERE id = ?`, itemID).Scan(&payload, &updatedAt)
	if err != nil {
		return VideoItem{}, false
	}
	updated, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil || time.Since(updated) > maxAge {
		return VideoItem{}, false
	}
	var item VideoItem
	if err := json.Unmarshal([]byte(payload), &item); err != nil {
		return VideoItem{}, false
	}
	return item, true
}

// ListCrawlItems 素材库：跨任务复用已入库的 VideoItem。
func (a *App) ListCrawlItems(limit int) ([]VideoItem, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := a.db.Query(`SELECT payload_json FROM crawl_items ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("读取爬取条目失败: %w", err)
	}
	defer rows.Close()

	items := make([]VideoItem, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("读取爬取条目失败: %w", err)
		}
		var item VideoItem
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历爬取条目失败: %w", err)
	}
	return items, nil
}

/* ---------------- 站点画像：site_profiles ---------------- */

func (a *App) siteProfileByKey(siteKey string) (*SiteProfile, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	var profile SiteProfile
	var selectorsJSON, cleanersJSON string
	var requiresRender, enabled, autoGenerated int
	err := a.db.QueryRow(
		`SELECT site_key, host_pattern, requires_render, selectors_json, cleaners_json,
		        hit_success, hit_fail, last_verified_at, enabled, auto_generated, review_status, updated_at
		 FROM site_profiles WHERE site_key = ?`, siteKey,
	).Scan(
		&profile.SiteKey, &profile.HostPattern, &requiresRender, &selectorsJSON, &cleanersJSON,
		&profile.HitSuccess, &profile.HitFail, &profile.LastVerifiedAt, &enabled, &autoGenerated,
		&profile.ReviewStatus, &profile.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取站点画像失败: %w", err)
	}
	profile.RequiresRender = requiresRender != 0
	profile.Enabled = enabled != 0
	profile.AutoGenerated = autoGenerated != 0
	if err := json.Unmarshal([]byte(selectorsJSON), &profile.Selectors); err != nil {
		profile.Selectors = map[string][]string{}
	}
	if err := json.Unmarshal([]byte(cleanersJSON), &profile.Cleaners); err != nil {
		profile.Cleaners = nil
	}
	return &profile, nil
}

func (a *App) upsertSiteProfile(profile SiteProfile) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	selectors, err := json.Marshal(profile.Selectors)
	if err != nil {
		return fmt.Errorf("序列化站点规则失败: %w", err)
	}
	cleaners, err := json.Marshal(profile.Cleaners)
	if err != nil {
		return fmt.Errorf("序列化清洗规则失败: %w", err)
	}
	if profile.ReviewStatus == "" {
		profile.ReviewStatus = "auto-approved"
	}
	_, err = a.db.Exec(
		`INSERT INTO site_profiles (site_key, host_pattern, requires_render, selectors_json, cleaners_json,
		   hit_success, hit_fail, last_verified_at, enabled, auto_generated, review_status, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(site_key) DO UPDATE SET
		   host_pattern = excluded.host_pattern,
		   requires_render = excluded.requires_render,
		   selectors_json = excluded.selectors_json,
		   cleaners_json = excluded.cleaners_json,
		   last_verified_at = excluded.last_verified_at,
		   enabled = excluded.enabled,
		   auto_generated = excluded.auto_generated,
		   review_status = excluded.review_status,
		   updated_at = excluded.updated_at`,
		profile.SiteKey, profile.HostPattern, boolToInt(profile.RequiresRender), string(selectors), string(cleaners),
		profile.HitSuccess, profile.HitFail, profile.LastVerifiedAt, boolToInt(profile.Enabled),
		boolToInt(profile.AutoGenerated), profile.ReviewStatus, profile.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("保存站点规则失败: %w", err)
	}
	return nil
}

// recordSiteProfileHit 命中/失手都回写统计；连续失手由前端提示人工清理（§6 L4 TTL）。
func (a *App) recordSiteProfileHit(siteKey string, success bool) {
	profile, err := a.siteProfileByKey(siteKey)
	if err != nil || profile == nil {
		return
	}
	if success {
		profile.HitSuccess++
		profile.LastVerifiedAt = time.Now().Format(time.RFC3339)
	} else {
		profile.HitFail++
	}
	// 连续 3 次失手自动禁用，把该 host 拉回 L3 通道（§6 L4）
	if profile.HitFail >= 3 && profile.HitSuccess == 0 {
		profile.Enabled = false
		profile.ReviewStatus = "stale"
	}
	profile.UpdatedAt = time.Now().Format(time.RFC3339)
	// 直接 UPDATE 统计列：upsertSiteProfile 的冲突分支不覆盖计数器，避免互相抹平。
	if _, err := a.db.Exec(
		`UPDATE site_profiles SET hit_success = ?, hit_fail = ?, last_verified_at = ?, enabled = ?, review_status = ?, updated_at = ?
		 WHERE site_key = ?`,
		profile.HitSuccess, profile.HitFail, profile.LastVerifiedAt, boolToInt(profile.Enabled),
		profile.ReviewStatus, profile.UpdatedAt, profile.SiteKey,
	); err != nil {
		return
	}
}

// ListSiteProfiles 站点画像列表：证明“越跑越准”的可观测面。
func (a *App) ListSiteProfiles() ([]SiteProfile, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	rows, err := a.db.Query(
		`SELECT site_key, host_pattern, requires_render, selectors_json, cleaners_json,
		        hit_success, hit_fail, last_verified_at, enabled, auto_generated, review_status, updated_at
		 FROM site_profiles ORDER BY hit_success DESC, updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("读取站点画像失败: %w", err)
	}
	defer rows.Close()

	profiles := make([]SiteProfile, 0, 8)
	for rows.Next() {
		var profile SiteProfile
		var selectorsJSON, cleanersJSON string
		var requiresRender, enabled, autoGenerated int
		if err := rows.Scan(
			&profile.SiteKey, &profile.HostPattern, &requiresRender, &selectorsJSON, &cleanersJSON,
			&profile.HitSuccess, &profile.HitFail, &profile.LastVerifiedAt, &enabled, &autoGenerated,
			&profile.ReviewStatus, &profile.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("解析站点画像失败: %w", err)
		}
		profile.RequiresRender = requiresRender != 0
		profile.Enabled = enabled != 0
		profile.AutoGenerated = autoGenerated != 0
		_ = json.Unmarshal([]byte(selectorsJSON), &profile.Selectors)
		_ = json.Unmarshal([]byte(cleanersJSON), &profile.Cleaners)
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历站点画像失败: %w", err)
	}
	return profiles, nil
}

// DeleteSiteProfile 手动回收失效规则。
func (a *App) DeleteSiteProfile(siteKey string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	siteKey = strings.TrimSpace(siteKey)
	if siteKey == "" {
		return errors.New("站点标识不能为空")
	}
	if _, err := a.db.Exec(`DELETE FROM site_profiles WHERE site_key = ?`, siteKey); err != nil {
		return fmt.Errorf("删除站点规则失败: %w", err)
	}
	return nil
}

/* ---------------- 审计日志：crawl_audit ---------------- */

func (a *App) appendCrawlAudit(taskID string, result CrawlResult) {
	if a.db == nil {
		return
	}
	for _, step := range result.Steps {
		_, err := a.db.Exec(
			`INSERT INTO crawl_audit (task_id, item_id, url, step, tier, latency_ms, status, err_code, note, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, result.ItemID, result.CanonicalURL, step.Step, step.Tier, step.LatencyMS,
			step.Status, result.ErrorCode, step.Note, step.At,
		)
		if err != nil {
			return
		}
	}
}
