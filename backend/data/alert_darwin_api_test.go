//go:build darwin
// +build darwin

package data

import (
	"testing"
)

// @Author 2lovecode
// @Date 2025/02/06 17:50
// @Desc
// -----------------------------------------------------------------------------------

func TestAlert(t *testing.T) {
	if api := NewAlertWindowsApi("go-stock", "Hello, World!", "This is a toast notification.", "../../build/appicon.png"); api == nil {
		t.Fatal("expected alert API")
	}
}
