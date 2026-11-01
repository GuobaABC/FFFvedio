package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const videoTaskColumns = `id, title, stage, status, error, config_json, context_json, created_at, updated_at, template_id, project_id`

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func encodeVideoJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("序列化任务数据失败: %w", err)
	}
	return string(data), nil
}

func scanVideoTask(scanner interface{ Scan(dest ...any) error }) (VideoTask, error) {
	var task VideoTask
	var configJSON, contextJSON string
	if err := scanner.Scan(
		&task.ID, &task.Title, &task.Stage, &task.Status, &task.Error,
		&configJSON, &contextJSON, &task.CreatedAt, &task.UpdatedAt, &task.TemplateID, &task.ProjectID,
	); err != nil {
		return task, err
	}
	if err := json.Unmarshal([]byte(configJSON), &task.Config); err != nil {
		return task, fmt.Errorf("解析任务配置失败: %w", err)
	}
	if err := json.Unmarshal([]byte(contextJSON), &task.Run); err != nil {
		return task, fmt.Errorf("解析任务产物失败: %w", err)
	}
	// 旧任务里的阶段名可能是拆分前的别名（如 BUILD_PROMPT_SUBS）
	task.Stage = normalizeVideoStage(task.Stage)
	ensureVideoGates(&task.Run)
	return task, nil
}

// ListVideoTasks 返回全部视频任务（最新的在前），任务视图直接消费。
func (a *App) ListVideoTasks() ([]VideoTask, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	rows, err := a.db.Query(`SELECT ` + videoTaskColumns + ` FROM video_tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("读取视频任务列表失败: %w", err)
	}
	defer rows.Close()

	tasks := make([]VideoTask, 0, 8)
	for rows.Next() {
		task, err := scanVideoTask(rows)
		if err != nil {
			return nil, fmt.Errorf("解析视频任务失败: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历视频任务失败: %w", err)
	}
	return tasks, nil
}

// GetVideoTask 读取单个任务，前端轮询执行进度时调用。
func (a *App) GetVideoTask(id string) (VideoTask, error) {
	return a.videoTaskByID(id)
}

func (a *App) videoTaskByID(id string) (VideoTask, error) {
	if a.db == nil {
		return VideoTask{}, errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return VideoTask{}, errors.New("任务 ID 不能为空")
	}
	row := a.db.QueryRow(`SELECT `+videoTaskColumns+` FROM video_tasks WHERE id = ?`, id)
	task, err := scanVideoTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return VideoTask{}, errors.New("视频任务不存在")
	}
	if err != nil {
		return VideoTask{}, fmt.Errorf("读取视频任务失败: %w", err)
	}
	return task, nil
}

// CreateVideoTask 归一化用户输入并落库；此时还不会调用任何模型。
// 若配置里带 templateRef，先按「单次覆盖 > 模板 > 系统默认」解析出生效配置并固化版本快照。
func (a *App) CreateVideoTask(input VideoTaskInput) (VideoTask, error) {
	if a.db == nil {
		return VideoTask{}, errors.New("数据库未初始化")
	}

	cfg := input.Config
	var templateRun *VideoTemplateRun
	templateWarnings := make([]string, 0, 2)
	if ref := cfg.TemplateRef; ref != nil && strings.TrimSpace(ref.ID) != "" {
		merged, run, refWarnings, refErr := a.applyTemplateRef(cfg, *ref)
		if refErr != nil {
			return VideoTask{}, refErr
		}
		cfg = merged
		templateRun = run
		templateWarnings = append(templateWarnings, refWarnings...)
	}

	cfg, warnings, err := normalizeVideoConfig(cfg)
	if err != nil {
		return VideoTask{}, err
	}
	warnings = append(templateWarnings, warnings...)

	templateID := ""
	sources := map[string]string{}
	if templateRun != nil {
		templateID = templateRun.ID
		for key, source := range templateRun.Sources {
			sources[key] = source
		}
	}

	now := time.Now().Format(time.RFC3339)
	// 归属项目必须存在；选了个不存在的项目就当未归类，不给创建添堵
	projectID := strings.TrimSpace(input.ProjectID)
	if projectID != "" && !a.videoProjectExists(projectID) {
		warnings = append(warnings, "归属项目不存在或已被删除，任务已按未归类创建")
		projectID = ""
	}
	task := VideoTask{
		ID:         newVideoTaskID(),
		Title:      strings.TrimSpace(input.Title),
		Stage:      VideoStageDraftScript,
		Status:     VideoStatusDraft,
		TemplateID: templateID,
		ProjectID:  projectID,
		Config:     cfg,
		Run: VideoRunContext{
			Adapter:       cfg.Adapter,
			Template:      templateRun,
			ConfigSources: sources,
			Gates:         map[string]string{VideoGateScript: videoGatePending, VideoGatePrompt: videoGatePending},
			Warnings:      warnings,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if task.Title == "" {
		task.Title = defaultVideoTitle(cfg.Topic)
	}

	configJSON, err := encodeVideoJSON(task.Config)
	if err != nil {
		return VideoTask{}, err
	}
	contextJSON, err := encodeVideoJSON(task.Run)
	if err != nil {
		return VideoTask{}, err
	}

	if _, err := a.db.Exec(
		`INSERT INTO video_tasks (`+videoTaskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.Title, task.Stage, task.Status, task.Error, configJSON, contextJSON, task.CreatedAt, task.UpdatedAt, task.TemplateID, task.ProjectID,
	); err != nil {
		return VideoTask{}, fmt.Errorf("创建视频任务失败: %w", err)
	}

	// 提前建立工作目录，后续产物按任务 ID 归属（clips / output）。
	if _, err := a.ensureTaskWorkDir(task.ID); err != nil {
		task.Run.Warnings = append(task.Run.Warnings, "创建任务工作目录失败: "+err.Error())
	}
	return task, nil
}

// DeleteVideoTask 删除任务记录；已生成的本地文件保留，避免误删成片。
func (a *App) DeleteVideoTask(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	if a.isVideoTaskRunning(id) {
		return errors.New("任务正在执行，无法删除")
	}
	result, err := a.db.Exec(`DELETE FROM video_tasks WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("删除视频任务失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除视频任务失败: %w", err)
	}
	if affected == 0 {
		return errors.New("要删除的视频任务不存在")
	}
	return nil
}

// updateVideoTaskRecord 持久化阶段/状态/产物，是断点恢复的基础。
func (a *App) updateVideoTaskRecord(task VideoTask) error {
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
		`UPDATE video_tasks SET title = ?, stage = ?, status = ?, error = ?, config_json = ?, context_json = ?, updated_at = ?
		 WHERE id = ?`,
		task.Title, task.Stage, task.Status, task.Error, configJSON, contextJSON, task.UpdatedAt, task.ID,
	)
	if err != nil {
		return fmt.Errorf("更新视频任务失败: %w", err)
	}
	return nil
}
