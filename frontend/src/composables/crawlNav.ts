import { reactive } from 'vue'

import type { CrawlItem } from '../types/crawl'

/**
 * 爬取条目带入视频生成的草稿：
 * 「爬取结果」里点「带入视频生成」时写入，VideoTaskCreate 挂载/切换时消费一次。
 */
export interface CrawlVideoDraft {
  sourceId: string
  topic: string
  note: string
  thumbnail: string
  url: string
  tags: string[]
}

/**
 * 热点爬取模块的跨组件导航状态：
 * 「新建任务」创建成功后把 focusTaskId 交给「爬取结果」，避免事件里复制任务数据。
 */
export const crawlNav = reactive({
  focusTaskId: '',
  draft: null as CrawlVideoDraft | null,
})

export function focusCrawlTask(taskId: string) {
  crawlNav.focusTaskId = taskId
}

/** 把爬取条目整理成「视频主题 + 参考素材」草稿，交给视频生成模块 */
export function handoffCrawlItem(item: CrawlItem) {
  const description = item.description.trim()
  const topic = description ? `${item.title}\n\n${description}` : item.title
  crawlNav.draft = {
    sourceId: item.id,
    topic,
    note: `来源：${item.siteKey || '站点'} · ${item.canonicalUrl}`,
    thumbnail: item.thumbnail.url,
    url: item.canonicalUrl,
    tags: item.optional?.tags ?? [],
  }
}

export function consumeCrawlDraft(): CrawlVideoDraft | null {
  const draft = crawlNav.draft
  crawlNav.draft = null
  return draft
}
