package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// openDatabase 打开（不存在则创建）本地 SQLite 数据库，并返回数据库路径。
// 位置：<用户配置目录>/FFFVedioAgent/FFFVedioAgent.db
// Windows 为 %AppData%\FFFVedioAgent，macOS 为 ~/Library/Application Support/FFFVedioAgent。
func openDatabase() (*sql.DB, string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}

	appDir := filepath.Join(configDir, appName)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("创建数据目录失败: %w", err)
	}

	dbPath := filepath.Join(appDir, appName+".db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, dbPath, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 桌面端单进程单连接即可满足需求，避免并发写入时的锁竞争
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, dbPath, fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, dbPath, fmt.Errorf("开启 WAL 失败: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, dbPath, err
	}

	return db, dbPath, nil
}

// migrate 建立当前版本需要的表结构（幂等）。
func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS providers (
	id         TEXT PRIMARY KEY,
	category   TEXT NOT NULL,
	name       TEXT NOT NULL,
	base_url   TEXT NOT NULL,
	api_key    TEXT NOT NULL,
	model      TEXT NOT NULL,
	dialect    TEXT NOT NULL DEFAULT 'auto',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_providers_category ON providers (category, created_at);

-- 视频生成任务：config_json 存输入契约，context_json 存 RunContext（状态机 + 产物寻址）
-- template_id 记录任务引用的模板，用于模板使用次数与变体轮换
CREATE TABLE IF NOT EXISTS video_tasks (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL,
	stage        TEXT NOT NULL,
	status       TEXT NOT NULL,
	error        TEXT NOT NULL DEFAULT '',
	config_json  TEXT NOT NULL,
	context_json TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL,
	template_id  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_video_tasks_created ON video_tasks (created_at);

-- 视频模板：payload_json 存完整模板，name/scope/tags 供模板库检索
CREATE TABLE IF NOT EXISTS video_templates (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	version      TEXT NOT NULL,
	scope        TEXT NOT NULL,
	tags_json    TEXT NOT NULL DEFAULT '[]',
	payload_json TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_templates_scope ON video_templates (scope, updated_at);

-- 热点爬取任务：config_json 存输入契约，context_json 存 URL 结果/条目/统计
CREATE TABLE IF NOT EXISTS crawl_tasks (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL,
	stage        TEXT NOT NULL,
	status       TEXT NOT NULL,
	error        TEXT NOT NULL DEFAULT '',
	config_json  TEXT NOT NULL,
	context_json TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_crawl_tasks_created ON crawl_tasks (created_at);

-- video_item 条目库：主键 = sha1(canonical_url) 前缀，天然幂等
CREATE TABLE IF NOT EXISTS crawl_items (
	id            TEXT PRIMARY KEY,
	site_key      TEXT NOT NULL,
	canonical_url TEXT NOT NULL,
	original_url  TEXT NOT NULL,
	title         TEXT NOT NULL DEFAULT '',
	description   TEXT NOT NULL DEFAULT '',
	thumbnail_url TEXT NOT NULL DEFAULT '',
	payload_json  TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_crawl_items_canonical ON crawl_items (canonical_url);
CREATE INDEX IF NOT EXISTS idx_crawl_items_updated ON crawl_items (updated_at);

-- site_profile 站点画像：抽取规则沉淀，加站点只加配置
CREATE TABLE IF NOT EXISTS site_profiles (
	site_key         TEXT PRIMARY KEY,
	host_pattern     TEXT NOT NULL DEFAULT '',
	requires_render  INTEGER NOT NULL DEFAULT 0,
	selectors_json   TEXT NOT NULL DEFAULT '{}',
	cleaners_json    TEXT NOT NULL DEFAULT '[]',
	hit_success      INTEGER NOT NULL DEFAULT 0,
	hit_fail         INTEGER NOT NULL DEFAULT 0,
	last_verified_at TEXT NOT NULL DEFAULT '',
	enabled          INTEGER NOT NULL DEFAULT 1,
	auto_generated   INTEGER NOT NULL DEFAULT 0,
	review_status    TEXT NOT NULL DEFAULT 'auto-approved',
	updated_at       TEXT NOT NULL
);

-- audit_log：每一步落审计，便于回归与排查
CREATE TABLE IF NOT EXISTS crawl_audit (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    TEXT NOT NULL,
	item_id    TEXT NOT NULL DEFAULT '',
	url        TEXT NOT NULL,
	step       TEXT NOT NULL,
	tier       TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT '',
	err_code   TEXT NOT NULL DEFAULT '',
	note       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_crawl_audit_task ON crawl_audit (task_id, id);

-- 尾帧资产：任务最后一段的尾帧图片，供其他任务选作首帧；每任务保留一条（重生成后覆盖）
CREATE TABLE IF NOT EXISTS tail_frame_assets (
	id         TEXT PRIMARY KEY,
	task_id    TEXT NOT NULL,
	task_title TEXT NOT NULL DEFAULT '',
	segment_id TEXT NOT NULL DEFAULT '',
	image_path TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tail_frame_assets_task ON tail_frame_assets (task_id, created_at);

-- 视频项目：任务归组容器，从项目里看任务与尾帧
CREATE TABLE IF NOT EXISTS video_projects (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_projects_created ON video_projects (created_at);

-- 用量流水：文本调用记 token，视频渲染记片段与秒数，供首页大屏聚合
-- reported = 0 表示服务商没返回 usage 字段（只计次，不计量）
CREATE TABLE IF NOT EXISTS usage_events (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at     TEXT NOT NULL,
	kind           TEXT NOT NULL,
	purpose        TEXT NOT NULL DEFAULT '',
	provider_name  TEXT NOT NULL DEFAULT '',
	model          TEXT NOT NULL DEFAULT '',
	dialect        TEXT NOT NULL DEFAULT '',
	prompt_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens  INTEGER NOT NULL DEFAULT 0,
	total_tokens   INTEGER NOT NULL DEFAULT 0,
	clips          INTEGER NOT NULL DEFAULT 0,
	seconds        REAL NOT NULL DEFAULT 0,
	latency_ms     INTEGER NOT NULL DEFAULT 0,
	failed         INTEGER NOT NULL DEFAULT 0,
	reported       INTEGER NOT NULL DEFAULT 0,
	task_id        TEXT NOT NULL DEFAULT '',
	reference_text TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_events_created ON usage_events (created_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_purpose ON usage_events (purpose, created_at);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("初始化数据表失败: %w", err)
	}

	// 老库升级：video_tasks 追加 template_id（已存在时忽略重复列错误）
	if _, err := db.Exec(`ALTER TABLE video_tasks ADD COLUMN template_id TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("升级 video_tasks 表失败: %w", err)
	}

	// 老库升级：providers 追加 dialect（请求方言，默认 auto 按域名推断）
	if _, err := db.Exec(`ALTER TABLE providers ADD COLUMN dialect TEXT NOT NULL DEFAULT 'auto'`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("升级 providers 表失败: %w", err)
	}

	// 老库升级：video_tasks 追加 project_id（空串 = 未归类，不强制归属）
	if _, err := db.Exec(`ALTER TABLE video_tasks ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("升级 video_tasks 表失败: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_video_tasks_project ON video_tasks (project_id)`); err != nil {
		return fmt.Errorf("创建项目索引失败: %w", err)
	}

	return seedBuiltinVideoTemplates(db)
}
