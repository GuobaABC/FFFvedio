//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// configureHiddenProcess 避免调用 ffmpeg 时弹出控制台窗口。
func configureHiddenProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
