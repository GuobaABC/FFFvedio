# FFFVedioAgent Windows 一键打包：前端构建 → 资源嵌入 → Go 编译 → NSIS 安装包 + 便携 zip。
# 依赖缺失时先执行 scripts/fetch-deps-windows.ps1。
param(
    [string]$Version = '',
    [switch]$SkipFrontend,
    [switch]$SkipTests,
    [switch]$SkipInstaller
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Invoke-Step([string]$name, [scriptblock]$body) {
    Write-Host "`n==> $name" -ForegroundColor Cyan
    & $body
    if ($LASTEXITCODE -ne 0) { throw "$name 失败（exit=$LASTEXITCODE）" }
}

if (-not $Version) {
    $info = Get-Content (Join-Path $root 'wails.json') -Raw | ConvertFrom-Json
    $Version = $info.Info.ProductVersion
}
if (-not $Version) { throw '未能从 wails.json 读取 productVersion' }

$binDir = Join-Path $root 'build\bin'
$depsBin = Join-Path $root 'build\deps\bin'
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
$exe = Join-Path $binDir "FFFVedioAgent.exe"
$arch = 'amd64'

# ---- 1. 前端产物（Go 侧 go:embed all:frontend/dist 依赖它） ----
if (-not $SkipFrontend) {
    Push-Location (Join-Path $root 'frontend')
    if (-not (Test-Path 'node_modules')) { npm install }
    Invoke-Step '前端构建（vue-tsc + vite）' { npm run build }
    Pop-Location
}

# ---- 2. 单元测试 ----
if (-not $SkipTests) {
    Invoke-Step 'Go 测试' { go test ./... }
}

# ---- 3. Windows 资源（FFF 图标 + 版本信息 + DPI 清单） ----
$goversioninfo = Join-Path (& go env GOPATH) 'bin\goversioninfo.exe'
if (-not (Test-Path $goversioninfo)) {
    Invoke-Step '安装 goversioninfo' { go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest }
}
$syso = Join-Path $root "resource_windows_$arch.syso"
Invoke-Step '生成资源文件' {
    & $goversioninfo -64 -charset 1200 -o $syso `
        -icon (Join-Path $root 'build\windows\icon.ico') `
        -manifest (Join-Path $root 'build\windows\app.manifest') `
        (Join-Path $root 'build\windows\versioninfo.json')
}

# ---- 4. 编译主程序（desktop,production 标签缺一不可，否则 Wails 启动报错） ----
Invoke-Step "编译 FFFVedioAgent.exe $Version" {
    go build -tags desktop,production -trimpath -ldflags "-w -s -H windowsgui" -o $exe .
}

$sizeMb = [math]::Round((Get-Item $exe).Length / 1MB, 2)
Write-Host "主程序：$exe ($sizeMb MB)"

# ---- 5. 便携版 zip（解压即用，含内置 ffmpeg） ----
$portable = Join-Path $binDir "FFFVedioAgent-$Version-windows-$arch-portable"
if (Test-Path $portable) { Remove-Item -Recurse -Force $portable }
New-Item -ItemType Directory -Force -Path $portable | Out-Null
Copy-Item $exe (Join-Path $portable 'FFFVedioAgent.exe')
if (Test-Path (Join-Path $depsBin 'ffmpeg.exe')) {
    New-Item -ItemType Directory -Force -Path (Join-Path $portable 'bin') | Out-Null
    Copy-Item (Join-Path $depsBin '*.exe') (Join-Path $portable 'bin') -Force
}
@"
FFFVedioAgent $Version 便携版
1. 解压到任意目录（路径避免中文与空格更稳妥）。
2. 双击 FFFVedioAgent.exe 启动；bin 目录内的 ffmpeg 会被自动加载，无需另装或配置 PATH。
3. 数据（数据库、任务产物）保存在 %AppData%\FFFVedioAgent，删除目录即彻底重置。
"@ | Set-Content -Path (Join-Path $portable 'README.txt') -Encoding utf8
$portableZip = Join-Path $binDir "FFFVedioAgent-$Version-windows-$arch-portable.zip"
if (Test-Path $portableZip) { Remove-Item -Force $portableZip }
Compress-Archive -Path (Join-Path $portable '*') -DestinationPath $portableZip
Remove-Item -Recurse -Force $portable
Write-Host "便携包：$portableZip"

# ---- 6. NSIS 安装包 ----
if (-not $SkipInstaller) {
    $makensis = Get-ChildItem (Join-Path $root 'build\tools') -Recurse -Filter 'makensis.exe' -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $makensis) { $makensis = Get-Command makensis -ErrorAction SilentlyContinue }
    if (-not $makensis) { throw '找不到 makensis，请先执行 scripts/fetch-deps-windows.ps1' }
    $makensisPath = if ($makensis.FullName) { $makensis.FullName } else { $makensis.Source }

    $installer = Join-Path $binDir "FFFVedioAgent-$Version-windows-$arch-installer.exe"
    if (Test-Path $installer) { Remove-Item -Force $installer }
    Invoke-Step '生成安装包' {
        & $makensisPath `
            "-DAPP_EXE=$exe" `
            "-DFFF_DEPS_BIN=$depsBin" `
            "-DAPP_VERSION=$Version" `
            "-DOUT_FILE=$installer" `
            (Join-Path $root 'build\windows\installer\fff.nsi')
    }
    if (-not (Test-Path $installer)) { throw '安装包未生成' }
    $installerMb = [math]::Round((Get-Item $installer).Length / 1MB, 2)
    Write-Host "安装包：$installer ($installerMb MB)" -ForegroundColor Green
}
