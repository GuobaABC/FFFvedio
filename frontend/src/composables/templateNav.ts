import { reactive } from 'vue'

/**
 * 模板库 → 新建任务的跨组件交接：
 * 「模板库」点击「用此模板新建任务」时只传模板 ID，新建任务页再按 ID 拉取最新版本，
 * 避免把模板内容复制成第二份真相。
 */
export const templateNav = reactive({
  pendingTemplateId: '',
})

export function useTemplateForTask(templateId: string) {
  templateNav.pendingTemplateId = templateId
}

export function consumePendingTemplate(): string {
  const templateId = templateNav.pendingTemplateId
  templateNav.pendingTemplateId = ''
  return templateId
}
