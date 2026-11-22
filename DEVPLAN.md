# mailcat development plan

Implements `SPEC.md`. Each milestone ends with a testable "done when". Section references (e.g. 4.1) point into the spec.

| Release | After | Capability |
|---|---|---|
| v0.1 | M4 | Phone to laptop |
| v0.2 | M8 | Both directions |
| v0.3 | M10 | Any network, via Tailscale |

## Repository layout

```
mailcat/
  cmd/mailcat/main.go     # subcommand dispatch (flag package)
  internal/config/        # load, validate, write config (3)
  internal/store/
    inbox.go              # sanitise (4.3), publish (4.1)
    outbox.go             # add, list, open, ack, remove, prune (4.2)
  internal/server/
    auth.go               # bearer middleware
    upload.go             # POST /upload (5.1)
    outbox.go             # outbox routes (5.2, 5.3)
  internal/convert/       # HEIC worker pool (7)
  internal/notify/        # D-Bus notifications (8)
  shortcuts/              # exported .shortcut files and setup notes
  dist/mailcat.service
  SPEC.md
  DEVPLAN.md
```

**Conventions:** Go 1.22+. Only external module: `godbus/dbus/v5`. `go vet` and `go test ./...` pass on every commit. Table-driven tests in `store`; `httptest` tests in `server`. Tag each release.

---

## M0: Network sanity check

**Goal:** prove the phone can reach the laptop before writing any code.

- [ ] `pacman -S go avahi nss-mdns libvips perl-image-exiftool`
- [ ] `systemctl enable --now avahi-daemon`; confirm systemd-resolved is not also answering mDNS (`resolvectl mdns` shows `no`, or `MulticastDNS=no` in `resolved.conf`).
- [ ] If a firewall is active, allow TCP 8765 from the LAN and hotspot subnets only.
- [ ] Run `python -m http.server 8765` and load `http://<host>.local:8765` in Safari.

**Done when:** Safari loads the page on home Wi-Fi and on the phone's hotspot. If `.local` fails on the hotspot, note the laptop's `172.20.10.x` address as the fallback.

## M1: Skeleton, config, auth

- [ ] `config` package: defaults, XDG path resolution, JSON load, validation, refuse to start if mode is not `0600`.
- [ ] `mailcat init [--force]` and `mailcat token`.
- [ ] `mailcat serve`: bind every address in `listen`; graceful shutdown on SIGINT/SIGTERM.
- [ ] `GET /health` (unauthenticated).
- [ ] Bearer middleware with constant-time comparison and a JSON 401.

**Done when:**
- `curl -i localhost:8765/health` returns 200.
- An authenticated route without the token, or with a wrong one, returns 401 JSON.
- Tests cover config defaults, the permission check and the middleware.

## M2: Streaming upload

- [ ] `store.Sanitise` (4.3).
- [ ] `store.Publish`: partial file, fsync, no-clobber `link()` loop (4.1).
- [ ] Remove stale `.partial-*` on startup.
- [ ] `POST /upload` with `MultipartReader` + `MaxBytesReader`; response shape and partial-failure behaviour from 5.1.

**Done when:**
- Sanitiser table tests pass: `../../etc/passwd`, `""`, `.`, `..`, `.hidden`, NUL bytes, 300-byte UTF-8 names, missing filename with and without `Content-Type`.
- Two concurrent uploads of the same name produce `x.jpg` and `x (1).jpg`, both intact (compare hashes).
- A body over `max_upload_bytes` returns 413 and leaves no partial file behind.
- RSS stays roughly flat while uploading a 3 GB file (`ps -o rss` during `curl -F file=@big.mov`).

## M3: Notifications

- [ ] `notify` package: one long-lived session-bus connection; `GetCapabilities` on startup.
- [ ] `Notify(summary, body, path)` with actions when supported, plain otherwise.
- [ ] Signal loop for `ActionInvoked` and `NotificationClosed`; ID-to-path map with cleanup.
- [ ] `open` → `xdg-open`; `show` → `FileManager1.ShowItems`, falling back to opening the directory.
- [ ] One notification per upload request.
- [ ] No session bus: log and continue.

**Done when:**
- Both actions work under your desktop's notification daemon.
- `dbus-monitor --session` shows the expected `Notify` calls.
- After 50 dismissed notifications the map is empty (expose its size in a debug log line).
- Running with `DBUS_SESSION_BUS_ADDRESS` unset still accepts uploads.

## M4: "Send to mailcat" Shortcut → v0.1

- [ ] Build the Shortcut per 9.1.
- [ ] Export it to `shortcuts/` with the token removed; write setup notes.

**Done when:**
- A photo, a PDF and a 1-minute video all arrive from the share sheet.
- A wrong token produces an alert on the phone.
- You've recorded whether photos arrive as HEIC or JPEG, and whether filenames survive.
- Tag `v0.1`.

## M5: HEIC conversion

- [ ] `convert` package: worker pool (`convert_workers`) with a bounded queue.
- [ ] HEIC detection by extension and `Content-Type`.
- [ ] `vips copy` for `png`, `jpeg`, `avif`; `keep` skips the pool entirely.
- [ ] Publish the output via 4.1; delete the original or move it to `.originals/`.
- [ ] Notify after conversion; on failure keep the original and notify the failure.
- [ ] Copy EXIF/ICC with `exiftool` only if the checks below show `vips` drops them.

**Done when:**
- For each format: `exiftool` shows the original capture date and GPS on the output.
- Colours of a Display P3 photo match the phone when viewed side by side.
- A Portrait-mode photo yields exactly one output file.
- Uploading 40 HEICs at once never runs more than `convert_workers` `vips` processes.
- A corrupt `.heic` leaves the original in place and produces a failure notification.

## M6: Outbox store and CLI

- [ ] `store.OutboxAdd`: `.tmp-<id>/`, hardlink with `EXDEV` copy fallback, `meta.json`, directory rename.
- [ ] ID generation and validation (4.2).
- [ ] `mailcat send [--once]`, `mailcat ls [--all]`, `mailcat rm`.

**Done when:**
- Listing in a tight loop while `mailcat send` copies a 2 GB file never shows the item before it's complete.
- Sending a file from another filesystem (USB stick or tmpfs) takes the copy path and succeeds.
- IDs created in sequence sort in creation order.

## M7: Outbox API

- [ ] `GET /outbox` (+ `?all=1`), filtering `.tmp-*`, acked and expired items.
- [ ] `GET /outbox/{id}` with ID validation, `ServeContent` and `Content-Disposition`.
- [ ] `POST /outbox/{id}/ack` with the `once` / `delete_on_pull` logic; atomic `meta.json` update.
- [ ] Pruner: run on startup and every `prune_interval`.

**Done when:**
- `curl -r 0-99` returns 206 with 100 bytes.
- `GET /outbox/..%2F..%2Fetc%2Fpasswd` and malformed IDs return 404.
- An acked item disappears from the default listing but appears with `?all=1`.
- A `--once` item is gone from disk after ack.
- Acking twice returns 200 both times.
- With `outbox_ttl` set to `1m`, items vanish within one prune interval.

## M8: "Get from mailcat" Shortcut → v0.2

- [ ] Build the Shortcut per 9.2; export to `shortcuts/`.
- [ ] Create `Files/mailcat` on the phone.

**Done when:**
- Media lands in Photos; other files land in `Files/mailcat`.
- Running the Shortcut twice saves no duplicates.
- Cancelling the Shortcut halfway leaves unsaved items pending; the next run picks them up.
- Tag `v0.2`.

## M9: systemd service and hardening

- [ ] Write `dist/mailcat.service` per 10.3; install the binary to `~/.local/bin`.
- [ ] Set `listen` to specific LAN/hotspot interface addresses.
- [ ] Verify the session environment reaches the unit.

**Done when:**
- The service starts after reboot and login, and notifications work from inside it.
- `systemd-analyze --user security mailcat` shows the sandboxing directives in effect (note any that user units ignore).
- From a different network, `nmap -p 8765 <laptop>` reports the port closed or filtered.
- Idle check: `systemctl --user status mailcat` shows memory under about 15 MB and no CPU use over 10 idle minutes.

## M10: Tailscale → v0.3

- [ ] `pacman -S tailscale`; `systemctl enable --now tailscaled`; `tailscale up`.
- [ ] Install the iOS Tailscale app; enable VPN On Demand.
- [ ] Enable MagicDNS and HTTPS certificates in the tailnet admin console.
- [ ] `tailscale serve --bg 8765`; set `listen` to `["127.0.0.1:8765"]`.
- [ ] Change `Host` in both Shortcuts to `https://<host>.<tailnet>.ts.net`.
- [ ] If anyone else ever joins the tailnet, add an ACL restricting port 443 on the laptop to your phone.

**Done when:**
- Both Shortcuts work with the phone on mobile data and Wi-Fi off.
- On the same LAN or hotspot, `tailscale status` shows a direct connection (not via DERP).
- `ss -tlnp | grep 8765` shows only `127.0.0.1`.
- The M9 port scan still finds nothing.
- Tag `v0.3`.

---

## Later (unordered)

- Progress notifications via `replaces_id` for large uploads.
- ntfy push on `mailcat send`.
- Clipboard sync (`/clip`).
- Socket activation with idle exit.
- Per-device acks.
