# SoundTouch-Device

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

## Setting up a brand-new device

### 1. Enable SSH (one-time, per speaker)

The speaker has no SSH access out of the box. Enable it once with the `remote_services` USB
trick:

1. Format a USB drive as FAT (FAT32/exFAT).
2. Create an empty file named exactly `remote_services` at the root of the drive.
3. Insert the drive into the speaker's USB port **while it's powered on**.
4. Reboot the speaker (power-cycle it).

After the reboot, root SSH is available with **no password**:

```sh
ssh -o HostKeyAlgorithms=+ssh-rsa root@<speaker-ip>
```

The `HostKeyAlgorithms=+ssh-rsa` flag is required because the speaker's SSH server only
offers a legacy `ssh-rsa` host key, which modern OpenSSH clients disable by default. Every
script and Makefile target in this repo already passes it for you.

> **Security note:** this gives root, no-password SSH to anyone who can reach the speaker's IP.
> Treat it like any other unauthenticated root shell on your LAN — don't expose the speaker's
> SSH port beyond your home network.

### 2. Install soundtouchd — safety first

> **Always, in this order.** The `/tmp` step is the safety net: it persists nothing, so
> a reboot fully reverts the speaker if something's wrong.

```sh
make backup  HOST=<speaker-ip>   # 1. Phase-0 snapshot -> ./backup/<date>/
make run-tmp HOST=<speaker-ip>   # 2. run from /tmp (nothing persisted) and validate
make install HOST=<speaker-ip>   # 3. persist to /mnt/nv + auto-start
make uninstall HOST=<speaker-ip> # rollback: remove everything we added
```

This project is **additive** — it adds files under `/mnt/nv/soundtouchd/`, an
`/etc/init.d/soundtouchd` service, and an `/opt/soundtouchd` symlink. It does **not**
modify any Bose configuration, so `make uninstall` returns the device to stock.

### 3. (Optional) Set an API token

By default, anyone on your LAN can change presets or restart the service — fine for a home
network, but you can lock it down. Set `api_token` in the config to require
`Authorization: Bearer <token>` on `POST /config`, `/bass`, and `/restart`:

```sh
ssh -o HostKeyAlgorithms=+ssh-rsa root@<speaker-ip>
vi /mnt/nv/soundtouchd/config.json   # add: "api_token": "<a long random string>"
/etc/init.d/soundtouchd restart
```

Generate a good token with `openssl rand -hex 24`. You can also add/edit it via the
[config editor](#config-editor-built-in)'s **Advanced → Raw JSON** panel instead of SSH.
`GET /config` always redacts the token in its response, and re-saving the config from the
editor never clears it. Leave it unset (the default) and mutating requests need no auth — this
is what the [iOS companion app](#related) expects unless you also set a token there
(Settings → Change device → API token).

### Reinstalling / recovering

A Bose firmware update can wipe everything under `/mnt/nv`, including this daemon and its
config. Recovery is the same install flow — SSH itself is unaffected by firmware updates (it's
a device-level modification, not a `/mnt/nv` file), so no need to repeat the USB step:

```sh
make backup  HOST=<speaker-ip>   # snapshot first, in case anything else changed
make install HOST=<speaker-ip>   # re-installs the binary, service, and config.json
```

`install.sh` only seeds a fresh `config.example.json` when `/mnt/nv/soundtouchd/config.json` is
**absent** — if it survived the firmware update, your presets and `api_token` are preserved
automatically. If it didn't, `make backup` (run regularly, or right before an update if you know
one's coming) saves a copy of `config.json` alongside the Bose config files in
`./backup/<date>/`; copy it back with:

```sh
scp -O -o HostKeyAlgorithms=+ssh-rsa ./backup/<date>/config.json root@<speaker-ip>:/mnt/nv/soundtouchd/config.json
ssh -o HostKeyAlgorithms=+ssh-rsa root@<speaker-ip> '/etc/init.d/soundtouchd restart'
```

Without a prior backup, just re-enter your presets via the [config editor](#config-editor-built-in).

## Usage

The service listens on **port 8099** (set `proxy_port` in the config to change it). Call it from any
device on your LAN, or from the speaker itself via `127.0.0.1`. Replace `<speaker-ip>` with e.g.
`192.168.1.29`.

| Method | Endpoint        | Purpose                                                            |
| ------ | --------------- | ----------------------------------------------------------------- |
| GET    | `/`             | Built-in config editor (HTML page, served by the daemon).          |
| GET    | `/play/<id>`    | Play preset `<id>` (1–6). Returns `{"ok":true,"preset":<id>}`.     |
| GET    | `/stream/<id>`  | Audio proxy for preset `<id>` — the speaker fetches this, not you. |
| GET    | `/status`       | Current now-playing (JSON, from the speaker's own API).           |
| GET    | `/healthz`      | Liveness: `{"ok":true,"version":"…","rendererReady":<bool>}`.      |

Mutating endpoints (`POST /config`, `POST /bass`, `POST /restart`) require
`Content-Type: application/json` (blocks cross-site form posts). Optionally set `api_token`
in the config to also require `Authorization: Bearer <token>` on those endpoints — empty
(the default) means no auth, which keeps the iOS companion app working unchanged.

`GET /play/<id>` and `GET /stream/<id>` can't use that guard (they're GETs), so they reject
requests a browser labels cross-site — a public page can't start playback with a hidden
`<img src="http://<speaker-ip>:8099/play/1">`. Non-browser clients (curl, the iOS app, the
speaker's own renderer) send no such labelling and are unaffected.

```sh
# play a preset
curl http://<speaker-ip>:8099/play/1     # BBC Radio 4
curl http://<speaker-ip>:8099/play/5     # Jazz24

# check state
curl http://<speaker-ip>:8099/healthz
curl http://<speaker-ip>:8099/status
```

The last station played is remembered and **auto-resumes when the speaker powers on**.

**Physical preset buttons** (and the SoundTouch app's presets) play your stations too: on startup and
after every config change the daemon writes the presets into the speaker's 6 hardware slots, and it
watches the device's WebSocket — when a preset button is pressed it plays that station via UPnP (the
speaker's own recall of internet-radio presets is unreliable, so the daemon drives it).

### Config editor (built in)

The daemon serves a self-contained editor page at `http://<speaker-ip>:8099/` — open it in a
browser to edit presets and bass without SSH. It pre-fills the connection from the URL and loads
automatically; **Save & Apply** takes effect immediately. Use **Restart service** only after
changing `proxy_port`.

Because the editor is served by the daemon itself it is same-origin with the API — no CORS
involved. CORS for other tools is restricted to localhost/private-network origins (`Origin: null`
is deliberately rejected), so a public website can't reach your speaker through your browser.
It applies to every endpoint, so a LAN dashboard can read `/status` and `/healthz` too.

### Editing stations (SSH)

Presets live in `/mnt/nv/soundtouchd/config.json` on the speaker — edit over SSH and restart:

```sh
ssh -o HostKeyAlgorithms=+ssh-rsa root@<speaker-ip>
vi /mnt/nv/soundtouchd/config.json       # set name / stream_url per preset (ids 1–6)
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
cmd/soundtouchd     entrypoint (HTTP proxy + control, UPnP, auto-resume, embedded editor.html)
internal/streamproxy  HTTPS→HTTP + playlist resolution + SSRF guard (tested)
internal/presets      JSON config (tested)
internal/upnp         AVTransport SetAVTransportURI + Play
internal/resume       power-on detection via WebSocket
packaging/            init script, install/uninstall (run on device)
scripts/              backup.sh, deploy-tmp.sh (run from the Mac)
```

## Related

- **[SoundTouch-Device-Companion-App](https://github.com/jeffevertse/SoundTouch-Device-Companion-App)** — native iOS app for controlling presets, viewing now-playing, and adjusting bass over this HTTP API
