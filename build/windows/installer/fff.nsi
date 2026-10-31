Unicode true

; ---------------------------------------------------------------------------
; FFFVedioAgent Windows 安装包脚本
; 由 scripts/build-windows.ps1 调用 makensis 时传入以下定义：
;   APP_EXE       已编译好的 FFFVedioAgent.exe 绝对路径
;   FFF_DEPS_BIN  内置依赖目录（含 ffmpeg.exe / ffprobe.exe）
;   APP_VERSION   版本号，例如 0.0.1
;   OUT_FILE      安装包输出路径
; 采用「当前用户安装」，不弹 UAC，装完即可用。
; ---------------------------------------------------------------------------

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "WinVer.nsh"
!include "x64.nsh"
!include "FileFunc.nsh"

!ifndef APP_EXE
  !error "缺少 APP_EXE：请用 -DAPP_EXE=<可执行文件路径> 指定主程序"
!endif
!ifndef FFF_DEPS_BIN
  !define FFF_DEPS_BIN ""
!endif
!ifndef APP_VERSION
  !define APP_VERSION "0.0.1"
!endif
!ifndef OUT_FILE
  !define OUT_FILE "FFFVedioAgent-${APP_VERSION}-windows-amd64-installer.exe"
!endif

!define APP_NAME      "FFFVedioAgent"
!define APP_PRODUCT   "FFFVedio 视频生成智能体"
!define APP_EXE_NAME  "FFFVedioAgent.exe"
!define APP_PUBLISHER "FFFVedioAgent"
!define APP_UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\FFFVedioAgent"

Name "${APP_PRODUCT}"
BrandingText "${APP_NAME} ${APP_VERSION}"
OutFile "${OUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\FFFVedioAgent"
InstallDirRegKey HKCU "Software\FFFVedioAgent" "InstallDir"
RequestExecutionLevel user
SetCompressor /SOLID lzma
ShowInstDetails show
ShowUninstDetails show

VIProductVersion "${APP_VERSION}.0"
VIAddVersionKey "CompanyName"     "${APP_PUBLISHER}"
VIAddVersionKey "FileDescription" "${APP_PRODUCT} 安装程序"
VIAddVersionKey "ProductVersion"  "${APP_VERSION}"
VIAddVersionKey "FileVersion"     "${APP_VERSION}"
VIAddVersionKey "LegalCopyright"  "Copyright (C) 2026 ${APP_PUBLISHER}"
VIAddVersionKey "ProductName"     "${APP_NAME}"

ManifestDPIAware true

!define MUI_ICON   "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\${APP_EXE_NAME}"
!define MUI_FINISHPAGE_RUN_TEXT "立即启动 ${APP_PRODUCT}"
!define MUI_FINISHPAGE_NOAUTOCLOSE

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"

Function .onInit
  ; 只支持 64 位 Windows 10 及以上
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "本程序仅支持 64 位 Windows。" /SD IDOK
    Abort
  ${EndIf}
  ${IfNot} ${AtLeastWin10}
    MessageBox MB_ICONSTOP "本程序需要 Windows 10 及以上版本。" /SD IDOK
    Abort
  ${EndIf}
FunctionEnd

; WebView2 运行时：系统已装则跳过，否则静默安装（安装包内已内置引导程序）
!macro ensureWebView2
  SetRegView 64
  ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ${If} $0 == ""
    ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ${EndIf}
  ${If} $0 == ""
    DetailPrint "正在安装 WebView2 运行时…"
    InitPluginsDir
    SetOutPath "$pluginsdir"
    File "tmp\MicrosoftEdgeWebview2Setup.exe"
    ExecWait '"$pluginsdir\MicrosoftEdgeWebview2Setup.exe" /silent /install'
    SetOutPath "$INSTDIR"
  ${Else}
    DetailPrint "已检测到 WebView2 运行时 $0，跳过安装"
  ${EndIf}
!macroend

Section "主程序" SecMain
  SetShellVarContext current
  SetOutPath "$INSTDIR"

  !insertmacro ensureWebView2

  DetailPrint "正在安装主程序…"
  File "/oname=${APP_EXE_NAME}" "${APP_EXE}"

  ; 内置 ffmpeg：应用启动时优先从 bin 目录加载，用户无需自行安装或配置 PATH
  !if "${FFF_DEPS_BIN}" != ""
    !if /FILEEXISTS "${FFF_DEPS_BIN}\ffmpeg.exe"
      DetailPrint "正在安装内置 ffmpeg…"
      SetOutPath "$INSTDIR\bin"
      File "${FFF_DEPS_BIN}\ffmpeg.exe"
      !if /FILEEXISTS "${FFF_DEPS_BIN}\ffprobe.exe"
        File "${FFF_DEPS_BIN}\ffprobe.exe"
      !endif
      SetOutPath "$INSTDIR"
    !endif
  !endif

  WriteRegStr HKCU "Software\FFFVedioAgent" "InstallDir" "$INSTDIR"

  DetailPrint "正在创建快捷方式…"
  CreateDirectory "$SMPROGRAMS\${APP_NAME}"
  CreateShortCut "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE_NAME}" "" "$INSTDIR\${APP_EXE_NAME}" 0
  CreateShortCut "$SMPROGRAMS\${APP_NAME}\卸载 ${APP_NAME}.lnk" "$INSTDIR\uninstall.exe"
  CreateShortCut "$DESKTOP\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE_NAME}" "" "$INSTDIR\${APP_EXE_NAME}" 0

  WriteUninstaller "$INSTDIR\uninstall.exe"

  SetRegView 64
  WriteRegStr HKCU "${APP_UNINST_KEY}" "Publisher" "${APP_PUBLISHER}"
  WriteRegStr HKCU "${APP_UNINST_KEY}" "DisplayName" "${APP_PRODUCT}"
  WriteRegStr HKCU "${APP_UNINST_KEY}" "DisplayVersion" "${APP_VERSION}"
  WriteRegStr HKCU "${APP_UNINST_KEY}" "DisplayIcon" "$INSTDIR\${APP_EXE_NAME}"
  WriteRegStr HKCU "${APP_UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${APP_UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${APP_UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${APP_UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${APP_UNINST_KEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD HKCU "${APP_UNINST_KEY}" "EstimatedSize" "$0"
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  SetRegView 64

  Delete "$DESKTOP\${APP_NAME}.lnk"
  Delete "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk"
  Delete "$SMPROGRAMS\${APP_NAME}\卸载 ${APP_NAME}.lnk"
  RMDir  "$SMPROGRAMS\${APP_NAME}"

  ; 只删安装目录，用户数据（%AppData%\FFFVedioAgent）保留，避免误删任务与模板
  RMDir /r "$INSTDIR"

  DeleteRegKey HKCU "${APP_UNINST_KEY}"
  DeleteRegKey HKCU "Software\FFFVedioAgent"
SectionEnd
