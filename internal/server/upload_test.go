package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hanleo-6/mailcat/internal/config"
)

func newTestServer(t *testing.T, maxUploadBytes int64) (*Server, *config.Config) {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.Token = "test-token"
	cfg.InboxDir = t.TempDir()
	cfg.OutboxDir = t.TempDir()
	if maxUploadBytes > 0 {
		cfg.MaxUploadBytes = maxUploadBytes
	}
	logger := log.New(io.Discard, "", 0)
	return New(cfg, logger), cfg
}

func buildMultipartBody(t *testing.T, parts []struct {
	field    string
	filename string
	content  []byte
}) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.filename != "" {
			w, err = mw.CreateFormFile(p.field, p.filename)
		} else {
			w, err = mw.CreateFormField(p.field)
		}
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := w.Write(p.content); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func TestUploadSingleAndMultipleFiles(t *testing.T) {
	srv, cfg := newTestServer(t, 0)

	body, ct := buildMultipartBody(t, []struct {
		field    string
		filename string
		content  []byte
	}{
		{field: "note", filename: "", content: []byte("ignored")},
		{field: "file", filename: "hello.txt", content: []byte("hello world")},
		{field: "file", filename: "../../evil.pdf", content: []byte("%PDF-1.4")},
	})

	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Saved) != 2 {
		t.Fatalf("saved count = %d, want 2 (%+v)", len(resp.Saved), resp)
	}
	if resp.Saved[0].Name != "hello.txt" || resp.Saved[1].Name != "evil.pdf" {
		t.Errorf("saved names = %+v, want hello.txt and evil.pdf", resp.Saved)
	}

	got1, err := os.ReadFile(filepath.Join(cfg.InboxDir, "hello.txt"))
	if err != nil || string(got1) != "hello world" {
		t.Errorf("hello.txt = %q, err = %v", string(got1), err)
	}
	got2, err := os.ReadFile(filepath.Join(cfg.InboxDir, "evil.pdf"))
	if err != nil || string(got2) != "%PDF-1.4" {
		t.Errorf("evil.pdf = %q, err = %v", string(got2), err)
	}
}

func TestUploadConcurrentSameName(t *testing.T) {
	srv, cfg := newTestServer(t, 0)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	payloadA := bytes.Repeat([]byte("1"), 128*1024)
	payloadB := bytes.Repeat([]byte("2"), 128*1024)
	hashA := sha256.Sum256(payloadA)
	hashB := sha256.Sum256(payloadB)

	start := make(chan struct{})
	var wg sync.WaitGroup
	savedNames := make([]string, 2)
	errs := make([]error, 2)

	for i, data := range [][]byte{payloadA, payloadB} {
		wg.Add(1)
		go func(idx int, content []byte) {
			defer wg.Done()
			body, ct := buildMultipartBody(t, []struct {
				field    string
				filename string
				content  []byte
			}{{field: "file", filename: "x.jpg", content: content}})

			req, err := http.NewRequest(http.MethodPost, ts.URL+"/upload", body)
			if err != nil {
				errs[idx] = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Content-Type", ct)

			<-start
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs[idx] = err
				return
			}
			defer resp.Body.Close()

			var parsed uploadResponse
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				errs[idx] = err
				return
			}
			if resp.StatusCode != http.StatusOK || len(parsed.Saved) != 1 {
				t.Errorf("upload %d: status=%d resp=%+v", idx, resp.StatusCode, parsed)
				return
			}
			savedNames[idx] = parsed.Saved[0].Name
		}(i, data)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
	}

	seen := map[string]bool{savedNames[0]: true, savedNames[1]: true}
	if !seen["x.jpg"] || !seen["x (1).jpg"] {
		t.Fatalf("saved names = %v, want x.jpg and x (1).jpg", savedNames)
	}

	hashes := make(map[[32]byte]bool)
	for _, name := range savedNames {
		b, err := os.ReadFile(filepath.Join(cfg.InboxDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		hashes[sha256.Sum256(b)] = true
	}
	if !hashes[hashA] || !hashes[hashB] {
		t.Fatalf("file hashes did not match uploaded payloads")
	}
}

func TestUploadOversizeReturns413AndNoPartial(t *testing.T) {
	srv, cfg := newTestServer(t, 1024) // 1 KiB limit

	body, ct := buildMultipartBody(t, []struct {
		field    string
		filename string
		content  []byte
	}{{field: "file", filename: "too-big.bin", content: bytes.Repeat([]byte("Z"), 4096)}})

	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}

	entries, err := os.ReadDir(cfg.InboxDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty inbox after 413, found %d files", len(entries))
	}
}

func TestUploadPartialFailurePreservesSavedAndReturns500(t *testing.T) {
	srv, cfg := newTestServer(t, 0)

	// Build a valid first part, then truncate the second part so MultipartReader fails midway.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w1, _ := mw.CreateFormFile("file", "first.txt")
	_, _ = w1.Write([]byte("good data"))
	w2, _ := mw.CreateFormFile("file", "second.txt")
	_, _ = w2.Write([]byte("truncated stream without closing boundary"))
	// Intentionally do NOT call mw.Close() to simulate a mid-stream failure.

	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal 500 response: %v", err)
	}
	if resp.Error == "" {
		t.Errorf("expected non-empty error field in 500 response")
	}
	if len(resp.Saved) != 1 || resp.Saved[0].Name != "first.txt" {
		t.Errorf("saved = %+v, want [first.txt]", resp.Saved)
	}

	// first.txt remains on disk; no .partial-* files remain.
	if data, err := os.ReadFile(filepath.Join(cfg.InboxDir, "first.txt")); err != nil || string(data) != "good data" {
		t.Errorf("first.txt = %q, err = %v", string(data), err)
	}
	entries, _ := os.ReadDir(cfg.InboxDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial-") {
			t.Errorf("leftover partial file: %s", e.Name())
		}
	}
}

func TestUploadBadRequests(t *testing.T) {
	srv, cfg := newTestServer(t, 0)

	// Non-multipart request -> 400.
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("plain"))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-multipart status = %d, want 400", rec.Code)
	}

	// Multipart with no "file" parts -> 400.
	body, ct := buildMultipartBody(t, []struct {
		field    string
		filename string
		content  []byte
	}{{field: "other", filename: "x.txt", content: []byte("abc")}})
	req = httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", ct)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no file parts status = %d, want 400", rec.Code)
	}
}

type mockNotifier struct {
	mu    sync.Mutex
	calls []struct{ summary, body, path string }
}

func (m *mockNotifier) Notify(summary, body, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, struct{ summary, body, path string }{summary, body, path})
}

func TestUploadNotifications(t *testing.T) {
	srv, cfg := newTestServer(t, 0)
	mn := &mockNotifier{}
	srv.SetNotifier(mn)

	// Single file -> notification points to the saved file.
	body, ct := buildMultipartBody(t, []struct {
		field    string
		filename string
		content  []byte
	}{{field: "file", filename: "one.txt", content: []byte("1")}})
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("single upload status = %d, want 200", rec.Code)
	}

	// Multiple files -> one notification ("Received 2 files") pointing to inbox_dir.
	body, ct = buildMultipartBody(t, []struct {
		field    string
		filename string
		content  []byte
	}{
		{field: "file", filename: "a.txt", content: []byte("a")},
		{field: "file", filename: "b.txt", content: []byte("b")},
	})
	req = httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", ct)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("multi upload status = %d, want 200", rec.Code)
	}

	if len(mn.calls) != 2 {
		t.Fatalf("notification count = %d, want 2", len(mn.calls))
	}
	if wantPath := filepath.Join(cfg.InboxDir, "one.txt"); mn.calls[0].path != wantPath {
		t.Errorf("single notify path = %q, want %q", mn.calls[0].path, wantPath)
	}
	if mn.calls[1].body != "Received 2 files" || mn.calls[1].path != cfg.InboxDir {
		t.Errorf("multi notify = %+v, want body 'Received 2 files' and path %q", mn.calls[1], cfg.InboxDir)
	}
}

type mockConverter struct {
	mu       sync.Mutex
	enqueued []string
}

func (m *mockConverter) Enqueue(filename string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueued = append(m.enqueued, filename)
	return true
}

func TestUploadHEICQueuesConversionAndDefersNotification(t *testing.T) {
	srv, cfg := newTestServer(t, 0)
	mn := &mockNotifier{}
	mc := &mockConverter{}
	srv.SetNotifier(mn)
	srv.SetConverter(mc)

	body, ct := buildMultipartBody(t, []struct {
		field    string
		filename string
		content  []byte
	}{{field: "file", filename: "IMG_0412.HEIC", content: []byte("heic-bytes")}})

	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Saved) != 1 || resp.Saved[0].Name != "IMG_0412.HEIC" || !resp.Saved[0].Converting {
		t.Fatalf("saved = %+v, want IMG_0412.HEIC converting=true", resp.Saved)
	}
	if len(mc.enqueued) != 1 || mc.enqueued[0] != "IMG_0412.HEIC" {
		t.Errorf("enqueued = %v, want [IMG_0412.HEIC]", mc.enqueued)
	}
	if len(mn.calls) != 0 {
		t.Errorf("immediate notifications = %d, want 0 (deferred until conversion finishes)", len(mn.calls))
	}
}
