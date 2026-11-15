package main

import (
	"strings"
	"testing"
)

// 角色库要能新建、更新、列出、删除；删掉后已经创建的任务仍持有快照。
func TestVideoCharacterLibraryCRUD(t *testing.T) {
	app := newTestApp(t)

	created, err := app.SaveVideoCharacter(VideoCharacter{
		Name:            "橘座",
		RoleID:          "host",
		AppearanceBlock: "成年橘色短毛猫，圆脸，耳尖一小撮白毛",
		Persona:         "慢热、嘴硬心软，说话短句",
		NegativePrompt:  "不要出现项圈铃铛",
		VoiceHint:       "温暖中音，4.2 字/秒",
		VoiceProfile:    &VideoVoiceProfile{VoiceID: "warm-male", Rate: 1.1, Emotion: "calm"},
		Images:          []string{"https://example.com/host.png", "https://example.com/host.png"},
		Tags:            []string{"主角", "猫"},
		Note:            "咖啡馆系列固定主角",
	})
	if err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	if created.ID == "" || created.RoleID != "host" || created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("角色保存结果不完整: %+v", created)
	}
	if len(created.Images) != 1 {
		t.Fatalf("角色参考图应去重: %+v", created.Images)
	}

	list, err := app.ListVideoCharacters()
	if err != nil || len(list) != 1 || list[0].Name != "橘座" {
		t.Fatalf("角色库列表异常: %v %+v", err, list)
	}

	created.Name = "橘座（新版）"
	created.Persona = "更话痨一点"
	updated, err := app.SaveVideoCharacter(created)
	if err != nil {
		t.Fatalf("更新角色失败: %v", err)
	}
	if updated.ID != created.ID || updated.CreatedAt != created.CreatedAt || updated.Name != "橘座（新版）" {
		t.Fatalf("角色更新应保留 ID / 创建时间: %+v", updated)
	}

	if err := app.DeleteVideoCharacter(created.ID); err != nil {
		t.Fatalf("删除角色失败: %v", err)
	}
	if list, _ := app.ListVideoCharacters(); len(list) != 0 {
		t.Fatalf("删除后角色库应为空: %+v", list)
	}

	if _, err := app.SaveVideoCharacter(VideoCharacter{AppearanceBlock: "没有名字"}); err == nil ||
		!strings.Contains(err.Error(), "角色名称") {
		t.Fatalf("空角色名应报错: %v", err)
	}
}

// 任务引用角色库时要把档案与默认参考图快照进去；之后删库也不影响任务复现。
func TestCreateVideoTaskSnapshotsCharacterLibrary(t *testing.T) {
	withTempVideoRoot(t)
	app := newTestApp(t)

	character, err := app.SaveVideoCharacter(VideoCharacter{
		Name:            "橘座",
		RoleID:          "host",
		AppearanceBlock: "成年橘色短毛猫，圆脸，耳尖一小撮白毛",
		Persona:         "慢热、嘴硬心软",
		NegativePrompt:  "不要出现项圈铃铛",
		VoiceHint:       "温暖中音",
		Images:          []string{"https://example.com/host.png"},
	})
	if err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}

	task, err := app.CreateVideoTask(VideoTaskInput{Config: VideoRunConfig{
		Topic:     "角色库复用",
		DurationS: 6,
		CharacterRefs: []VideoCharacterRef{{
			CharacterID: character.ID,
		}},
	}})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if len(task.Config.CharacterRefs) != 1 {
		t.Fatalf("任务应保留一个角色参考: %+v", task.Config.CharacterRefs)
	}
	ref := task.Config.CharacterRefs[0]
	if ref.RoleID != "host" || ref.Name != "橘座" {
		t.Fatalf("角色库身份没快照进任务: %+v", ref)
	}
	if ref.AppearanceBlock != character.AppearanceBlock || ref.Persona != character.Persona ||
		ref.NegativePrompt != character.NegativePrompt || ref.VoiceHint != character.VoiceHint {
		t.Fatalf("角色档案没快照进任务: %+v", ref)
	}
	if len(ref.Images) != 1 || ref.ImageSource != CharacterImageSourceUser {
		t.Fatalf("角色库默认参考图应带进任务并按用户图优先: %+v", ref)
	}

	// 删除角色库条目后，旧任务仍应正常加载并保留档案
	if err := app.DeleteVideoCharacter(character.ID); err != nil {
		t.Fatalf("删除角色失败: %v", err)
	}
	loaded, err := app.videoTaskByID(task.ID)
	if err != nil {
		t.Fatalf("删除角色后任务应仍可加载: %v", err)
	}
	if loaded.Config.CharacterRefs[0].AppearanceBlock != character.AppearanceBlock {
		t.Fatalf("任务快照不应受角色库删除影响: %+v", loaded.Config.CharacterRefs[0])
	}
}

// 外观锚点要在 Agent1 / Agent2 / 本地兜底三处保持一致，且不依赖模板。
func TestCharacterAppearanceAnchorAcrossAgentsAndLocalFallback(t *testing.T) {
	cfg := VideoRunConfig{
		Topic:     "角色一致性",
		DurationS: 6,
		CharacterRefs: []VideoCharacterRef{{
			RoleID:          "host",
			Name:            "橘座",
			AppearanceBlock: "成年橘色短毛猫，圆脸，耳尖一小撮白毛",
			Persona:         "慢热、嘴硬心软",
			NegativePrompt:  "不要出现项圈铃铛",
			VoiceHint:       "温暖中音",
		}},
	}
	caps := defaultVideoCapabilities()
	caps.CharacterReference = false

	scriptPrompt := buildScriptUserPrompt(cfg, nil)
	promptPrompt := buildPromptUserPrompt(cfg, "[]", caps, nil)
	for name, prompt := range map[string]string{"Agent1": scriptPrompt, "Agent2": promptPrompt} {
		for _, want := range []string{"外观锚点：成年橘色短毛猫", "人设/口吻：慢热", "角色禁止：不要出现项圈铃铛", "声音：温暖中音"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s 输入缺少角色档案 %q：\n%s", name, want, prompt)
			}
		}
	}

	shot := VideoShot{ID: "s1", Roles: []string{"host"}, SceneIntent: "推门进店"}
	visual := applyTemplatePromptExtras(localVisualPrompt(cfg, shot, caps), shot, caps, nil, cfg)
	if !strings.Contains(visual, "角色外观（橘座）：成年橘色短毛猫") {
		t.Fatalf("本地兜底也要注入同一外观锚点: %s", visual)
	}
}

// 模板角色应映射到统一角色档案，外观只在角色表输出一次，不再由模板提示重复。
func TestTemplateCharactersMapToUnifiedProfile(t *testing.T) {
	app := newTestApp(t)
	tpl := testTemplate(t, app)
	if len(tpl.Characters) == 0 {
		t.Fatal("测试模板应带角色")
	}

	merged, _, _ := applyVideoTemplate(&tpl, VideoRunConfig{Topic: "模板角色映射", DurationS: 12}, nil)
	if len(merged.CharacterRefs) == 0 {
		t.Fatalf("模板角色应带入任务角色参考: %+v", merged.CharacterRefs)
	}
	ref := merged.CharacterRefs[0]
	if ref.AppearanceBlock != tpl.Characters[0].AppearanceBlock || ref.Perspective != tpl.Characters[0].Perspective {
		t.Fatalf("模板角色档案没映射到统一结构: %+v", ref)
	}

	run := &VideoTemplateRun{VideoTemplate: tpl}
	table := characterTable(merged, run)
	if count := strings.Count(table, tpl.Characters[0].AppearanceBlock); count != 1 {
		t.Fatalf("外观锚点应只出现一次，实际 %d 次：\n%s", count, table)
	}
	if hints := templateScriptHints(run, merged); strings.Contains(hints, tpl.Characters[0].AppearanceBlock) {
		t.Fatalf("模板提示不应重复角色外观：\n%s", hints)
	}
}

// 旧任务数据没有新字段也要能正常反序列化与加载。
func TestLegacyTaskWithoutCharacterProfileLoads(t *testing.T) {
	app := newTestApp(t)

	const configJSON = `{"topic":"旧任务","durationS":6,"characterRefs":[{"roleId":"host","name":"主理人","images":["https://example.com/host.png"],"imageSource":"user","voiceHint":"低沉男声"}]}`
	if _, err := app.db.Exec(
		`INSERT INTO video_tasks (`+videoTaskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"vt-legacy-character", "旧任务", VideoStageDraftScript, VideoStatusDraft, "",
		configJSON, `{"adapter":"mock","gates":{}}`, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z", "", "", 0,
	); err != nil {
		t.Fatalf("构造旧任务失败: %v", err)
	}

	task, err := app.videoTaskByID("vt-legacy-character")
	if err != nil {
		t.Fatalf("旧任务应可正常加载: %v", err)
	}
	ref := task.Config.CharacterRefs[0]
	if ref.CharacterID != "" || ref.AppearanceBlock != "" || ref.Persona != "" {
		t.Fatalf("旧任务不应凭空多出新档案字段: %+v", ref)
	}
	if table := characterTable(task.Config, nil); !strings.Contains(table, "形象：用户上传的定妆图") {
		t.Fatalf("旧任务角色表应保持原行为: %s", table)
	}
}
