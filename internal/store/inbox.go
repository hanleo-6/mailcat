package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	partialPrefix   = ".partial-"
	maxFilenameSize = 255
	maxLinkAttempts = 1000
)

// Sanitise cleans a filename received from the network according to SPEC.md 4.3.
func Sanitise(name, contentType string) string {
	return sanitiseAt(name, contentType, time.Now())
}

func sanitiseAt(name, contentType string, now time.Time) string {
	name = strings.ToValidUTF8(name, "")
	if name != "" {
		name = filepath.Base(name)
	}

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimLeft(b.String(), ".")

	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return fallbackName(contentType, now)
	}

	return truncateUTF8PreserveExt(cleaned, maxFilenameSize)
}

func fallbackName(contentType string, now time.Time) string {
	ext := ".bin"
	if contentType != "" {
		if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && mediaType != "" && mediaType != "application/octet-stream" {
			if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
				ext = exts[0]
			}
		}
	}
	return fmt.Sprintf("mailcat-%s%s", now.Format("20060102-150405"), ext)
}

func truncateUTF8PreserveExt(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if ext != "" && stem != "" && len(ext) < limit {
		maxStem := limit - len(ext)
		if t := truncateUTF8(stem, maxStem); t != "" {
			return t + ext
		}
	}
	return truncateUTF8(name, limit)
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	n := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		n = i
	}
	// Check if the rune starting at n fits within maxBytes.
	_, size := utf8.DecodeRuneInString(s[n:])
	if n+size <= maxBytes {
		return s[:n+size]
	}
	return s[:n]
}

// Publish streams r into a .partial-<rand> file in inboxDir, fsyncs it, and
// atomically publishes it under name (or "stem (N).ext" on collision) via link(2).
// Partial files are always removed before returning.
func Publish(inboxDir, name string, r io.Reader) (string, int64, error) {
	f, partialPath, err := createPartial(inboxDir)
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(partialPath)

	written, copyErr := io.Copy(f, r)
	if copyErr != nil {
		_ = f.Close()
		return "", written, copyErr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", written, fmt.Errorf("fsync partial: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", written, fmt.Errorf("close partial: %w", err)
	}

	finalName, err := PublishFile(inboxDir, partialPath, name)
	if err != nil {
		return "", written, err
	}
	return finalName, written, nil
}

// PublishFile atomically links partialPath into inboxDir under name (or
// "stem (1).ext", "stem (2).ext", ... on EEXIST) and removes partialPath.
func PublishFile(inboxDir, partialPath, name string) (string, error) {
	defer os.Remove(partialPath)

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	for attempt := 0; attempt < maxLinkAttempts; attempt++ {
		candidate := name
		if attempt > 0 {
			suffix := fmt.Sprintf(" (%d)", attempt)
			maxStem := maxFilenameSize - len(ext) - len(suffix)
			if maxStem > 0 {
				candidate = truncateUTF8(stem, maxStem) + suffix + ext
			} else {
				candidate = truncateUTF8(name, maxFilenameSize-len(suffix)) + suffix
			}
		}

		dst := filepath.Join(inboxDir, candidate)
		err := os.Link(partialPath, dst)
		if err == nil {
			return candidate, nil
		}
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			continue
		}
		return "", fmt.Errorf("link %s: %w", candidate, err)
	}

	return "", fmt.Errorf("could not publish %q after %d attempts", name, maxLinkAttempts)
}

func createPartial(inboxDir string) (*os.File, string, error) {
	var rnd [8]byte
	for i := 0; i < 10; i++ {
		if _, err := rand.Read(rnd[:]); err != nil {
			return nil, "", fmt.Errorf("generate partial name: %w", err)
		}
		path := filepath.Join(inboxDir, partialPrefix+hex.EncodeToString(rnd[:]))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, path, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, "", fmt.Errorf("create partial file: %w", err)
	}
	return nil, "", errors.New("could not create unique partial file")
}

// CleanPartials removes any stale .partial-* files in inboxDir left behind by
// an interrupted transfer.
func CleanPartials(inboxDir string) (int, error) {
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read inbox dir: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), partialPrefix) {
			continue
		}
		if err := os.Remove(filepath.Join(inboxDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("remove stale partial %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}
