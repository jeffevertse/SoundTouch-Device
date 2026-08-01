#!/bin/sh
# Run ON the SoundTouch (from the staging dir holding the binary + this script).
# Persistent install into /mnt/nv with auto-start. Additive & reversible — see
# uninstall.sh. Test with the /tmp run first (scripts/deploy-tmp.sh) before this.
set -e

APP=soundtouchd
INSTALL_DIR=/mnt/nv/$APP
STAGE_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "[install] remount rootfs read-write"
rw 2>/dev/null || mount -o remount,rw / 2>/dev/null || true

echo "[install] disk before:"; df -h /mnt/nv 2>/dev/null || df -h

mkdir -p "$INSTALL_DIR"

# Stop a running instance first — you can't overwrite a running binary (ETXTBSY).
if [ -x /etc/init.d/$APP ]; then
    /etc/init.d/$APP stop 2>/dev/null
    sleep 1
fi

# Back up any existing binary before overwriting (rollback point).
if [ -f "$INSTALL_DIR/$APP" ]; then
    ver=$("$INSTALL_DIR/$APP" -version 2>/dev/null || date +%Y%m%d-%H%M%S)
    cp -p "$INSTALL_DIR/$APP" "$INSTALL_DIR/$APP.$ver.backup"
    echo "[install] backed up existing binary -> $APP.$ver.backup"
fi

cp "$STAGE_DIR/$APP" "$INSTALL_DIR/$APP"
chmod +x "$INSTALL_DIR/$APP"
# rm before ln: `ln -sf` onto an existing symlink-to-dir would create the link
# INSIDE the target dir (clobbering the binary). Remove the old symlink first.
rm -f /opt/$APP
ln -s "$INSTALL_DIR" /opt/$APP

# Seed config only if absent (preserve user edits).
[ -f "$INSTALL_DIR/config.json" ] || cp "$STAGE_DIR/config.example.json" "$INSTALL_DIR/config.json"

# Init script + auto-start.
cp "$STAGE_DIR/$APP.initd" /etc/init.d/$APP
chmod +x /etc/init.d/$APP
update-rc.d $APP defaults 2>/dev/null || true

# Keep only the newest backup to protect the small /mnt/nv partition.
# No `xargs -r` here: it's a GNU extension that BusyBox may not have, and under
# `set -e` a failure would abort before the service is started below.
ls -t "$INSTALL_DIR/$APP".*.backup 2>/dev/null | tail -n +2 | while read -r old; do
    rm -f "$old"
done

/etc/init.d/$APP restart 2>/dev/null || /etc/init.d/$APP start
sleep 2

echo "[install] disk after:"; df -h /mnt/nv 2>/dev/null || df -h

# Health-check the port the config actually uses, not a hardcoded 8099.
PORT=$(sed -n 's/.*"proxy_port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
    "$INSTALL_DIR/config.json" 2>/dev/null | head -n 1)
[ -n "$PORT" ] || PORT=8099

echo "[install] health (port $PORT):"
curl -fsS --max-time 5 "http://127.0.0.1:$PORT/healthz" || echo "(health check failed — see /tmp/$APP.log)"
echo
echo "[install] done. Uninstall with uninstall.sh."
