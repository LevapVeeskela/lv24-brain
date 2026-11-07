package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Some embedded terminals start without %APPDATA% / $XDG_CONFIG_HOME / $HOME
// config variables; the pointer must still be found under the home dir.
func TestUserConfigDirFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("APPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := userConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && dir != filepath.Join(home, "AppData", "Roaming") {
		t.Fatalf("dir = %s", dir)
	}
	if _, err := os.Stat(home); err != nil || dir == "" {
		t.Fatalf("dir = %q", dir)
	}
}
