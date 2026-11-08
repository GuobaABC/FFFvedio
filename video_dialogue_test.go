package main

import (
	"context"
	"strings"
	"testing"
)

// Agent1 输出的 dialogueLines 必须被完整保留：多角色同段时不能退化成旁白。
func TestAgent1DialogueLinesPreserveMultipleSpeakers(t *testing.T) {
	server, _ := feedbackStub(t, `{"logline":"两人对话","shots":[{"id":"s1","durationS":6,"roles":["host","guest"],"sceneIntent":"两人在柜台前对话","dialogueLines":[{"speaker":"host","text":"欢迎光临"},{"speaker":"guest","text":"谢谢"}]}]}`)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "台词测试", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	cfg := speakerTestConfig()
	cfg.DurationS = 6

	script, warnings := app.generateScript(context.Background(), cfg, "")
	if len(script.Shots) != 1 {
		t.Fatalf("期望 1 段，得到 %d / %v", len(script.Shots), warnings)
	}
	lines := script.Shots[0].DialogueLines
	if len(lines) != 2 || lines[0].Speaker != "host" || lines[1].Speaker != "guest" {
		t.Fatalf("多说话人台词应按顺序保留，得到 %+v（warnings=%v）", lines, warnings)
	}
	if lines[0].Text != "欢迎光临" || lines[1].Text != "谢谢" {
		t.Fatalf("台词文本被改写: %+v", lines)
	}
	if script.Shots[0].Speaker != "" {
		t.Fatalf("多说话人时兼容字段 Speaker 应为空，得到 %q", script.Shots[0].Speaker)
	}
}

// 旧版单说话人 JSON 仍要兼容：speaker 字段不能再被解析层丢掉。
func TestAgent1LegacySpeakerStillBinds(t *testing.T) {
	server, _ := feedbackStub(t, `{"logline":"旁白收尾","shots":[{"id":"s1","durationS":6,"roles":["host"],"sceneIntent":"橘猫推门","dialogue":"今日开业","speaker":"narration"}]}`)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "台词测试", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	cfg := speakerTestConfig()
	cfg.DurationS = 6

	script, warnings := app.generateScript(context.Background(), cfg, "")
	if got := script.Shots[0].Speaker; got != VideoSpeakerNarration {
		t.Fatalf("单角色段落明确写旁白时不应被改判成角色，得到 %q（warnings=%v）", got, warnings)
	}
}

// 前缀 role_id 必须精确优先：char_12 不能被 char_1 的包含匹配抢走。
func TestSpeakerExactMatchBeatsPrefix(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "char_1", Name: "一号"},
		{RoleID: "char_12", Name: "十二号"},
	}}
	bound, warnings := bindSpeakers(VideoScript{Shots: []VideoShot{
		{ID: "s1", Roles: []string{"char_1", "char_12"}, Dialogue: "这句是十二号说的", Speaker: "char_12"},
	}}, cfg)
	if got := bound.Shots[0].Speaker; got != "char_12" {
		t.Fatalf("精确 role_id 应优先于前缀包含，得到 %q（warnings=%v）", got, warnings)
	}
}

// 角色名里带“解说/旁白”时仍然是角色在说话，不能被旁白别名抢走。
func TestNarrationAliasDoesNotStealCharacter(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "narrator", Name: "解说员"},
	}}
	bound, warnings := bindSpeakers(VideoScript{Shots: []VideoShot{
		{ID: "s1", Roles: []string{"narrator"}, Dialogue: "球进了", Speaker: "解说员"},
	}}, cfg)
	if got := bound.Shots[0].Speaker; got != "narrator" {
		t.Fatalf("角色名命中角色表时不应落到旁白，得到 %q（warnings=%v）", got, warnings)
	}
}

// 说话人块对所有视频模型都生效；多说话人要按顺序写清谁开口，旁白显式写无人开口。
// 无原生音频时不写台词原文，避免模型把台词画成画面文字。
func TestSpeakerBlockAppliesToEveryVideoModel(t *testing.T) {
	cfg := speakerTestConfig()
	segment := VideoSegment{
		Prompt: "推镜，橘猫与边牧在柜台前",
		DialogueLines: []VideoDialogueLine{
			{Speaker: "host", Text: "欢迎光临"},
			{Speaker: "guest", Text: "谢谢"},
			{Speaker: VideoSpeakerNarration, Text: "三个月后"},
		},
	}

	silent := composeRenderPrompt(segment, defaultVideoCapabilities(), cfg)
	for _, want := range []string{"【本段台词归属】", "主理人（橘猫）", "客人（边牧）", "旁白", "轮流", "画面内无人开口"} {
		if !strings.Contains(silent, want) {
			t.Fatalf("无原生音频模型也要带说话人块，缺少 %q：\n%s", want, silent)
		}
	}
	if strings.Contains(silent, "欢迎光临") {
		t.Fatalf("无原生音频模型不应把台词原文写进提示词：\n%s", silent)
	}

	caps := defaultVideoCapabilities()
	caps.NativeAudio = true
	voiced := composeRenderPrompt(segment, caps, cfg)
	for _, want := range []string{"欢迎光临", "谢谢", "三个月后", "语速约"} {
		if !strings.Contains(voiced, want) {
			t.Fatalf("原生音频模型应带上台词原文与语速，缺少 %q：\n%s", want, voiced)
		}
	}
}

// 多说话人时每个说话人的定妆图都要带上，不能只看兼容字段 Speaker。
func TestMultiSpeakerBringsAllCharacterImages(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{
		{RoleID: "host", Name: "主理人", Images: []string{"host.png"}},
		{RoleID: "guest", Name: "客人", Images: []string{"guest.png"}},
		{RoleID: "other", Name: "路人", Images: []string{"other.png"}},
	}}
	segment := VideoSegment{
		ShotID: "s1",
		DialogueLines: []VideoDialogueLine{
			{Speaker: "host", Text: "欢迎"},
			{Speaker: "guest", Text: "谢谢"},
		},
	}
	images, warning := characterImagesForSegment(cfg, &VideoScript{Shots: []VideoShot{{ID: "s1"}}}, segment)
	if warning != "" || len(images) != 2 {
		t.Fatalf("多说话人的定妆图应全部带上: %d 张 / %q", len(images), warning)
	}
}

// role_id 重复必须在创建任务时被拦截，否则台词归属永远绑不到确定的人。
func TestDuplicateRoleIDRejected(t *testing.T) {
	_, _, err := normalizeVideoConfig(VideoRunConfig{
		Topic: "重复角色",
		CharacterRefs: []VideoCharacterRef{
			{RoleID: "char_1", Name: "甲"},
			{RoleID: "char_1", Name: "乙"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 role_id 应直接报错，得到 %v", err)
	}
}

// 生成阶段改台词归属要真的生效，并同步回提示词关卡。
func TestSegmentEditRebindsMultiSpeaker(t *testing.T) {
	run := &VideoRunContext{
		Script: &VideoScript{Shots: []VideoShot{{ID: "s1", Roles: []string{"host", "guest"}}}},
		Prompts: []VideoPromptItem{{
			ShotID: "s1", VisualPrompt: "推镜", Dialogue: "欢迎光临", Speaker: "host",
			DialogueLines: []VideoDialogueLine{{Speaker: "host", Text: "欢迎光临"}},
		}},
		Plan: &VideoPlan{Segments: []VideoSegment{
			{ID: "s1-1", ShotID: "s1", Index: 0, Prompt: "推镜", Speaker: "host", Dialogue: "欢迎光临"},
			{ID: "s1-2", ShotID: "s1", Index: 1, DurationS: 5},
		}},
	}
	edit := VideoSegmentRegen{
		SegmentID:    "s1-1",
		VisualPrompt: "推镜",
		DialogueLines: []VideoDialogueLine{
			{Speaker: "host", Text: "欢迎光临"},
			{Speaker: "guest", Text: "谢谢"},
		},
	}
	if err := applySegmentEdit(run, 0, edit, speakerTestConfig()); err != nil {
		t.Fatalf("改归属失败: %v", err)
	}
	if got := run.Plan.Segments[0].Speaker; got != "" {
		t.Fatalf("多说话人时兼容字段应为空，得到 %q", got)
	}
	if len(run.Plan.Segments[0].DialogueLines) != 2 {
		t.Fatalf("片段应保留两行台词: %+v", run.Plan.Segments[0].DialogueLines)
	}
	if len(run.Prompts[0].DialogueLines) != 2 || run.Prompts[0].DialogueLines[1].Speaker != "guest" {
		t.Fatalf("归属没有同步回提示词关卡: %+v", run.Prompts[0].DialogueLines)
	}
}

// 长镜头第 2 段不允许单独填台词：否则会出现“有声音、无字幕”。
func TestSegmentEditRejectsDialogueOnLaterPart(t *testing.T) {
	run := &VideoRunContext{
		Script: &VideoScript{Shots: []VideoShot{{ID: "s1", Roles: []string{"host"}}}},
		Prompts: []VideoPromptItem{{ShotID: "s1", VisualPrompt: "推镜"}},
		Plan: &VideoPlan{Segments: []VideoSegment{
			{ID: "s1-1", ShotID: "s1", Index: 0},
			{ID: "s1-2", ShotID: "s1", Index: 1},
		}},
	}
	edit := VideoSegmentRegen{SegmentID: "s1-2", VisualPrompt: "推镜", Dialogue: "第二段台词"}
	if err := applySegmentEdit(run, 1, edit, speakerTestConfig()); err == nil {
		t.Fatal("第 2 段填台词应被拒绝")
	}
}