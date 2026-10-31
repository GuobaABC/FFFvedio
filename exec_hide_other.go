//go:build !windows

package main

import "os/exec"

// configureHiddenProcess 非 Windows 平台无需隐藏窗口。
func configureHiddenProcess(_ *exec.Cmd) {}
