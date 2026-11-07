package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
)

// defaultPointerName is the file under <UserConfigDir>/lv24-brain that holds
// the absolute path of the brain.yaml used when no other config is found.
const defaultPointerName = "config-path"

// userPointerPath returns <UserConfigDir>/lv24-brain/config-path
// (%APPDATA% on Windows, ~/Library/Application Support on macOS,
// $XDG_CONFIG_HOME or ~/.config on Linux).
func userPointerPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lv24-brain", defaultPointerName), nil
}

// userConfigDir is os.UserConfigDir with a fallback for shells that start
// without %APPDATA% (some embedded terminals and agent runners): the same
// directory derived from the home directory.
func userConfigDir() (string, error) {
	if dir, err := os.UserConfigDir(); err == nil {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(home, "AppData", "Roaming"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support"), nil
	default:
		return filepath.Join(home, ".config"), nil
	}
}

// defaultConfigPath resolves the config used when --config is not passed:
// $BRAIN_CONFIG, then ./brain.yaml if it exists, then the user default set by
// `brain config use`, then "brain.yaml" (so errors still name a path).
func defaultConfigPath() string {
	if env := strings.TrimSpace(os.Getenv("BRAIN_CONFIG")); env != "" {
		return env
	}
	if _, err := os.Stat(config.FileName); err == nil {
		return config.FileName
	}
	if pointer, err := userPointerPath(); err == nil {
		if raw, err := os.ReadFile(pointer); err == nil {
			if target := strings.TrimSpace(string(raw)); target != "" {
				return target
			}
		}
	}
	return config.FileName
}

// runConfig manages the user default config pointer.
func runConfig(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: brain config use <path-to-brain.yaml> | show | clear")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	pointer, err := userPointerPath()
	if err != nil {
		return reportError(stderr, "config", err)
	}
	switch args[0] {
	case "use":
		if len(args) != 2 {
			return usage()
		}
		target, err := filepath.Abs(args[1])
		if err != nil {
			return reportError(stderr, "config", err)
		}
		if _, err := config.Load(target); err != nil {
			return reportError(stderr, "config", fmt.Errorf("%s is not a usable brain.yaml: %w", target, err))
		}
		if err := os.MkdirAll(filepath.Dir(pointer), 0o755); err != nil {
			return reportError(stderr, "config", err)
		}
		if err := os.WriteFile(pointer, []byte(target+"\n"), 0o644); err != nil {
			return reportError(stderr, "config", err)
		}
		fmt.Fprintf(stdout, "default config: %s\n(pointer: %s)\n", target, pointer)
		return 0
	case "show":
		if len(args) != 1 {
			return usage()
		}
		resolved := defaultConfigPath()
		source := "fallback"
		switch {
		case strings.TrimSpace(os.Getenv("BRAIN_CONFIG")) != "":
			source = "BRAIN_CONFIG"
		case resolved == config.FileName:
			if _, err := os.Stat(config.FileName); err == nil {
				source = "working directory"
			}
		default:
			source = "user default (" + pointer + ")"
		}
		fmt.Fprintf(stdout, "config: %s\nsource: %s\n", resolved, source)
		return 0
	case "clear":
		if len(args) != 1 {
			return usage()
		}
		if err := os.Remove(pointer); err != nil && !errors.Is(err, os.ErrNotExist) {
			return reportError(stderr, "config", err)
		}
		fmt.Fprintln(stdout, "default config cleared")
		return 0
	default:
		return usage()
	}
}
