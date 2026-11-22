package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitise(t *testing.T) {
	fixedNow := time.Date(2026, 11, 21, 14, 30, 5, 0, time.UTC)

	cases := []struct {
		name        string
		input       string
		contentType string
		want        string
		wantPrefix  string // used when mime.ExtensionsByType varies by OS
	}{
		{
			name:  "path traversal",
			input: "../../etc/passwd",
			want:  "passwd",
		},
		{
			name:        "empty without content type",
			input:       "",
			contentType: "",
			want:        "mailcat-20261121-143005.bin",
		},
		{
			name:        "empty with octet-stream",
			input:       "",
			contentType: "application/octet-stream",
			want:        "mailcat-20261121-143005.bin",
		},
		{
			name:        "empty with png content type",
			input:       "",
			contentType: "image/png",
			want:        "mailcat-20261121-143005.png",
		},
		{
			name:        "empty with parameterized content type",
			input:       "",
			contentType: "text/plain; charset=utf-8",
			wantPrefix:  "mailcat-20261121-143005.",
		},
		{
			name:  "single dot",
			input: ".",
			want:  "mailcat-20261121-143005.bin",
		},
		{
			name:  "double dot",
			input: "..",
			want:  "mailcat-20261121-143005.bin",
		},
		{
			name:  "leading dot hidden file",
			input: ".hidden",
			want:  "hidden",
		},
		{
			name:  "multiple leading dots",
			input: "...secret.txt",
			want:  "secret.txt",
		},
		{
			name:  "NUL bytes and control characters",
			input: "hello\x00world\r\n\t\x7f.txt",
			want:  "helloworld.txt",
		},
		{
			name:  "backslashes stripped",
			input: `foo\bar\baz.jpg`,
			want:  "foobarbaz.jpg",
		},
		{
			name:  "normal filename preserved",
			input: "IMG_0412.HEIC",
			want:  "IMG_0412.HEIC",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitiseAt(tc.input, tc.contentType, fixedNow)
			if tc.wantPrefix != "" {
				if !strings.HasPrefix(got, tc.wantPrefix) {
					t.Errorf("sanitiseAt(%q, %q) = %q, want prefix %q", tc.input, tc.contentType, got, tc.wantPrefix)
				}
				if strings.HasSuffix(got, ".bin") {
					t.Errorf("sanitiseAt(%q, %q) = %q, expected MIME-derived extension instead of .bin", tc.input, tc.contentType, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("sanitiseAt(%q, %q) = %q, want %q", tc.input, tc.contentType, got, tc.want)
			}
		})
	}
}

func TestSanitiseLongUTF8Name(t *testing.T) {
	// 100 3-byte runes (300 bytes) + ".png" = 304 bytes.
	longName := strings.Repeat("猫", 100) + ".png"
	got := Sanitise(longName, "image/png")

	if len(got) > 255 {
		t.Fatalf("len(got) = %d, want <= 255", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("got is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("extension not preserved: %q", got)
	}
	// 255 - 4 (".png") = 251 bytes -> floor(251 / 3) = 83 runes = 249 bytes + 4 = 253 bytes.
	if len(got) != 253 {
		t.Errorf("len(got) = %d, want 253", len(got))
	}
}

func TestPublishConcurrentNoClobber(t *testing.T) {
	dir := t.TempDir()

	payloadA := bytes.Repeat([]byte("A"), 64*1024)
	payloadB := bytes.Repeat([]byte("B"), 64*1024)
	hashA := sha256.Sum256(payloadA)
	hashB := sha256.Sum256(payloadB)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)

	for i, payload := range [][]byte{payloadA, payloadB} {
		wg.Add(1)
		go func(idx int, data []byte) {
			defer wg.Done()
			<-start
			name, _, err := Publish(dir, "x.jpg", bytes.NewReader(data))
			results[idx] = name
			errs[idx] = err
		}(i, payload)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("publish %d failed: %v", i, err)
		}
	}

	seenNames := map[string]bool{results[0]: true, results[1]: true}
	if !seenNames["x.jpg"] || !seenNames["x (1).jpg"] {
		t.Fatalf("published names = %v, want x.jpg and x (1).jpg", results)
	}

	// Verify both files are intact and match the original payloads.
	gotHashes := make(map[[32]byte]bool)
	for _, name := range results {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		gotHashes[sha256.Sum256(data)] = true
	}
	if !gotHashes[hashA] || !gotHashes[hashB] {
		t.Fatalf("published file hashes do not match original payloads")
	}

	// Verify no .partial-* files remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), partialPrefix) {
			t.Errorf("leftover partial file: %s", e.Name())
		}
	}
}

type failingReader struct {
	data []byte
	read bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.read {
		f.read = true
		n := copy(p, f.data)
		return n, nil
	}
	return 0, errors.New("simulated read failure")
}

func TestPublishRemovesPartialOnError(t *testing.T) {
	dir := t.TempDir()

	_, _, err := Publish(dir, "broken.bin", &failingReader{data: []byte("half")})
	if err == nil {
		t.Fatalf("Publish() = nil, want error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty inbox after failed publish, found %d entries", len(entries))
	}
}

func TestCleanPartials(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{".partial-aaa", ".partial-bbb", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ".originals"), 0o755); err != nil {
		t.Fatalf("mkdir .originals: %v", err)
	}

	removed, err := CleanPartials(dir)
	if err != nil {
		t.Fatalf("CleanPartials: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Errorf("keep.txt should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".originals")); err != nil {
		t.Errorf(".originals should still exist: %v", err)
	}
	for _, name := range []string{".partial-aaa", ".partial-bbb"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should have been removed", name)
		}
	}
}

// Compile-time check that failingReader implements io.Reader.
var _ io.Reader = (*failingReader)(nil)
