# mailcat specification

mailcat is an AirDrop-style file transfer between an iPhone and an Arch Linux machine. The laptop runs a single Go daemon; the phone uses two iOS Shortcuts.
The phone always initiates: it pushes files to the laptop's inbox, and pulls files the laptop has placed in an outbox.

## 1. Goals and non-goals

**Goals**
- Send files from the iOS share sheet to the laptop in one tap, with a desktop notification on arrival.
- Send files from the laptop to the phone via `mailcat send`, collected by a pull Shortcut.
- Never corrupt, overwrite or half-publish a file.
- Work on home Wi-Fi, on the phone's personal hotspot, and (via Tailscale) on any network.
- Single static Go binary; idle cost close to zero.

**Non-goals**
- Appearing in Apple's native AirDrop UI (requires AWDL and Apple-signed identity).
- Working with no network link at all.
- Multiple users. Multiple devices are out of scope for v1 (see 11).
- A native iOS app.

## 2. Components

| Component | Description |
|---|---|
| `mailcat` binary | Go 1.22+, stdlib plus `github.com/godbus/dbus/v5`. Subcommands in 6. |
| `mailcat.service` | systemd user unit running `mailcat serve`. |
| "Send to mailcat" Shortcut | Share sheet action; uploads files to the inbox. |
| "Get from mailcat" Shortcut | Pulls pending outbox items and saves them. |
| External tools | `vips` (HEIC conversion), `xdg-open`. Tailscale for off-LAN use. |

## 3. Configuration

Path: `$XDG_CONFIG_HOME/mailcat/config.json` (default `~/.config/mailcat/config.json`), mode `0600`. Created by `mailcat init`. The server refuses to start if the file is group- or world-readable.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen` | string[] | `["127.0.0.1:8765"]` | Addresses to bind. Never `:8765` (all interfaces). |
| `token` | string | 32 random bytes, base64url | Bearer token required on authenticated routes. |
| `inbox_dir` | string | `$XDG_DOWNLOAD_DIR/mailcat`, else `~/Downloads/mailcat` | Where received files land. |
| `outbox_dir` | string | `$XDG_STATE_HOME/mailcat/outbox` | Items waiting for the phone. |
| `max_upload_bytes` | int | `4294967296` (4 GiB) | Per-request body limit. |
| `outbox_ttl` | duration | `"24h"` | Items older than this are pruned regardless of state. |
| `prune_interval` | duration | `"15m"` | How often the pruner runs. |
| `delete_on_pull` | bool | `false` | Treat every item as `once` (see 5.3). |
| `heic_format` | enum | `"png"` | `keep`, `png`, `jpeg`, `avif`. |
| `jpeg_quality` | int | `90` | Used when `heic_format` is `jpeg`. |
| `keep_heic` | bool | `false` | After conversion, move the original to `inbox_dir/.originals/` instead of deleting it. |
| `convert_workers` | int | `1` | Concurrent HEIC conversions. |

Durations use Go `time.ParseDuration` syntax.

## 4. Filesystem layout

```
inbox_dir/
  IMG_0412.png
  report.pdf
  .partial-<rand>          # in-flight writes; removed on startup
  .originals/IMG_0412.HEIC # only if keep_heic
outbox_dir/
  <id>/
    <filename>
    meta.json
  .tmp-<id>/               # under construction; ignored by the server
```

### 4.1 Inbox publish protocol

1. Write the payload to `inbox_dir/.partial-<rand>`, then `fsync`.
2. Publish with `link(partial, candidate)`, where `candidate` is `name.ext`, then `name (1).ext`, `name (2).ext`, ... until `link` does not return `EEXIST`. Give up after 1000 attempts.
3. Remove the partial file.

`link(2)` fails atomically if the target exists, so concurrent uploads of the same name never overwrite each other, and no file appears under its final name until it is complete. All files mailcat creates in the inbox (including conversion outputs) use this protocol.

### 4.2 Outbox item

- **ID:** 16 hex digits of Unix time in nanoseconds followed by 8 hex digits of randomness (24 chars, `^[0-9a-f]{24}$`). IDs sort by creation time.
- **meta.json:**
  ```json
  {"name": "notes.pdf", "size": 48213, "created": "2026-11-20T11:33:00Z", "once": false, "acked_at": null}
  ```
- **Creation:** build `.tmp-<id>/` (hardlink the source in, falling back to a copy on `EXDEV`), write `meta.json`, then `rename(".tmp-<id>", "<id>")`. Directory rename is atomic, so the server never sees a partial item.
- **meta.json updates** are written to a temp file and renamed over the original.

### 4.3 Filename sanitisation

Applied to every name received from the network:
- Take `filepath.Base`; strip `/`, `\` and control characters (including NUL).
- Reject `""`, `.`, `..` and names starting with `.` (strip the leading dots).
- Truncate to 255 bytes on a UTF-8 rune boundary, preserving the extension.
- If nothing survives, use `mailcat-YYYYMMDD-HHMMSS.<ext>`, with `<ext>` derived from the part's `Content-Type` via `mime.ExtensionsByType` (`.bin` if unknown).

## 5. HTTP API

All responses are JSON unless noted. Errors use `{"error": "<message>"}`. Authenticated routes require `Authorization: Bearer <token>`, compared with `crypto/subtle.ConstantTimeCompare`. Shortcuts does not treat non-2xx statuses as failures, so clients must check the body.

| Method | Path | Auth | Success | Errors |
|---|---|---|---|---|
| GET | `/health` | no | `200` `ok` (text/plain) | |
| POST | `/upload` | yes | `200` (5.1) | 400, 401, 413, 500 |
| GET | `/outbox` | yes | `200` (5.2) | 401 |
| GET | `/outbox/{id}` | yes | `200`/`206` file bytes | 401, 404 |
| POST | `/outbox/{id}/ack` | yes | `200` (5.3) | 401, 404 |

### 5.1 POST /upload

- Body: `multipart/form-data` with one or more parts named `file`. Other parts are ignored.
- Read with `MultipartReader` inside `http.MaxBytesReader(max_upload_bytes)`; never buffer a whole file in memory.
- Each part is sanitised (4.3) and published (4.1).
- HEIC parts (extension `.heic`/`.heif`, case-insensitive, or `Content-Type: image/heic`/`image/heif`) are queued for conversion (7) after publishing, unless `heic_format` is `keep`. The response does not wait for conversion.
- Response:
  ```json
  {"saved": [{"name": "IMG_0412.HEIC", "converting": true}]}
  ```
- If a part fails midway, parts already published stay published. The response is `500` and includes both `saved` (what succeeded) and `error`.
- Oversize body: `413`.

### 5.2 GET /outbox

- Returns unacknowledged items, oldest first. `?all=1` also returns acknowledged items.
- Reads the directory on every request (no cache). Ignores `.tmp-*` entries and items past `outbox_ttl`.
  ```json
  [{"id": "18a7...", "name": "notes.pdf", "size": 48213, "created": "2026-11-20T11:33:00Z", "once": false, "acked": false}]
  ```

### 5.3 GET /outbox/{id} and POST /outbox/{id}/ack

- `id` must match `^[0-9a-f]{24}$` before touching the filesystem; anything else is `404`.
- Downloads use `http.ServeContent` (Range requests, `Last-Modified`) with `Content-Disposition: attachment; filename*=UTF-8''<name>`.
- **ack:** if the item is `once` or `delete_on_pull` is true, delete the item directory and return `{"status": "deleted"}`. Otherwise set `acked_at` and return `{"status": "acked"}`. Acking an already-acked item returns `200` again (idempotent).
- **Delivery semantics:** clients ack only after a successful save, giving at-least-once delivery. A failed save leaves the item pending for the next pull.

## 6. CLI

| Command | Behaviour |
|---|---|
| `mailcat init [--force]` | Create config (0600) with a fresh token, create directories, print the token once. Refuses to overwrite an existing config without `--force`. |
| `mailcat serve` | Run the server in the foreground. Logs one line per transfer to stderr. |
| `mailcat send [--once] <file>...` | Add each file to the outbox (4.2). Operates on the filesystem directly; the server need not be running. |
| `mailcat ls [--all]` | List outbox items: id, name, size, age, state (`pending`, `pulled`, `once`). |
| `mailcat rm <id>... \| --all` | Remove outbox items. |
| `mailcat token` | Print the token from the config. |

Exit codes: `0` success, `1` runtime error, `2` usage error.

## 7. HEIC conversion

- Converter: `vips` (libvips), invoked as an external process:
  - `png`: `vips copy in.heic out.png`
  - `jpeg`: `vips copy in.heic out.jpg[Q=<jpeg_quality>]`
  - `avif`: `vips copy in.heic out.avif`
- Output is written to a `.partial-*` file, then published with 4.1 using the original stem and the new extension.
- EXIF (capture date, GPS) and the ICC profile must survive conversion. If `vips` drops either, copy them with `exiftool -tagsFromFile`.
- Only the primary image of a multi-image HEIC (e.g. Portrait mode) is converted.
- On success: delete the original, or move it to `.originals/` if `keep_heic`. On failure: keep the original, log the error, and send a notification saying conversion failed.
- Conversions run in a pool of `convert_workers` goroutines with a bounded queue.
- Size note: PNG output is roughly 8-12x the HEIC size. `jpeg` at quality 90 is about 1.5x and visually lossless for photos.

## 8. Notifications

- Sent over the session D-Bus using `org.freedesktop.Notifications`, via one long-lived `godbus` connection. No `notify-send`.
- On startup, call `GetCapabilities`. If `actions` is supported, attach actions `open` ("Open") and `show` ("Show in folder"); otherwise send plain notifications with the path in the body.
- Keep a map from notification ID to path. Handle `ActionInvoked`:
  - `open`: `xdg-open <path>`
  - `show`: call `org.freedesktop.FileManager1.ShowItems` with the file URI; fall back to `xdg-open <dir>`.
- Delete map entries on `NotificationClosed`.
- A request saving several files produces one notification ("Received N files"); its actions open the inbox directory.
- For HEIC files, notify after conversion finishes, pointing at the converted file.
- If no session bus is available, log and continue; notification failure never fails a transfer.

## 9. iOS Shortcuts

Both Shortcuts begin with two Text actions stored in variables `Host` (e.g. `http://arch.local:8765` or `https://arch.<tailnet>.ts.net`) and `Token`.

### 9.1 Send to mailcat
- Show in Share Sheet; accepts Images, Media, Files, PDFs.
- Repeat with each item in Shortcut Input:
  - Get Contents of URL: POST `Host/upload`, header `Authorization: Bearer Token`, body Form, field `file` (File) = Repeat Item.
  - If the result does not contain `saved`: Show Alert with the result; Stop Shortcut.
- Show Notification: "Sent N to mailcat".

### 9.2 Get from mailcat
- Get Contents of URL: GET `Host/outbox` with the auth header.
- If the count is 0: Show Notification "Nothing waiting"; Stop Shortcut.
- Repeat with each item:
  - Get `id` and `name`; GET `Host/outbox/<id>`; Set Name to `name`.
  - If `name` matches `(?i)\.(jpe?g|png|heic|heif|gif|mov|mp4|avif)$`: Save to Photo Album. Otherwise: Save File to `Files/mailcat`, Ask Where to Save off, Overwrite If File Exists off.
  - POST `Host/outbox/<id>/ack`.
- Show Notification: "Got N from mailcat".

## 10. Deployment and security

### 10.1 Network modes

| Mode | `listen` | `Host` in Shortcuts | Transport security |
|---|---|---|---|
| LAN / hotspot | specific interface IPs (e.g. `192.168.1.20:8765`, `172.20.10.2:8765`) | `http://<host>.local:8765` | Wi-Fi link encryption only |
| Tailscale (recommended) | `127.0.0.1:8765`, fronted by `tailscale serve --bg 8765` | `https://<host>.<tailnet>.ts.net` | WireGuard + TLS |

### 10.2 Threat model

- The token is the only credential. In LAN mode it travels in plaintext inside the Wi-Fi link, so anyone with the Wi-Fi password (or who cracks a weak WPA2 password offline) can capture it. WPA2-PSK has no forward secrecy.
- Tailscale mode removes dependence on link security and leaves no externally bound socket.
- Path traversal is prevented by filename sanitisation (4.3) and ID validation (5.3).
- Resource exhaustion is bounded by `max_upload_bytes`, `MemoryMax`, and the conversion pool size.
- Out of scope: a compromised laptop or phone.

### 10.3 systemd user unit

```ini
[Unit]
Description=mailcat file transfer
After=graphical-session.target

[Service]
ExecStart=%h/.local/bin/mailcat serve
Restart=on-failure
Nice=10
IOSchedulingClass=idle
MemoryMax=256M
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ReadWritePaths=%h/Downloads/mailcat %h/.local/state/mailcat

[Install]
WantedBy=graphical-session.target
```

The unit's environment must include `DBUS_SESSION_BUS_ADDRESS` and `WAYLAND_DISPLAY` or `DISPLAY` (check with `systemctl --user show-environment`). On compositors that do not activate `graphical-session.target`, run `systemctl --user import-environment` and start the unit from the compositor config.

## 11. Future work

- Per-device acknowledgement (`X-Device` header, set of acked devices in `meta.json`).
- Push notification to the phone on `mailcat send` (self-hosted ntfy).
- `POST /clip` / `GET /clip` clipboard sync via `wl-copy`/`wl-paste`.
- Socket activation (`mailcat.socket`, idle exit, prune on startup).
- In-place progress notifications via `replaces_id`.
