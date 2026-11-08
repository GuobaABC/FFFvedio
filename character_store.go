package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// video_characters：角色库实体落库。payload_json 存完整档案，name / tags 单独成列供检索。
// 任务只保存档案快照，不依赖角色库仍然可复现；删除角色不影响已经创建的任务。
const videoCharacterColumns = `id, name, tags_json, payload_json, created_at, updated_at`

func newVideoCharacterID() string {
	return fmt.Sprintf("char_%d_%s", time.Now().UnixNano(), randomHex(3))
}

func encodeCharacterJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("序列化角色失败: %w", err)
	}
	return string(data), nil
}

// normalizeVideoCharacter 入口归一化：角色名必填；其余字段宽松清洗，允许只存一部分。
func normalizeVideoCharacter(input VideoCharacter) (VideoCharacter, error) {
	character := input
	character.ID = strings.TrimSpace(character.ID)
	character.Name = strings.TrimSpace(character.Name)
	character.RoleID = strings.TrimSpace(character.RoleID)
	character.AppearanceBlock = strings.TrimSpace(character.AppearanceBlock)
	character.Persona = strings.TrimSpace(character.Persona)
	character.NegativePrompt = strings.TrimSpace(character.NegativePrompt)
	character.VoiceHint = strings.TrimSpace(character.VoiceHint)
	character.VoiceProfile = normalizeVideoVoiceProfile(character.VoiceProfile)
	character.Perspective = strings.TrimSpace(character.Perspective)
	character.Note = strings.TrimSpace(character.Note)
	character.Tags = uniqueStrings(character.Tags)
	character.Images = uniqueStrings(character.Images)
	if character.Name == "" {
		return character, errors.New("请填写角色名称")
	}
	if character.RoleID == "" {
		// role_id 只用于分镜与台词归属；没填时用稳定 ID 兜底，避免前端拿到空标识。
		character.RoleID = character.ID
	}
	return character, nil
}

// ListVideoCharacters 返回角色库（按更新时间倒序）。
func (a *App) ListVideoCharacters() ([]VideoCharacter, error) {
	if a.db == nil {
		return nil, errors.New("数据库未初始化")
	}
	rows, err := a.db.Query(`SELECT ` + videoCharacterColumns + ` FROM video_characters`)
	if err != nil {
		return nil, fmt.Errorf("读取角色库失败: %w", err)
	}
	defer rows.Close()

	characters := make([]VideoCharacter, 0, 8)
	for rows.Next() {
		var id, name, tagsJSON, payloadJSON, createdAt, updatedAt string
		if err := rows.Scan(&id, &name, &tagsJSON, &payloadJSON, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("读取角色库失败: %w", err)
		}
		character, err := decodeVideoCharacter(payloadJSON)
		if err != nil {
			return nil, err
		}
		character.ID = firstNonEmpty(character.ID, id)
		character.Name = firstNonEmpty(character.Name, name)
		character.CreatedAt = firstNonEmpty(character.CreatedAt, createdAt)
		character.UpdatedAt = firstNonEmpty(character.UpdatedAt, updatedAt)
		characters = append(characters, character)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取角色库失败: %w", err)
	}
	// 更新时间倒序：新建/最近编辑的角色排在前面
	for i := 1; i < len(characters); i++ {
		for j := i; j > 0 && characters[j].UpdatedAt > characters[j-1].UpdatedAt; j-- {
			characters[j], characters[j-1] = characters[j-1], characters[j]
		}
	}
	return characters, nil
}

// videoCharacterByID 读取单个角色库条目。
func (a *App) videoCharacterByID(id string) (VideoCharacter, error) {
	if a.db == nil {
		return VideoCharacter{}, errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return VideoCharacter{}, errors.New("角色不存在")
	}
	var name, payload string
	err := a.db.QueryRow(
		`SELECT name, payload_json FROM video_characters WHERE id = ?`, id,
	).Scan(&name, &payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VideoCharacter{}, errors.New("角色不存在（可能已被删除）")
		}
		return VideoCharacter{}, fmt.Errorf("读取角色失败: %w", err)
	}
	character, err := decodeVideoCharacter(payload)
	if err != nil {
		return VideoCharacter{}, err
	}
	character.ID = firstNonEmpty(character.ID, id)
	character.Name = firstNonEmpty(character.Name, name)
	return character, nil
}

func decodeVideoCharacter(payload string) (VideoCharacter, error) {
	var character VideoCharacter
	if err := json.Unmarshal([]byte(payload), &character); err != nil {
		return character, fmt.Errorf("解析角色内容失败: %w", err)
	}
	return character, nil
}

// SaveVideoCharacter 新建或更新角色库条目。
func (a *App) SaveVideoCharacter(input VideoCharacter) (VideoCharacter, error) {
	if a.db == nil {
		return VideoCharacter{}, errors.New("数据库未初始化")
	}
	character, err := normalizeVideoCharacter(input)
	if err != nil {
		return VideoCharacter{}, err
	}
	if character.ID == "" {
		character.ID = newVideoCharacterID()
	}
	if character.RoleID == "" {
		character.RoleID = character.ID
	}

	now := time.Now().Format(time.RFC3339)
	existing, err := a.videoCharacterByID(character.ID)
	switch {
	case err == nil:
		character.CreatedAt = firstNonEmpty(existing.CreatedAt, now)
		character.UpdatedAt = now
	case strings.Contains(err.Error(), "角色不存在"):
		character.CreatedAt = now
		character.UpdatedAt = now
	default:
		return VideoCharacter{}, err
	}

	payload, err := encodeCharacterJSON(character)
	if err != nil {
		return VideoCharacter{}, err
	}
	tagsJSON, err := encodeCharacterJSON(character.Tags)
	if err != nil {
		return VideoCharacter{}, err
	}
	if _, err := a.db.Exec(
		`INSERT INTO video_characters (`+videoCharacterColumns+`) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, tags_json = excluded.tags_json,
		 payload_json = excluded.payload_json, updated_at = excluded.updated_at`,
		character.ID, character.Name, tagsJSON, payload, character.CreatedAt, character.UpdatedAt,
	); err != nil {
		return VideoCharacter{}, fmt.Errorf("保存角色失败: %w", err)
	}
	return character, nil
}

// DeleteVideoCharacter 删除角色库条目；已经创建的任务持有档案快照，不受影响。
func (a *App) DeleteVideoCharacter(id string) error {
	if a.db == nil {
		return errors.New("数据库未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("角色不存在")
	}
	if _, err := a.db.Exec(`DELETE FROM video_characters WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除角色失败: %w", err)
	}
	return nil
}

// hydrateCharacterRefs 在任务创建时把绑定角色库的档案补进角色参考并固化快照。
// 任务里已经填写的字段优先（允许任务级覆盖）；图片为空时才使用角色库的默认定妆图。
func (a *App) hydrateCharacterRefs(cfg *VideoRunConfig) ([]string, error) {
	if cfg == nil || len(cfg.CharacterRefs) == 0 {
		return nil, nil
	}
	warnings := make([]string, 0, 2)
	for i := range cfg.CharacterRefs {
		ref := &cfg.CharacterRefs[i]
		id := strings.TrimSpace(ref.CharacterID)
		if id == "" {
			continue
		}
		character, err := a.videoCharacterByID(id)
		if err != nil {
			return nil, fmt.Errorf("角色「%s」绑定的角色库条目已不存在: %v", firstNonEmpty(ref.Name, id), err)
		}
		if ref.RoleID == "" {
			ref.RoleID = firstNonEmpty(character.RoleID, character.ID)
		}
		if ref.Name == "" {
			ref.Name = character.Name
		}
		if ref.AppearanceBlock == "" {
			ref.AppearanceBlock = character.AppearanceBlock
		}
		if ref.Persona == "" {
			ref.Persona = character.Persona
		}
		if ref.NegativePrompt == "" {
			ref.NegativePrompt = character.NegativePrompt
		}
		if ref.Perspective == "" {
			ref.Perspective = character.Perspective
		}
		if ref.VoiceHint == "" {
			ref.VoiceHint = character.VoiceHint
		}
		if ref.VoiceProfile == nil {
			ref.VoiceProfile = character.VoiceProfile
		}
		if len(ref.Images) == 0 && len(character.Images) > 0 {
			ref.Images = append([]string{}, character.Images...)
			// 角色库的定妆图是用户维护的权威形象，按上传图处理，优先于模板生成图。
			ref.ImageSource = CharacterImageSourceUser
		}
		if len(ref.Images) > 0 && ref.ImageSource == "" {
			ref.ImageSource = CharacterImageSourceUser
		}
	}
	return warnings, nil
}
