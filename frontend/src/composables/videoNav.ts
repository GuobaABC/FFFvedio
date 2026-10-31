import { reactive } from 'vue'

/**
 * 视频模块的跨组件导航状态：
 * 「新建任务」创建成功后把 focusTaskId 传给「视频生成结果」，避免用事件把任务数据复制一份。
 */
export const videoNav = reactive({
  focusTaskId: '',
})

export function focusVideoTask(taskId: string) {
  videoNav.focusTaskId = taskId
}
