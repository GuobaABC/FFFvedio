import type { Provider, ProviderDraft } from '../types/provider'
import type {
  CrawlEnvironment,
  CrawlItem,
  CrawlTask,
  CrawlTaskInput,
  SiteProfile,
} from '../types/crawl'
import type {
  TailFrameAsset,
  VideoEnvironment,
  VideoGateEdits,
  VideoProject,
  VideoSegmentRegen,
  VideoTask,
  VideoTaskInput,
} from '../types/video'
import type { VideoTemplate, VideoTemplatePreview } from '../types/template'

/**
 * Wails 运行时注入的绑定对象（window.go.main.App.*）。
 * 这里手写类型而不 import frontend/wailsjs，前端才能在没有生成绑定的情况下
 * 独立运行 `npm run dev` / `npm run build`，仅在调用时提示缺少运行时。
 */
interface AppBindings {
  ListProviders(): Promise<Provider[]>
  SaveProvider(input: ProviderDraft): Promise<Provider>
  DeleteProvider(id: string): Promise<void>
  FetchModels(baseUrl: string, apiKey: string): Promise<string[]>

  /* 视频生成任务 */
  ListVideoTasks(): Promise<VideoTask[]>
  GetVideoTask(id: string): Promise<VideoTask>
  CreateVideoTask(input: VideoTaskInput): Promise<VideoTask>
  DeleteVideoTask(id: string): Promise<void>
  RunVideoTask(id: string): Promise<void>
  ApproveVideoGate(id: string): Promise<void>
  RegenerateVideoStage(id: string, feedback: string): Promise<void>
  RegenerateVideoMeta(id: string, platforms: string[], feedback: string): Promise<void>
  SaveVideoStageEdits(id: string, edits: VideoGateEdits): Promise<VideoTask>
  GetVideoEnvironment(): Promise<VideoEnvironment>

  /* 逐段确认关卡 + 尾帧资产 */
  ApproveVideoSegment(id: string): Promise<void>
  RegenerateVideoSegment(id: string, edit: VideoSegmentRegen): Promise<void>
  SuggestSegmentPrompt(id: string, segmentId: string, feedback: string): Promise<string>
  ListTailFrameAssets(): Promise<TailFrameAsset[]>
  DeleteTailFrameAsset(id: string): Promise<void>

  /* 项目视图：任务归组 + 归属调整 */
  ListVideoProjects(): Promise<VideoProject[]>
  CreateVideoProject(name: string): Promise<VideoProject>
  RenameVideoProject(id: string, name: string): Promise<void>
  DeleteVideoProject(id: string): Promise<void>
  MoveVideoTaskToProject(taskId: string, projectId: string): Promise<void>

  /* 视频模板（默认值来源，不绕过确认门） */
  ListVideoTemplates(): Promise<VideoTemplate[]>
  GetVideoTemplate(id: string): Promise<VideoTemplate>
  SaveVideoTemplate(input: VideoTemplate): Promise<VideoTemplate>
  DeleteVideoTemplate(id: string): Promise<void>
  PreviewVideoTemplate(templateId: string, topic: string, vars: Record<string, string>): Promise<VideoTemplatePreview>

  OpenVideoTaskFolder(id: string): Promise<void>
  RevealVideoOutput(id: string): Promise<void>
  PickImageFile(): Promise<string>
  PickImageFiles(): Promise<string[]>

  /* 热点爬取任务 */
  ListCrawlTasks(): Promise<CrawlTask[]>
  GetCrawlTask(id: string): Promise<CrawlTask>
  CreateCrawlTask(input: CrawlTaskInput): Promise<CrawlTask>
  DeleteCrawlTask(id: string): Promise<void>
  RunCrawlTask(id: string): Promise<void>
  RecrawlCrawlTask(id: string): Promise<void>
  ListCrawlItems(limit: number): Promise<CrawlItem[]>
  ListSiteProfiles(): Promise<SiteProfile[]>
  DeleteSiteProfile(siteKey: string): Promise<void>
  GetCrawlEnvironment(): Promise<CrawlEnvironment>
}

type WailsWindow = Window & {
  go?: { main?: { App?: AppBindings } }
}

const MISSING_RUNTIME_MESSAGE =
  '未检测到 Wails 运行时：请在桌面应用（wails dev / wails build）中操作，浏览器单独预览时无法访问本地数据库。'

function bindings(): AppBindings {
  const app = (window as WailsWindow).go?.main?.App
  if (!app) {
    throw new Error(MISSING_RUNTIME_MESSAGE)
  }
  return app
}

export const backend = {
  listProviders: (): Promise<Provider[]> => bindings().ListProviders(),
  saveProvider: (input: ProviderDraft): Promise<Provider> => bindings().SaveProvider(input),
  deleteProvider: (id: string): Promise<void> => bindings().DeleteProvider(id),
  fetchModels: (baseUrl: string, apiKey: string): Promise<string[]> =>
    bindings().FetchModels(baseUrl, apiKey),

  listVideoTasks: (): Promise<VideoTask[]> => bindings().ListVideoTasks(),
  getVideoTask: (id: string): Promise<VideoTask> => bindings().GetVideoTask(id),
  createVideoTask: (input: VideoTaskInput): Promise<VideoTask> =>
    bindings().CreateVideoTask(input),
  deleteVideoTask: (id: string): Promise<void> => bindings().DeleteVideoTask(id),
  runVideoTask: (id: string): Promise<void> => bindings().RunVideoTask(id),
  approveVideoGate: (id: string): Promise<void> => bindings().ApproveVideoGate(id),
  regenerateVideoStage: (id: string, feedback: string): Promise<void> =>
    bindings().RegenerateVideoStage(id, feedback),
  regenerateVideoMeta: (id: string, platforms: string[], feedback: string): Promise<void> =>
    bindings().RegenerateVideoMeta(id, platforms, feedback),
  saveVideoStageEdits: (id: string, edits: VideoGateEdits): Promise<VideoTask> =>
    bindings().SaveVideoStageEdits(id, edits),
  getVideoEnvironment: (): Promise<VideoEnvironment> => bindings().GetVideoEnvironment(),

  approveVideoSegment: (id: string): Promise<void> => bindings().ApproveVideoSegment(id),
  regenerateVideoSegment: (id: string, edit: VideoSegmentRegen): Promise<void> =>
    bindings().RegenerateVideoSegment(id, edit),
  suggestSegmentPrompt: (id: string, segmentId: string, feedback: string): Promise<string> =>
    bindings().SuggestSegmentPrompt(id, segmentId, feedback),
  listTailFrameAssets: (): Promise<TailFrameAsset[]> => bindings().ListTailFrameAssets(),
  deleteTailFrameAsset: (id: string): Promise<void> => bindings().DeleteTailFrameAsset(id),

  listVideoProjects: (): Promise<VideoProject[]> => bindings().ListVideoProjects(),
  createVideoProject: (name: string): Promise<VideoProject> => bindings().CreateVideoProject(name),
  renameVideoProject: (id: string, name: string): Promise<void> => bindings().RenameVideoProject(id, name),
  deleteVideoProject: (id: string): Promise<void> => bindings().DeleteVideoProject(id),
  moveVideoTaskToProject: (taskId: string, projectId: string): Promise<void> =>
    bindings().MoveVideoTaskToProject(taskId, projectId),

  listVideoTemplates: (): Promise<VideoTemplate[]> => bindings().ListVideoTemplates(),
  getVideoTemplate: (id: string): Promise<VideoTemplate> => bindings().GetVideoTemplate(id),
  saveVideoTemplate: (input: VideoTemplate): Promise<VideoTemplate> =>
    bindings().SaveVideoTemplate(input),
  deleteVideoTemplate: (id: string): Promise<void> => bindings().DeleteVideoTemplate(id),
  previewVideoTemplate: (templateId: string, topic: string, vars: Record<string, string>): Promise<VideoTemplatePreview> =>
    bindings().PreviewVideoTemplate(templateId, topic, vars),

  openVideoTaskFolder: (id: string): Promise<void> => bindings().OpenVideoTaskFolder(id),
  revealVideoOutput: (id: string): Promise<void> => bindings().RevealVideoOutput(id),
  pickImageFile: (): Promise<string> => bindings().PickImageFile(),
  pickImageFiles: (): Promise<string[]> => bindings().PickImageFiles(),

  listCrawlTasks: (): Promise<CrawlTask[]> => bindings().ListCrawlTasks(),
  getCrawlTask: (id: string): Promise<CrawlTask> => bindings().GetCrawlTask(id),
  createCrawlTask: (input: CrawlTaskInput): Promise<CrawlTask> =>
    bindings().CreateCrawlTask(input),
  deleteCrawlTask: (id: string): Promise<void> => bindings().DeleteCrawlTask(id),
  runCrawlTask: (id: string): Promise<void> => bindings().RunCrawlTask(id),
  recrawlCrawlTask: (id: string): Promise<void> => bindings().RecrawlCrawlTask(id),
  listCrawlItems: (limit = 200): Promise<CrawlItem[]> => bindings().ListCrawlItems(limit),
  listSiteProfiles: (): Promise<SiteProfile[]> => bindings().ListSiteProfiles(),
  deleteSiteProfile: (siteKey: string): Promise<void> => bindings().DeleteSiteProfile(siteKey),
  getCrawlEnvironment: (): Promise<CrawlEnvironment> => bindings().GetCrawlEnvironment(),
}

/** 统一处理 Go 侧返回的字符串错误与普通 Error */
export function errorText(error: unknown): string {
  if (typeof error === 'string') {
    return error
  }
  if (error instanceof Error) {
    return error.message
  }
  return '发生未知错误'
}
