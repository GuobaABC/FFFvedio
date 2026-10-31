<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'

import { backend, errorText } from '../services/backend'
import {
  createEmptyCrawlConfig,
  type CrawlEnvironment,
  type CrawlRunConfig,
} from '../types/crawl'
import type { Provider } from '../types/provider'

const emit = defineEmits<{ (event: 'created', taskId: string): void }>()

const form = reactive<CrawlRunConfig>(createEmptyCrawlConfig())
const title = ref('')
const tagsText = ref('')
const submitting = ref(false)
const submitError = ref('')
const loadError = ref('')
const providers = ref<Provider[]>([])
const environment = ref<CrawlEnvironment | null>(null)

const textProviders = computed(() =>
  providers.value.filter((item) => item.category === 'text' || item.category === 'multimodal'),
)

/** 粗略的链接提示：只做前端友好提示，真正的归一化在后端 */
const detectedCount = computed(() => {
  const matched = form.input.match(/https?:\/\/[^\s，。；、）)"']+/g)
  return matched ? matched.length : 0
})

const llmHint = computed(() => {
  if (environment.value?.llmAvailable) {
    return `L3 零样本抽取可用（${environment.value.textModel}）：新域名首爬会多一次模型调用，并把结果反推成站点规则。`
  }
  return '未登记文本模型：仅使用 L0 oEmbed / L1 站点规则 / L2 结构化标记，SPA 空壳页可能抽不到三要素。'
})

onMounted(async () => {
  try {
    const [providerList, env] = await Promise.all([
      backend.listProviders().catch((error) => {
        loadError.value = errorText(error)
        return [] as Provider[]
      }),
      backend.getCrawlEnvironment().catch((error) => {
        loadError.value = errorText(error)
        return null
      }),
    ])
    providers.value = providerList
    environment.value = env
    if (env && !env.llmAvailable) {
      form.useLlm = false
    }
  } catch (error) {
    loadError.value = errorText(error)
  }
})

function parseTags(value: string): string[] {
  return value
    .split(/[,，;；\n]/)
    .map((item) => item.trim().replace(/^#/, ''))
    .filter(Boolean)
}

async function submit() {
  submitError.value = ''
  if (!form.input.trim()) {
    submitError.value = '请先粘贴要爬取的链接，或包含链接的分享文案'
    return
  }

  const config: CrawlRunConfig = {
    ...form,
    input: form.input.trim(),
    note: form.note.trim(),
    tags: parseTags(tagsText.value),
    maxUrls: Number(form.maxUrls) || 10,
    intervalMs: Number(form.intervalMs) || 1500,
    timeoutS: Number(form.timeoutS) || 20,
  }

  submitting.value = true
  try {
    const task = await backend.createCrawlTask({ title: title.value.trim(), config })
    emit('created', task.id)
  } catch (error) {
    submitError.value = errorText(error)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="create">
    <header class="create__head">
      <div>
        <h2 class="create__title">新建热点爬取任务</h2>
        <p class="create__subtitle">
          粘贴链接或分享文案 → 收录归一化 → 静态抓取 → 抽取瀑布（L0 oEmbed / L1 站点规则 / L2 结构化标记 / L3 LLM）
          → 三要素校验入库；创建后到「爬取结果」点「执行任务」开始
        </p>
      </div>
      <span class="ui-chip">识别到 {{ detectedCount }} 条 http(s) 链接</span>
    </header>

    <p v-if="loadError" class="ui-notice ui-notice--error">{{ loadError }}</p>
    <p v-if="submitError" class="ui-notice ui-notice--error">{{ submitError }}</p>

    <form class="create__form" @submit.prevent="submit">
      <section class="ui-card block">
        <h3 class="ui-h3">1. 用户输入：链接或分享文案 *</h3>
        <label class="ui-field">
          <span class="ui-label">可混合粘贴，多条用换行或空格分隔</span>
          <textarea
            v-model="form.input"
            class="ui-textarea ui-textarea--tall"
            rows="6"
            spellcheck="false"
            placeholder="例：
【凌晨四点的城市】 https://www.bilibili.com/video/BV1xx411c7mD?spm_id_from=333.999
7.43 复制打开抖音，看看【阿力的作品】 https://v.douyin.com/xxxxxx/"
            required
          ></textarea>
        </label>
        <div class="grid grid--2">
          <label class="ui-field">
            <span class="ui-label">任务标题（可选）</span>
            <input v-model="title" class="ui-input" type="text" placeholder="留空则按首个站点自动生成" />
          </label>
          <label class="ui-field">
            <span class="ui-label">选题池标签（可选，逗号分隔）</span>
            <input v-model="tagsText" class="ui-input" type="text" placeholder="城市清晨, 治愈, 纪录" />
          </label>
        </div>
        <label class="ui-field">
          <span class="ui-label">备注（可选，仅自己看）</span>
          <input v-model="form.note" class="ui-input" type="text" placeholder="为什么收进选题池" />
        </label>
        <p class="hint">
          收录阶段会：小写 host、去 utm/spm/from/share 等追踪参数、去 fragment、补 scheme、展开短链；
          同一条视频在多个任务里只会留一份条目。
        </p>
      </section>

      <section class="ui-card block">
        <h3 class="ui-h3">2. 抓取策略</h3>
        <div class="grid grid--3">
          <label class="ui-field">
            <span class="ui-label">单任务最多收录（条）</span>
            <input v-model.number="form.maxUrls" class="ui-input" type="number" min="1" max="50" />
          </label>
          <label class="ui-field">
            <span class="ui-label">同站抓取间隔（毫秒）</span>
            <input v-model.number="form.intervalMs" class="ui-input" type="number" min="200" max="10000" step="100" />
          </label>
          <label class="ui-field">
            <span class="ui-label">单次请求超时（秒）</span>
            <input v-model.number="form.timeoutS" class="ui-input" type="number" min="5" max="60" />
          </label>
        </div>
        <label class="check">
          <input v-model="form.respectRobots" type="checkbox" />
          遵守 robots.txt（推荐保留；关闭仅在你确认目标站允许抓取时使用）
        </label>
        <p class="hint">
          只抓静态 HTML：不启动无头浏览器、不下载原视频与封面原图，仅保留标题 / 简介 / 封面 URL 三要素与可选元信息。
        </p>
      </section>

      <section class="ui-card block">
        <h3 class="ui-h3">3. 抽取瀑布与文本模型</h3>
        <label class="check">
          <input v-model="form.useLlm" type="checkbox" />
          新域名首爬时启用 L3 零样本抽取，并把结果反推成站点规则（L4 越跑越准）
        </label>
        <div class="grid grid--2">
          <label class="ui-field">
            <span class="ui-label">L3 / L4 使用的文本提供商</span>
            <select v-model="form.textProviderId" class="ui-select" :disabled="!form.useLlm">
              <option value="">自动选择文本模型</option>
              <option v-for="provider in textProviders" :key="provider.id" :value="provider.id">
                {{ provider.name }} · {{ provider.model }}
              </option>
            </select>
          </label>
          <div class="ui-field">
            <span class="ui-label">当前能力</span>
            <span class="ui-chip" :class="environment?.llmAvailable ? 'ui-chip--ok' : 'ui-chip--warn'">
              {{ environment?.llmAvailable ? 'L3 可用' : '仅 L0/L1/L2' }}
            </span>
          </div>
        </div>
        <p class="hint">{{ llmHint }}</p>
        <div v-if="environment" class="sites">
          <span class="ui-label">已适配站点</span>
          <div class="sites__row">
            <span v-for="site in environment.supportedSites" :key="site" class="ui-chip">{{ site }}</span>
          </div>
        </div>
        <ul v-if="environment?.notes?.length" class="notes">
          <li v-for="note in environment.notes" :key="note">{{ note }}</li>
        </ul>
      </section>

      <div class="create__actions">
        <button class="ui-btn ui-btn--primary" type="submit" :disabled="submitting">
          {{ submitting ? '创建中…' : '创建任务' }}
        </button>
        <span class="hint">创建后跳到「热点爬取 → 爬取结果」，点「执行任务」开始抓取。</span>
      </div>
    </form>
  </section>
</template>

<style scoped>
.create {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  min-height: 0;
  padding-right: 4px;
  overflow: auto;
}

.create__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.create__title {
  font-size: 16px;
  font-weight: 600;
}

.create__subtitle {
  max-width: 660px;
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.create__form {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding-bottom: 8px;
}

.block {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
}

.ui-textarea--tall {
  min-height: 132px;
  font-family: var(--font-mono);
  font-size: 12.5px;
}

.grid {
  display: grid;
  gap: 12px;
}

.grid--2 {
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
}

.grid--3 {
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
}

.check {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
  cursor: pointer;
}

.sites {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.sites__row {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.notes {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin: 0;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.hint {
  font-size: 12px;
  line-height: 1.7;
  color: var(--color-text-muted);
}

.create__actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  padding: 4px 2px 12px;
}
</style>
