# FFFVedioAgent

FFF = Fast Fast Fast。`Vedio` 为有意保留的拼写彩蛋。

跨平台桌面应用（Windows / macOS）的 **v0.0.1 基础工程**：深色界面，左右 3:7 分隔——左侧为 logo 与可折叠选项卡导航，右侧为选项卡内容区（右上角悬浮实时时间 `YYYY:MM:DD HH:mm:ss`）。「系统设置 → 提供商」已可用：按文本模型 / 多模态模型 / 多模态生成模型三类登记服务商，通过 `/models` 拉取模型名称，配置写入本地 SQLite；「爬虫」仍为占位页。

## 技术选型

| 层 | 选型 | 说明 |
| --- | --- | --- |
| 前端框架 | Vue 3.5（Composition API + `<script setup>`） | 无状态管理、无 UI 框架 |
| 语言 | TypeScript 5.9（`strict` 严格模式） | 类型检查由 `vue-tsc` 完成 |
| 构建 | Vite 7 | 开发服务器 / 生产构建 |
| 桌面框架 | Wails v2.17.0 | 见下方版本说明 |
| 后端语言 | Go 1.25 | 窗口与生命周期、提供商配置、模型列表请求 |
| 本地存储 | SQLite（`modernc.org/sqlite` v1.61.0） | 纯 Go 实现，无需 CGO，存放提供商配置 |

### 为什么用 Wails v2.17.0

- **v2 是当前稳定线**，v2.17.0 为 v2 最新稳定版本；Wails v3 仍处于 alpha，桌面端 API 与工程结构还在变化，不适合作为 v0.0.1 的基线。
- v2 在该环境下已实测可编译、可运行（Go 1.25.6 + WebView2），并提供 `wails doctor` 环境自检、`wails dev` 热更新、跨平台打包（Windows exe/NSIS、macOS .app/.dmg）。
- 前端侧主版本刻意与 Wails 官方 `vue-ts` 模板保持一致（Vite 7 / TypeScript 5.9），而不是追最新的 Vite 8 / TS 6，避免脚手架与工具链适配风险。

## 目录结构

```
FFFVedio/
├─ main.go                     # 入口：Wails 窗口、内嵌前端资源、绑定 App
├─ app.go                      # 应用实例：生命周期 + 本地数据库句柄
├─ db.go                       # SQLite 打开 / PRAGMA / 建表
├─ provider.go                 # 提供商 CRUD（文本 / 多模态 / 多模态生成）
├─ provider_models.go          # GET /models 拉取模型名（兼容三种响应与鉴权）
├─ provider_test.go            # Go 测试：CRUD + 模型列表拉取
├─ go.mod / go.sum
├─ wails.json                  # Wails 工程配置（含产品信息、前端命令）
├─ build/                      # 各平台打包资源
│  ├─ appicon.png              # 应用图标源图（1024x1024）
│  ├─ darwin/                  # macOS Info.plist
│  └─ windows/                 # icon.ico、info.json、manifest、NSIS 脚本
└─ frontend/
   ├─ index.html
   ├─ package.json
   ├─ tsconfig.json            # TypeScript 严格模式配置
   ├─ vite.config.ts
   ├─ public/favicon.svg
   ├─ dist/                    # 前端构建产物（git 忽略，Wails 会内嵌它）
   └─ src/
      ├─ main.ts               # 挂载入口
      ├─ App.vue               # 主布局：左侧 logo + 选项卡，右侧内容 + 悬浮时钟
      ├─ components/ProviderSettings.vue  # 提供商：三分类 + 表单 + 列表
      ├─ services/backend.ts   # Wails 运行时桥接（不依赖 wailsjs）
      ├─ types/provider.ts     # 提供商类型与分类定义
      ├─ vite-env.d.ts
      └─ assets/styles/global.css   # 深色主题变量与全局基础样式
```

## 设置 - 提供商

「系统设置 → 提供商」按厂商文档合集的三类口径分组：

| 分类 | 取值 | 含义 |
| --- | --- | --- |
| 文本模型 | `text` | 文本输入 → 文本输出 |
| 多模态模型 | `multimodal` | 图像 / 音频 / 视频 / 文档输入 → 文本输出 |
| 多模态生成模型 | `multimodal_gen` | 生成图像、视频、语音、音乐等非文本内容 |

每条提供商保存：显示名称（留空取 Base URL 域名）、Base URL、API Key、模型名称。

- **模型名称来源**：点击「获取模型」由 Go 侧请求 `GET {baseURL}/models`（Base URL 未带版本段时以 `/v1/models` 作为次选端点），结果填入模型名称下拉，也可手动填写。
- **认证方式**（按手册「接入要点」）：默认 `Authorization: Bearer <key>`；`anthropic` 域名用 `x-api-key` + `anthropic-version`；`googleapis.com` 用 `x-goog-api-key`。
- **响应兼容**：OpenAI / Anthropic 的 `data[].id`、Gemini 的 `models[].name`（自动去掉 `models/` 前缀）、纯数组三种结构；去重后按名称排序。

### 本地存储

- 驱动：`modernc.org/sqlite`（纯 Go，无 CGO，Windows / macOS 均可直接构建）。
- 位置：Windows `%AppData%\FFFVedioAgent\FFFVedioAgent.db`，macOS `~/Library/Application Support/FFFVedioAgent/FFFVedioAgent.db`（WAL 模式）。
- 表结构：

```sql
CREATE TABLE providers (
  id TEXT PRIMARY KEY, category TEXT NOT NULL, name TEXT NOT NULL,
  base_url TEXT NOT NULL, api_key TEXT NOT NULL, model TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
```

### 前后端绑定

前端不 import `frontend/wailsjs`，而是在 `src/services/backend.ts` 中直接调用 Wails 注入的 `window.go.main.App.*`；浏览器预览时缺少运行时会给出明确提示，不影响前端独立构建。Go 侧方法在 `main.go` 的 `Bind: []interface{}{app}` 注册：

| 方法 | 说明 |
| --- | --- |
| `ListProviders()` | 读取全部提供商 |
| `SaveProvider(Provider)` | 新增（ID 为空）/ 更新 |
| `DeleteProvider(id)` | 删除 |
| `FetchModels(baseURL, apiKey)` | 拉取模型列表 |

## 环境要求

- Node.js ≥ 20（实测 24.10.0）与 npm
- Go ≥ 1.25（实测 1.25.6）
- Wails CLI v2.17.0：`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0`
- Windows：WebView2 Runtime（Windows 11 自带）；打包安装程序需 NSIS（可选）
- macOS：Xcode Command Line Tools（`xcode-select --install`）

首次使用先自检环境：

```bash
wails doctor
```

## 本地开发

### 1) 纯前端开发（无需 Go / Wails）

```bash
cd frontend
npm install
npm run dev        # http://localhost:5173
```

前端不引用 `wailsjs` 绑定，因此可以在浏览器里开发与预览布局；但「提供商」页需要 Wails 运行时才能读写 SQLite，浏览器中会提示缺少运行时。

### 2) 桌面开发（热更新）

```bash
wails dev
```

`wails dev` 会自动执行 `npm install`、启动 Vite dev server（`http://localhost:5173`）并编译运行桌面窗口；修改前端代码即时生效。

## 构建

### Windows

```bash
wails build                    # 产物：build/bin/FFFVedioAgent.exe
wails build -nsis              # 额外生成安装包（需要 NSIS）
```

也可指定架构：`wails build -platform windows/arm64`。

### macOS

```bash
wails build -platform darwin/universal   # 产物：build/bin/FFFVedioAgent.app
wails build -platform darwin/universal -dmg   # 额外生成 .dmg
```

注意：macOS 应用必须在 macOS 上构建（签名 / notarize 也需在 macOS 完成）。本工程已包含 `build/darwin/Info.plist`（Bundle ID `com.fffvedioagent.app`），并对 macOS 强制使用 Dark Aqua 外观。

### 仅构建前端

```bash
cd frontend
npm run build      # vue-tsc 类型检查 + vite build → frontend/dist
```

`main.go` 通过 `//go:embed all:frontend/dist` 把前端产物打进可执行文件，因此单独执行 `go build` 前需要先构建前端（`wails build` / `wails dev` 会自动完成这一步）。

## 运行与构建测试

```bash
# 1. 类型检查（严格模式）
cd frontend && npm run typecheck

# 2. 前端生产构建
cd frontend && npm run build

# 3. Go 静态检查、测试与编译
go vet ./...
go test ./...
go build ./...

# 4. 桌面打包
wails build

# 5. 启动验证
build\bin\FFFVedioAgent.exe        # Windows
open build/bin/FFFVedioAgent.app   # macOS
```

验收标准：窗口以 1100×720 打开，左右 3:7 分隔；左上角为 FFF logo、`FFFVedioAgent` 与 `v0.0.1`；logo 下方为可折叠选项卡「系统设置 → 爬虫 / 提供商」（「模型」子项已移除，避免与提供商中的模型配置重复）；右侧内容区右上角悬浮显示实时时间 `YYYY:MM:DD HH:mm:ss`。在「提供商」中可切换三类分类、新增 / 编辑 / 删除服务商，点击「获取模型」按 Base URL + API Key 拉取模型列表，保存后重启应用仍可从 SQLite 读回。

Go 测试（`go test ./...`）覆盖：提供商 CRUD（临时 SQLite 真实读写）、`/models` 拉取（`httptest` 模拟 OpenAI 兼容端点的端点回退与 Bearer 认证、401 错误透出）、三种响应结构解析与候选端点生成。前端以 `vue-tsc` 类型检查 + Vite 构建 + 桌面端真实操作（新增 → 保存 → 重启读取 → 删除）为准。

## 发布打包（不依赖 wails CLI）

仓库自带一套纯脚本打包流程，产物把运行所需依赖全部打进去：前端资源由 `go:embed` 内嵌、SQLite 为纯 Go 实现、ffmpeg 随安装包分发、WebView2 引导程序内置。

### Windows

```powershell
# 1. 拉取第三方依赖：ffmpeg 静态构建、NSIS、WebView2 引导程序（均落在 build/ 下，不入库）
./scripts/fetch-deps-windows.ps1

# 2. 一键打包：前端构建 → go test → 生成资源（FFF 图标 + 版本信息 + DPI 清单）→ 编译 → 安装包 + 便携 zip
./scripts/build-windows.ps1
```

产物在 `build/bin/`：

- `FFFVedioAgent-<版本>-windows-amd64-installer.exe`：NSIS 安装包，当前用户目录安装（不弹 UAC），中文界面，卸载只删程序目录、保留 `%AppData%\FFFVedioAgent` 数据。
- `FFFVedioAgent-<版本>-windows-amd64-portable.zip`：解压即用，`bin\ffmpeg.exe` 会被自动加载。

ffmpeg 查找顺序：环境变量 `FFFVedio_FFMPEG` → 可执行文件同级的 `bin/`、`deps/` → 系统 PATH，因此装完无需再配置环境变量。若需要在命令行使用 ffmpeg，可执行 `./scripts/install-ffmpeg-user.ps1`（安装到 `%LOCALAPPDATA%\ffmpeg\bin` 并写入用户 PATH）。

### macOS

必须在 macOS 上构建（Wails 的 darwin 目标依赖 cgo 与系统框架，无法交叉编译）：

```bash
./scripts/build-macos.sh              # 当前架构
ARCH=x86_64 ./scripts/build-macos.sh  # Intel
```

脚本会组装 `FFFVedioAgent.app`（`Contents/Info.plist`、由 `build/appicon.png` 生成的 `appicon.icns`、`Contents/Resources/bin/ffmpeg`）、ad-hoc 签名，并输出 `.dmg` 与 `.zip`。分发给他人时属于未公证构建，首次打开需右键 →「打开」。

### 自动发布

推送 `v*` 标签会触发 `.github/workflows/release.yml`：Windows 与 macOS（arm64 / x86_64）三个 runner 并行构建安装包，再由 `gh release create` 汇总为一次 Release。本地没有对应平台时，用这条路径产出正式包即可。

## 已知限制

- **API Key 明文存储**：为方便本地调试，API Key 以明文写入 SQLite，界面仅做掩码展示；后续可接入系统钥匙串（Windows Credential Manager / macOS Keychain）。
- **仅登记提供商**：不支持发起对话、生成图像 / 视频 / 语音等实际模型调用；「爬虫」子选项卡仍为占位页。
- **模型列表依赖 `/models`**：部分厂商（如仅提供生成式接口的服务）没有该端点，需手动填写模型名称；请求失败时界面会展示状态码与响应片段。
- **右上角时钟为纯展示**：每秒由前端本地时间刷新，未接入后端时间源或时区配置。
- **版本号是硬编码字面量**：前端 `v0.0.1` 与 `main.go` / `wails.json` 中的版本号需手动保持一致，尚未做构建期注入。
- **图标为几何占位图**：`build/appicon.png` 与 `build/windows/icon.ico` 由脚本生成的简易 FFF 图形，后续可替换为正式设计稿。
- **窗口标题栏跟随系统**：Windows 上标题栏颜色受系统主题与「在标题栏显示强调色」设置影响（本机开启后显示为强调色），应用内主体界面始终为深色。
- **macOS 未在真机验证**：当前开发环境为 Windows，macOS 仅提供配置与命令，需在 macOS 上执行一次 `wails build -platform darwin/universal` 确认。
- **首次跑 `go build` 前需要前端产物**：`frontend/dist` 被 git 忽略，克隆后请先执行 `npm run build` 或直接使用 `wails build` / `wails dev`。
