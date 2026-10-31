# 下载 Windows 打包所需的第三方依赖（ffmpeg / NSIS / WebView2 引导程序）。
# 全部落在 build/ 下，已被 .gitignore 忽略，不会进入仓库。
param(
    [string]$FfmpegWheel = 'https://pypi.tuna.tsinghua.edu.cn/packages/e5/55/1d50cdd78d862a30c2ce44f01cea387f77b364b21fa2fecf1590258b215b/imageio_ffmpeg-0.7.0-py3-none-win_amd64.whl',
    [string]$NsisZip = 'https://downloads.sourceforge.net/project/nsis/NSIS%203/3.11/nsis-3.11.zip',
    [string]$WebView2Bootstrapper = 'https://go.microsoft.com/fwlink/p/?LinkId=2124703',
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$deps = Join-Path $root 'build\deps'
$tools = Join-Path $root 'build\tools'
$bin = Join-Path $deps 'bin'
New-Item -ItemType Directory -Force -Path $bin, $tools | Out-Null

function Get-Remote([string]$url, [string]$out) {
    Write-Host "下载 $url"
    curl.exe -sSL --fail --retry 3 --max-time 1800 -o $out $url
    if ($LASTEXITCODE -ne 0) { throw "下载失败：$url" }
}

# ---- ffmpeg（静态构建，随安装包一起分发） ----
$ffmpeg = Join-Path $bin 'ffmpeg.exe'
if ($Force -or -not (Test-Path $ffmpeg)) {
    $whl = Join-Path $deps 'imageio-ffmpeg.whl'
    $stage = Join-Path $deps 'ffmpeg-stage'
    Get-Remote $FfmpegWheel $whl
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [IO.Compression.ZipFile]::ExtractToDirectory($whl, $stage)
    $found = Get-ChildItem $stage -Recurse -Filter 'ffmpeg-win-x86_64-*.exe' | Select-Object -First 1
    if (-not $found) { throw "wheel 里没找到 ffmpeg 可执行文件：$stage" }
    Copy-Item $found.FullName $ffmpeg -Force
    Remove-Item -Recurse -Force $stage, $whl
    Write-Host "ffmpeg 就绪：$ffmpeg"
} else {
    Write-Host "ffmpeg 已存在，跳过下载"
}

# ffprobe 不是必需项（应用只用它探测环境），存在则一并打包
$ffprobe = Join-Path $bin 'ffprobe.exe'
if (Test-Path $ffprobe) { Write-Host "ffprobe 就绪：$ffprobe" } else { Write-Host '未提供 ffprobe.exe，安装包将只包含 ffmpeg' }

# ---- WebView2 引导程序（安装包内置，目标机器缺运行时时可离线触发安装） ----
$wv2 = Join-Path $root 'build\windows\installer\tmp\MicrosoftEdgeWebview2Setup.exe'
if ($Force -or -not (Test-Path $wv2)) {
    New-Item -ItemType Directory -Force -Path (Split-Path $wv2) | Out-Null
    Get-Remote $WebView2Bootstrapper $wv2
}
Write-Host "WebView2 引导程序：$wv2"

# ---- NSIS（makensis） ----
$makensis = Get-ChildItem $tools -Recurse -Filter 'makensis.exe' -ErrorAction SilentlyContinue | Select-Object -First 1
if ($Force -or -not $makensis) {
    $zip = Join-Path $tools 'nsis.zip'
    Get-Remote $NsisZip $zip
    $dest = Join-Path $tools 'nsis'
    if (Test-Path $dest) { Remove-Item -Recurse -Force $dest }
    Expand-Archive -Path $zip -DestinationPath $dest -Force
    Remove-Item $zip -Force
    $makensis = Get-ChildItem $dest -Recurse -Filter 'makensis.exe' | Select-Object -First 1
}
if (-not $makensis) { throw 'NSIS 准备失败' }
Write-Host "makensis：$($makensis.FullName)"
