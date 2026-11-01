package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 项目视图后端：任务归组容器（项目）的增删改查与任务归属调整。
// 删除项目只解除归属，不级联删除任务与尾帧资产（文件也保留），避免误删成片。

const videoProjectNameMaxRunes = 40

func (a *App) videoProjectExists(id string) bool {
	if a.db == nil || strings.TrimSpace(id) == "" {
		return false
	}
	var exists int
	err := a.db.QueryRow(`SELECT COUNT(1) FROM video_projects WHERE id = ?`, strings.TrimSpace(id)).Scan(&exists)
	return err == nil && exists > 0
}

// videoProjectNameTaken 同名校验（去除首尾空格后精确匹配，排除自身）。
func (a *App) videoProjectNameTaken(name, excludeID string) (bool, error) {
	var count int
	err := a.db.QueryRow(`SELECT COUNT(1) FROM video_projects WHERE name = ? AND id <> ?`, name, excludeID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("检查项目名称失败: %w", err)
	}
	return count > 0, nil
}

// ListVideoProjects 返回全部项目（创建时间升序），带任务数与尾帧数统计。
func (a *App) ListVideoProjects() ([]VideoProject, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	rows, err := a.db.Query(`
		SELECT p.id, p.name, p.note, p.created_at, p.updated_at,
			(SELECT COUNT(1) FROM video_tasks t WHERE t.project_id = p.id),
			(SELECT COUNT(1) FROM tail_frame_assets ta JOIN video_tasks t2 ON t2.id = ta.task_id WHERE t2.project_id = p.id)
		FROM video_projects p ORDER BY p.created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("读取项目列表失败: %w", err)
	}
	defer rows.Close()

	projects := make([]VideoProject, 0, 8)
	for rows.Next() {
		var project VideoProject
		if err := rows.Scan(&project.ID, &project.Name, &project.Note, &project.CreatedAt, &project.UpdatedAt,
			&project.TaskCount, &project.TailCount); err != nil {
			return nil, fmt.Errorf("解析项目失败: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历项目失败: %w", err)
	}
	return projects, nil
}

// CreateVideoProject 新建项目：名称去空、限长、同名校验。
func (a *App) CreateVideoProject(name string) (VideoProject, error) {
	if a.db == nil {
		return VideoProject{}, errors.New("数据库未初始化")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return VideoProject{}, errors.New("项目名称不能为空")
	}
	if len([]rune(name)) > videoProjectNameMaxRunes {
		return VideoProject{}, fmt.Errorf("项目名称最多 %d 个字", videoProjectNameMaxRunes)
	}
	taken, err := a.videoProjectNameTaken(name, "")
	if err != nil {
		return VideoProject{}, err
	}
	if taken {
		return VideoProject{}, fmt.Errorf("已存在同名项目「%s」", name)
	}

	now := time.Now().Format(time.RFC3339)
	project := VideoProject{
		ID:        fmt.Sprintf("vp-%d-%s", time.Now().UnixNano(), randomHex(4)),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := a.db.Exec(
		`INSERT INTO video_projects (id, name, note, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		project.ID, project.Name, project.Note, project.CreatedAt, project.UpdatedAt,
	); err != nil {
		return VideoProject{}, fmt.Errorf("创建项目失败: %w", err)
	}
	return project, nil
}

// RenameVideoProject 重命名项目（同样做同名与长度校验）。
func (a *App) RenameVideoProject(id, name string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("项目名称不能为空")
	}
	if len([]rune(name)) > videoProjectNameMaxRunes {
		return fmt.Errorf("项目名称最多 %d 个字", videoProjectNameMaxRunes)
	}
	id = strings.TrimSpace(id)
	if !a.videoProjectExists(id) {
		return errors.New("项目不存在")
	}
	taken, err := a.videoProjectNameTaken(name, id)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("已存在同名项目「%s」", name)
	}
	if _, err := a.db.Exec(`UPDATE video_projects SET name = ?, updated_at = ? WHERE id = ?`,
		name, time.Now().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("重命名项目失败: %w", err)
	}
	return nil
}

// DeleteVideoProject 只删项目本身：其下任务与尾帧资产回到「未归类」，磁盘文件不动。
func (a *App) DeleteVideoProject(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	if !a.videoProjectExists(id) {
		return errors.New("项目不存在")
	}
	if _, err := a.db.Exec(`UPDATE video_tasks SET project_id = '' WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("解除任务归属失败: %w", err)
	}
	if _, err := a.db.Exec(`DELETE FROM video_projects WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除项目失败: %w", err)
	}
	return nil
}

// MoveVideoTaskToProject 调整任务归属；projectID 为空表示移回「未归类」。
// 任务运行中不允许改归属，避免项目视图轮询刷新时归属抖动。
func (a *App) MoveVideoTaskToProject(taskID, projectID string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	task, err := a.videoTaskByID(taskID)
	if err != nil {
		return err
	}
	if a.isVideoTaskRunning(task.ID) {
		return errors.New("任务正在执行，请稍候再调整归属")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID != "" && !a.videoProjectExists(projectID) {
		return errors.New("目标项目不存在")
	}
	if _, err := a.db.Exec(`UPDATE video_tasks SET project_id = ? WHERE id = ?`, projectID, task.ID); err != nil {
		return fmt.Errorf("调整任务归属失败: %w", err)
	}
	return nil
}
