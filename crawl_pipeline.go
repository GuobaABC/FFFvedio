package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const crawlLogLimit = 80

// CreateCrawlTask 只做收录与归一化：不联网、不抽字段，用户确认后再执行（任务式视图）。
func (a *App) CreateCrawlTask(input CrawlTaskInput) (CrawlTask, error) {
	if a.db == nil {
		return CrawlTask{}, errors.New("数据库未初始化")
	}
	cfg, warnings, err := normalizeCrawlConfig(input.Config)
	if err != nil {
		return CrawlTask{}, err
	}

	urls, problems := normalizeURLList(cfg.Input, cfg.MaxUrls)
	if len(urls) == 0 {
		message := "没有识别到可抓取的 http(s) 链接"
		if len(problems) > 0 {
			message += "：" + strings.Join(problems, "；")
		}
		return CrawlTask{}, errors.New(message)
	}

	results := make([]CrawlResult, 0, len(urls))
	for _, entry := range urls {
		results = append(results, CrawlResult{
			OriginalURL:  entry.Raw,
			CanonicalURL: entry.Canonical,
			SiteKey:      siteKeyFor(entry.Canonical),
			State:        CrawlStateCreated,
			ItemID:       videoItemIDFor(entry.Canonical),
			Steps:        []CrawlStep{},
		})
	}
	if len(problems) > 0 {
		warnings = append(warnings, problems...)
	}

	now := time.Now().Format(time.RFC3339)
	task := CrawlTask{
		ID:     newCrawlTaskID(),
		Title:  strings.TrimSpace(input.Title),
		Stage:  CrawlStageIntake,
		Status: CrawlStatusDraft,
		Config: cfg,
		Run: CrawlRunContext{
			Results:  results,
			Items:    []VideoItem{},
			Warnings: warnings,
			Logs:     []string{stageLog(fmt.Sprintf("已收录 %d 条 URL，等待执行", len(results)))},
			Stats:    CrawlStats{Total: len(results), TierHits: map[string]int{}},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if task.Title == "" {
		task.Title = defaultCrawlTitle(results)
	}
	if err := a.insertCrawlTask(task); err != nil {
		return CrawlTask{}, err
	}
	return task, nil
}

func defaultCrawlTitle(results []CrawlResult) string {
	if len(results) == 0 {
		return "热点爬取任务"
	}
	siteKey := results[0].SiteKey
	if siteKey == "" {
		siteKey = "站点"
	}
	if len(results) == 1 {
		return siteKey + " · 1 条链接"
	}
	return fmt.Sprintf("%s · %d 条链接", siteKey, len(results))
}

// RunCrawlTask 继续执行：已完成（且未过期）的 URL 走缓存，失败的重新入队。
func (a *App) RunCrawlTask(id string) error {
	return a.startCrawl(id, false)
}

// RecrawlCrawlTask 强制重爬：忽略缓存，把所有 URL 重置后重跑（“每次爬取刷新一下”）。
func (a *App) RecrawlCrawlTask(id string) error {
	return a.startCrawl(id, true)
}

func (a *App) startCrawl(id string, force bool) error {
	task, err := a.crawlTaskByID(id)
	if err != nil {
		return err
	}
	if a.isCrawlTaskRunning(id) {
		return errors.New("任务正在执行，请稍候")
	}
	// 不再因 task.Status == running 拒绝：进程重启后内存里没有执行记录，
	// 遗留的 running 状态应允许续跑，否则任务会永久卡住。

	if force {
		results := make([]CrawlResult, 0, len(task.Run.Results))
		for _, result := range task.Run.Results {
			result.State = CrawlStateCreated
			result.ErrorCode = ""
			result.Message = ""
			result.Tier = ""
			result.Confidence = 0
			result.Cached = false
			result.Steps = []CrawlStep{}
			results = append(results, result)
		}
		task.Run.Results = results
	} else {
		// 没有可执行项时提示用户
		pending := false
		for _, result := range task.Run.Results {
			if crawlResultNeedsRun(result, false) {
				pending = true
				break
			}
		}
		if !pending {
			return errors.New("所有链接都已完成；如需刷新请使用「强制重爬」")
		}
	}

	task.Status = CrawlStatusRunning
	task.Error = ""
	task.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := a.updateCrawlTaskRecord(task); err != nil {
		return err
	}
	if !a.beginCrawlRun(id) {
		return errors.New("任务正在执行，请稍候")
	}

	go func() {
		defer a.endCrawlRun(id)
		a.runCrawlPipeline(id, force)
	}()
	return nil
}

// runCrawlPipeline 逐条推进 URL 状态机；每条 URL 结束都落库，
// 「爬取结果」页轮询即可看到结果持续刷新。
func (a *App) runCrawlPipeline(id string, force bool) {
	task, err := a.crawlTaskByID(id)
	if err != nil {
		return
	}
	runner := &crawlRunner{app: a, task: &task}
	started := time.Now()

	task.Status = CrawlStatusRunning
	task.Stage = CrawlStageFetch
	task.Error = ""
	task.Run.FinishedAt = ""
	task.Run.LastRunAt = started.Format(time.RFC3339)
	runner.logf("开始执行：%d 条 URL，同站间隔 %dms，robots %s",
		len(task.Run.Results), task.Config.IntervalMS, map[bool]string{true: "开启", false: "关闭"}[task.Config.RespectRobots])

	provider := a.pickTextProvider(VideoRunConfig{TextProviderID: task.Config.TextProviderID})
	if task.Config.UseLLM && provider.ID == "" {
		runner.warn("没有可用的文本模型：L3 零样本抽取与 L4 规则沉淀已跳过，仅使用 L0/L1/L2")
	}
	_ = runner.save()

	fetcher := newCrawlFetcher(task.Config)
	ctx := context.Background()
	llmCalls := 0

	for index := range task.Run.Results {
		result := task.Run.Results[index]
		if !crawlResultNeedsRun(result, force) {
			continue
		}

		task.Stage = CrawlStageFetch
		task.Run.Results[index] = result
		_ = runner.save()

		updated, item, calls := runner.processURL(ctx, fetcher, provider, result, force)
		llmCalls += calls
		task.Run.Results[index] = updated
		if item != nil {
			task.Run.Items = upsertCrawlItem(task.Run.Items, *item)
		}
		task.Run.Stats = computeCrawlStats(task.Run.Results, llmCalls, time.Since(started))
		runner.logf("%s → %s%s", shortenURL(updated.CanonicalURL), updated.State, stateSuffix(updated))
		// 每步都落 audit_log（§8）：任务结束后可按 URL 回溯每一步的层级、耗时与错误码
		a.appendCrawlAudit(task.ID, updated)
		_ = runner.save()
	}

	task.Stage = CrawlStageDone
	task.Run.Stats = computeCrawlStats(task.Run.Results, llmCalls, time.Since(started))
	task.Run.FinishedAt = time.Now().Format(time.RFC3339)
	if task.Run.Stats.Stored == 0 && task.Run.Stats.Failed > 0 {
		task.Status = CrawlStatusFailed
		task.Error = fmt.Sprintf("全部 %d 条链接都没有抽到完整三要素", task.Run.Stats.Failed)
	} else {
		task.Status = CrawlStatusDone
	}
	runner.logf("任务结束：%s", crawlSummary(task.Run))
	_ = runner.save()
}

// crawlResultNeedsRun 本次执行需要处理的条目：新建、失败、被拦截、需要 LLM，
// 失败重试按类别退避（死链不再重爬，§8）。
func crawlResultNeedsRun(result CrawlResult, force bool) bool {
	if force {
		return true
	}
	switch result.State {
	case CrawlStateCreated, CrawlStateRouted, CrawlStateFetched, CrawlStateParsed,
		CrawlStateValidated, CrawlStateNeedsLLM, CrawlStateLLMDone,
		CrawlStateFailed, CrawlStateBlocked:
		if result.ErrorCode == CrawlErrDeadLink || result.ErrorCode == CrawlErrRobots {
			return false
		}
		return true
	}
	return false
}

// crawlRunner 任务级日志与落库
type crawlRunner struct {
	app  *App
	task *CrawlTask
}

func (r *crawlRunner) logf(format string, args ...any) {
	r.task.Run.Logs = append(r.task.Run.Logs, stageLog(fmt.Sprintf(format, args...)))
	if len(r.task.Run.Logs) > crawlLogLimit {
		r.task.Run.Logs = r.task.Run.Logs[len(r.task.Run.Logs)-crawlLogLimit:]
	}
}

func (r *crawlRunner) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, existing := range r.task.Run.Warnings {
		if existing == message {
			return
		}
	}
	r.task.Run.Warnings = append(r.task.Run.Warnings, message)
	r.logf("警告: %s", message)
}

func (r *crawlRunner) save() error {
	r.task.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := r.app.updateCrawlTaskRecord(*r.task); err != nil {
		r.logf("保存任务状态失败: %s", err.Error())
		return err
	}
	return nil
}

// processURL 单条 URL 的完整链路：短链展开 → 缓存 → 抓取 → 抽取 → 校验 → 沉淀。
func (r *crawlRunner) processURL(ctx context.Context, fetcher *crawlFetcher, provider Provider, result CrawlResult, force bool) (CrawlResult, *VideoItem, int) {
	// 本条 URL 里发生的模型调用都归到这个爬取任务名下（用途由 L3/L4 自己标）
	ctx = withUsageTask(ctx, r.task.ID)
	llmCalls := 0
	result.Steps = []CrawlStep{}
	result.ErrorCode = ""
	result.Message = ""
	result.Cached = false

	// 1. 收录：短链展开（§4.1 第 5 步）
	intakeStart := time.Now()
	canonical, note := expandShortLink(ctx, fetcher.client, result.CanonicalURL)
	result.CanonicalURL = canonical
	result.SiteKey = siteKeyFor(canonical)
	result.ItemID = videoItemIDFor(canonical)
	result.State = CrawlStateRouted
	result.Steps = append(result.Steps, CrawlStep{
		Step: "INTAKE", State: CrawlStateRouted, Status: "ok",
		LatencyMS: time.Since(intakeStart).Milliseconds(),
		Note:      firstNonEmpty(note, "canonical: "+shortenURL(canonical)),
		At:        time.Now().Format(time.RFC3339),
	})

	// 2. 缓存：30 天内抓过就直返（§12 第 8 条），短链展开后的 ID 一致
	if !force {
		if item, ok := r.app.cachedCrawlItem(result.ItemID, defaultCrawlCacheHours*time.Hour); ok {
			// 命中缓存也要把本任务新补的选题标签带上，不然素材库里的条目永远缺这一块
			if !crawlTagsCarried(r.task.Config.Tags, item.Optional.Tags) {
				item.Optional.Tags = mergeCrawlTaskTags(r.task.Config.Tags, item.Optional.Tags)
				item.UpdatedAt = time.Now().Format(time.RFC3339)
				if err := r.app.saveCrawlItem(item); err != nil {
					r.warn("选题标签写回条目失败: %s", err.Error())
				}
			}
			result.State = CrawlStateCached
			result.Tier = item.Meta.ExtractTier
			result.Confidence = item.Meta.Confidence
			result.HTTPStatus = item.Meta.HTTPStatus
			result.Cached = true
			result.Steps = append(result.Steps, CrawlStep{
				Step: "CACHE", State: CrawlStateCached, Status: "hit",
				Note: "命中 30 天缓存，未重新抓取", At: time.Now().Format(time.RFC3339),
			})
			return result, &item, llmCalls
		}
	}

	// 3. 抓取
	r.task.Stage = CrawlStageFetch
	fetchStart := time.Now()
	page, err := fetcher.fetchPage(ctx, canonical)
	fetchLatency := time.Since(fetchStart).Milliseconds()
	result.LatencyMS = fetchLatency
	if err != nil {
		code, message := crawlErrorInfo(err)
		result.ErrorCode = code
		result.Message = message
		result.State = CrawlStateBlocked
		if code != CrawlErrBotBlocked && code != CrawlErrLogin && code != CrawlErrRobots {
			result.State = CrawlStateFailed
		}
		result.Steps = append(result.Steps, CrawlStep{
			Step: "FETCH", State: result.State, Status: code, LatencyMS: fetchLatency,
			Note: message, At: time.Now().Format(time.RFC3339),
		})
		return result, nil, llmCalls
	}
	result.HTTPStatus = page.Status
	result.State = CrawlStateFetched
	result.Steps = append(result.Steps, CrawlStep{
		Step: "FETCH", State: CrawlStateFetched, Status: "ok", LatencyMS: fetchLatency,
		Note: fmt.Sprintf("HTTP %d · %d KB", page.Status, len(page.Body)>>10),
		At:   time.Now().Format(time.RFC3339),
	})
	if looksLikeSPAShell(string(page.Body)) {
		r.warn("%s 疑似 JS 渲染页面：静态 HTML 只是空壳，建议开启文本模型走 L3 兜底", shortenURL(canonical))
	}

	// 4. 抽取瀑布
	r.task.Stage = CrawlStageExtract
	_ = r.save()
	profile, _ := r.app.siteProfileByKey(result.SiteKey)
	if profile != nil && !profile.Enabled {
		profile = nil
	}
	session, err := newCrawlExtractSession(r.app, r.task.Config, provider, page, profile)
	if err != nil {
		failed, _, _ := r.failResult(result, "EXTRACT", err)
		return failed, nil, llmCalls
	}
	candidate, extractErr := session.run(ctx)
	result.Steps = append(result.Steps, session.steps...)
	if extractErr != nil {
		result.Steps = append(result.Steps, CrawlStep{
			Step: "EXTRACT", State: CrawlStateFailed, Status: "error",
			Note: extractErr.Error(), At: time.Now().Format(time.RFC3339),
		})
		code, message := crawlErrorInfo(extractErr)
		result.State = CrawlStateFailed
		result.ErrorCode = code
		result.Message = message
		return result, nil, llmCalls
	}
	if session.llmUsed {
		llmCalls++
	}
	if profile != nil {
		r.app.recordSiteProfileHit(profile.SiteKey, candidate.Tier == CrawlTierProfile)
	}

	// 5. 校验合并：三要素不全就再给 L3 一次机会
	r.task.Stage = CrawlStageValidate
	_ = r.save()
	score, problems := validateCandidate(candidate, canonical)
	if (len(problems) > 0 || score < 0.6) && !session.llmUsed && r.task.Config.UseLLM && provider.ID != "" {
		llmStart := time.Now()
		llmCandidate, err := session.fromLLM(ctx)
		llmLatency := time.Since(llmStart).Milliseconds()
		if err != nil {
			result.Steps = append(result.Steps, CrawlStep{
				Step: "VALIDATE", State: CrawlStateNeedsLLM, Status: CrawlErrLLM,
				LatencyMS: llmLatency, Note: err.Error(), At: time.Now().Format(time.RFC3339),
			})
		} else {
			llmCalls++
			session.llmUsed = true
			candidate = mergeCandidates([]crawlCandidate{candidate, llmCandidate})
			score, problems = validateCandidate(candidate, canonical)
			result.Steps = append(result.Steps, CrawlStep{
				Step: "VALIDATE", State: CrawlStateLLMDone, Tier: CrawlTierLLM, Status: "ok",
				LatencyMS: llmLatency, Note: describeCandidate(llmCandidate), At: time.Now().Format(time.RFC3339),
			})
		}
	}

	result.Confidence = score
	result.Tier = candidate.Tier
	if len(problems) > 0 || score < 0.6 {
		result.State = CrawlStateFailed
		result.ErrorCode = CrawlErrValidate
		result.Message = "校验未通过：" + strings.Join(problems, "；")
		result.Steps = append(result.Steps, CrawlStep{
			Step: "VALIDATE", State: CrawlStateFailed, Status: CrawlErrValidate,
			Note: result.Message, At: time.Now().Format(time.RFC3339),
		})
		return result, nil, llmCalls
	}

	// 6. 入库
	now := time.Now().Format(time.RFC3339)
	item := VideoItem{
		ID:           result.ItemID,
		CanonicalURL: canonical,
		OriginalURL:  result.OriginalURL,
		SiteKey:      result.SiteKey,
		Title:        candidate.Title,
		Description:  candidate.Description,
		Thumbnail: VideoThumbnail{
			URL:         candidate.Thumbnail,
			ContentType: crawlImageMimeType(candidate.Thumbnail),
		},
		Optional: VideoItemOptional{
			Author:      candidate.Author,
			PublishedAt: candidate.PublishedAt,
			DurationSec: candidate.DurationS,
			Tags:        mergeCrawlTaskTags(r.task.Config.Tags, candidate.Tags),
		},
		Meta: VideoItemMeta{
			ExtractedAt: now,
			ExtractTier: candidate.Tier,
			Confidence:  score,
			HTTPStatus:  page.Status,
			Rendered:    page.Rendered,
			LatencyMS:   fetchLatency,
			RawRef:      "crawl:" + r.task.ID,
			Variants:    candidate.Variants,
			SiteProfile: profile != nil,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.app.saveCrawlItem(item); err != nil {
		r.warn("条目入库失败: %s", err.Error())
	}
	result.ItemID = item.ID
	result.State = CrawlStateStored
	result.Steps = append(result.Steps, CrawlStep{
		Step: "STORE", State: CrawlStateStored, Status: "ok",
		Note: fmt.Sprintf("置信度 %.2f · 层级 %s", score, candidate.Tier),
		At:   now,
	})

	// 7. L4 规则沉淀：把 LLM 的结果反推成站点选择器（下次同站走高速路径）
	if session.llmUsed && candidate.Tier == CrawlTierLLM && provider.ID != "" {
		ruleStart := time.Now()
		selectors, err := r.app.llmProposeSiteSelectors(ctx, provider, page, session.doc, candidate)
		ruleLatency := time.Since(ruleStart).Milliseconds()
		if err != nil {
			result.Steps = append(result.Steps, CrawlStep{
				Step: "RULE", State: CrawlStateLLMDone, Status: "skip", LatencyMS: ruleLatency,
				Note: "规则编译跳过：" + err.Error(), At: time.Now().Format(time.RFC3339),
			})
		} else {
			profile := SiteProfile{
				SiteKey:        result.SiteKey,
				HostPattern:    hostOf(canonical),
				RequiresRender: looksLikeSPAShell(string(page.Body)),
				Selectors:      selectors,
				HitSuccess:     1,
				LastVerifiedAt: time.Now().Format(time.RFC3339),
				Enabled:        true,
				AutoGenerated:  true,
				ReviewStatus:   "auto-approved",
				UpdatedAt:      time.Now().Format(time.RFC3339),
			}
			if err := r.app.upsertSiteProfile(profile); err != nil {
				r.warn("站点规则入库失败: %s", err.Error())
			} else {
				result.State = CrawlStateRuleNew
				result.Steps = append(result.Steps, CrawlStep{
					Step: "RULE", State: CrawlStateRuleNew, Tier: CrawlTierRule, Status: "ok",
					LatencyMS: ruleLatency, Note: "已沉淀 " + strings.Join(selectorSummary(selectors), " / "),
					At: time.Now().Format(time.RFC3339),
				})
			}
		}
	}

	return result, &item, llmCalls
}

func (r *crawlRunner) failResult(result CrawlResult, step string, err error) (CrawlResult, *VideoItem, int) {
	code, message := crawlErrorInfo(err)
	result.State = CrawlStateFailed
	result.ErrorCode = code
	result.Message = message
	result.Steps = append(result.Steps, CrawlStep{
		Step: step, State: CrawlStateFailed, Status: code,
		Note: message, At: time.Now().Format(time.RFC3339),
	})
	return result, nil, 0
}

func selectorSummary(selectors map[string][]string) []string {
	out := make([]string, 0, 3)
	for _, field := range []string{"title", "description", "thumbnail"} {
		if list := selectors[field]; len(list) > 0 {
			out = append(out, field+": "+list[0])
		}
	}
	return out
}

// crawlErrorInfo 把错误拆成 {code, message}，供审计与前端展示。
func crawlErrorInfo(err error) (string, string) {
	var typed *crawlError
	if errors.As(err, &typed) {
		return typed.Code, typed.Error()
	}
	return CrawlErrParse, err.Error()
}

func shortenURL(raw string) string {
	cleaned := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if len(cleaned) > 64 {
		return cleaned[:61] + "…"
	}
	return cleaned
}

func stateSuffix(result CrawlResult) string {
	parts := make([]string, 0, 2)
	if result.Tier != "" {
		parts = append(parts, result.Tier)
	}
	if result.ErrorCode != "" {
		parts = append(parts, result.ErrorCode)
	}
	if result.Cached {
		parts = append(parts, "cache")
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, " · ") + "）"
}

// computeCrawlStats 统计每次保存都重算，前端直接展示。
func computeCrawlStats(results []CrawlResult, llmCalls int, elapsed time.Duration) CrawlStats {
	stats := CrawlStats{Total: len(results), LLMCalls: llmCalls, TierHits: make(map[string]int, 4)}
	for _, result := range results {
		switch result.State {
		case CrawlStateStored, CrawlStateRuleNew, CrawlStateLLMDone, CrawlStateValidated:
			stats.Stored++
			if result.Tier != "" {
				stats.TierHits[result.Tier]++
			}
		case CrawlStateCached:
			stats.Cached++
			stats.Stored++
			if result.Tier != "" {
				stats.TierHits[result.Tier]++
			}
		case CrawlStateFailed:
			stats.Failed++
		case CrawlStateBlocked:
			stats.Blocked++
		}
	}
	stats.DurationMS = elapsed.Milliseconds()
	return stats
}

// GetCrawlEnvironment 能力探测：有无文本模型、是否内置无头浏览器、支持站点清单。
func (a *App) GetCrawlEnvironment() CrawlEnvironment {
	env := CrawlEnvironment{
		BrowserSupport: false,
		SupportedSites: []string{
			"B站（API · BV 号）", "YouTube（oEmbed + ytInitialPlayerResponse）", "Vimeo（oEmbed）",
			"抖音（RENDER_DATA）", "小红书（__INITIAL_STATE__）", "任意站点（OG / JSON-LD / LLM）",
		},
		Notes: []string{
			"静态优先：能用 HTML 抽到的绝不启动浏览器；当前版本未内置无头浏览器，SPA 页面依赖文本模型兜底。",
			"正则只做尾部清洗，主抽取走选择器；同站规则会沉淀到站点画像，下次走快速通道。",
			"只保留标题、简介、封面 URL 三项与可选元信息，不下载原视频与封面原图。",
		},
	}
	providers, err := a.ListProviders()
	if err != nil {
		return env
	}
	for _, provider := range providers {
		if provider.Category == CategoryText || provider.Category == CategoryMultimodal {
			env.LLMAvailable = true
			env.TextModel = provider.Model
			break
		}
	}
	return env
}
