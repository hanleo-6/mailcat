package notify

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	notifyBusName   = "org.freedesktop.Notifications"
	notifyObjPath   = dbus.ObjectPath("/org/freedesktop/Notifications")
	notifyInterface = "org.freedesktop.Notifications"

	fileManagerBusName   = "org.freedesktop.FileManager1"
	fileManagerObjPath   = dbus.ObjectPath("/org/freedesktop/FileManager1")
	fileManagerInterface = "org.freedesktop.FileManager1"

	signalActionInvoked      = notifyInterface + ".ActionInvoked"
	signalNotificationClosed = notifyInterface + ".NotificationClosed"
)

// busClient abstracts the D-Bus operations so Notifier can be unit-tested
// without a live session bus.
type busClient interface {
	GetCapabilities() ([]string, error)
	Notify(summary, body string, actions []string) (uint32, error)
	ShowItems(uris []string, startupID string) error
	Signals() <-chan *dbus.Signal
	Close() error
}

type dbusClient struct {
	conn *dbus.Conn
	ch   chan *dbus.Signal
}

func dialSessionBus() (*dbusClient, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notifyObjPath),
		dbus.WithMatchInterface(notifyInterface),
		dbus.WithMatchMember("ActionInvoked"),
	); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("match ActionInvoked: %w", err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notifyObjPath),
		dbus.WithMatchInterface(notifyInterface),
		dbus.WithMatchMember("NotificationClosed"),
	); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("match NotificationClosed: %w", err)
	}

	ch := make(chan *dbus.Signal, 64)
	conn.Signal(ch)
	return &dbusClient{conn: conn, ch: ch}, nil
}

func (c *dbusClient) GetCapabilities() ([]string, error) {
	var caps []string
	obj := c.conn.Object(notifyBusName, notifyObjPath)
	err := obj.Call(notifyInterface+".GetCapabilities", 0).Store(&caps)
	return caps, err
}

func (c *dbusClient) Notify(summary, body string, actions []string) (uint32, error) {
	var id uint32
	obj := c.conn.Object(notifyBusName, notifyObjPath)
	err := obj.Call(
		notifyInterface+".Notify",
		0,
		"mailcat",
		uint32(0),
		"",
		summary,
		body,
		actions,
		map[string]dbus.Variant{},
		int32(-1),
	).Store(&id)
	return id, err
}

func (c *dbusClient) ShowItems(uris []string, startupID string) error {
	obj := c.conn.Object(fileManagerBusName, fileManagerObjPath)
	return obj.Call(fileManagerInterface+".ShowItems", 0, uris, startupID).Err
}

func (c *dbusClient) Signals() <-chan *dbus.Signal {
	return c.ch
}

func (c *dbusClient) Close() error {
	c.conn.RemoveSignal(c.ch)
	close(c.ch)
	return c.conn.Close()
}

// Notifier sends desktop notifications over the session D-Bus and handles
// ActionInvoked / NotificationClosed signals.
type Notifier struct {
	bus        busClient
	logger     *log.Logger
	hasActions bool
	openFn     func(target string) error

	mu    sync.Mutex
	paths map[uint32]string

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New connects to the session D-Bus and starts the notification signal loop.
// If no session bus is available, it logs the reason and returns a no-op
// Notifier so transfers are never blocked.
func New(logger *log.Logger) *Notifier {
	if logger == nil {
		logger = log.Default()
	}
	client, err := dialSessionBus()
	if err != nil {
		logger.Printf("notifications disabled (no session D-Bus): %v", err)
		return &Notifier{
			logger: logger,
			paths:  make(map[uint32]string),
			openFn: xdgOpen,
		}
	}
	return newWithClient(client, logger, xdgOpen)
}

func newWithClient(client busClient, logger *log.Logger, openFn func(string) error) *Notifier {
	if logger == nil {
		logger = log.Default()
	}
	if openFn == nil {
		openFn = xdgOpen
	}

	n := &Notifier{
		bus:    client,
		logger: logger,
		openFn: openFn,
		paths:  make(map[uint32]string),
		done:   make(chan struct{}),
	}

	caps, err := client.GetCapabilities()
	if err != nil {
		logger.Printf("notification GetCapabilities failed (falling back to plain): %v", err)
	} else {
		for _, cap := range caps {
			if cap == "actions" {
				n.hasActions = true
				break
			}
		}
	}

	n.wg.Add(1)
	go n.signalLoop()
	return n
}

// Notify sends a desktop notification. If the daemon supports actions and path
// is non-empty, "Open" and "Show in folder" actions are attached; otherwise
// path is appended to the body. Errors are logged and never returned.
func (n *Notifier) Notify(summary, body, path string) {
	if n == nil || n.bus == nil {
		return
	}

	var actions []string
	effectiveBody := body
	if path != "" {
		if n.hasActions {
			actions = []string{"open", "Open", "show", "Show in folder"}
		} else if !strings.Contains(effectiveBody, path) {
			if effectiveBody == "" {
				effectiveBody = path
			} else {
				effectiveBody = effectiveBody + "\n" + path
			}
		}
	}

	id, err := n.bus.Notify(summary, effectiveBody, actions)
	if err != nil {
		n.logger.Printf("notification send failed: %v", err)
		return
	}

	if n.hasActions && path != "" && id != 0 {
		n.mu.Lock()
		n.paths[id] = path
		tracked := len(n.paths)
		n.mu.Unlock()
		n.logger.Printf("notification %d sent (tracked=%d)", id, tracked)
	}
}

// TrackedCount returns the number of active notification IDs in the map.
func (n *Notifier) TrackedCount() int {
	if n == nil {
		return 0
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.paths)
}

// Close shuts down the D-Bus connection and stops the signal loop.
func (n *Notifier) Close() error {
	if n == nil || n.bus == nil {
		return nil
	}
	var err error
	n.closeOnce.Do(func() {
		close(n.done)
		err = n.bus.Close()
		n.wg.Wait()
	})
	return err
}

func (n *Notifier) signalLoop() {
	defer n.wg.Done()
	ch := n.bus.Signals()
	for {
		select {
		case <-n.done:
			return
		case sig, ok := <-ch:
			if !ok {
				return
			}
			n.handleSignal(sig)
		}
	}
}

func (n *Notifier) handleSignal(sig *dbus.Signal) {
	if sig == nil {
		return
	}
	switch sig.Name {
	case signalActionInvoked:
		if len(sig.Body) < 2 {
			return
		}
		id, ok1 := sig.Body[0].(uint32)
		action, ok2 := sig.Body[1].(string)
		if !ok1 || !ok2 {
			return
		}
		n.mu.Lock()
		path, found := n.paths[id]
		n.mu.Unlock()
		if !found {
			return
		}
		go n.runAction(action, path)

	case signalNotificationClosed:
		if len(sig.Body) < 1 {
			return
		}
		id, ok := sig.Body[0].(uint32)
		if !ok {
			return
		}
		n.mu.Lock()
		_, existed := n.paths[id]
		if existed {
			delete(n.paths, id)
		}
		remaining := len(n.paths)
		n.mu.Unlock()
		if existed {
			n.logger.Printf("notification %d closed (tracked=%d)", id, remaining)
		}
	}
}

func (n *Notifier) runAction(action, path string) {
	switch action {
	case "open":
		if err := n.openFn(path); err != nil {
			n.logger.Printf("notification open %q failed: %v", path, err)
		}
	case "show":
		uri := fileURI(path)
		if err := n.bus.ShowItems([]string{uri}, ""); err != nil {
			dir := dirForShowFallback(path)
			if openErr := n.openFn(dir); openErr != nil {
				n.logger.Printf("notification show %q failed (FileManager1: %v; xdg-open %q: %v)", path, err, dir, openErr)
			}
		}
	}
}

func fileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func dirForShowFallback(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}
	return filepath.Dir(path)
}

func xdgOpen(target string) error {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
