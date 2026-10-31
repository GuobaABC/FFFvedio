# 把仓库内已下载的 ffmpeg 安装到当前用户目录并写入用户 PATH，
# 便于命令行直接使用 ffmpeg（桌面应用本身优先加载安装目录内的内置副本，不依赖 PATH）。
param(
    [string]$Dst = (Join-Path $env:LOCALAPPDATA 'ffmpeg\bin')
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$src = Join-Path $root 'build\deps\bin\ffmpeg.exe'
if (-not (Test-Path $src)) {
    throw '未找到 build\deps\bin\ffmpeg.exe，请先执行 scripts\fetch-deps-windows.ps1'
}

New-Item -ItemType Directory -Force -Path $Dst | Out-Null
Copy-Item $src (Join-Path $Dst 'ffmpeg.exe') -Force

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike ('*' + $Dst + '*')) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $Dst), 'User')
    Write-Host "已把 $Dst 加入用户 PATH（新开终端生效）"
} else {
    Write-Host "用户 PATH 已包含 $Dst"
}
& (Join-Path $Dst 'ffmpeg.exe') -hide_banner -version 2>&1 | Select-Object -First 1
