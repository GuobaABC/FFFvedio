package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// 安装包会把 ffmpeg / ffprobe 一并装进应用目录（Windows 为 exe 同级目录，
// macOS 为 App 包内的 Contents/Resources/bin），这样用户装完即可用，
// 不需要另外安装依赖或配置 PATH。

// exeSuffix 返回当前平台可执行文件的后缀，用于在内置目录里找依赖。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// appDir 返回可执行文件所在目录；跟随软链，避免 macOS / Linux 的链接启动路径。
func appDir() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), true
}

// toolCandidates 列出内置依赖的候选路径，命中顺序即优先级。
func toolCandidates(name string) []string {
	dir, ok := appDir()
	if !ok {
		return nil
	}
	return []string{
		filepath.Join(dir, name),
		filepath.Join(dir, "bin", name),
		filepath.Join(dir, "deps", name),
		// macOS App 包：Contents/MacOS/<binary> 与 Contents/Resources/bin
		filepath.Join(dir, "..", "Resources", "bin", name),
	}
}

// toolPath 定位外部工具：环境变量覆盖 → 安装目录内置副本 → 系统 PATH。
func toolPath(envKey, name string) (string, bool) {
	if custom := strings.TrimSpace(os.Getenv(envKey)); custom != "" && fileExists(custom) {
		return custom, true
	}
	for _, candidate := range toolCandidates(name) {
		if fileExists(candidate) {
			return candidate, true
		}
	}
	// 老版本用户只装了系统级 ffmpeg，仍需保持可用
	if path, err := exec.LookPath(name); err == nil {
		return path, true
	}
	// Windows 上 PATH 里的项可能不带后缀
	if runtime.GOOS == "windows" && !strings.Contains(name, ".") {
		if path, err := exec.LookPath(name + ".exe"); err == nil {
			return path, true
		}
	}
	return "", false
}

// ffmpegPath 定位 ffmpeg；允许通过 FFFVEDIO_FFMPEG 环境变量指定绝对路径。
func ffmpegPath() (string, bool) {
	return toolPath("FFFVedio_FFMPEG", "ffmpeg"+exeSuffix())
}

// ffprobePath 定位 ffprobe，用于读取素材时长与分辨率。
func ffprobePath() (string, bool) {
	return toolPath("FFFVedio_FFPROBE", "ffprobe"+exeSuffix())
}
