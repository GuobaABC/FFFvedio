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
	template_id  TEXT NOT NULL DEFAULT '',
	episode      INTEGER NOT NULL DEFAULT 0
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

-- 视频角色库：跨任务复用的角色档案（外观锚点 / 人设 / 负向约束 / 音色 / 默认定妆图）
CREATE TABLE IF NOT EXISTS video_characters (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	tags_json    TEXT NOT NULL DEFAULT '[]',
	payload_json TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_characters_updated ON video_characters (updated_at);

-- 项目记忆：短剧跨集复用的角色/剧情/世界设定；kind 区分 character/plot/event/relationship/world/summary
CREATE TABLE IF NOT EXISTS project_memory (
	id           TEXT PRIMARY KEY,
	project_id   TEXT NOT NULL,
	episode      INTEGER NOT NULL DEFAULT 0,
	kind         TEXT NOT NULL,
	title        TEXT NOT NULL DEFAULT '',
	content      TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL DEFAULT '{}',
	source_task  TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_project_memory_project ON project_memory (project_id, kind, episode);

-- 关键词检索：trigram 对中文子串匹配比 unicode61 更友好；旧 SQLite 不支持时会退回 unicode61
CREATE VIRTUAL TABLE IF NOT EXISTS project_memory_fts USING fts5(
	title, content, kind, content='project_memory', content_rowid='rowid', tokenize='trigram'
);

-- 语义检索：复用文本提供商的 /embeddings 接口，向量以 float32 小端 BLOB 存储
CREATE TABLE IF NOT EXISTS project_memory_embedding (
	memory_id  TEXT PRIMARY KEY,
	dim        INTEGER NOT NULL,
	vector     BLOB NOT NULL,
	model      TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);

CREATE TRIGGER IF NOT EXISTS project_memory_ai AFTER INSERT ON project_memory BEGIN
	INSERT INTO project_memory_fts(rowid, title, content, kind) VALUES (new.rowid, new.title, new.content, new.kind);
END;
CREATE TRIGGER IF NOT EXISTS project_memory_ad AFTER DELETE ON project_memory BEGIN
	INSERT INTO project_memory_fts(project_memory_fts, rowid, title, content, kind) VALUES ('delete', old.rowid, old.title, old.content, old.kind);
END;
CREATE TRIGGER IF NOT EXISTS project_memory_au AFTER UPDATE ON project_memory BEGIN
	INSERT INTO project_memory_fts(project_memory_fts, rowid, title, content, kind) VALUES ('delete', old.rowid, old.title, old.content, old.kind);
	INSERT INTO project_memory_fts(rowid, title, content, kind) VALUES (new.rowid, new.title, new.content, new.kind);
END;

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

	// 老库升级：video_tasks 追加 episode（集数；0 = 未分集）
	if _, err := db.Exec(`ALTER TABLE video_tasks ADD COLUMN episode INTEGER NOT NULL DEFAULT 0`); err != nil &&
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
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_video_tasks_episode ON video_tasks (project_id, episode)`); err != nil {
		return fmt.Errorf("创建集数索引失败: %w", err)
	}

	// 默认模板已全部下线：只清理历史库里的内置行，不再写入任何示例模板。
	return removeLegacyBuiltinVideoTemplates(db)
}
