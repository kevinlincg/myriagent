//go:build unix

package agent

import "syscall"

// detachAttr：新的工作階段（同 setsid／nohup 開的程式：脫離終端機與父程序）
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
