package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// 应用元信息，与 wails.json 中的 info 保持一致
const (
	appName    = "FFFVedioAgent"
	appVersion = "v0.1.2"
)

// 前端构建产物由 Wails 内嵌到可执行文件中，
// 因此桌面端运行时不需要额外的静态资源目录。
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     appName,
		Width:     1100,
		Height:    720,
		MinWidth:  900,
		MinHeight: 600,
		// 深色主题背景，避免窗口创建瞬间出现白屏
		BackgroundColour: &options.RGBA{R: 11, G: 11, B: 14, A: 1},
		AssetServer: &assetserver.Options{
			Assets: assets,
			// /media/ 路由到视频任务工作目录，前端可直接播放本地成片
			Middleware: videoAssetMiddleware,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		// 将 App 暴露给前端：window.go.main.App.* 即为 Provider 相关方法
		Bind: []interface{}{app},
		Mac: &mac.Options{
			TitleBar:   mac.TitleBarDefault(),
			Appearance: mac.NSAppearanceNameDarkAqua,
			About: &mac.AboutInfo{
				Title:   appName + " " + appVersion,
				Message: "Fast Fast Fast",
			},
		},
		Windows: &windows.Options{
			Theme: windows.Dark,
		},
	})

	if err != nil {
		log.Fatalf("%s %s 启动失败: %v", appName, appVersion, err)
	}
}
