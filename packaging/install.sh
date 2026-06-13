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

# Back up any existing binary before overwriting (rollback point).
if [ -f "$INSTALL_DIR/$APP" ]; then
    ver=$("$INSTALL_DIR/$APP" -version 2>/dev/null || date +%Y%m%d-%H%M%S)
    cp -p "$INSTALL_DIR/$APP" "$INSTALL_DIR/$APP.$ver.backup"
    echo "[install] backed up existing binary -> $APP.$ver.backup"
fi

cp "$STAGE_DIR/$APP" "$INSTALL_DIR/$APP"
chmod +x "$INSTALL_DIR/$APP"
ln -sf "$INSTALL_DIR" /opt/$APP

# Seed config only if absent (preserve user edits).
[ -f "$INSTALL_DIR/config.json" ] || cp "$STAGE_DIR/config.example.json" "$INSTALL_DIR/config.json"

# Init script + auto-start.
cp "$STAGE_DIR/$APP.initd" /etc/init.d/$APP
chmod +x /etc/init.d/$APP
update-rc.d $APP defaults 2>/dev/null || true

# Keep only the newest backup to protect the small /mnt/nv partition.
ls -t "$INSTALL_DIR/$APP".*.backup 2>/dev/null | tail -n +2 | xargs -r rm -f

/etc/init.d/$APP restart 2>/dev/null || /etc/init.d/$APP start
sleep 2

echo "[install] disk after:"; df -h /mnt/nv 2>/dev/null || df -h
echo "[install] health:"
curl -fsS --max-time 5 "http://127.0.0.1:8091/healthz" || echo "(health check failed — see /tmp/$APP.log)"
echo
echo "[install] done. Uninstall with uninstall.sh."
