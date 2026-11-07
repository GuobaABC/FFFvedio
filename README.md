# FFFVedioAgent

> FFF = Fast Fast Fast。`Vedio` 是刻意保留的拼写彩蛋。

**v0.1.0** · 基于 Wails v2 + Vue 3 + Go 的跨平台桌面应用（Windows / macOS），把「热点素材采集 → 分镜与提示词 → 多段 AI 视频渲染 → 字幕合成 → 发布元数据」串成一条带人工确认关卡的本地流水线。所有数据落在本地 SQLite，不依赖云端后端。

---

## 它能做什么

| 模块 | 能力 |
| --- | --- |
| **首页大屏** | 今日 / 近 7 日用量、调用趋势、最近事件，模型调用与计费记录可追溯 |
| **热点爬取** | 任务式采集：URL 归一化 → 抓取路由 → 抽取瀑布（规则 / LLM）→ 校验合并 → 入库；结果条目可一键带入视频生成 |
| **视频生成** | 主题 → 分镜（Agent1）→ 提示词与字幕（Agent2）→ 逐段渲染 → 合成烧录字幕 → 发布元数据，含多道人工确认关卡 |
| **项目视图** | 按项目整理任务，尾帧资产库支持「用为下一条视频首帧」，实现跨任务画面延续 |
| **模板库** | 用户自建 / 团队模板，分层覆盖（单次 > 模板 > 系统默认）并标注字段来源 |
| **系统设置** | 按「文本模型 / 多模态模型 / 多模态生成模型」登记服务商，`/models` 拉取模型列表 |

### 视频生成流水线

任务按阶段推进，**贵操作一律放在人工确认之后**，确认关卡不能直接跳过：

| 阶段 | 说明 |
| --- | --- |
| `DRAFT_SCRIPT` → `CONFIRM_SCRIPT` | Agent1 出分镜；也可切「手动分段」自己定段数与段时长，跳过 Agent1 |
| `BUILD_PROMPT` → `CONFIRM_PROMPT` | Agent2 产出每段的 i2v 提示词、台词与字幕文案 |
| `RENDER_VIDEO` | 逐段调用视频模型；可开「逐段确认」，单段不满意可改提示词（或让 AI 重写）后仅重渲该段 |
| `BUILD_SUBS` → `CONFIRM_SUBS` | 生成字幕与时间轴，关卡位置和条目可增删 |
| `COMPOSE` | ffmpeg 拼接、冻帧裁剪、字幕声学对齐后烧录，输出成片 |
| `BUILD_PUBLISH_META` → `CONFIRM_META` → `DONE` | 生成标题 / 标签 / 简介等发布元数据 |

**v0.1.0 的主体约束逻辑**：有首帧或角色参考图时，角色外观以用户图片为准，不再强制动物主体；仅在完全没有图片输入时才用动物主体兜底。模板自带的角色外观与风格词在遇到用户图片时跳过，内置示例模板已全部下线（升级时清理历史 `builtin` 行，用户自建与团队模板保留）。

### 内置视频适配器

| 适配器 | 厂商 | 成熟度 |
| --- | --- | --- |
| `ark` | 火山方舟 Seedance（豆包视频） | 主力：任务类型互斥、参考图上限、画幅锁定、按模型代际推断能力 |
| `dashscope` | 阿里云百炼（万相 / 可灵） | 部分：首帧 / 尾帧主链路可用 |
| `runway` | Runway Gen-4 | 部分：基础生视频可用，无角色参考图 |
| `veo` | Google Veo（Gemini API） | 部分：任务提交 / 轮询可用 |
| `generic` | 通用 HTTP 网关 | 部分：适配自建网关 |
| `mock` | 本地 ffmpeg 占位 | 本地联调、不计费 |

适配器会按已登记提供商的 Base URL / 模型 ID 自动推断，也可手动指定。能力差异（是否支持首帧、尾帧、参考图、音画同步、画幅）统一由 `VideoCapabilities` 声明，上层只提交抽象能力。

### 成片合成

- **字幕声学对齐**：从成片提取 16k 单声道 wav，用 ffmpeg `silencedetect` 检出说话区间，按字数权重把台词铺进发声窗口；无音轨 / 覆盖率过低 / 时间轴越界时自动退回按实测时长对齐，不阻断合成。每次生成 `output/subtitle-timing.json` 记录对齐依据。
- **接缝冻帧裁剪**：尾帧延续模式下，逐帧比对下一段开头与上一段尾帧的 SSIM，高于 0.995 判为重放冻帧并删除，音视频与字幕时间轴同步裁剪。
- **重试与降级**：网络失败按错误类型退避重试，单段失败不拖垮整条流水线。

---

## 技术架构

| 层 | 选型 |
| --- | --- |
| 桌面框架 | Wails v2.17.0（v2 稳定线，v3 仍为 alpha） |
| 后端 | Go 1.26，`go:embed all:frontend/dist` 内嵌前端资源 |
| 前端 | Vue 3.5（Composition API + `<script setup>`）+ TypeScript 5.9 strict + Vite 7，无 UI 框架、无状态管理库 |
| 本地存储 | SQLite via `modernc.org/sqlite`（纯 Go，无 CGO），WAL 模式 |
| 外部工具 | ffmpeg / ffprobe（生成字幕、拼接、测时、抽帧） |
| 前后端通信 | 直接调用 Wails 注入的 `window.go.main.App.*`，不 import `frontend/wailsjs` |

窗口 1100×720（最小 900×600），深色主题，左侧可折叠选项卡导航 + 右侧内容区，右上角悬浮实时时钟。`/media/` 路由指向视频任务工作目录，前端可直接播放本地成片。

## 目录结构

```
FFFVedio/
├─ main.go / app.go                 # 入口与生命周期、任务执行互斥
├─ db.go                            # SQLite 打开、PRAGMA、建表与迁移
├─ provider*.go / llm_client.go     # 提供商 CRUD、/models、文本模型调用
├─ crawl_*.go                       # 爬取流水线：URL、抓取、抽取、选择器、LLM、存储
├─ video_models.go / video_pipeline.go / video_agent.go
│                                   # 任务模型、阶段状态机、Agent1/2/4
├─ video_adapter.go                 # 各厂商视频适配器与能力声明
├─ video_compose.go / video_media.go# ffmpeg 合成、字幕对齐、冻帧裁剪
├─ video_retry.go / video_publish.go / video_assets.go / video_project.go
├─ video_store.go / template_*.go / usage_*.go
├─ *_test.go                        # Go 单元测试与 ffmpeg 集成测试
├─ wails.json / go.mod / go.sum
├─ build/                           # 图标、Info.plist、Windows 打包资源
├─ scripts/                         # 各平台一键打包脚本
├─ .github/workflows/release.yml    # 推送 v* 标签自动出 Release
└─ frontend/src/
   ├─ App.vue                      # 主布局与四个主选项卡
   ├─ components/                  # Dashboard / Crawl* / Video* / Template* / ProviderSettings
   ├─ composables/                 # 跨页面交接（爬取条目→新建任务、模板→任务、尾帧→首帧）
   ├─ services/backend.ts          # Wails 运行时桥接
   └─ types/                       # 各模块 TS 类型
```

## 快速开始

### 直接使用

到 [Releases](https://github.com/GuobaABC/FFFvedio/releases) 下载对应平台安装包：

- Windows：`FFFVedioAgent-<版本>-windows-amd64-installer.exe`（当前用户目录安装，不弹 UAC，中文界面）或 `-portable.zip`（解压即用）
- macOS：`.dmg` / `.zip`（未公证构建，首次打开需右键 →「打开」）

启动后先到「系统设置 → 提供商」登记至少一个**文本模型**和一个**多模态生成模型**服务商，填好 Base URL、API Key 与模型名，再到「视频生成 → 新建任务」填主题开跑。ffmpeg 会随安装包分发，查找顺序为 `FFFVedio_FFMPEG` 环境变量 → 可执行文件同级的 `bin/`、`deps/` → 系统 PATH，通常无需额外配置。

### 环境要求

- Node.js ≥ 20 与 npm
- Go ≥ 1.26
- Wails CLI v2.17.0：`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0`
- Windows：WebView2 Runtime（Windows 11 自带）；打安装包需 NSIS
- macOS：Xcode Command Line Tools；**macOS 包必须在 macOS 上构建**（依赖 cgo 与系统框架，无法交叉编译）
- ffmpeg / ffprobe（本地开发时需自行安装）

```bash
wails doctor      # 环境自检
```

### 本地开发

```bash
# 纯前端开发（无需 Go / Wails，浏览器里可预览布局；数据类页面会提示缺少运行时）
cd frontend && npm install && npm run dev        # http://localhost:5173

# 桌面开发（自动装依赖、起 Vite、编译运行桌面窗口，前端热更新）
wails dev
```

### 构建与测试

```bash
cd frontend && npm run typecheck && npm run build   # vue-tsc 严格检查 + vite build → dist/
go vet ./... && go test ./... && go build ./...    # 需先构建前端产物
wails build                                       # → build/bin/FFFVedioAgent.exe / .app
wails build -nsis                                  # Windows 安装包
```

`go test ./...` 覆盖提供商 CRUD 与 `/models` 解析、爬取流水线、分镜与手动分段、字幕对齐与冻帧裁剪、逐段确认与单段重渲、项目 / 模板 / 用量存储等；部分用例会用本机 ffmpeg 构造素材做集成测试。

### 一键打包（不依赖 wails CLI）

```powershell
./scripts/fetch-deps-windows.ps1   # 拉取 ffmpeg / NSIS / WebView2 引导程序（不入库）
./scripts/build-windows.ps1        # 前端构建 → go test → 资源生成 → 编译 → 安装包 + 便携 zip
```

```bash
./scripts/build-macos.sh              # 当前架构
ARCH=x86_64 ./scripts/build-macos.sh  # Intel
```

推送 `v*` 标签会触发 `.github/workflows/release.yml`：Windows 与 macOS（arm64 / x86_64）并行构建，再汇总为一次 GitHub Release。

## 数据与配置

- 数据库位置：Windows `%AppData%\FFFVedioAgent\FFFVedioAgent.db`；macOS `~/Library/Application Support/FFFVedioAgent/FFFVedioAgent.db`。
- 表包含 `providers`（服务商配置）、视频任务与运行记录、爬取任务与条目、模板、项目、尾帧资产、`usage_events`（用量）等。
- 视频任务的产物（分段视频、字幕、成片、`subtitle-timing.json`）写在各任务的工作目录下，通过 `/media/` 在界面内预览。
- 删除项目只解除归属，不删除任务、尾帧与磁盘文件；卸载安装包只删程序目录，保留 `%AppData%\FFFVedioAgent` 数据。

## 已知限制

- **API Key 明文存储**：写进本地 SQLite，界面仅做掩码展示；后续可接入系统钥匙串。
- **模型列表依赖 `/models`**：部分厂商无该端点，需手动填写模型名。
- **适配器成熟度不一**：只有火山方舟 Seedance 完成逐参数对齐与验收，其余为「部分」或「本地联调」，正式投产前建议先小样本试跑。
- **macOS 未在真机完整验证**：主开发环境为 Windows，macOS 配置与脚本齐备但需实机确认。
- **需自备模型额度**：视频渲染、LLM 抽取与提示词生成都会调用外部付费接口，请留意用量总览。

## 版本

当前 **v0.1.0**（2026-11-07）。完整变更见 [CHANGELOG.md](CHANGELOG.md)。
