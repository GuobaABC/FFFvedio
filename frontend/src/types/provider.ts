/** 「设置 - 提供商」的数据结构，与 Go 侧 Provider 的 JSON 字段一一对应 */

export type ProviderCategory = 'text' | 'multimodal' | 'multimodal_gen'

export interface Provider {
  id: string
  category: ProviderCategory
  name: string
  baseUrl: string
  apiKey: string
  model: string
  createdAt: string
  updatedAt: string
}

/** 提交给后端的表单数据：id 为空表示新增 */
export type ProviderDraft = Omit<Provider, 'createdAt' | 'updatedAt'>

export interface CategoryOption {
  id: ProviderCategory
  label: string
  hint: string
}

/** 分类口径对应厂商文档合集：文本 → 文本 / 多模态理解 → 文本 / 生成非文本内容 */
export const PROVIDER_CATEGORIES: CategoryOption[] = [
  { id: 'text', label: '文本模型', hint: '文本输入 → 文本输出（对话、推理、代码）' },
  { id: 'multimodal', label: '多模态模型', hint: '图像 / 音频 / 视频 / 文档输入 → 文本输出' },
  {
    id: 'multimodal_gen',
    label: '多模态生成模型',
    hint: '生成图像、视频、语音、音乐等非文本内容',
  },
]

export function createEmptyDraft(category: ProviderCategory): ProviderDraft {
  return { id: '', category, name: '', baseUrl: '', apiKey: '', model: '' }
}
