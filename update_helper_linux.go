//go:build linux
// +build linux

package main

import "os"

func IsRunningAsAdmin() bool {
	return os.Geteuid() == 0
}

func (a *App) RestartAsAdmin() error {
	return nil
}
