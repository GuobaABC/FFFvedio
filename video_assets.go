package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// 尾帧资产库：把任务最后一段的尾帧图片登记为可复用资产，
// 新建任务时可以直接选作首帧，实现跨任务的画面延续。

// replaceTailFrameAsset 每个任务只保留一条尾帧资产：重生成最后一段后自动覆盖旧帧。
func (a *App) replaceTailFrameAsset(taskID, taskTitle, segmentID, imagePath string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	taskID = strings.TrimSpace(taskID)
	imagePath = strings.TrimSpace(imagePath)
	if taskID == "" || imagePath == "" {
		return errors.New("尾帧资产缺少任务 ID 或图片路径")
	}
	if _, err := a.db.Exec(`DELETE FROM tail_frame_assets WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("清理旧尾帧资产失败: %w", err)
	}
	_, err := a.db.Exec(
		`INSERT INTO tail_frame_assets (id, task_id, task_title, segment_id, image_path, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		fmt.Sprintf("ta-%d-%s", time.Now().UnixNano(), randomHex(4)),
		taskID, strings.TrimSpace(taskTitle), strings.TrimSpace(segmentID), imagePath,
		time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("登记尾帧资产失败: %w", err)
	}
	return nil
}

// ListTailFrameAssets 返回全部尾帧资产（最新的在前）；文件已丢失的记录 mediaUrl 为空。
func (a *App) ListTailFrameAssets() ([]TailFrameAsset, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	// LEFT JOIN 带出来源任务的归属项目；任务被删后资产保留，归入「未归类」
	rows, err := a.db.Query(`SELECT ta.id, ta.task_id, ta.task_title, ta.segment_id, ta.image_path, ta.created_at,
		COALESCE(t.project_id, '')
		FROM tail_frame_assets ta LEFT JOIN video_tasks t ON t.id = ta.task_id
		ORDER BY ta.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("读取尾帧资产失败: %w", err)
	}
	defer rows.Close()

	root, rootErr := videoWorkRoot()
	assets := make([]TailFrameAsset, 0, 8)
	for rows.Next() {
		var asset TailFrameAsset
		if err := rows.Scan(&asset.ID, &asset.TaskID, &asset.TaskTitle, &asset.SegmentID, &asset.ImagePath, &asset.CreatedAt,
			&asset.ProjectID); err != nil {
			return nil, fmt.Errorf("解析尾帧资产失败: %w", err)
		}
		if rootErr == nil && fileExists(asset.ImagePath) {
			// /media/<taskID>/<相对任务工作目录的路径> 由 videoAssetMiddleware 直接服务本地文件
			if relative, relErr := filepath.Rel(filepath.Join(root, asset.TaskID), asset.ImagePath); relErr == nil &&
				!strings.HasPrefix(relative, "..") {
				asset.MediaURL = videoMediaURL(asset.TaskID, filepath.ToSlash(relative))
			}
		}
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历尾帧资产失败: %w", err)
	}
	return assets, nil
}

// DeleteTailFrameAsset 删除一条资产记录；磁盘上的图片保留，避免误删任务目录内容。
func (a *App) DeleteTailFrameAsset(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	result, err := a.db.Exec(`DELETE FROM tail_frame_assets WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("删除尾帧资产失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除尾帧资产失败: %w", err)
	}
	if affected == 0 {
		return errors.New("要删除的尾帧资产不存在")
	}
	return nil
}
