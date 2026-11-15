package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func createMemoryTestProject(t *testing.T, app *App) VideoProject {
	t.Helper()
	project, err := app.CreateVideoProject("短剧项目")
	if err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}
	return project
}

func createMemoryTestTask(t *testing.T, app *App, projectID string, episode int) VideoTask {
	t.Helper()
	task, err := app.CreateVideoTask(VideoTaskInput{
		Title:     "第" + string(rune('0'+episode)) + "集",
		ProjectID: projectID,
		Episode:   episode,
		Config: VideoRunConfig{
			Topic:      "橘猫面馆真假慈善",
			DurationS:  15,
			Language:   "zh-CN",
			SpeechRate: 4,
		},
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	task.Run.Script = &VideoScript{
		Logline: "猫老板揭穿假慈善",
		Treatment: &VideoTreatment{
			Mode:    VideoModeOriginal,
			Premise: "免费面馆抢走生意，猫老板发现剩汤真相。",
			Cast: []VideoCastMember{{
				RoleID: "char_1", Name: "猪老板", Species: "小猪", Appearance: "灰白围裙的小猪", Persona: "精明护店", VoiceHint: "沙哑男声，语速偏快",
			}},
			Beats: []VideoBeat{{
				ID: "b1", Function: "hook", Summary: "猫老板站在冷清店门口",
				NewInfo: "对面免费面馆排长队", Characters: []string{"char_1"},
			}},
			Continuity: []string{"夜晚街边，同一家面馆"},
		},
	}
	task.Run.PublishMeta = &PublishMeta{
		Title:       PublishTitle{Primary: "真假慈善面馆"},
		Description: "猫老板揭穿免费面馆的剩汤生意",
		Tags:        PublishTags{Hashtags: []string{"#橘猫", "#短剧"}},
	}
	if err := app.updateVideoTaskRecord(task); err != nil {
		t.Fatalf("保存测试任务失败: %v", err)
	}
	return task
}

func TestProjectMemoryRebuildAndSearch(t *testing.T) {
	app := newTestApp(t)
	project := createMemoryTestProject(t, app)
	createMemoryTestTask(t, app, project.ID, 1)

	count, err := app.RebuildProjectMemory(project.ID)
	if err != nil {
		t.Fatalf("重建项目记忆失败: %v", err)
	}
	if count == 0 {
		t.Fatal("项目记忆应至少写入摘要/角色/剧情")
	}
	memories, err := app.SearchProjectMemory(project.ID, "猪老板", 10)
	if err != nil {
		t.Fatalf("检索项目记忆失败: %v", err)
	}
	foundCharacter := false
	for _, memory := range memories {
		if memory.Kind == ProjectMemoryKindCharacter && strings.Contains(memory.Content, "猪老板") {
			foundCharacter = true
		}
	}
	if !foundCharacter {
		t.Fatalf("关键词检索应命中角色记忆: %+v", memories)
	}
}

func TestSearchProjectMemoryFallsBackToRecentWhenNoHit(t *testing.T) {
	app := newTestApp(t)
	project := createMemoryTestProject(t, app)
	createMemoryTestTask(t, app, project.ID, 1)
	if _, err := app.RebuildProjectMemory(project.ID); err != nil {
		t.Fatalf("重建项目记忆失败: %v", err)
	}
	memories, err := app.SearchProjectMemory(project.ID, "完全无关的词", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(memories) == 0 {
		t.Fatal("无命中时应回退到最近的项目记忆")
	}
}

func TestNextProjectEpisode(t *testing.T) {
	app := newTestApp(t)
	project := createMemoryTestProject(t, app)
	if next, err := app.NextVideoEpisode(project.ID); err != nil || next != 1 {
		t.Fatalf("空项目下一集应为 1: next=%d err=%v", next, err)
	}
	createMemoryTestTask(t, app, project.ID, 1)
	if next, err := app.NextVideoEpisode(project.ID); err != nil || next != 2 {
		t.Fatalf("已有第 1 集时下一集应为 2: next=%d err=%v", next, err)
	}
}

func TestProjectMemoryEmbeddingAndSemanticSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Input []string `json:"input"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		data := make([]any, 0, len(body.Input))
		for range body.Input {
			data = append(data, map[string]any{"embedding": []float64{1, 0, 0}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)

	app := newTestApp(t)
	project := createMemoryTestProject(t, app)
	createMemoryTestTask(t, app, project.ID, 1)
	if _, err := app.RebuildProjectMemory(project.ID); err != nil {
		t.Fatalf("重建项目记忆失败: %v", err)
	}
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "embedding-test"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	embedded, err := app.EmbedProjectMemory(project.ID)
	if err != nil {
		t.Fatalf("补 embedding 失败: %v", err)
	}
	if embedded == 0 {
		t.Fatal("应至少写入一条 embedding")
	}
	memories, err := app.SearchProjectMemory(project.ID, "完全不同的查询", 10)
	if err != nil {
		t.Fatalf("语义检索失败: %v", err)
	}
	if len(memories) == 0 {
		t.Fatal("语义检索应命中项目记忆")
	}
}
