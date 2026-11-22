package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanleo-6/mailcat/internal/config"
)

type recordedNotice struct {
	summary string
	body    string
	path    string
}

type mockNotifier struct {
	mu      sync.Mutex
	notices []recordedNotice
}

func (m *mockNotifier) Notify(summary, body, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notices = append(m.notices, recordedNotice{summary, body, path})
}

func (m *mockNotifier) all() []recordedNotice {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedNotice(nil), m.notices...)
}

func TestIsHEIC(t *testing.T) {
	cases := []struct {
		name string
		file string
		ct   string
		want bool
	}{
		{"lowercase heic", "IMG_0001.heic", "", true},
		{"uppercase HEIC", "IMG_0001.HEIC", "", true},
		{"heif ext", "photo.HEIF", "", true},
		{"content-type heic", "photo.bin", "image/heic", true},
		{"content-type heif with param", "photo.bin", "image/heif; charset=binary", true},
		{"jpeg", "photo.jpg", "image/jpeg", false},
		{"png", "photo.png", "image/png", false},
		{"pdf", "doc.pdf", "application/pdf", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHEIC(tc.file, tc.ct); got != tc.want {
				t.Errorf("IsHEIC(%q, %q) = %v, want %v", tc.file, tc.ct, got, tc.want)
			}
		})
	}
}

func TestNewKeepReturnsNil(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.HEICFormat = "keep"

	p := New(cfg, nil, log.New(io.Discard, "", 0))
	if p != nil {
		p.Close()
		t.Fatalf("New() with heic_format=keep = %v, want nil", p)
	}
}

func TestPoolConvertDeleteAndKeepOriginals(t *testing.T) {
	for _, keepHEIC := range []bool{false, true} {
		name := "delete_original"
		if keepHEIC {
			name = "keep_original"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "IMG_0412.HEIC")
			if err := os.WriteFile(src, []byte("fake-heic"), 0o644); err != nil {
				t.Fatalf("write src: %v", err)
			}

			cfg, _ := config.Default()
			cfg.InboxDir = dir
			cfg.HEICFormat = "jpeg"
			cfg.JPEGQuality = 90
			cfg.KeepHEIC = keepHEIC
			cfg.ConvertWorkers = 1

			mn := &mockNotifier{}
			p := newWithRunner(cfg, mn, log.New(io.Discard, "", 0), 8, func(ctx context.Context, srcPath, dstPath, format string, q int) error {
				if format != "jpeg" || q != 90 {
					return fmt.Errorf("unexpected format=%s q=%d", format, q)
				}
				return os.WriteFile(dstPath, []byte("converted-jpeg"), 0o644)
			})

			if !p.Enqueue("IMG_0412.HEIC") {
				t.Fatalf("Enqueue returned false")
			}
			p.Close()

			// Converted file must exist.
			dst := filepath.Join(dir, "IMG_0412.jpg")
			data, err := os.ReadFile(dst)
			if err != nil || string(data) != "converted-jpeg" {
				t.Fatalf("converted file = %q, err = %v", string(data), err)
			}

			// Original must be gone from inbox root.
			if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("original %s still exists in inbox root", src)
			}

			// Original must be in .originals/ iff keepHEIC.
			origCopy := filepath.Join(dir, ".originals", "IMG_0412.HEIC")
			_, origErr := os.Stat(origCopy)
			if keepHEIC && origErr != nil {
				t.Errorf("expected original in .originals/, got err: %v", origErr)
			}
			if !keepHEIC && !errors.Is(origErr, os.ErrNotExist) {
				t.Errorf("expected no .originals copy when keep_heic=false")
			}

			// Notification points to the converted file.
			notices := mn.all()
			if len(notices) != 1 || notices[0].path != dst {
				t.Errorf("notices = %+v, want 1 notice pointing at %s", notices, dst)
			}
		})
	}
}

func TestPoolCorruptHEICKeepsOriginalAndNotifiesFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "corrupt.heic")
	if err := os.WriteFile(src, []byte("not a valid heic"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	cfg, _ := config.Default()
	cfg.InboxDir = dir
	cfg.HEICFormat = "png"
	cfg.ConvertWorkers = 1

	mn := &mockNotifier{}
	p := newWithRunner(cfg, mn, log.New(io.Discard, "", 0), 8, func(ctx context.Context, srcPath, dstPath, format string, q int) error {
		// Simulate partial write before error.
		_ = os.WriteFile(dstPath, []byte("half"), 0o644)
		return errors.New("vips: corrupt heif")
	})

	p.Enqueue("corrupt.heic")
	p.Close()

	// Original must remain untouched.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("corrupt original should remain in place: %v", err)
	}

	// No .partial-* or .png left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial-") || strings.HasSuffix(e.Name(), ".png") {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}

	// Failure notification sent pointing to original.
	notices := mn.all()
	if len(notices) != 1 || !strings.Contains(notices[0].body, "Failed to convert") || notices[0].path != src {
		t.Errorf("notices = %+v, want failure notice for %s", notices, src)
	}
}

func TestPoolRespectsConvertWorkersLimit(t *testing.T) {
	dir := t.TempDir()
	const (
		workers = 2
		total   = 40
	)

	for i := 0; i < total; i++ {
		name := fmt.Sprintf("IMG_%04d.heic", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("heic"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cfg, _ := config.Default()
	cfg.InboxDir = dir
	cfg.HEICFormat = "png"
	cfg.ConvertWorkers = workers

	var active atomic.Int32
	var maxSeen atomic.Int32
	var done atomic.Int32

	p := newWithRunner(cfg, nil, log.New(io.Discard, "", 0), 64, func(ctx context.Context, srcPath, dstPath, format string, q int) error {
		cur := active.Add(1)
		for {
			prev := maxSeen.Load()
			if cur <= prev || maxSeen.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		done.Add(1)
		return os.WriteFile(dstPath, []byte("png"), 0o644)
	})

	for i := 0; i < total; i++ {
		name := fmt.Sprintf("IMG_%04d.heic", i)
		if !p.Enqueue(name) {
			t.Fatalf("Enqueue(%s) returned false", name)
		}
	}
	p.Close()

	if done.Load() != total {
		t.Errorf("converted = %d, want %d", done.Load(), total)
	}
	if maxSeen.Load() > workers {
		t.Errorf("max concurrent workers = %d, want <= %d", maxSeen.Load(), workers)
	}
}
