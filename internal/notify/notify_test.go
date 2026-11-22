package notify

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type notifyCall struct {
	summary string
	body    string
	actions []string
}

type fakeBus struct {
	mu           sync.Mutex
	caps         []string
	capsErr      error
	nextID       uint32
	notifyCalls  []notifyCall
	showCalls    [][]string
	showErr      error
	sigCh        chan *dbus.Signal
	closed       bool
	showCalledCh chan []string
}

func newFakeBus(caps []string) *fakeBus {
	return &fakeBus{
		caps:         caps,
		sigCh:        make(chan *dbus.Signal, 128),
		showCalledCh: make(chan []string, 8),
	}
}

func (f *fakeBus) GetCapabilities() ([]string, error) {
	return f.caps, f.capsErr
}

func (f *fakeBus) Notify(summary, body string, actions []string) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.notifyCalls = append(f.notifyCalls, notifyCall{
		summary: summary,
		body:    body,
		actions: append([]string(nil), actions...),
	})
	return f.nextID, nil
}

func (f *fakeBus) ShowItems(uris []string, startupID string) error {
	f.mu.Lock()
	cp := append([]string(nil), uris...)
	f.showCalls = append(f.showCalls, cp)
	err := f.showErr
	f.mu.Unlock()
	f.showCalledCh <- cp
	return err
}

func (f *fakeBus) Signals() <-chan *dbus.Signal {
	return f.sigCh
}

func (f *fakeBus) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.sigCh)
	}
	return nil
}

func TestNotifyWithActionsAndSignals(t *testing.T) {
	bus := newFakeBus([]string{"body", "actions"})
	opened := make(chan string, 4)

	n := newWithClient(bus, log.New(io.Discard, "", 0), func(target string) error {
		opened <- target
		return nil
	})
	defer n.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	n.Notify("mailcat", "Received photo.png", filePath)

	if len(bus.notifyCalls) != 1 {
		t.Fatalf("notify calls = %d, want 1", len(bus.notifyCalls))
	}
	call := bus.notifyCalls[0]
	wantActions := []string{"open", "Open", "show", "Show in folder"}
	if strings.Join(call.actions, ",") != strings.Join(wantActions, ",") {
		t.Errorf("actions = %v, want %v", call.actions, wantActions)
	}
	if call.body != "Received photo.png" {
		t.Errorf("body = %q, want %q", call.body, "Received photo.png")
	}
	if n.TrackedCount() != 1 {
		t.Fatalf("TrackedCount = %d, want 1", n.TrackedCount())
	}

	// Trigger "open" action -> xdg-open <filePath>.
	bus.sigCh <- &dbus.Signal{Name: signalActionInvoked, Body: []any{uint32(1), "open"}}
	select {
	case got := <-opened:
		if got != filePath {
			t.Errorf("open target = %q, want %q", got, filePath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for open action")
	}

	// Trigger "show" action -> FileManager1.ShowItems with file:// URI.
	bus.sigCh <- &dbus.Signal{Name: signalActionInvoked, Body: []any{uint32(1), "show"}}
	select {
	case uris := <-bus.showCalledCh:
		wantURI := fileURI(filePath)
		if len(uris) != 1 || uris[0] != wantURI {
			t.Errorf("ShowItems uris = %v, want [%s]", uris, wantURI)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for show action")
	}

	// If FileManager1.ShowItems fails, fall back to opening parent directory.
	bus.mu.Lock()
	bus.showErr = errors.New("service unknown")
	bus.mu.Unlock()
	bus.sigCh <- &dbus.Signal{Name: signalActionInvoked, Body: []any{uint32(1), "show"}}
	<-bus.showCalledCh
	select {
	case got := <-opened:
		if got != dir {
			t.Errorf("show fallback target = %q, want dir %q", got, dir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for show fallback")
	}
}

func TestNotifyWithoutActionsCapability(t *testing.T) {
	bus := newFakeBus([]string{"body"})
	n := newWithClient(bus, log.New(io.Discard, "", 0), nil)
	defer n.Close()

	n.Notify("mailcat", "Received report.pdf", "/tmp/report.pdf")

	if len(bus.notifyCalls) != 1 {
		t.Fatalf("notify calls = %d, want 1", len(bus.notifyCalls))
	}
	call := bus.notifyCalls[0]
	if len(call.actions) != 0 {
		t.Errorf("actions = %v, want empty", call.actions)
	}
	if !strings.Contains(call.body, "/tmp/report.pdf") {
		t.Errorf("plain body %q should contain path", call.body)
	}
	if n.TrackedCount() != 0 {
		t.Errorf("TrackedCount = %d, want 0 when actions unsupported", n.TrackedCount())
	}
}

func TestDismissFiftyNotificationsCleansMap(t *testing.T) {
	bus := newFakeBus([]string{"actions"})
	var logBuf bytes.Buffer
	n := newWithClient(bus, log.New(&logBuf, "", 0), nil)
	defer n.Close()

	for i := 0; i < 50; i++ {
		n.Notify("mailcat", "file", "/tmp/file")
	}
	if got := n.TrackedCount(); got != 50 {
		t.Fatalf("TrackedCount after 50 sends = %d, want 50", got)
	}

	for id := uint32(1); id <= 50; id++ {
		bus.sigCh <- &dbus.Signal{Name: signalNotificationClosed, Body: []any{id, uint32(2)}}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n.TrackedCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := n.TrackedCount(); got != 0 {
		t.Fatalf("TrackedCount after 50 closes = %d, want 0", got)
	}
	// Close stops signalLoop so reading logBuf is race-free.
	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(logBuf.String(), "tracked=0") {
		t.Errorf("expected debug log line with tracked=0, got:\n%s", logBuf.String())
	}
}

func TestNoSessionBusIsNoOp(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	// Ensure fallback XDG_RUNTIME_DIR/bus does not exist.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var logBuf bytes.Buffer
	n := New(log.New(&logBuf, "", 0))
	defer n.Close()

	// Must not panic or block.
	n.Notify("mailcat", "Received a.txt", "/tmp/a.txt")
	if n.TrackedCount() != 0 {
		t.Errorf("TrackedCount = %d, want 0", n.TrackedCount())
	}
	if !strings.Contains(logBuf.String(), "notifications disabled") {
		t.Errorf("expected disabled log message, got %q", logBuf.String())
	}
}
