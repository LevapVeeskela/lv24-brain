package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var binaryMagic = struct {
	zip  []byte
	gzip []byte
}{
	zip:  []byte{'P', 'K'},
	gzip: []byte{0x1f, 0x8b},
}

// stageBinary materializes a writable binary next to target. Raw binaries are
// copied and zip/tar.gz archives are extracted. The returned path is always in
// target's directory so the final rename stays on one filesystem.
func stageBinary(artifactPath, targetDir, targetBase string) (string, error) {
	raw, err := os.Open(artifactPath)
	if err != nil {
		return "", fmt.Errorf("update: open artifact: %w", err)
	}
	defer func() { _ = raw.Close() }()
	head := make([]byte, 4)
	n, _ := io.ReadFull(raw, head)
	head = head[:n]
	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("update: rewind artifact: %w", err)
	}

	switch {
	case bytes.HasPrefix(head, binaryMagic.zip):
		return extractZipBinary(raw, targetDir, targetBase)
	case bytes.HasPrefix(head, binaryMagic.gzip):
		return extractTarGzBinary(raw, targetDir, targetBase)
	default:
		return copyToTemp(raw, targetDir, "."+targetBase+"-new-*", 0o755)
	}
}

func extractZipBinary(source io.ReaderAt, targetDir, targetBase string) (string, error) {
	size, ok := readerSize(source)
	if !ok {
		return "", errors.New("update: cannot determine zip size")
	}
	archive, err := zip.NewReader(source, size)
	if err != nil {
		return "", fmt.Errorf("update: open zip release: %w", err)
	}
	entry, err := pickBinaryEntry(archive.File, func(f *zip.File) (string, bool) {
		return filepath.Base(f.Name), f.FileInfo().IsDir()
	})
	if err != nil {
		return "", err
	}
	reader, err := entry.Open()
	if err != nil {
		return "", fmt.Errorf("update: open zip entry %s: %w", entry.Name, err)
	}
	defer func() { _ = reader.Close() }()
	return copyToTemp(reader, targetDir, "."+targetBase+"-new-*", 0o755)
}

func extractTarGzBinary(source io.Reader, targetDir, targetBase string) (string, error) {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return "", fmt.Errorf("update: open gzip release: %w", err)
	}
	defer func() { _ = gz.Close() }()
	archive := tar.NewReader(gz)
	foundFallback := false
	var fallbackBody []byte
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("update: read tar release: %w", err)
		}
		if header.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(header.Name)
		if isBinaryName(base) {
			return copyToTemp(archive, targetDir, "."+targetBase+"-new-*", 0o755)
		}
		if !foundFallback {
			body, err := io.ReadAll(io.LimitReader(archive, MaxDownloadBytes+1))
			if err != nil {
				return "", fmt.Errorf("update: read tar entry: %w", err)
			}
			if int64(len(body)) > MaxDownloadBytes {
				return "", fmt.Errorf("update: tar entry %s exceeds %d bytes", header.Name, MaxDownloadBytes)
			}
			fallbackBody = body
			foundFallback = true
		}
	}
	if foundFallback {
		return copyToTemp(bytes.NewReader(fallbackBody), targetDir, "."+targetBase+"-new-*", 0o755)
	}
	return "", errors.New("update: release archive contains no binary")
}

func readerSize(reader io.ReaderAt) (int64, bool) {
	seeker, ok := reader.(io.Seeker)
	if !ok {
		return 0, false
	}
	current, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, false
	}
	size, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, false
	}
	if _, err := seeker.Seek(current, io.SeekStart); err != nil {
		return 0, false
	}
	return size, true
}

func pickBinaryEntry[T any](entries []T, describe func(T) (string, bool)) (T, error) {
	var zero T
	var fallback T
	foundFallback := false
	for _, entry := range entries {
		base, dir := describe(entry)
		if dir {
			continue
		}
		if isBinaryName(base) {
			return entry, nil
		}
		if !foundFallback {
			fallback = entry
			foundFallback = true
		}
	}
	if foundFallback {
		return fallback, nil
	}
	return zero, errors.New("update: release archive contains no binary")
}

func isBinaryName(base string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	return base == "brain" || base == "brain.exe"
}

func copyToTemp(source io.Reader, dir, pattern string, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("update: create binary directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("update: create staged binary: %w", err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	written, err := copyLimited(tmp, source, MaxDownloadBytes)
	if err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("update: write staged binary: %w", err)
	}
	if written == 0 {
		_ = tmp.Close()
		return "", errors.New("update: release binary is empty")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("update: sync staged binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("update: close staged binary: %w", err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return "", fmt.Errorf("update: chmod staged binary: %w", err)
	}
	ok = true
	return name, nil
}
