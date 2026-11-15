package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// 项目记忆：把同项目历史任务里的角色/剧情/世界设定沉淀下来，
// 供短剧续写时做「关键词 + 语义」混合检索。删除任务不会删除记忆，
// 除非用户手动重建或删除项目。
const (
	ProjectMemoryKindCharacter    = "character"
	ProjectMemoryKindPlot         = "plot"
	ProjectMemoryKindEvent        = "event"
	ProjectMemoryKindRelationship = "relationship"
	ProjectMemoryKindWorld        = "world"
	ProjectMemoryKindSummary      = "summary"
)

type ProjectMemory struct {
	ID         string  `json:"id"`
	ProjectID  string  `json:"projectId"`
	Episode    int     `json:"episode"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	Content    string  `json:"content"`
	Payload    string  `json:"payload"`
	SourceTask string  `json:"sourceTask"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
	Score      float64 `json:"score,omitempty"`
}

const projectMemoryColumns = `id, project_id, episode, kind, title, content, payload_json, source_task, created_at, updated_at`

func scanProjectMemory(scanner interface{ Scan(dest ...any) error }) (ProjectMemory, error) {
	var memory ProjectMemory
	err := scanner.Scan(&memory.ID, &memory.ProjectID, &memory.Episode, &memory.Kind,
		&memory.Title, &memory.Content, &memory.Payload, &memory.SourceTask, &memory.CreatedAt, &memory.UpdatedAt)
	return memory, err
}

func (a *App) ListProjectMemory(projectID string, limit int) ([]ProjectMemory, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := a.db.Query(`SELECT `+projectMemoryColumns+` FROM project_memory WHERE project_id = ?
		ORDER BY episode DESC, updated_at DESC LIMIT ?`, strings.TrimSpace(projectID), limit)
	if err != nil {
		return nil, fmt.Errorf("读取项目记忆失败: %w", err)
	}
	defer rows.Close()
	memories := make([]ProjectMemory, 0, 16)
	for rows.Next() {
		memory, err := scanProjectMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("解析项目记忆失败: %w", err)
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

func (a *App) UpsertProjectMemory(input ProjectMemory) (ProjectMemory, error) {
	if a.db == nil {
		return ProjectMemory{}, errors.New("数据库未初始化")
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.Kind = strings.TrimSpace(input.Kind)
	input.Title = strings.TrimSpace(input.Title)
	input.Content = strings.TrimSpace(input.Content)
	if input.ProjectID == "" || !a.videoProjectExists(input.ProjectID) {
		return ProjectMemory{}, errors.New("项目不存在")
	}
	if input.Kind == "" {
		input.Kind = ProjectMemoryKindSummary
	}
	if input.Content == "" {
		return ProjectMemory{}, errors.New("记忆内容不能为空")
	}
	now := time.Now().Format(time.RFC3339)
	if strings.TrimSpace(input.ID) == "" {
		input.ID = fmt.Sprintf("pm-%d-%s", time.Now().UnixNano(), randomHex(4))
		input.CreatedAt = now
	}
	input.UpdatedAt = now
	if input.Payload == "" {
		input.Payload = "{}"
	}
	_, err := a.db.Exec(`INSERT INTO project_memory (`+projectMemoryColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id, episode=excluded.episode,
		kind=excluded.kind, title=excluded.title, content=excluded.content,
		payload_json=excluded.payload_json, source_task=excluded.source_task, updated_at=excluded.updated_at`,
		input.ID, input.ProjectID, input.Episode, input.Kind, input.Title, input.Content,
		input.Payload, input.SourceTask, input.CreatedAt, input.UpdatedAt)
	if err != nil {
		return ProjectMemory{}, fmt.Errorf("保存项目记忆失败: %w", err)
	}
	return input, nil
}

func (a *App) DeleteProjectMemory(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	result, err := a.db.Exec(`DELETE FROM project_memory WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("删除项目记忆失败: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return errors.New("项目记忆不存在")
	}
	return nil
}

// RebuildProjectMemory 手动重建项目记忆（会先清空该项目旧记忆），并尝试生成语义向量。
func (a *App) RebuildProjectMemory(projectID string) (int, error) {
	count, err := a.rebuildProjectMemory(projectID, true)
	if err != nil {
		return count, err
	}
	return count, nil
}

func (a *App) rebuildProjectMemory(projectID string, withEmbeddings bool) (int, error) {
	projectID = strings.TrimSpace(projectID)
	if a.db == nil {
		return 0, errors.New("数据库未初始化")
	}
	if projectID == "" || !a.videoProjectExists(projectID) {
		return 0, errors.New("项目不存在")
	}
	rows, err := a.db.Query(`SELECT `+videoTaskColumns+` FROM video_tasks WHERE project_id = ? ORDER BY episode ASC, created_at ASC`, projectID)
	if err != nil {
		return 0, fmt.Errorf("读取项目任务失败: %w", err)
	}
	tasks := make([]VideoTask, 0, 16)
	for rows.Next() {
		task, err := scanVideoTask(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		tasks = append(tasks, task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM project_memory_embedding WHERE memory_id IN (SELECT id FROM project_memory WHERE project_id = ?)`, projectID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM project_memory WHERE project_id = ?`, projectID); err != nil {
		return 0, err
	}
	count := 0
	for _, task := range tasks {
		for _, draft := range memoryDraftsFromTask(task) {
			if _, err := tx.Exec(`INSERT INTO project_memory (`+projectMemoryColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				draft.ID, draft.ProjectID, draft.Episode, draft.Kind, draft.Title, draft.Content,
				draft.Payload, draft.SourceTask, draft.CreatedAt, draft.UpdatedAt); err != nil {
				return count, fmt.Errorf("写入项目记忆失败: %w", err)
			}
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	if withEmbeddings {
		_, _ = a.EmbedProjectMemory(projectID)
	}
	return count, nil
}

// memoryDraftsFromTask 从已完成任务的企划/发布元数据抽取记忆。
func memoryDraftsFromTask(task VideoTask) []ProjectMemory {
	now := time.Now().Format(time.RFC3339)
	drafts := make([]ProjectMemory, 0, 8)
	add := func(kind, title, content string, payload any) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		payloadJSON := "{}"
		if payload != nil {
			if data, err := json.Marshal(payload); err == nil {
				payloadJSON = string(data)
			}
		}
		drafts = append(drafts, ProjectMemory{
			ID:         fmt.Sprintf("pm-%s-%s-%d", task.ID, kind, len(drafts)),
			ProjectID:  task.ProjectID,
			Episode:    task.Episode,
			Kind:       kind,
			Title:      strings.TrimSpace(title),
			Content:    content,
			Payload:    payloadJSON,
			SourceTask: task.ID,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}

	script := task.Run.Script
	if script != nil && script.Treatment != nil {
		treatment := script.Treatment
		add(ProjectMemoryKindSummary, fmt.Sprintf("第%d集 %s", task.Episode, task.Title),
			fmt.Sprintf("立意：%s\n梗概：%s", treatment.Premise, script.Logline), treatment)
		for _, member := range treatment.Cast {
			add(ProjectMemoryKindCharacter, fmt.Sprintf("第%d集 角色：%s", task.Episode, member.Name),
				fmt.Sprintf("角色：%s（%s）\n物种：%s\n外观：%s\n性格：%s\n声音：%s",
					member.Name, member.RoleID, member.Species, member.Appearance, member.Persona, member.VoiceHint), member)
		}
		for index, beat := range treatment.Beats {
			add(ProjectMemoryKindPlot, fmt.Sprintf("第%d集 第%d拍 %s", task.Episode, index+1, beat.Summary),
				fmt.Sprintf("节拍：%s\n新信息：%s\n情绪：%s\n地点：%s\n角色：%s\n衔接：%s",
					beat.Summary, beat.NewInfo, beat.Emotion, beat.Location,
					strings.Join(beat.Characters, "、"), beat.Link), beat)
		}
		if len(treatment.Continuity) > 0 {
			add(ProjectMemoryKindWorld, fmt.Sprintf("第%d集 连续性设定", task.Episode),
				strings.Join(treatment.Continuity, "\n"), treatment.Continuity)
		}
	}
	if meta := task.Run.PublishMeta; meta != nil {
		add(ProjectMemoryKindSummary, fmt.Sprintf("第%d集 发布信息", task.Episode),
			fmt.Sprintf("标题：%s\n描述：%s\n标签：%s", meta.Title.Primary, meta.Description,
				strings.Join(meta.Tags.Hashtags, " ")), meta)
	}
	return drafts
}

// EmbedProjectMemory 为项目里还没有向量的记忆补 embedding；失败时保持纯关键词检索。
func (a *App) EmbedProjectMemory(projectID string) (int, error) {
	provider, err := a.pickEmbeddingProvider()
	if err != nil {
		return 0, err
	}
	rows, err := a.db.Query(`SELECT `+projectMemoryColumns+` FROM project_memory m
		WHERE m.project_id = ? AND NOT EXISTS (SELECT 1 FROM project_memory_embedding e WHERE e.memory_id = m.id)
		ORDER BY m.updated_at ASC LIMIT 200`, strings.TrimSpace(projectID))
	if err != nil {
		return 0, err
	}
	memories := make([]ProjectMemory, 0, 32)
	for rows.Next() {
		memory, err := scanProjectMemory(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		memories = append(memories, memory)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	embedded := 0
	for start := 0; start < len(memories); start += 8 {
		end := start + 8
		if end > len(memories) {
			end = len(memories)
		}
		batch := memories[start:end]
		inputs := make([]string, 0, len(batch))
		for _, memory := range batch {
			inputs = append(inputs, memory.Title+"\n"+memory.Content)
		}
		vectors, err := a.embedTexts(provider, inputs)
		if err != nil {
			return embedded, err
		}
		for index, vector := range vectors {
			if len(vector) == 0 {
				continue
			}
			blob := float32Blob(vector)
			if _, err := a.db.Exec(`INSERT INTO project_memory_embedding (memory_id, dim, vector, model, updated_at)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT(memory_id) DO UPDATE SET dim=excluded.dim,
				vector=excluded.vector, model=excluded.model, updated_at=excluded.updated_at`,
				batch[index].ID, len(vector), blob, provider.Model, time.Now().Format(time.RFC3339)); err != nil {
				return embedded, err
			}
			embedded++
		}
	}
	return embedded, nil
}

// SearchProjectMemory 关键词 + 语义混合检索；无 embedding 能力时自动只用关键词。
func (a *App) SearchProjectMemory(projectID, query string, limit int) ([]ProjectMemory, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	projectID = strings.TrimSpace(projectID)
	query = strings.TrimSpace(query)
	if limit <= 0 || limit > 50 {
		limit = 8
	}
	if projectID == "" {
		return nil, nil
	}
	if query == "" {
		return a.ListProjectMemory(projectID, limit)
	}

	lexical, _ := a.searchProjectMemoryLexical(projectID, query, limit*3)
	semantic, _ := a.searchProjectMemorySemantic(projectID, query, limit*3)
	if len(lexical) == 0 && len(semantic) == 0 {
		// 关键词与语义都没命中时，至少把最近几集的项目记忆带上，避免续写完全失忆。
		return a.ListProjectMemory(projectID, limit)
	}
	type scored struct {
		memory ProjectMemory
		score  float64
	}
	merged := map[string]*scored{}
	for index, memory := range lexical {
		item := &scored{memory: memory, score: 1 / float64(60+index)}
		merged[memory.ID] = item
	}
	for index, memory := range semantic {
		if item, ok := merged[memory.ID]; ok {
			item.score += 1 / float64(60+index)
			continue
		}
		merged[memory.ID] = &scored{memory: memory, score: 1 / float64(60+index)}
	}
	result := make([]scored, 0, len(merged))
	for _, item := range merged {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].score > result[j].score })
	if len(result) > limit {
		result = result[:limit]
	}
	memories := make([]ProjectMemory, 0, len(result))
	for _, item := range result {
		memory := item.memory
		memory.Score = math.Round(item.score*10000) / 10000
		memories = append(memories, memory)
	}
	return memories, nil
}

func (a *App) searchProjectMemoryLexical(projectID, query string, limit int) ([]ProjectMemory, error) {
	// trigram 对 3 字以上中文更有效；短查询直接走 LIKE，避免空结果。
	if len([]rune(query)) >= 3 {
		rows, err := a.db.Query(`SELECT `+projectMemoryColumns+` FROM project_memory_fts f
			JOIN project_memory m ON m.rowid = f.rowid
			WHERE m.project_id = ? AND project_memory_fts MATCH ?
			ORDER BY bm25(project_memory_fts) LIMIT ?`, projectID, query, limit)
		if err == nil {
			defer rows.Close()
			memories := make([]ProjectMemory, 0, limit)
			for rows.Next() {
				memory, scanErr := scanProjectMemory(rows)
				if scanErr != nil {
					return nil, scanErr
				}
				memories = append(memories, memory)
			}
			if len(memories) > 0 {
				return memories, rows.Err()
			}
		}
	}
	pattern := "%" + query + "%"
	rows, err := a.db.Query(`SELECT `+projectMemoryColumns+` FROM project_memory
		WHERE project_id = ? AND (title LIKE ? OR content LIKE ?)
		ORDER BY episode DESC, updated_at DESC LIMIT ?`, projectID, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memories := make([]ProjectMemory, 0, limit)
	for rows.Next() {
		memory, scanErr := scanProjectMemory(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

func (a *App) searchProjectMemorySemantic(projectID, query string, limit int) ([]ProjectMemory, error) {
	provider, err := a.pickEmbeddingProvider()
	if err != nil {
		return nil, err
	}
	vectors, err := a.embedTexts(provider, []string{query})
	if err != nil || len(vectors) == 0 || len(vectors[0]) == 0 {
		return nil, err
	}
	queryVec := vectors[0]
	rows, err := a.db.Query(`SELECT `+projectMemoryColumns+`, e.vector FROM project_memory m
		JOIN project_memory_embedding e ON e.memory_id = m.id
		WHERE m.project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		memory ProjectMemory
		score  float64
	}
	items := make([]scored, 0, 32)
	for rows.Next() {
		var memory ProjectMemory
		var vectorBlob []byte
		if err := rows.Scan(&memory.ID, &memory.ProjectID, &memory.Episode, &memory.Kind,
			&memory.Title, &memory.Content, &memory.Payload, &memory.SourceTask,
			&memory.CreatedAt, &memory.UpdatedAt, &vectorBlob); err != nil {
			return nil, err
		}
		score := cosineSimilarity(queryVec, float32Slice(vectorBlob))
		items = append(items, scored{memory: memory, score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].score > items[j].score })
	if len(items) > limit {
		items = items[:limit]
	}
	memories := make([]ProjectMemory, 0, len(items))
	for _, item := range items {
		memories = append(memories, item.memory)
	}
	return memories, nil
}

func (a *App) pickEmbeddingProvider() (Provider, error) {
	providers, err := a.ListProviders()
	if err != nil {
		return Provider{}, err
	}
	for _, provider := range providers {
		if provider.Category == CategoryText && strings.TrimSpace(provider.BaseURL) != "" && strings.TrimSpace(provider.Model) != "" {
			return provider, nil
		}
	}
	return Provider{}, errors.New("没有可用的文本提供商用于 embedding")
}

// projectMemoryContext 把项目记忆格式化成 Agent1 企划输入；返回文本与命中的标题。
func (a *App) projectMemoryContext(cfg VideoRunConfig) (string, []string) {
	if strings.TrimSpace(cfg.ProjectID) == "" || strings.TrimSpace(cfg.Topic) == "" {
		return "", nil
	}
	memories, err := a.SearchProjectMemory(cfg.ProjectID, cfg.Topic, 8)
	if err != nil || len(memories) == 0 {
		return "", nil
	}
	lines := make([]string, 0, len(memories)+1)
	lines = append(lines, "【项目历史记忆】（同项目过往剧集，只作连贯性参考；不得照抄旧剧情）")
	titles := make([]string, 0, len(memories))
	for _, memory := range memories {
		label := memory.Kind
		title := memory.Title
		lines = append(lines, fmt.Sprintf("- [%s] 第%d集 %s\n%s", label, memory.Episode, title, memory.Content))
		titles = append(titles, fmt.Sprintf("第%d集 %s", memory.Episode, title))
	}
	return strings.Join(lines, "\n"), titles
}

func float32Blob(vector []float32) []byte {
	blob := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(blob[index*4:], math.Float32bits(value))
	}
	return blob
}

func float32Slice(blob []byte) []float32 {
	if len(blob)%4 != 0 {
		return nil
	}
	vector := make([]float32, len(blob)/4)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(blob[index*4:]))
	}
	return vector
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for index := range a {
		av, bv := float64(a[index]), float64(b[index])
		dot += av * bv
		normA += av * av
		normB += bv * bv
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
