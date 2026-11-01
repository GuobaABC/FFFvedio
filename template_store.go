package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// video_templates：模板实体落库。payload_json 存完整模板，便于版本快照与前端直接消费；
// name / scope / tags 单独成列，供列表检索（§6 模板库可检索）。
const videoTemplateColumns = `id, name, version, scope, tags_json, payload_json, created_at, updated_at`

func encodeTemplateJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("序列化模板失败: %w", err)
	}
	return string(data), nil
}

// ListVideoTemplates 返回模板库（内置在前，其余按更新时间倒序）。
func (a *App) ListVideoTemplates() ([]VideoTemplate, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	rows, err := a.db.Query(`SELECT ` + videoTemplateColumns + ` FROM video_templates`)
	if err != nil {
		return nil, fmt.Errorf("读取模板列表失败: %w", err)
	}
	defer rows.Close()

	templates := make([]VideoTemplate, 0, 8)
	for rows.Next() {
		var id, name, version, scope, tagsJSON, payloadJSON, createdAt, updatedAt string
		if err := rows.Scan(&id, &name, &version, &scope, &tagsJSON, &payloadJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("解析模板行失败: %w", err)
		}
		template, err := decodeVideoTemplate(payloadJSON)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历模板失败: %w", err)
	}

	sort.SliceStable(templates, func(i, j int) bool {
		left, right := templateScopeRank(templates[i].Scope), templateScopeRank(templates[j].Scope)
		if left != right {
			return left < right
		}
		if templates[i].UpdatedAt != templates[j].UpdatedAt {
			return templates[i].UpdatedAt > templates[j].UpdatedAt
		}
		return templates[i].Name < templates[j].Name
	})
	return templates, nil
}

func templateScopeRank(scope string) int {
	switch scope {
	case TemplateScopeBuiltin:
		return 0
	case TemplateScopeTeam:
		return 1
	default:
		return 2
	}
}

func decodeVideoTemplate(payload string) (VideoTemplate, error) {
	var template VideoTemplate
	if err := json.Unmarshal([]byte(payload), &template); err != nil {
		return template, fmt.Errorf("解析模板内容失败: %w", err)
	}
	return template, nil
}

// GetVideoTemplate 读取单个模板。
func (a *App) GetVideoTemplate(id string) (VideoTemplate, error) {
	return a.videoTemplateByID(id)
}

func (a *App) videoTemplateByID(id string) (VideoTemplate, error) {
	if a.db == nil {
		return VideoTemplate{}, errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return VideoTemplate{}, errors.New("模板 ID 不能为空")
	}
	row := a.db.QueryRow(`SELECT payload_json FROM video_templates WHERE id = ?`, id)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VideoTemplate{}, errors.New("模板不存在（可能已被删除）")
		}
		return VideoTemplate{}, fmt.Errorf("读取模板失败: %w", err)
	}
	return decodeVideoTemplate(payload)
}

// SaveVideoTemplate 新建或更新模板；内置模板只读，需要改动请「另存为」。
func (a *App) SaveVideoTemplate(input VideoTemplate) (VideoTemplate, error) {
	if a.db == nil {
		return VideoTemplate{}, errors.New("数据库未初始化")
	}
	template, err := normalizeVideoTemplate(input)
	if err != nil {
		return VideoTemplate{}, err
	}
	if template.Scope == TemplateScopeBuiltin {
		return VideoTemplate{}, errors.New("内置作用域由系统保留：请另存为个人或团队模板")
	}

	now := time.Now().Format(time.RFC3339)
	existing, err := a.videoTemplateByID(template.ID)
	switch {
	case err == nil:
		if existing.Scope == TemplateScopeBuiltin {
			return VideoTemplate{}, errors.New("内置模板不可修改：请复制为自己的模板再调整")
		}
		template.CreatedAt = firstNonEmpty(existing.CreatedAt, now)
		template.UpdatedAt = now
	case strings.Contains(err.Error(), "模板不存在"):
		template.CreatedAt = now
		template.UpdatedAt = now
	default:
		return VideoTemplate{}, err
	}

	payload, err := encodeTemplateJSON(template)
	if err != nil {
		return VideoTemplate{}, err
	}
	tagsJSON, err := encodeTemplateJSON(template.Tags)
	if err != nil {
		return VideoTemplate{}, err
	}
	if _, err := a.db.Exec(
		`INSERT INTO video_templates (`+videoTemplateColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, version = excluded.version,
		 scope = excluded.scope, tags_json = excluded.tags_json, payload_json = excluded.payload_json,
		 updated_at = excluded.updated_at`,
		template.ID, template.Name, template.Version, template.Scope, tagsJSON, payload, template.CreatedAt, template.UpdatedAt,
	); err != nil {
		return VideoTemplate{}, fmt.Errorf("保存模板失败: %w", err)
	}
	return template, nil
}

// DeleteVideoTemplate 删除模板；内置模板与正在被任务引用的模板不删。
func (a *App) DeleteVideoTemplate(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	template, err := a.videoTemplateByID(id)
	if err != nil {
		return err
	}
	if template.Scope == TemplateScopeBuiltin {
		return errors.New("内置模板不可删除")
	}
	if count := a.videoTemplateUsageCount(id); count > 0 {
		return fmt.Errorf("已有 %d 个任务引用该模板，无法删除（任务内的版本快照仍保证可复现）", count)
	}
	if _, err := a.db.Exec(`DELETE FROM video_templates WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除模板失败: %w", err)
	}
	return nil
}

// PreviewVideoTemplate 选模板时渲染「样例开场白 + 风格片段 + 字幕/发布档」预览（§6）。
func (a *App) PreviewVideoTemplate(templateID, topic string, vars map[string]string) (VideoTemplatePreview, error) {
	template, err := a.videoTemplateByID(templateID)
	if err != nil {
		return VideoTemplatePreview{}, err
	}
	ref := VideoTemplateRef{ID: template.ID, OpeningVars: vars}
	cfg := VideoRunConfig{
		Topic:     strings.TrimSpace(topic),
		DurationS: template.Defaults.DurationS,
		Vertical:  template.PublishDefaults.Vertical,
		Platforms: template.PublishDefaults.Platforms,
	}
	values := templateVarValues(cfg, ref)
	if values["topic"] == "" {
		values["topic"] = "本次主题"
	}
	return buildVideoTemplatePreview(&template, values), nil
}

// videoTemplateUsageCount 统计引用次数，用于开场白变体轮换（多样性护栏）。
func (a *App) videoTemplateUsageCount(templateID string) int {
	if a.db == nil || strings.TrimSpace(templateID) == "" {
		return 0
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM video_tasks WHERE template_id = ?`, templateID).Scan(&count); err != nil {
		return 0
	}
	return count
}

// applyTemplateRef 解析 template_ref：合并配置、固化快照、合规拦截。
func (a *App) applyTemplateRef(cfg VideoRunConfig, ref VideoTemplateRef) (VideoRunConfig, *VideoTemplateRun, []string, error) {
	warnings := make([]string, 0, 3)
	ref.ID = strings.TrimSpace(ref.ID)
	if ref.ID == "" {
		return cfg, nil, warnings, nil
	}
	template, err := a.videoTemplateByID(ref.ID)
	if err != nil {
		return cfg, nil, warnings, err
	}

	// 合规前置：命中违禁词不放行进 gate（§6）
	issues, complianceWarnings := checkVideoTemplateCompliance(&template)
	warnings = append(warnings, complianceWarnings...)
	if len(issues) > 0 {
		return cfg, nil, warnings, errors.New("模板合规校验未通过，已阻止进入流水线: " + strings.Join(issues, "；"))
	}
	if ref.Version != "" && ref.Version != template.Version {
		warnings = append(warnings, fmt.Sprintf("任务锁定模板 v%s，当前模板已是 v%s：本次按最新内容创建并已写入版本快照", ref.Version, template.Version))
	}

	merged, sources, applyWarnings := applyVideoTemplate(&template, cfg, ref.Overrides)
	warnings = append(warnings, applyWarnings...)

	usage := a.videoTemplateUsageCount(template.ID)
	values := templateVarValues(merged, ref)
	line, all, missing := templateOpeningLine(&template, values, usage)
	if len(missing) > 0 {
		warnings = append(warnings, "开场白变量未提供："+strings.Join(missing, "、")+"（可在新建任务时补充「模板变量」）")
	}
	if line != "" {
		estimate := estimateSpeechSeconds(line, merged.Language, merged.SpeechRate)
		if merged.DurationS > 0 && estimate > float64(merged.DurationS)*0.5 {
			warnings = append(warnings, fmt.Sprintf("开场白约 %.1fs，超过目标时长 %.0f%%：建议精简或延长视频", estimate, 50.0))
		}
	}

	run := &VideoTemplateRun{
		VideoTemplate: template,
		Sources:       sources,
		Overrides:     templateOverrideKeys(ref.Overrides, sources),
		OpeningLine:   line,
		OpeningAll:    all,
		StyleSnippet:  templateStyleSnippet(&template, "generic"),
		Compliance:    issues,
	}
	if usage > 0 && len(all) > 1 {
		run.Note = fmt.Sprintf("该模板已使用 %d 次：开场钩子已按轮换选中第 %d 个变体", usage, usage%len(all)+1)
	}
	return merged, run, warnings, nil
}

// templateOverrideKeys 列出本次被用户改过的字段，供任务视图标注来源。
func templateOverrideKeys(overrides map[string]string, sources map[string]string) []string {
	keys := make([]string, 0, len(overrides))
	for key, value := range overrides {
		if strings.TrimSpace(value) == "" {
			continue
		}
		keys = append(keys, key)
	}
	for key, source := range sources {
		if source != TemplateSourceOverride {
			continue
		}
		keys = append(keys, key)
	}
	keys = uniqueStrings(keys)
	sort.Strings(keys)
	return keys
}

// seedBuiltinVideoTemplates 首次启动写入内置模板（幂等）；内置模板升版时同步刷新存量库，
// 避免老库里继续留着旧的角色锚点；用户自建/团队模板不受影响（WHERE scope = 'builtin'）。
func seedBuiltinVideoTemplates(db *sql.DB) error {
	for _, template := range builtinVideoTemplates() {
		now := time.Now().Format(time.RFC3339)
		template.CreatedAt = now
		template.UpdatedAt = now
		payload, err := encodeTemplateJSON(template)
		if err != nil {
			return err
		}
		tagsJSON, err := encodeTemplateJSON(template.Tags)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`
			INSERT INTO video_templates (`+videoTemplateColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				version = excluded.version,
				scope = excluded.scope,
				tags_json = excluded.tags_json,
				payload_json = excluded.payload_json,
				updated_at = excluded.updated_at
			WHERE video_templates.scope = '`+TemplateScopeBuiltin+`' AND video_templates.version <> excluded.version`,
			template.ID, template.Name, template.Version, template.Scope, tagsJSON, payload, template.CreatedAt, template.UpdatedAt,
		); err != nil {
			return fmt.Errorf("写入内置模板失败: %w", err)
		}
	}
	return nil
}

// builtinVideoTemplates 内置模板：三个可直接跑通全链路的示例（品牌口播 / 知识解说 / 生活纪实）。
func builtinVideoTemplates() []VideoTemplate {
	floatPtr := func(value float64) *float64 { return &value }
	boolPtr := func(value bool) *bool { return &value }

	return []VideoTemplate{
		{
			ID:          "tpl_builtin_brand_warm",
			Name:        "品牌口播·暖调",
			Version:     "1.3",
			Scope:       TemplateScopeBuiltin,
			Description: "9:16 竖版暖调口播：暖琥珀色调 + 35mm 浅景深，猫主理人出镜，适合美食 / 好物上新。",
			Tags:        []string{"品牌", "口播", "美食", "抖音"},
			Style: VideoTemplateStyle{
				Tokens: map[string]string{
					"lighting": "侧逆光 45°，暖调反光板补亮面部",
					"grade":    "暖琥珀色调，暗部保留细节不压黑",
					"lens":     "35mm 定焦，f/2.0 浅景深",
					"camera":   "手持轻微呼吸感，缓慢推近",
					"texture":  "细颗粒胶片质感",
					"mood":     "温暖、松弛、有食欲",
				},
				PromptSnippets: map[string]string{
					"kling":   "35mm 定焦手持轻推，侧逆光 45°，暖琥珀色调，暗部保留细节，细颗粒胶片质感，猫主理人温暖口播氛围",
					"veo":     "handheld 35mm slowly pushing in, 45-degree backlight, warm amber grade with lifted shadows, fine film grain, cozy animal presenter mood",
					"runway":  "slow handheld push-in, 35mm lens, warm amber color grade, soft 45-degree backlight, fine grain",
					"generic": "手持缓慢推近，35mm 定焦浅景深，侧逆光 45°，暖琥珀色调，细颗粒胶片质感",
				},
			},
			Characters: []VideoTemplateCharacter{
				{
					RoleID:          "host",
					Name:            "主理人（橘猫）",
					Images:          []string{},
					AppearanceBlock: "成年橘色短毛猫，圆脸、耳尖有一小撮白毛，脖子系米白色针织围巾，亚麻小围裙挂在胸前，右前爪常扶着杯具；双后足站立，表情松弛",
					VoiceHint:       "温暖中音，语速 4.2 字/秒",
					Perspective:     "主视角，正对镜头半身",
				},
			},
			Opening: VideoTemplateOpening{
				Variants: []string{
					"{product}不是越贵越好，这三步就能喝出差别",
					"先说结论：{brand}这次把{product}做对了什么",
					"同样一杯{product}，为什么店里和家里差这么多？",
				},
				Vars:       []string{"brand", "product"},
				SpeechRate: 4.2,
			},
			SubtitleStyle: VideoTemplateSubtitleStyle{
				FontName:     "思源黑体",
				FontSizePct:  floatPtr(4.6),
				PrimaryColor: "#FFFFFF",
				OutlineColor: "#1A1A1A",
				OutlineWidth: floatPtr(2),
				Alignment:    2,
				MarginVPct:   floatPtr(6),
				Bold:         boolPtr(true),
			},
			PublishDefaults: VideoTemplatePublishDefaults{
				Platforms: []string{"douyin"},
				Region:    "Asia/Shanghai",
				Vertical:  "美食",
			},
			Defaults: VideoTemplateDefaults{
				AspectRatio:      "9:16",
				DurationS:        24,
				Fps:              24,
				SpeechRate:       floatPtr(4.2),
				ContinuationMode: "last_frame",
				Language:         "zh-CN",
			},
		},
		{
			ID:          "tpl_builtin_knowledge_cool",
			Name:        "知识解说·冷调",
			Version:     "1.1",
			Scope:       TemplateScopeBuiltin,
			Description: "16:9 横版知识解说：冷青蓝色调 + 固定机位，狗讲解人出镜，适合 B 站 / 深度内容。",
			Tags:        []string{"知识", "解说", "B站", "16:9"},
			Style: VideoTemplateStyle{
				Tokens: map[string]string{
					"lighting": "顶光 + 45° 侧补，背景冷白不发灰",
					"grade":    "冷青蓝色调低饱和，毛色补偿 +3",
					"lens":     "50mm 中景为主",
					"camera":   "固定机位 + 缓慢横移",
					"texture":  "数字锐利，无颗粒",
					"mood":     "克制、理性、可信",
				},
				PromptSnippets: map[string]string{
					"kling":   "50mm 中景固定机位缓慢横移，顶光加 45° 侧补，冷青蓝低饱和色调，毛色自然，画面锐利无颗粒",
					"veo":     "locked-off 50mm medium shot with slow lateral dolly, top light plus 45-degree fill, cool teal grade, clean digital look",
					"generic": "50mm 中景固定机位缓慢横移，冷青蓝低饱和色调，顶光加侧补，画面锐利无颗粒",
				},
			},
			Characters: []VideoTemplateCharacter{
				{
					RoleID:          "narrator",
					Name:            "讲解人（边牧）",
					Images:          []string{},
					AppearanceBlock: "黑白边境牧羊犬，眼神专注，深灰圆领针织披肩搭在肩背，坐姿挺直，两只前爪在胸前比划",
					VoiceHint:       "平稳中低音，语速 4.6 字/秒",
					Perspective:     "主视角，正面中景",
				},
			},
			Opening: VideoTemplateOpening{
				Variants: []string{
					"先别急着下结论，{topic}里有个反直觉的点",
					"三分钟讲清{topic}，第一条最容易被忽略",
				},
				Vars:       []string{"topic"},
				SpeechRate: 4.6,
			},
			SubtitleStyle: VideoTemplateSubtitleStyle{
				FontName:     "思源黑体",
				FontSizePct:  floatPtr(4),
				PrimaryColor: "#FFFFFF",
				OutlineColor: "#000000",
				OutlineWidth: floatPtr(1.6),
				Alignment:    2,
				MarginVPct:   floatPtr(8),
			},
			PublishDefaults: VideoTemplatePublishDefaults{
				Platforms: []string{"bilibili"},
				Region:    "Asia/Shanghai",
				Vertical:  "知识",
			},
			Defaults: VideoTemplateDefaults{
				AspectRatio:      "16:9",
				DurationS:        30,
				Fps:              30,
				SpeechRate:       floatPtr(4.6),
				ContinuationMode: "none",
				Language:         "zh-CN",
			},
		},
		{
			ID:          "tpl_builtin_slice_life",
			Name:        "生活纪实·清晨",
			Version:     "1.0",
			Scope:       TemplateScopeBuiltin,
			Description: "9:16 生活流：清晨窗侧光 + 28mm 近景，无固定角色，适合视频号日常记录。",
			Tags:        []string{"生活", "纪实", "视频号"},
			Style: VideoTemplateStyle{
				Tokens: map[string]string{
					"lighting": "清晨窗侧光，阴影柔和",
					"grade":    "低饱和暖灰，高光轻微奶白",
					"lens":     "28mm 广角近景",
					"camera":   "固定机位 + 轻微手持",
					"texture":  "轻胶片颗粒 3%",
					"mood":     "松弛、真实、生活流",
				},
				PromptSnippets: map[string]string{
					"kling":   "28mm 广角近景固定机位轻微手持，清晨窗侧光柔和阴影，低饱和暖灰调，轻胶片颗粒",
					"veo":     "28mm wide close shot, mostly locked with subtle handheld sway, soft morning window light, low-saturation warm grey grade, light grain",
					"generic": "28mm 广角近景，固定机位轻微手持，清晨窗侧光，低饱和暖灰调，轻胶片颗粒",
				},
			},
			Opening: VideoTemplateOpening{
				Variants: []string{
					"今天不赶时间，就做一件小事",
					"早上七点，城市还没醒，我先醒了",
				},
				Vars:       []string{},
				SpeechRate: 3.8,
			},
			SubtitleStyle: VideoTemplateSubtitleStyle{
				FontName:     "思源黑体",
				FontSizePct:  floatPtr(4.4),
				PrimaryColor: "#FFFFFF",
				OutlineColor: "#000000",
				OutlineWidth: floatPtr(2),
				Alignment:    2,
				MarginVPct:   floatPtr(7),
			},
			PublishDefaults: VideoTemplatePublishDefaults{
				Platforms: []string{"channels"},
				Region:    "Asia/Shanghai",
				Vertical:  "生活",
			},
			Defaults: VideoTemplateDefaults{
				AspectRatio:      "9:16",
				DurationS:        15,
				Fps:              24,
				SpeechRate:       floatPtr(3.8),
				ContinuationMode: "last_frame",
				Language:         "zh-CN",
			},
		},
	}
}
