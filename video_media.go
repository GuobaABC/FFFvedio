package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// videoWorkRootOverride 仅用于测试：把工作目录重定向到临时目录。
var videoWorkRootOverride string

// videoWorkRoot 视频任务工作目录根：
// <用户配置目录>/FFFVedioAgent/video/<taskID>/{clips,output}
func videoWorkRoot() (string, error) {
	if videoWorkRootOverride != "" {
		return videoWorkRootOverride, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return filepath.Join(configDir, appName, "video"), nil
}

// ensureTaskWorkDir 确保任务工作目录存在，返回绝对路径。
func (a *App) ensureTaskWorkDir(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return "", errors.New("任务 ID 不合法")
	}
	root, err := videoWorkRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, id)
	for _, sub := range []string{"", "clips", "output"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return "", fmt.Errorf("创建任务工作目录失败: %w", err)
		}
	}
	return dir, nil
}

func (a *App) isVideoTaskRunning(id string) bool {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	return a.videoRunning[id]
}

// beginVideoRun 抢占任务执行权，防止同一任务并发跑两遍（重复计费）。
func (a *App) beginVideoRun(id string) bool {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	if a.videoRunning == nil {
		a.videoRunning = make(map[string]bool, 4)
	}
	if a.videoRunning[id] {
		return false
	}
	a.videoRunning[id] = true
	return true
}

func (a *App) endVideoRun(id string) {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	delete(a.videoRunning, id)
}

// videoAssetMiddleware 把 /media/ 路由到任务工作目录，让前端可以直接播放本地成片。
// 其余请求继续交给 Wails 默认的资源服务，避免影响前端静态资源与运行时注入。
func videoAssetMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/media/") {
			serveVideoMedia(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveVideoMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	root, err := videoWorkRoot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	relative := strings.TrimPrefix(r.URL.Path, "/media/")
	if decoded, err := url.PathUnescape(relative); err == nil {
		relative = decoded
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(root, clean)
	if within, err := filepath.Rel(root, full); err != nil || strings.HasPrefix(within, "..") {
		http.NotFound(w, r)
		return
	}
	if info, err := os.Stat(full); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// 字幕文件按纯文本返回，便于用户核对时间轴
	switch strings.ToLower(filepath.Ext(full)) {
	case ".srt", ".ass":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	http.ServeFile(w, r, full)
}

// openLocalPath 用系统默认方式打开目录/文件，reveal=true 时在文件管理器中定位文件。
func openLocalPath(target string, reveal bool) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("路径为空")
	}
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("路径不存在: %w", err)
	}

	switch runtime.GOOS {
	case "windows":
		if reveal {
			return exec.Command("explorer", "/select,"+target).Start()
		}
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			return exec.Command("explorer", target).Start()
		}
		return exec.Command("cmd", "/c", "start", "", target).Start()
	case "darwin":
		if reveal {
			return exec.Command("open", "-R", target).Start()
		}
		return exec.Command("open", target).Start()
	default:
		if reveal {
			return exec.Command("xdg-open", filepath.Dir(target)).Start()
		}
		return exec.Command("xdg-open", target).Start()
	}
}

// OpenVideoTaskFolder 打开任务工作目录（成片、分段、字幕都在里面）。
func (a *App) OpenVideoTaskFolder(id string) error {
	dir, err := a.ensureTaskWorkDir(id)
	if err != nil {
		return err
	}
	return openLocalPath(dir, false)
}

// RevealVideoOutput 用系统播放器打开成片；没有成片时退回打开任务目录。
func (a *App) RevealVideoOutput(id string) error {
	task, err := a.videoTaskByID(id)
	if err != nil {
		return err
	}
	if task.Run.Output != nil {
		if task.Run.Output.FinalPath != "" && fileExists(task.Run.Output.FinalPath) {
			return openLocalPath(task.Run.Output.FinalPath, true)
		}
		if task.Run.Output.MergedPath != "" && fileExists(task.Run.Output.MergedPath) {
			return openLocalPath(task.Run.Output.MergedPath, true)
		}
	}
	return a.OpenVideoTaskFolder(id)
}

// PickImageFile 通过系统文件对话框选择首帧图片，避免用户手抄路径。
func (a *App) PickImageFile() (string, error) {
	if a.ctx == nil {
		return "", errors.New("窗口尚未就绪")
	}
	return wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择首帧图片",
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "图片", Pattern: "*.png;*.jpg;*.jpeg;*.webp"},
			{DisplayName: "全部文件", Pattern: "*.*"},
		},
	})
}

// PickImageFiles 多选角色参考图。
func (a *App) PickImageFiles() ([]string, error) {
	if a.ctx == nil {
		return nil, errors.New("窗口尚未就绪")
	}
	files, err := wailsRuntime.OpenMultipleFilesDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择角色参考图",
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "图片", Pattern: "*.png;*.jpg;*.jpeg;*.webp"},
			{DisplayName: "全部文件", Pattern: "*.*"},
		},
	})
	if err != nil || files == nil {
		return []string{}, err
	}
	return files, nil
}
