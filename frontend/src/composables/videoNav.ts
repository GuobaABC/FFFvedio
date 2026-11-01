import { reactive } from 'vue'

/**
 * 视频模块的跨组件导航状态：
 * 「新建任务」创建成功后把 focusTaskId 传给「视频生成结果」，避免用事件把任务数据复制一份。
 */
export const videoNav = reactive({
  focusTaskId: '',
  /** 项目视图尾帧「用为新建首帧」的交接：新建任务页挂载时自动填入并清空 */
  pendingFirstFramePath: '',
})

export function focusVideoTask(taskId: string) {
  videoNav.focusTaskId = taskId
}

export function handoffTailFrame(imagePath: string) {
  videoNav.pendingFirstFramePath = imagePath
}
