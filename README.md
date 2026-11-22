# mailcat

[NOTE: all other text here is AI gen, currently very much vibe coding an MVP ]

AirDrop-style file transfer between an iPhone and an Arch Linux machine. The laptop runs a single Go daemon; the iPhone uses two iOS Shortcuts.

## Features

- **One-tap phone → laptop uploads** from the iOS Share Sheet, streamed directly to disk with atomic no-clobber publishing.
- **Desktop notifications** over session D-Bus with *Open* and *Show in folder* actions.
- **Automatic HEIC conversion** to PNG, JPEG, or AVIF via `libvips` (with optional `.originals/` backup).
- **Laptop → phone outbox** *(in progress — see [DEVPLAN.md](DEVPLAN.md))*.

## Requirements

- Go 1.22+
- `libvips` + `libheif` (optional — for HEIC conversion; set `"heic_format": "keep"` to disable)
- `avahi` for `<hostname>.local` discovery on LAN, or Tailscale for off-LAN HTTPS access

## Quick start

```sh
# Build
go build -o ~/.local/bin/mailcat ./cmd/mailcat

# Create ~/.config/mailcat/config.json (0600), inbox/outbox dirs, and print the bearer token
mailcat init

# Run the server
mailcat serve
```

In LAN mode, add your laptop's Wi-Fi IP to `listen` in `~/.config/mailcat/config.json` (e.g. `["127.0.0.1:8766", "192.168.1.20:8766"]`). For iOS Shortcut setup, see [shortcuts/README.md](shortcuts/README.md).

## Commands

| Command | Description |
|---|---|
| `mailcat init [--force]` | Create config (`0600`) with a fresh token and data directories |
| `mailcat serve` | Run the HTTP server in the foreground |
| `mailcat token` | Print the bearer token from the config |

## Documentation

- [SPEC.md](SPEC.md) — full architecture, HTTP API, security model, and systemd unit
- [DEVPLAN.md](DEVPLAN.md) — implementation milestones and test criteria
- [shortcuts/README.md](shortcuts/README.md) — iOS Shortcut setup notes
