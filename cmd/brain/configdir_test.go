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

// A process that sees a different %APPDATA% (packaged-app virtualization)
// still finds the default through the home copy, which the first read made.
func TestConfigPointerHomeCopySurvivesVirtualizedAppData(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	home := t.TempDir()
	realAppData := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", realAppData)
	t.Chdir(t.TempDir())
	target := filepath.Join(t.TempDir(), "brain.yaml")
	first, err := userPointerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writePointer(first, target); err != nil {
		t.Fatal(err)
	}
	if got := defaultConfigPath(); got != target {
		t.Fatalf("default = %s, want %s", got, target)
	}

	t.Setenv("APPDATA", t.TempDir()) // a view without the original pointer
	if got := defaultConfigPath(); got != target {
		t.Fatalf("default with another APPDATA = %s, want %s (home copy)", got, target)
	}
}
