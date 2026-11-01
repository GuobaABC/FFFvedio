package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVideoProjectCRUD(t *testing.T) {
	app := newTestApp(t)

	if _, err := app.CreateVideoProject("  "); err == nil ||
		!strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空名称应报错: %v", err)
	}
	if _, err := app.CreateVideoProject(strings.Repeat("长", videoProjectNameMaxRunes+1)); err == nil ||
		!strings.Contains(err.Error(), "最多") {
		t.Fatalf("超长名称应报错: %v", err)
	}

	project, err := app.CreateVideoProject("手冲咖啡系列")
	if err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}
	if project.ID == "" || project.Name != "手冲咖啡系列" {
		t.Fatalf("返回的项目不完整: %+v", project)
	}
	if _, err := app.CreateVideoProject("手冲咖啡系列"); err == nil ||
		!strings.Contains(err.Error(), "同名") {
		t.Fatalf("同名项目应被拒绝: %v", err)
	}

	projects, err := app.ListVideoProjects()
	if err != nil {
		t.Fatalf("读取项目列表失败: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("列表应与创建一致: %+v", projects)
	}
	if projects[0].TaskCount != 0 || projects[0].TailCount != 0 {
		t.Fatalf("新项目的统计应为 0: %+v", projects[0])
	}

	if err := app.RenameVideoProject(project.ID, "  "); err == nil ||
		!strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("重命名为空应报错: %v", err)
	}
	if err := app.RenameVideoProject("vp-none", "改名"); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("重命名不存在的项目应报错: %v", err)
	}
	if err := app.RenameVideoProject(project.ID, " 清晨咖啡馆 "); err != nil {
		t.Fatalf("重命名失败: %v", err)
	}
	projects, err = app.ListVideoProjects()
	if err != nil || projects[0].Name != "清晨咖啡馆" {
		t.Fatalf("重命名未生效: %v %+v", err, projects)
	}

	if err := app.DeleteVideoProject("vp-none"); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("删除不存在的项目应报错: %v", err)
	}
	if err := app.DeleteVideoProject(project.ID); err != nil {
		t.Fatalf("删除项目失败: %v", err)
	}
	projects, err = app.ListVideoProjects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("删除后列表应为空: %v %+v", err, projects)
	}
}

func TestCreateVideoTaskProjectBinding(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	project, err := app.CreateVideoProject("归属校验")
	if err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}

	plain, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "无项目任务", DurationS: 6, Adapter: "mock",
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if plain.ProjectID != "" {
		t.Fatalf("未选项目的任务 projectId 应为空: %q", plain.ProjectID)
	}

	bound, err := app.CreateVideoTask(VideoTaskInput{
		ProjectID: project.ID,
		Config:    VideoRunConfig{Topic: "带项目任务", DurationS: 6, Adapter: "mock"},
	})
	if err != nil {
		t.Fatalf("创建带项目任务失败: %v", err)
	}
	if bound.ProjectID != project.ID {
		t.Fatalf("任务应归属项目: %+v", bound)
	}

	// 落库读取也应带上 projectId
	list, err := app.ListVideoTasks()
	if err != nil {
		t.Fatalf("读取任务列表失败: %v", err)
	}
	for _, task := range list {
		if task.ID == bound.ID && task.ProjectID != project.ID {
			t.Fatalf("列表读取的 projectId 不一致: %+v", task)
		}
	}

	// 不存在的项目：按未归类创建并给出警告
	fallback, err := app.CreateVideoTask(VideoTaskInput{
		ProjectID: "vp-not-exist",
		Config:    VideoRunConfig{Topic: "悬空归属任务", DurationS: 6, Adapter: "mock"},
	})
	if err != nil {
		t.Fatalf("悬空归属不应阻断创建: %v", err)
	}
	if fallback.ProjectID != "" {
		t.Fatalf("不存在的项目应置空: %q", fallback.ProjectID)
	}
	if !strings.Contains(strings.Join(fallback.Run.Warnings, "\n"), "归属项目不存在") {
		t.Fatalf("应给出归属项目警告: %+v", fallback.Run.Warnings)
	}

	// 项目统计随之更新
	projects, err := app.ListVideoProjects()
	if err != nil || len(projects) != 1 {
		t.Fatalf("读取项目失败: %v %+v", err, projects)
	}
	if projects[0].TaskCount != 1 {
		t.Fatalf("项目任务数应为 1: %+v", projects[0])
	}
}

func TestMoveVideoTaskToProject(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	project, err := app.CreateVideoProject("移动目标")
	if err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}
	task, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic: "调整归属", DurationS: 6, Adapter: "mock",
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	if err := app.MoveVideoTaskToProject("vt-none", project.ID); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("移动不存在的任务应报错: %v", err)
	}
	if err := app.MoveVideoTaskToProject(task.ID, "vp-none"); err == nil ||
		!strings.Contains(err.Error(), "目标项目不存在") {
		t.Fatalf("移入不存在的项目应报错: %v", err)
	}

	// 运行中的任务禁止改归属
	app.videoMu.Lock()
	if app.videoRunning == nil {
		app.videoRunning = map[string]bool{}
	}
	app.videoRunning[task.ID] = true
	app.videoMu.Unlock()
	if err := app.MoveVideoTaskToProject(task.ID, project.ID); err == nil ||
		!strings.Contains(err.Error(), "正在执行") {
		t.Fatalf("运行中应拒绝改归属: %v", err)
	}
	app.videoMu.Lock()
	delete(app.videoRunning, task.ID)
	app.videoMu.Unlock()

	if err := app.MoveVideoTaskToProject(task.ID, project.ID); err != nil {
		t.Fatalf("移入项目失败: %v", err)
	}
	moved, err := app.videoTaskByID(task.ID)
	if err != nil || moved.ProjectID != project.ID {
		t.Fatalf("移入未生效: %v %+v", err, moved)
	}

	if err := app.MoveVideoTaskToProject(task.ID, ""); err != nil {
		t.Fatalf("移回未归类失败: %v", err)
	}
	back, err := app.videoTaskByID(task.ID)
	if err != nil || back.ProjectID != "" {
		t.Fatalf("移回未生效: %v %+v", err, back)
	}
}

func TestDeleteVideoProjectUnassignsTasks(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	project, err := app.CreateVideoProject("将被删除")
	if err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}
	task, err := app.CreateVideoTask(VideoTaskInput{
		ProjectID: project.ID,
		Config:    VideoRunConfig{Topic: "项目解散回归未归类", DurationS: 6, Adapter: "mock"},
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	if err := app.DeleteVideoProject(project.ID); err != nil {
		t.Fatalf("删除项目失败: %v", err)
	}
	after, err := app.videoTaskByID(task.ID)
	if err != nil {
		t.Fatalf("删除项目不应删除任务: %v", err)
	}
	if after.ProjectID != "" {
		t.Fatalf("任务的归属应被解除: %q", after.ProjectID)
	}
}

func TestTailFrameAssetsCarryProjectID(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	project, err := app.CreateVideoProject("尾帧归组")
	if err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}
	task, err := app.CreateVideoTask(VideoTaskInput{
		ProjectID: project.ID,
		Config:    VideoRunConfig{Topic: "尾帧关联项目", DurationS: 6, Adapter: "mock"},
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	workDir, err := app.ensureTaskWorkDir(task.ID)
	if err != nil {
		t.Fatalf("创建任务目录失败: %v", err)
	}
	imagePath := filepath.Join(workDir, "clips", "s1-2-tail.png")
	if err := app.replaceTailFrameAsset(task.ID, task.Title, "s1-2", imagePath); err != nil {
		t.Fatalf("登记尾帧资产失败: %v", err)
	}

	assets, err := app.ListTailFrameAssets()
	if err != nil || len(assets) != 1 {
		t.Fatalf("读取尾帧资产失败: %v %+v", err, assets)
	}
	if assets[0].ProjectID != project.ID {
		t.Fatalf("资产应带出来源任务的归属项目: %+v", assets[0])
	}

	projects, err := app.ListVideoProjects()
	if err != nil || projects[0].TailCount != 1 {
		t.Fatalf("项目尾帧统计应为 1: %v %+v", err, projects)
	}

	// 来源任务删除后资产保留，归入「未归类」
	if err := app.DeleteVideoTask(task.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	assets, err = app.ListTailFrameAssets()
	if err != nil || len(assets) != 1 {
		t.Fatalf("任务删除不应清理尾帧资产: %v %+v", err, assets)
	}
	if assets[0].ProjectID != "" {
		t.Fatalf("来源任务被删后资产应归未归类: %+v", assets[0])
	}
}
