package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// 内置字幕字体：Noto Sans SC Regular（SIL OFL 1.1）。
// 不依赖用户机器安装「思源黑体」，烧录时通过 ffmpeg ass 滤镜的 fontsdir 指定该目录。
//
//go:embed assets/fonts/NotoSansSC-Regular.otf
var subtitleFontData []byte

const subtitleFontFileName = "NotoSansSC-Regular.otf"

var (
	subtitleFontMu   sync.Mutex
	subtitleFontPath string
)

// ensureSubtitleFontDir 首次调用把内置字体解压到 <workRoot>/fonts，之后按哈希复用。
// 返回字体目录；失败时调用方应降级为系统字体，但不要阻断成片。
func ensureSubtitleFontDir() (string, error) {
	subtitleFontMu.Lock()
	defer subtitleFontMu.Unlock()

	if subtitleFontPath != "" {
		if fileExists(subtitleFontPath) {
			return filepath.Dir(subtitleFontPath), nil
		}
		subtitleFontPath = ""
	}
	if len(subtitleFontData) == 0 {
		return "", errors.New("内置字幕字体资源为空")
	}
	root, err := videoWorkRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "fonts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建字幕字体目录失败: %w", err)
	}
	sum := sha256.Sum256(subtitleFontData)
	hash := hex.EncodeToString(sum[:])
	fontPath := filepath.Join(dir, subtitleFontFileName)
	versionPath := filepath.Join(dir, "version.txt")

	if fileExists(fontPath) {
		if data, readErr := os.ReadFile(versionPath); readErr == nil && strings.TrimSpace(string(data)) == hash {
			subtitleFontPath = fontPath
			return dir, nil
		}
	}
	if err := os.WriteFile(fontPath, subtitleFontData, 0o644); err != nil {
		return "", fmt.Errorf("写入内置字幕字体失败: %w", err)
	}
	if err := os.WriteFile(versionPath, []byte(hash), 0o644); err != nil {
		return "", fmt.Errorf("写入字幕字体版本失败: %w", err)
	}
	subtitleFontPath = fontPath
	return dir, nil
}

// subtitleFontAvailable 只做“明显不存在”的粗判：内置 Noto Sans SC 永远可用，
// 用户自定义字体在 Windows 上按常见字体文件名探测；其他平台无法可靠枚举，直接放行。
func subtitleFontAvailable(fontName string) bool {
	name := strings.ToLower(strings.TrimSpace(fontName))
	if name == "" || strings.Contains(name, "noto sans sc") || strings.Contains(name, "思源黑体") {
		return true
	}
	if runtime.GOOS != "windows" {
		return true
	}
	fontsDir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	if fontsDir == "Fonts" || fontsDir == "" {
		fontsDir = `C:\Windows\Fonts`
	}
	candidates := map[string][]string{
		"microsoft yahei": {"msyh.ttc", "msyhbd.ttc"},
		"微软雅黑":            {"msyh.ttc", "msyhbd.ttc"},
		"simhei":          {"simhei.ttf"},
		"黑体":              {"simhei.ttf"},
		"simsun":          {"simsun.ttc"},
		"宋体":              {"simsun.ttc"},
		"kaiti":           {"simkai.ttf"},
		"楷体":              {"simkai.ttf"},
	}
	files, ok := candidates[name]
	if !ok {
		// 未收录的字体名不做猜测：libass 会回退，误报比漏报更烦人
		return true
	}
	for _, file := range files {
		if fileExists(filepath.Join(fontsDir, file)) {
			return true
		}
	}
	return false
}

// bundledFontDir 给 ffmpeg 用的字体目录；拿不到时返回空串，由调用方退回系统字体。
func bundledFontDir() string {
	dir, err := ensureSubtitleFontDir()
	if err != nil {
		return ""
	}
	return dir
}
