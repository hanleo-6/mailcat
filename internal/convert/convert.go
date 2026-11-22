package convert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hanleo-6/mailcat/internal/config"
	"github.com/hanleo-6/mailcat/internal/store"
)

// Notifier matches the desktop notification interface used by the server.
type Notifier interface {
	Notify(summary, body, path string)
}

// RunnerFunc converts srcPath into dstPath in the requested format ("png", "jpeg", "avif").
type RunnerFunc func(ctx context.Context, srcPath, dstPath, format string, jpegQuality int) error

// IsHEIC reports whether a file should be treated as HEIC/HEIF based on its
// filename extension or Content-Type header (SPEC.md 5.1).
func IsHEIC(filename, contentType string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".heic", ".heif":
		return true
	}
	if contentType != "" {
		if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
			switch strings.ToLower(mediaType) {
			case "image/heic", "image/heif":
				return true
			}
		}
	}
	return false
}

// OutputExt returns the target filename extension for a conversion format.
func OutputExt(format string) (string, error) {
	switch format {
	case "png":
		return ".png", nil
	case "jpeg":
		return ".jpg", nil
	case "avif":
		return ".avif", nil
	default:
		return "", fmt.Errorf("unsupported conversion format %q", format)
	}
}

// VipsRunner invokes `vips copy` per SPEC.md section 7.
func VipsRunner(ctx context.Context, srcPath, dstPath, format string, jpegQuality int) error {
	dstSpec := dstPath
	if format == "jpeg" {
		dstSpec = fmt.Sprintf("%s[Q=%d]", dstPath, jpegQuality)
	}
	cmd := exec.CommandContext(ctx, "vips", "copy", srcPath, dstSpec)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("vips copy: %w: %s", err, msg)
		}
		return fmt.Errorf("vips copy: %w", err)
	}
	return nil
}

// Pool runs HEIC conversions concurrently in a bounded worker pool.
type Pool struct {
	inboxDir    string
	format      string
	jpegQuality int
	keepHEIC    bool
	notifier    Notifier
	logger      *log.Logger
	runner      RunnerFunc

	mu     sync.Mutex
	closed bool
	jobs   chan string
	wg     sync.WaitGroup
}

// New creates and starts a conversion worker pool. If cfg.HEICFormat is "keep",
// New returns nil and starts no goroutines.
func New(cfg *config.Config, notifier Notifier, logger *log.Logger) *Pool {
	if cfg.HEICFormat == "keep" {
		return nil
	}
	queueSize := cfg.ConvertWorkers * 16
	if queueSize < 64 {
		queueSize = 64
	}
	return newWithRunner(cfg, notifier, logger, queueSize, VipsRunner)
}

func newWithRunner(cfg *config.Config, notifier Notifier, logger *log.Logger, queueSize int, runner RunnerFunc) *Pool {
	if logger == nil {
		logger = log.Default()
	}
	if runner == nil {
		runner = VipsRunner
	}

	p := &Pool{
		inboxDir:    cfg.InboxDir,
		format:      cfg.HEICFormat,
		jpegQuality: cfg.JPEGQuality,
		keepHEIC:    cfg.KeepHEIC,
		notifier:    notifier,
		logger:      logger,
		runner:      runner,
		jobs:        make(chan string, queueSize),
	}

	for i := 0; i < cfg.ConvertWorkers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// Enqueue adds a published inbox filename to the conversion queue.
// Returns false if the pool is nil, closed, or its bounded queue is full.
func (p *Pool) Enqueue(filename string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.jobs <- filename:
		return true
	default:
		p.logger.Printf("conversion queue full; keeping %s unconverted", filename)
		return false
	}
}

// Close stops accepting new jobs and waits for queued conversions to complete.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.jobs)
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for filename := range p.jobs {
		p.process(filename)
	}
}

func (p *Pool) process(originalName string) {
	originalPath := filepath.Join(p.inboxDir, originalName)

	ext, err := OutputExt(p.format)
	if err != nil {
		p.fail(originalName, originalPath, err)
		return
	}

	partialPath, err := partialOutputPath(p.inboxDir, ext)
	if err != nil {
		p.fail(originalName, originalPath, err)
		return
	}
	defer os.Remove(partialPath)

	if err := p.runner(context.Background(), originalPath, partialPath, p.format, p.jpegQuality); err != nil {
		p.fail(originalName, originalPath, err)
		return
	}

	stem := strings.TrimSuffix(originalName, filepath.Ext(originalName))
	targetName := stem + ext
	convertedName, err := store.PublishFile(p.inboxDir, partialPath, targetName)
	if err != nil {
		p.fail(originalName, originalPath, err)
		return
	}

	if p.keepHEIC {
		origDir := filepath.Join(p.inboxDir, ".originals")
		if err := os.MkdirAll(origDir, 0o755); err != nil {
			p.logger.Printf("create .originals dir failed (keeping %s in inbox): %v", originalName, err)
		} else if _, err := store.PublishFile(origDir, originalPath, originalName); err != nil {
			p.logger.Printf("move %s to .originals failed (keeping in inbox): %v", originalName, err)
		}
	} else {
		if err := os.Remove(originalPath); err != nil {
			p.logger.Printf("remove original %s failed: %v", originalName, err)
		}
	}

	p.logger.Printf("converted %s -> %s", originalName, convertedName)
	if p.notifier != nil {
		p.notifier.Notify("mailcat", "Received "+convertedName, filepath.Join(p.inboxDir, convertedName))
	}
}

func (p *Pool) fail(originalName, originalPath string, err error) {
	p.logger.Printf("conversion failed for %s: %v", originalName, err)
	if p.notifier != nil {
		p.notifier.Notify("mailcat", fmt.Sprintf("Failed to convert %s (kept original)", originalName), originalPath)
	}
}

func partialOutputPath(inboxDir, ext string) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("generate partial output name: %w", err)
	}
	return filepath.Join(inboxDir, ".partial-"+hex.EncodeToString(rnd[:])+ext), nil
}
