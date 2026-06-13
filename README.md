# soundtouch-device

Run a small internet-radio controller **on a Bose SoundTouch speaker itself** — no
Raspberry Pi or Home Assistant box required. It's a from-scratch Go re-implementation
of the useful parts of [SoundTouch-Pi](https://github.com/jeffevertse/SoundTouch-Pi),
because the speaker can't run the original Python app.

## Why a rewrite (feasibility)

The SoundTouch (firmware 27.x) is **armv7 embedded Linux with BusyBox only** — no
Python/Node/compiler — and the writable `/mnt/nv` partition has just ~20–40 MB free.
So the Python app can't run there. The working pattern (cf. gesellix *AfterTouch*) is a
**single static armv7 binary**; this one is ~6.5 MB.

## What it does (focused subset)

- **Stream proxy** — fetches internet radio, downgrades HTTPS→HTTP (the SoundTouch 20
  can't do TLS on media), resolves PLS/M3U, and serves it to the speaker's own renderer.
  SSRF-hardened (resolve-once, pin the IP, reject private addresses).
- **UPnP playback** — pushes the stream to the device's AVTransport renderer.
- **6 presets** — JSON config in `/mnt/nv`; trigger with `GET /play/<id>`.
- **Auto-resume** — replays the last station when the speaker powers on (SoundTouch
  WebSocket events).

Built on the MIT-licensed Go library
[`github.com/gesellix/bose-soundtouch`](https://github.com/gesellix/Bose-SoundTouch)
(`pkg/client` for the device API + WebSocket).

## Build

```sh
make armv7      # static armv7 binary -> dist/soundtouchd  (needs Go)
make test vet   # unit tests + vet (host)
```

## Install on the speaker — safety first

SSH must be enabled (the `remote_services` USB trick). Modern clients need the legacy
host-key flag (the Makefile/scripts include it).

> **Always, in this order.** The `/tmp` step is the safety net: it persists nothing, so
> a reboot fully reverts the speaker.

```sh
make backup  HOST=<speaker-ip>   # 1. Phase-0 snapshot -> ./backup/<date>/
make run-tmp HOST=<speaker-ip>   # 2. run from /tmp (nothing persisted) and validate
make install HOST=<speaker-ip>   # 3. persist to /mnt/nv + auto-start
make uninstall HOST=<speaker-ip> # rollback: remove everything we added
```

This project is **additive** — it adds files under `/mnt/nv/soundtouchd/`, an
`/etc/init.d/soundtouchd` service, and an `/opt/soundtouchd` symlink. It does **not**
modify any Bose configuration, so `make uninstall` returns the device to stock.

## Usage

The service listens on **port 8099** (set `proxy_port` in the config to change it). Call it from any
device on your LAN, or from the speaker itself via `127.0.0.1`. Replace `<speaker-ip>` with e.g.
`192.168.1.29`.

| Method | Endpoint        | Purpose                                                            |
| ------ | --------------- | ----------------------------------------------------------------- |
| GET    | `/play/<id>`    | Play preset `<id>` (1–6). Returns `{"ok":true,"preset":<id>}`.     |
| GET    | `/stream/<id>`  | Audio proxy for preset `<id>` — the speaker fetches this, not you. |
| GET    | `/status`       | Current now-playing (JSON, from the speaker's own API).           |
| GET    | `/healthz`      | Liveness: `{"ok":true,"version":"…","rendererReady":<bool>}`.      |

```sh
# play a preset
curl http://<speaker-ip>:8099/play/1     # BBC Radio 4
curl http://<speaker-ip>:8099/play/5     # Jazz24

# check state
curl http://<speaker-ip>:8099/healthz
curl http://<speaker-ip>:8099/status
```

The last station played is remembered and **auto-resumes when the speaker powers on**.

### Config editor (local HTML)

`editor/config-editor.html` is a self-contained page (no dependencies) for editing presets without
SSH. Open it in a browser, enter the speaker's IP (and port `8099`), click **Load**, edit the
presets, then **Save & Apply** — changes take effect immediately. Use **Restart service** only after
changing `proxy_port`.

```sh
open editor/config-editor.html        # macOS — or just double-click the file
```

It talks to the daemon's `/config` API over your LAN. CORS is restricted to `file://`/localhost/
private-network origins, so a random public website can't reach your speaker.

### Editing stations (SSH)

Presets live in `/mnt/nv/soundtouchd/config.json` on the speaker — edit over SSH and restart:

```sh
ssh -o HostKeyAlgorithms=+ssh-rsa root@<speaker-ip>
vi /mnt/nv/soundtouchd/config.json       # set name / stream_url / icon per preset (ids 1–6)
/etc/init.d/soundtouchd restart
```

Use any public MP3/AAC stream URL, or a `.pls`/`.m3u` playlist; HTTPS is downgraded automatically.

## Constraints & risks

- `/mnt/nv` is tiny (~20–40 MB); the installer backs up + garbage-collects and prints `df`.
- A Bose firmware update may wipe `/mnt/nv` additions — re-run `make install`.
- Modifying an embedded device carries a bricking risk. The `/tmp`-first workflow and the
  verified `uninstall.sh` are your safety nets.

## Layout

```
cmd/soundtouchd     entrypoint (HTTP proxy + control, UPnP, auto-resume)
internal/streamproxy  HTTPS→HTTP + playlist resolution + SSRF guard (tested)
internal/presets      JSON config (tested)
internal/upnp         AVTransport SetAVTransportURI + Play
internal/resume       power-on detection via WebSocket
packaging/            init script, install/uninstall (run on device)
scripts/              backup.sh, deploy-tmp.sh (run from the Mac)
```
