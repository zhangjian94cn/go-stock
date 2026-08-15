//go:build darwin
// +build darwin

package main

func IsRunningAsAdmin() bool {
	return true
}

func (a *App) RestartAsAdmin() error {
	return nil
}
