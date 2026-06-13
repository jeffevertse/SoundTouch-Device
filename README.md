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
