package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Install sources whose files belong to another package manager. brain must
// not replace those files itself; the manager updates them.
const (
	installSourceNPM = "npm"
)

// installSource reports which package manager installed the running binary,
// or "" for a standalone install (installer or manual download). The npm
// launcher sets BRAIN_INSTALL_SOURCE=npm; a binary under node_modules is npm
// too, for direct invocations.
func installSource() string {
	if source := strings.ToLower(strings.TrimSpace(os.Getenv("BRAIN_INSTALL_SOURCE"))); source == installSourceNPM {
		return installSourceNPM
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(filepath.ToSlash(exe), "/") {
		if part == "node_modules" {
			return installSourceNPM
		}
	}
	return ""
}

// managerUpdateCommand is the command that updates a package-managed brain.
func managerUpdateCommand(source string) string {
	switch source {
	case installSourceNPM:
		return "npm i -g lv24-brain@latest"
	}
	return ""
}
