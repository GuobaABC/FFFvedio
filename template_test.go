package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestVideoTemplateBuiltinsSeeded(t *testing.T) {
	app := newTestApp(t)

	templates, err := app.ListVideoTemplates()
	if err != nil {
		t.Fatalf("读取模板库失败: %v", err)
	}
	if len(templates) < 3 {
		t.Fatalf("内置模板未写入，当前 %d 个", len(templates))
	}
	for _, template := range templates {
		if template.Scope != TemplateScopeBuiltin {
			t.Fatalf("内置模板应排在最前且作用域为 builtin: %s / %s", template.Name, template.Scope)
		}
		if template.Version == "" || template.Name == "" {
			t.Fatalf("内置模板缺少名称或版本: %+v", template)
		}
	}
}

// 内置模板升版（真人角色 → 动物角色）时，存量库的 builtin 行要刷新，个人模板不受牵连。
func TestBuiltinTemplateSeedRefreshesLegacyAnchors(t *testing.T) {
	app := newTestApp(t)

	const legacyPayload = `{"id":"tpl_builtin_brand_warm","name":"品牌口播·暖调","version":"1.2","scope":"builtin","characters":[{"roleId":"host","name":"主理人","appearanceBlock":"35 岁女性，齐肩黑发别在耳后"}]}`
	if _, err := app.db.Exec(`UPDATE video_templates SET version = '1.2', payload_json = ? WHERE id = 'tpl_builtin_brand_warm'`, legacyPayload); err != nil {
		t.Fatalf("构造老库模板失败: %v", err)
	}
	if _, err := app.db.Exec(`INSERT INTO video_templates (`+videoTemplateColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"tpl_mine", "我的模板", "1.2", TemplateScopePersonal, "[]", legacyPayload, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("写入个人模板失败: %v", err)
	}

	if err := seedBuiltinVideoTemplates(app.db); err != nil {
		t.Fatalf("刷新内置模板失败: %v", err)
	}

	var wantVersion string
	for _, item := range builtinVideoTemplates() {
		if item.ID == "tpl_builtin_brand_warm" {
			wantVersion = item.Version
		}
	}
	template, err := app.videoTemplateByID("tpl_builtin_brand_warm")
	if err != nil {
		t.Fatalf("读取内置模板失败: %v", err)
	}
	if template.Version != wantVersion {
		t.Fatalf("内置模板版本未刷新: %s / 期望 %s", template.Version, wantVersion)
	}
	if len(template.Characters) == 0 || !strings.Contains(template.Characters[0].AppearanceBlock, "猫") {
		t.Fatalf("内置模板角色应刷新为动物主体: %+v", template.Characters)
	}

	var personalPayload string
	if err := app.db.QueryRow(`SELECT payload_json FROM video_templates WHERE id = 'tpl_mine'`).Scan(&personalPayload); err != nil {
		t.Fatalf("读取个人模板失败: %v", err)
	}
	if !strings.Contains(personalPayload, "35 岁女性") {
		t.Fatalf("个人模板不应被内置刷新波及: %s", personalPayload)
	}
}

func TestVideoTemplateLayeredOverrideAndSources(t *testing.T) {
	app := newTestApp(t)

	template, err := app.videoTemplateByID("tpl_builtin_brand_warm")
	if err != nil {
		t.Fatalf("读取内置模板失败: %v", err)
	}
	cfg := VideoRunConfig{
		Topic:     "清晨手冲咖啡",
		DurationS: 30, // 本次显式覆盖模板的 24s
	}
	overrides := map[string]string{"durationS": "30"}

	merged, sources, _ := applyVideoTemplate(&template, cfg, overrides)
	if merged.DurationS != 30 {
		t.Fatalf("单次覆盖应优先于模板: durationS=%d", merged.DurationS)
	}
	if sources[templateFieldDuration] != TemplateSourceOverride {
		t.Fatalf("durationS 来源应为 override，实际 %q", sources[templateFieldDuration])
	}
	if merged.AspectRatio != "9:16" {
		t.Fatalf("画幅应来自模板 9:16，实际 %q", merged.AspectRatio)
	}
	if sources[templateFieldAspect] != TemplateSourceTemplate {
		t.Fatalf("画幅来源应为 template，实际 %q", sources[templateFieldAspect])
	}
	if merged.SubtitleStyle.FontName != "思源黑体" || merged.SubtitleStyle.FontSizePct != 4.6 {
		t.Fatalf("字幕样式未从模板带入: %+v", merged.SubtitleStyle)
	}
	if len(merged.Platforms) != 1 || merged.Platforms[0] != "douyin" {
		t.Fatalf("发布平台未从模板带入: %v", merged.Platforms)
	}
	if len(merged.CharacterRefs) != 1 || merged.CharacterRefs[0].RoleID != "host" {
		t.Fatalf("角色参考未从模板带入: %+v", merged.CharacterRefs)
	}
	if merged.VideoParams.Style == "" {
		t.Fatal("风格 token 未合并进 videoParams.style")
	}
}

// 用户上传的角色图优先于模板里的生成图：同一角色只吃一份来源，
// 模板图只补到用户没给图的角色上（旧做法是整个字段二选一，用户选好的图会被模板盖掉）。
func TestApplyVideoTemplatePrefersUserUploadedCharacterImages(t *testing.T) {
	tpl := &VideoTemplate{Characters: []VideoTemplateCharacter{
		{RoleID: "host", Name: "橘座（猫）", Images: []string{"tpl-host.png"}, AppearanceBlock: "成年橘猫"},
		{RoleID: "guest", Name: "边牧", Images: []string{"tpl-guest.png"}, AppearanceBlock: "黑白毛边牧"},
	}}
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		// 只写了角色名（没写 role_id）也要能对上模板里那个角色
		{RoleID: "host", Name: "橘座", Images: []string{"C:\\Users\\me\\host.png"}},
	}}

	merged, sources, warnings := applyVideoTemplate(tpl, cfg, nil)
	if sources[templateFieldCharacters] != TemplateSourceOverride {
		t.Fatalf("用了上传图时字段来源应为 override，实际 %q", sources[templateFieldCharacters])
	}
	if len(merged.CharacterRefs) != 2 {
		t.Fatalf("应只留两个角色条目: %+v", merged.CharacterRefs)
	}
	host := merged.CharacterRefs[0]
	if host.RoleID != "host" || host.ImageSource != CharacterImageSourceUser || len(host.Images) != 1 || host.Images[0] != "C:\\Users\\me\\host.png" {
		t.Fatalf("上传图应踢掉同角色的模板生成图: %+v", host)
	}
	guest := merged.CharacterRefs[1]
	if guest.RoleID != "guest" || guest.ImageSource != CharacterImageSourceTemplate || len(guest.Images) != 1 {
		t.Fatalf("用户没给图的角色应继续用模板定妆图: %+v", guest)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "用户上传") {
		t.Fatalf("模板图被替换要告知用户: %v", warnings)
	}

	// 用户一张图也没给：完全回退模板，来源标 template，不报替换警告
	noUpload, noSources, noWarnings := applyVideoTemplate(tpl, VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "host", Name: "橘座"},
	}}, nil)
	if noSources[templateFieldCharacters] != TemplateSourceTemplate {
		t.Fatalf("没上传图时来源应为 template，实际 %q", noSources[templateFieldCharacters])
	}
	if len(noUpload.CharacterRefs) != 3 || noUpload.CharacterRefs[1].ImageSource != CharacterImageSourceTemplate {
		t.Fatalf("模板定妆图应全部带入: %+v", noUpload.CharacterRefs)
	}
	for _, warning := range noWarnings {
		if strings.Contains(warning, "用户上传") {
			t.Fatalf("没有上传图时不该报替换警告: %v", noWarnings)
		}
	}
}

func TestCreateVideoTaskWithTemplate(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	builtin, err := app.videoTemplateByID("tpl_builtin_brand_warm")
	if err != nil {
		t.Fatalf("读取内置模板失败: %v", err)
	}

	task, err := app.CreateVideoTask(VideoTaskInput{
		Config: VideoRunConfig{
			Topic: "清晨手冲咖啡",
			TemplateRef: &VideoTemplateRef{
				ID:          "tpl_builtin_brand_warm",
				Version:     "1.0", // 老任务锁定的历史版本
				Overrides:   map[string]string{"durationS": "30"},
				OpeningVars: map[string]string{"brand": "晨光", "product": "手冲咖啡"},
			},
			DurationS: 30,
		},
	})
	if err != nil {
		t.Fatalf("按模板创建任务失败: %v", err)
	}
	if task.TemplateID != "tpl_builtin_brand_warm" {
		t.Fatalf("任务未记录模板 ID: %q", task.TemplateID)
	}
	if task.Run.Template == nil {
		t.Fatal("任务缺少模板快照")
	}
	if task.Run.Template.Version != builtin.Version {
		t.Fatalf("模板快照应按最新版本固化: %q / 期望 %q", task.Run.Template.Version, builtin.Version)
	}
	if task.Run.ConfigSources[templateFieldAspect] != TemplateSourceTemplate {
		t.Fatalf("字段来源未落库: %+v", task.Run.ConfigSources)
	}
	if !strings.Contains(task.Run.Template.OpeningLine, "手冲咖啡") {
		t.Fatalf("开场白变量未插值: %q", task.Run.Template.OpeningLine)
	}
	if len(task.Run.Template.OpeningAll) == 0 {
		t.Fatal("模板快照应保留全部插值后的开场变体")
	}
	joined := strings.Join(task.Run.Template.OpeningAll, " / ")
	if !strings.Contains(joined, "晨光") || strings.Contains(joined, "{") {
		t.Fatalf("全部变体都应完成插值且不留占位符: %q", joined)
	}
	if task.Config.TemplateRef == nil || task.Config.TemplateRef.ID == "" {
		t.Fatal("任务的输入契约应保留模板引用")
	}
}

func TestTemplateOpeningRotatesAcrossTasks(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	create := func() string {
		task, err := app.CreateVideoTask(VideoTaskInput{
			Config: VideoRunConfig{
				Topic:       "清晨手冲咖啡",
				TemplateRef: &VideoTemplateRef{ID: "tpl_builtin_slice_life"},
			},
		})
		if err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
		if task.Run.Template == nil {
			t.Fatal("任务缺少模板快照")
		}
		return task.Run.Template.OpeningLine
	}

	first := create()
	second := create()
	if first == "" || second == "" {
		t.Fatalf("开场白不应为空: %q / %q", first, second)
	}
	if first == second {
		t.Fatalf("同模板连续任务应轮换开场变体，实际都是 %q", first)
	}
}

func TestTemplateComplianceBlocksTask(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	template, err := app.SaveVideoTemplate(VideoTemplate{
		Name:    "违规模板",
		Version: "1.0",
		Scope:   TemplateScopePersonal,
		Opening: VideoTemplateOpening{Variants: []string{"全网最强的产品"}},
	})
	if err != nil {
		t.Fatalf("保存模板失败: %v", err)
	}

	_, err = app.CreateVideoTask(VideoTaskInput{
		Config: VideoRunConfig{
			Topic:       "测试主题",
			TemplateRef: &VideoTemplateRef{ID: template.ID},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "合规校验未通过") {
		t.Fatalf("命中违禁词的模板应阻止进入流水线，实际 %v", err)
	}
}

func TestVideoTemplateLifecycle(t *testing.T) {
	app := newTestApp(t)

	created, err := app.SaveVideoTemplate(VideoTemplate{
		Name:  "我的暖调口播",
		Scope: TemplateScopePersonal,
		Style: VideoTemplateStyle{Tokens: map[string]string{"lighting": "窗侧光"}},
	})
	if err != nil {
		t.Fatalf("新建模板失败: %v", err)
	}
	if created.ID == "" || created.Version != "1.0" {
		t.Fatalf("新建模板应补 ID 与默认版本: %+v", created)
	}

	created.Name = "我的暖调口播 v2"
	created.Version = "2.0"
	updated, err := app.SaveVideoTemplate(created)
	if err != nil {
		t.Fatalf("更新模板失败: %v", err)
	}
	if updated.Name != "我的暖调口播 v2" || updated.Version != "2.0" {
		t.Fatalf("模板更新未生效: %+v", updated)
	}

	builtin, err := app.videoTemplateByID("tpl_builtin_brand_warm")
	if err != nil {
		t.Fatalf("读取内置模板失败: %v", err)
	}
	builtin.Name = "改个名"
	if _, err := app.SaveVideoTemplate(builtin); err == nil {
		t.Fatal("内置模板不允许修改")
	}
	if err := app.DeleteVideoTemplate("tpl_builtin_brand_warm"); err == nil {
		t.Fatal("内置模板不允许删除")
	}
	if err := app.DeleteVideoTemplate(updated.ID); err != nil {
		t.Fatalf("未被引用的个人模板应可删除: %v", err)
	}
}

func TestTemplateInjectionIntoPromptAndLocalScript(t *testing.T) {
	app := newTestApp(t)

	template, err := app.videoTemplateByID("tpl_builtin_brand_warm")
	if err != nil {
		t.Fatalf("读取内置模板失败: %v", err)
	}
	cfg, _, err := normalizeVideoConfig(VideoRunConfig{Topic: "清晨手冲咖啡", DurationS: 24, AspectRatio: "9:16"})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	merged, run, _, err := app.applyTemplateRef(cfg, VideoTemplateRef{
		ID:          template.ID,
		OpeningVars: map[string]string{"brand": "晨光", "product": "手冲咖啡"},
	})
	if err != nil {
		t.Fatalf("解析模板失败: %v", err)
	}

	script := normalizeScript(localVideoScript(merged, run), merged, run)
	if len(script.Shots) == 0 {
		t.Fatal("本地分镜不应为空")
	}
	if script.Shots[0].Dialogue != run.OpeningLine {
		t.Fatalf("shot#1 台词应取自模板开场白: %q vs %q", script.Shots[0].Dialogue, run.OpeningLine)
	}
	if units := speechUnitCount(script.Shots[0].Dialogue, merged.Language); units > int(script.Shots[0].DurationS*merged.SpeechRate) {
		t.Fatalf("开场白超出 shot#1 语速预算: %d 字 / %.1fs", units, script.Shots[0].DurationS)
	}

	caps := defaultVideoCapabilities()
	caps.CharacterReference = false
	caps.PromptStyle = "kling"
	prompts, warnings := (&App{}).generatePrompts(t.Context(), merged, script, caps, "", run)
	if len(prompts) == 0 {
		t.Fatal("提示词不应为空")
	}
	if !strings.Contains(prompts[0].VisualPrompt, "风格参考") {
		t.Fatalf("提示词缺少模板风格片段: %q", prompts[0].VisualPrompt)
	}
	if !strings.Contains(prompts[0].VisualPrompt, "角色外观") {
		t.Fatalf("不支持主体参考的模型应注入外观描述块: %q", prompts[0].VisualPrompt)
	}
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning, "主体参考图") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应提示模型不支持主体参考图: %v", warnings)
	}
}

func TestTemplatePreviewRendersSamples(t *testing.T) {
	app := newTestApp(t)

	preview, err := app.PreviewVideoTemplate("tpl_builtin_knowledge_cool", "利率为什么影响房价", nil)
	if err != nil {
		t.Fatalf("预览模板失败: %v", err)
	}
	if preview.Opening == "" || !strings.Contains(preview.Opening, "利率为什么影响房价") {
		t.Fatalf("预览开场白应完成 topic 插值: %q", preview.Opening)
	}
	if preview.StyleSummary == "" || preview.StyleSnippet == "" {
		t.Fatalf("预览应展示风格摘要与风格片段: %+v", preview)
	}
	if len(preview.Characters) == 0 {
		t.Fatal("预览应展示角色图文包")
	}
	if preview.Subtitle == "" || preview.Publish == "" {
		t.Fatalf("预览应展示字幕样式与发布档: %+v", preview)
	}
}

// 老库（video_tasks 无 template_id、无 video_templates 表）启动时应能平滑升级。
func TestMigrateUpgradesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开临时数据库失败: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE video_tasks (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, stage TEXT NOT NULL, status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '', config_json TEXT NOT NULL, context_json TEXT NOT NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("构造老库失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO video_tasks (id, title, stage, status, error, config_json, context_json, created_at, updated_at)
		VALUES ('legacy', '老任务', 'DRAFT_SCRIPT', 'draft', '', '{}', '{}', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("写入老数据失败: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("老库迁移失败: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("迁移应可重复执行: %v", err)
	}

	var templateID string
	if err := db.QueryRow(`SELECT template_id FROM video_tasks WHERE id = 'legacy'`).Scan(&templateID); err != nil {
		t.Fatalf("template_id 列未添加: %v", err)
	}
	if templateID != "" {
		t.Fatalf("老任务应补空 template_id，实际 %q", templateID)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM video_templates`).Scan(&count); err != nil {
		t.Fatalf("模板表未创建: %v", err)
	}
	if count < 3 {
		t.Fatalf("内置模板应随迁移写入，当前 %d 个", count)
	}
}
