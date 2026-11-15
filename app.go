package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
)

// App 是桌面应用的生命周期与本地数据载体。
// 当前承载「设置 - 提供商」的本地配置与「视频生成」任务式流水线。
type App struct {
	ctx context.Context
	db  *sql.DB

	// 视频任务执行互斥：同一任务不允许并发跑两遍，避免重复计费
	videoMu      sync.Mutex
	videoRunning map[string]bool
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	db, dbPath, err := openDatabase()
	if err != nil {
		log.Printf("初始化本地数据库失败: %v", err)
	} else {
		a.db = db
		log.Printf("本地数据库已就绪: %s", dbPath)
	}

	log.Printf("%s %s 已启动", appName, appVersion)
}

func (a *App) shutdown(_ context.Context) {
	if a.db != nil {
		if err := a.db.Close(); err != nil {
			log.Printf("关闭本地数据库失败: %v", err)
		}
	}
	log.Printf("%s 正在退出", appName)
}
