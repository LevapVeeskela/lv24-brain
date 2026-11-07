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

// defaultPointerName is the file that holds the absolute path of the
// brain.yaml used when no other config is found.
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

// homePointerPath returns ~/.config/lv24-brain/config-path. It is a second
// copy outside the OS config directory: processes started by packaged
// Windows apps (MSIX, e.g. a desktop app's terminal) see a virtualized
// %APPDATA% and cannot read the first one.
func homePointerPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "lv24-brain", defaultPointerName), nil
}

// pointerPaths lists the pointer locations in lookup order, without
// duplicates (on Linux both resolve to ~/.config).
func pointerPaths() []string {
	var paths []string
	for _, resolve := range []func() (string, error){userPointerPath, homePointerPath} {
		path, err := resolve()
		if err != nil || path == "" {
			continue
		}
		duplicate := false
		for _, seen := range paths {
			duplicate = duplicate || filepath.Clean(seen) == filepath.Clean(path)
		}
		if !duplicate {
			paths = append(paths, path)
		}
	}
	return paths
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

// readPointer returns the configured default and the pointer it came from.
// A default found only in the first location is copied to the others (best
// effort), so shells that cannot see the first one find it next time.
func readPointer() (target, from string) {
	paths := pointerPaths()
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if target = strings.TrimSpace(string(raw)); target != "" {
			for _, other := range paths {
				if _, err := os.Stat(other); errors.Is(err, os.ErrNotExist) {
					_ = writePointer(other, target)
				}
			}
			return target, path
		}
	}
	return "", ""
}

func writePointer(path, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(target+"\n"), 0o644)
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
	if target, _ := readPointer(); target != "" {
		return target
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
	paths := pointerPaths()
	if len(paths) == 0 {
		return reportError(stderr, "config", errors.New("cannot determine a user config or home directory"))
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
		if err := writePointer(paths[0], target); err != nil {
			return reportError(stderr, "config", err)
		}
		for _, other := range paths[1:] {
			_ = writePointer(other, target)
		}
		fmt.Fprintf(stdout, "default config: %s\n(pointer: %s)\n", target, strings.Join(paths, ", "))
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
			if _, from := readPointer(); from != "" {
				source = "user default (" + from + ")"
			}
		}
		fmt.Fprintf(stdout, "config: %s\nsource: %s\n", resolved, source)
		if source == "fallback" {
			// Say where the pointer was looked for, so a shell that sees a
			// different user config directory can be diagnosed.
			for _, path := range paths {
				_, statErr := os.Stat(path)
				fmt.Fprintf(stdout, "pointer: %s (%v)\n", path, statErr)
			}
		}
		return 0
	case "clear":
		if len(args) != 1 {
			return usage()
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return reportError(stderr, "config", err)
			}
		}
		fmt.Fprintln(stdout, "default config cleared")
		return 0
	default:
		return usage()
	}
}
