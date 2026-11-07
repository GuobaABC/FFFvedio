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

// ListVideoTemplates 返回模板库（团队在前，其余按更新时间倒序）。
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

// removeLegacyBuiltinVideoTemplates 清理历史版本写入的内置模板。
// 默认模板已全部下线：升级后的库里不应再出现只读示例，用户自建/团队模板不受影响。
// 旧任务的模板版本快照保存在任务自身，删除模板行不影响已创建任务的复现。
func removeLegacyBuiltinVideoTemplates(db *sql.DB) error {
	if _, err := db.Exec(`DELETE FROM video_templates WHERE scope = ?`, TemplateScopeBuiltin); err != nil {
		return fmt.Errorf("清理历史内置模板失败: %w", err)
	}
	return nil
}
