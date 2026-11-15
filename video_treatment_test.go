package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sequenceStub 按请求顺序返回不同 JSON；请求数超出后重复最后一条，便于测“修复重试”。
func sequenceStub(t *testing.T, contents ...string) (*httptest.Server, *[]string) {
	t.Helper()
	users := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		for _, message := range body.Messages {
			if message.Role != "user" {
				continue
			}
			if text, ok := message.Content.(string); ok {
				*users = append(*users, text)
			}
		}
		index := len(*users) - 1
		if index < 0 {
			index = 0
		}
		if index >= len(contents) {
			index = len(contents) - 1
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": contents[index]}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, string(payload))
	}))
	t.Cleanup(server.Close)
	return server, users
}

func treatmentTestConfig(topic string) VideoRunConfig {
	return VideoRunConfig{
		Topic:       topic,
		DurationS:   15,
		Fps:         24,
		AspectRatio: "9:16",
		SpeechRate:  6,
		Language:    "zh-CN",
		Subtitles:   true,
		Vertical:    "生活",
		Platforms:   []string{"douyin"},
		Adapter:     "mock",
	}
}

func TestShotCountRangeAllowsLongVideoFastCuts(t *testing.T) {
	cases := []struct {
		duration int
		low      int
		high     int
	}{
		{15, 2, 3},
		{30, 3, 6},
		{60, 6, 12},
		{90, 8, 16},
	}
	for _, item := range cases {
		cfg := VideoRunConfig{DurationS: item.duration}
		low, high := shotCountRange(cfg)
		if low != item.low || high != item.high {
			t.Fatalf("%ds 推荐镜头数 = %d-%d，期望 %d-%d", item.duration, low, high, item.low, item.high)
		}
	}
}

func TestTwoStageGenerationCreatesAnimalCastAndVoice(t *testing.T) {
	treatment := `{"cast":[{"roleId":"char_1","name":"猫老板","species":"真人","appearance":"白围裙橘猫","persona":"慢条斯理","voiceHint":""}],"premise":"免费面馆抢走生意，猫老板发现剩汤真相。","beats":[{"id":"b1","function":"hook","summary":"猫老板站在冷清店门口","newInfo":"对面免费面馆排长队","characters":["char_1"]},{"id":"b2","function":"turn","summary":"猫老板探到后厨","newInfo":"免费汤是剩汤兑的","characters":["char_1"]}],"continuity":["夜晚街边，同一家面馆"]}`
	storyboard := `{"logline":"真假慈善面馆","shots":[{"id":"s1","durationS":7,"beatId":"b1","function":"hook","newInfo":"对面排长队","roles":["char_1"],"sceneIntent":"夜晚街边，猫老板站在冷清面馆门口","dialogueLines":[{"speaker":"char_1","text":"对面免费吃面，我这三天没开张。"}],"camera":"中景缓慢平移","mood":"失落","audioCue":"油锅余响"},{"id":"s2","durationS":8,"beatId":"b2","function":"turn","newInfo":"免费汤是剩汤","roles":["char_1"],"sceneIntent":"后厨，猫老板探头看见剩汤","dialogueLines":[{"speaker":"char_1","text":"你这免费汤，是剩汤兑的吧？"}],"camera":"侧向推近","mood":"揭穿","audioCue":"汤桶咕嘟"}]}`
	server, users := sequenceStub(t, treatment, storyboard)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	script, warnings, err := app.generateScriptChecked(context.Background(), treatmentTestConfig("免费面馆真假慈善"), "")
	if err != nil {
		t.Fatalf("两阶段生成失败: %v（warnings=%v）", err, warnings)
	}
	if len(*users) < 2 {
		t.Fatalf("应调用企划和分镜两次，实际 %d 次", len(*users))
	}
	if script.Treatment == nil || len(script.Treatment.Beats) != 2 {
		t.Fatalf("企划产物缺失: %+v", script.Treatment)
	}
	if len(script.Treatment.Cast) != 1 {
		t.Fatalf("企划角色数不对: %+v", script.Treatment.Cast)
	}
	member := script.Treatment.Cast[0]
	if looksHumanSpecies(member.Species) {
		t.Fatalf("无图任务不应生成人类角色: %+v", member)
	}
	if strings.TrimSpace(member.VoiceHint) == "" {
		t.Fatal("自动角色必须绑定声音风格")
	}
	if script.Source != "llm" || len(script.Shots) != 2 {
		t.Fatalf("分镜结果不对: source=%s shots=%d", script.Source, len(script.Shots))
	}
	if !strings.Contains((*users)[1], "故事企划") || !strings.Contains((*users)[1], "声音风格") {
		t.Fatalf("分镜阶段输入应带企划与声音要求：\n%s", (*users)[1])
	}
}

func TestStrictBindingFailureDoesNotFallbackToLocal(t *testing.T) {
	treatment := `{"cast":[{"roleId":"char_1","name":"猫老板","species":"拟人化橘猫","appearance":"橘猫","voiceHint":"低沉温和的男声，中速"}],"premise":"面馆故事","beats":[{"id":"b1","function":"hook","summary":"开场","newInfo":"新信息"}]}`
	badStoryboard := `{"logline":"面馆故事","shots":[{"id":"s1","durationS":15,"beatId":"b1","roles":[],"sceneIntent":"猫老板站在门口","dialogueLines":[{"speaker":"cat_boss","text":"我这三天没开张。"}],"camera":"固定","mood":"失落","audioCue":"环境音"}]}`
	server, _ := sequenceStub(t, treatment, badStoryboard, badStoryboard)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	script, warnings, err := app.generateScriptChecked(context.Background(), treatmentTestConfig("面馆故事"), "")
	if !errors.Is(err, errScriptBinding) {
		t.Fatalf("未知 speaker 应硬失败，实际 %v（warnings=%v）", err, warnings)
	}
	if len(script.Shots) != 0 {
		t.Fatalf("硬失败不该返回可确认的剧本: %+v", script.Shots)
	}
	if strings.Contains(strings.Join(warnings, "\n"), "已回退本地规则") {
		t.Fatalf("绑定失败不能回退本地规则: %v", warnings)
	}
}

func TestValidateScriptBindingsRejectsEmptyAndUnknown(t *testing.T) {
	cfg := VideoRunConfig{CharacterRefs: []VideoCharacterRef{{RoleID: "char_1", Name: "猫老板"}}}
	script := &VideoScript{Shots: []VideoShot{
		{ID: "s1", Roles: []string{"char_1"}, DialogueLines: []VideoDialogueLine{{Speaker: "char_1", Text: "你好"}}},
		{ID: "s2", Roles: []string{}, DialogueLines: []VideoDialogueLine{{Speaker: "", Text: "空归属"}}},
		{ID: "s3", Roles: []string{}, DialogueLines: []VideoDialogueLine{{Speaker: "cat_boss", Text: "未知角色"}}},
	}}
	issues := validateScriptBindings(script, cfg)
	if len(issues) != 2 {
		t.Fatalf("应报 2 个错误，实际 %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0], "没有 speaker") || !strings.Contains(issues[1], "cat_boss") {
		t.Fatalf("错误信息不对: %v", issues)
	}
	if len(script.Shots[0].Roles) != 1 || script.Shots[0].Roles[0] != "char_1" {
		t.Fatalf("已知角色应自动补进 roles: %+v", script.Shots[0])
	}
}

func TestParseLockedScriptKeepsDialogueAndBuildsAnimalCast(t *testing.T) {
	topic := `【镜头1】
老周把体检报告拍在桌上。
老婆：
你倒是拆啊。
老周（手悬在半空）：
急什么。
【镜头2】
老周拆开信封。`
	parsed, err := parseLockedScript(topic)
	if err != nil {
		t.Fatalf("解析锁定剧本失败: %v", err)
	}
	if len(parsed.Lines) != 2 {
		t.Fatalf("台词行数不对: %+v", parsed.Lines)
	}
	if parsed.Lines[0].Text != "你倒是拆啊。" || parsed.Lines[1].Text != "急什么。" {
		t.Fatalf("锁定台词被改写: %+v", parsed.Lines)
	}
	if len(parsed.Cast) != 2 {
		t.Fatalf("应自动创建两个角色: %+v", parsed.Cast)
	}
	for _, member := range parsed.Cast {
		if looksHumanSpecies(member.Species) {
			t.Fatalf("无图任务不能创建人类角色: %+v", member)
		}
		if strings.TrimSpace(member.VoiceHint) == "" {
			t.Fatalf("锁定剧本角色也必须绑定声音: %+v", member)
		}
	}
	if !looksLikeLockedScript(topic) {
		t.Fatal("完整剧本应被识别为锁定模式")
	}
	if looksLikeLockedScript("橘猫在咖啡馆里打工，突然遇到一位神秘客人。") {
		t.Fatal("普通主题不应被误判成锁定剧本")
	}
}

func TestMergeTreatmentCastLimitsAndVoice(t *testing.T) {
	cfg := VideoRunConfig{Topic: "动物故事"}
	incoming := []VideoCastMember{
		{RoleID: "c1", Name: "甲", Species: "真人", VoiceHint: ""},
		{RoleID: "c2", Name: "乙", VoiceHint: ""},
		{RoleID: "c3", Name: "丙", VoiceHint: ""},
		{RoleID: "c4", Name: "丁", VoiceHint: ""},
		{RoleID: "c5", Name: "戊", VoiceHint: ""},
	}
	cast, warnings := mergeTreatmentCast(cfg, incoming)
	if len(cast) > 4 {
		t.Fatalf("自动角色上限应为 4，实际 %d", len(cast))
	}
	for _, member := range cast {
		if strings.TrimSpace(member.VoiceHint) == "" {
			t.Fatalf("角色缺少声音绑定: %+v", member)
		}
		if looksHumanSpecies(member.Species) {
			t.Fatalf("无图任务角色不应是人类: %+v", member)
		}
	}
	if len(warnings) == 0 {
		t.Fatal("补默认声音/改动物应留下警告")
	}
}

func TestCharacterVoiceDefaultOnNormalize(t *testing.T) {
	cfg, warnings, err := normalizeVideoConfig(VideoRunConfig{
		Topic:         "角色声音",
		DurationS:     10,
		CharacterRefs: []VideoCharacterRef{{RoleID: "host", Name: "主理人"}},
	})
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	if strings.TrimSpace(cfg.CharacterRefs[0].VoiceHint) == "" {
		t.Fatal("角色必须自动补声音风格")
	}
	if len(warnings) == 0 {
		t.Fatal("补默认声音应提示用户")
	}
}

func TestBindingRepairKeepsTreatment(t *testing.T) {
	treatment := `{"cast":[{"roleId":"char_1","name":"猫老板","species":"橘猫","appearance":"橘猫","voiceHint":"低沉温和的男声，中速"}],"premise":"面馆故事","beats":[{"id":"b1","function":"hook","summary":"开场","newInfo":"新信息"}]}`
	bad := `{"logline":"面馆故事","shots":[{"id":"s1","durationS":15,"beatId":"b1","roles":[],"sceneIntent":"猫老板站在门口","dialogueLines":[{"speaker":"cat_boss","text":"我这三天没开张。"}],"camera":"固定","mood":"失落","audioCue":"环境音"}]}`
	good := `{"logline":"面馆故事","shots":[{"id":"s1","durationS":15,"beatId":"b1","roles":["char_1"],"sceneIntent":"猫老板站在门口","dialogueLines":[{"speaker":"char_1","text":"我这三天没开张。"}],"camera":"固定","mood":"失落","audioCue":"环境音"}]}`
	server, users := sequenceStub(t, treatment, bad, good)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	script, _, err := app.generateScriptChecked(context.Background(), treatmentTestConfig("面馆故事"), "")
	if err != nil {
		t.Fatalf("修复后应成功: %v", err)
	}
	if len(*users) != 3 {
		t.Fatalf("应调用企划、分镜、修复各一次，实际 %d", len(*users))
	}
	if script.Treatment == nil || len(script.Treatment.Beats) != 1 || script.Mode != VideoModeOriginal {
		t.Fatalf("修复重试后企划/模式丢失: treatment=%+v mode=%s", script.Treatment, script.Mode)
	}
	if len(script.Shots[0].Roles) != 1 || script.Shots[0].Roles[0] != "char_1" {
		t.Fatalf("修复后的角色绑定未保留: %+v", script.Shots[0])
	}
}

func TestRunScriptStageSyncsAutoCastToTask(t *testing.T) {
	withTempVideoRoot(t)
	treatment := `{"cast":[{"roleId":"char_1","name":"猫老板","species":"橘猫","appearance":"白围裙橘猫","voiceHint":"低沉温和的男声，中速"}],"premise":"面馆故事","beats":[{"id":"b1","function":"hook","summary":"开场","newInfo":"新信息"}]}`
	storyboard := `{"logline":"面馆故事","shots":[{"id":"s1","durationS":15,"beatId":"b1","roles":["char_1"],"sceneIntent":"猫老板站在门口","dialogueLines":[{"speaker":"char_1","text":"我这三天没开张。"}],"camera":"固定","mood":"失落","audioCue":"环境音"}]}`
	server, _ := sequenceStub(t, treatment, storyboard)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	created, err := app.CreateVideoTask(VideoTaskInput{Config: treatmentTestConfig("面馆故事")})
	if err != nil {
		t.Fatalf("新建任务失败: %v", err)
	}
	app.runVideoPipeline(created.ID, VideoStageDraftScript, "")
	final, err := app.videoTaskByID(created.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if final.Status == VideoStatusFailed {
		t.Fatalf("流水线失败: %s（warnings=%v）", final.Error, final.Run.Warnings)
	}
	if final.Stage != VideoStageConfirmScript {
		t.Fatalf("应停在分镜关卡，实际 %s", final.Stage)
	}
	if final.Run.Script == nil || final.Run.Script.Treatment == nil {
		t.Fatalf("任务应保存企划产物: %+v", final.Run.Script)
	}
	if len(final.Config.CharacterRefs) != 1 || !final.Config.CharacterRefs[0].AutoCreated {
		t.Fatalf("自动角色应同步到任务配置: %+v", final.Config.CharacterRefs)
	}
	if strings.TrimSpace(final.Config.CharacterRefs[0].VoiceHint) == "" {
		t.Fatal("自动角色必须带声音风格")
	}
}

func TestMergeTreatmentCastKeepsAutoFlagOnRerun(t *testing.T) {
	cfg := VideoRunConfig{Topic: "重跑", CharacterRefs: []VideoCharacterRef{{
		RoleID: "char_1", Name: "猫老板", Species: "橘猫", VoiceHint: "低沉男声，中速", AutoCreated: true,
	}}}
	incoming := []VideoCastMember{{RoleID: "char_2", Name: "猪老板", VoiceHint: "小猪嗓，快速"}}
	cast, _ := mergeTreatmentCast(cfg, incoming)
	if len(cast) != 2 {
		t.Fatalf("自动角色重跑应保留旧角色并允许新增: %+v", cast)
	}
	for _, member := range cast {
		if !member.AutoCreated {
			t.Fatalf("重跑后自动角色标记丢失: %+v", member)
		}
	}
}

func TestMergeTreatmentCastStopsAtOneExtraWhenUserCastExists(t *testing.T) {
	cfg := VideoRunConfig{Topic: "用户角色", CharacterRefs: []VideoCharacterRef{
		{RoleID: "host", Name: "主理人", VoiceHint: "温和女声，中速"},
	}}
	incoming := []VideoCastMember{
		{RoleID: "guest", Name: "嘉宾", VoiceHint: "低沉男声"},
		{RoleID: "extra", Name: "路人", VoiceHint: "中性声"},
	}
	cast, _ := mergeTreatmentCast(cfg, incoming)
	if len(cast) != 2 {
		t.Fatalf("有用户角色时最多补 1 个临时角色，实际 %d: %+v", len(cast), cast)
	}
}

func TestNormalizeAutoCastIdentityUsesNameOverSpecies(t *testing.T) {
	cfg := treatmentTestConfig("动物面馆")
	pig := VideoCastMember{Name: "猪老板", Species: "橘猫", Appearance: "中年家猪，灰白围裙、圆框老花镜", VoiceHint: "中年沙哑男声，语速偏快"}
	warnings := normalizeAutoCastIdentity(cfg, &pig)
	if pig.Species != "小猪" {
		t.Fatalf("名字里的猪应优先于错误物种，实际 %q（warnings=%v）", pig.Species, warnings)
	}
	if !strings.Contains(pig.Appearance, "家猪") {
		t.Fatalf("外观本来就与名字一致，不应被重写: %q", pig.Appearance)
	}
	dog := VideoCastMember{Name: "狗老板", Species: "布偶猫", Appearance: "黄白毛色田园犬，白色厨师服", VoiceHint: "清亮女声"}
	normalizeAutoCastIdentity(cfg, &dog)
	if dog.Species != "柴犬" {
		t.Fatalf("名字里的狗应优先于错误物种，实际 %q", dog.Species)
	}
}

func TestNormalizeAutoCastIdentityRewritesHumanAppearance(t *testing.T) {
	cfg := treatmentTestConfig("动物面馆")
	member := VideoCastMember{Name: "猫老板", Species: "人类", Appearance: "中年男人，穿着围裙", VoiceHint: "低沉男声"}
	warnings := normalizeAutoCastIdentity(cfg, &member)
	if member.Species != "橘猫" {
		t.Fatalf("名字里的猫应把人类角色改成橘猫，实际 %q（warnings=%v）", member.Species, warnings)
	}
	if strings.Contains(member.Appearance, "男人") || strings.Contains(member.Appearance, "人类") {
		t.Fatalf("人类外观描述应被重写: %q", member.Appearance)
	}
}

func TestAlignVisualSpeciesFixesSingleRolePrompt(t *testing.T) {
	profile := VideoCharacterRef{Name: "猪老板", Species: "小猪", AppearanceBlock: "中年家猪，灰白围裙"}
	got := alignVisualSpecies("圆框老花镜橘猫侧脸开口，手扶窗框", profile)
	if strings.Contains(got, "橘猫") || !strings.Contains(got, "小猪") {
		t.Fatalf("单角色画面应改成 cast 物种: %q", got)
	}
}

func TestValidateLockedLinesRejectsRewrite(t *testing.T) {
	locked := []VideoLockedLine{{ID: "L1", Speaker: "auto_1", Text: "你倒是拆啊。"}}
	script := VideoScript{Shots: []VideoShot{{ID: "s1", DialogueLines: []VideoDialogueLine{{Speaker: "auto_1", Text: "你拆开看看。"}}}}}
	issues := validateLockedLines(script, locked)
	if len(issues) == 0 || !strings.Contains(issues[0], "被改写") {
		t.Fatalf("锁定台词被改写应报错: %v", issues)
	}
}

func TestLockedScriptGenerationKeepsExactDialogue(t *testing.T) {
	topic := `【镜头1】
老周把体检报告拍在桌上。
老婆：
你倒是拆啊。
老周（手悬在半空）：
急什么。
【镜头2】
老周拆开信封。`
	storyboard := `{"logline":"体检报告","shots":[{"id":"s1","durationS":7,"beatId":"b1","function":"hook","roles":["auto_1"],"sceneIntent":"餐桌，老周把体检报告拍在桌上","dialogueLines":[{"speaker":"auto_1","text":"你倒是拆啊。"}],"camera":"中景固定","mood":"克制","audioCue":"纸张声"},{"id":"s2","durationS":8,"beatId":"b2","function":"payoff","roles":["auto_2"],"sceneIntent":"老周拆开信封，眉头凝固","dialogueLines":[{"speaker":"auto_2","text":"急什么。"}],"camera":"缓慢推近","mood":"紧张","audioCue":"拆纸声"}]}`
	server, users := sequenceStub(t, storyboard)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	cfg := treatmentTestConfig(topic)
	cfg.ScriptLocked = true
	script, warnings, err := app.generateScriptChecked(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("锁定剧本生成失败: %v（warnings=%v）", err, warnings)
	}
	if len(*users) != 1 {
		t.Fatalf("锁定模式应只调用分镜阶段一次，实际 %d 次", len(*users))
	}
	if script.Source != "llm_locked" || script.Treatment == nil || script.Treatment.Mode != VideoScriptModeLocked {
		t.Fatalf("锁定模式标记不对: source=%s treatment=%+v", script.Source, script.Treatment)
	}
	if len(script.Treatment.LockedLines) != 2 {
		t.Fatalf("锁定台词数量不对: %+v", script.Treatment.LockedLines)
	}
	if script.Treatment.LockedLines[0].Text != "你倒是拆啊。" || script.Treatment.LockedLines[1].Text != "急什么。" {
		t.Fatalf("锁定台词被改写: %+v", script.Treatment.LockedLines)
	}
	for _, member := range script.Treatment.Cast {
		if strings.TrimSpace(member.VoiceHint) == "" || looksHumanSpecies(member.Species) {
			t.Fatalf("锁定剧本角色必须是带声音的动物: %+v", member)
		}
	}
}

func TestLockedScriptRewriteIsBlocked(t *testing.T) {
	topic := `【镜头1】
老婆：你倒是拆啊。
老周：急什么。`
	rewritten := `{"logline":"体检报告","shots":[{"id":"s1","durationS":15,"beatId":"b1","roles":["auto_1"],"sceneIntent":"餐桌","dialogueLines":[{"speaker":"auto_1","text":"你拆开看看。"}],"camera":"固定","mood":"克制","audioCue":"环境音"}]}`
	server, _ := sequenceStub(t, rewritten, rewritten)
	app := newTestApp(t)
	if _, err := app.SaveProvider(Provider{Category: CategoryText, Name: "测试文本", BaseURL: server.URL, APIKey: "sk-test", Model: "test-model"}); err != nil {
		t.Fatalf("登记提供商失败: %v", err)
	}
	cfg := treatmentTestConfig(topic)
	cfg.ScriptLocked = true
	_, warnings, err := app.generateScriptChecked(context.Background(), cfg, "")
	if !errors.Is(err, errScriptBinding) {
		t.Fatalf("锁定台词被改写应硬失败，实际 %v（warnings=%v）", err, warnings)
	}
	if strings.Contains(strings.Join(warnings, "\n"), "已回退本地规则") {
		t.Fatalf("锁定台词失败不能回退本地规则: %v", warnings)
	}
}
