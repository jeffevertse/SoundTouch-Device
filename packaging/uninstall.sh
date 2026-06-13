#!/bin/sh
# Run ON the SoundTouch. Full rollback: removes everything this project added.
# We never modify Bose config, so this returns the device to stock.
APP=soundtouchd
INSTALL_DIR=/mnt/nv/$APP

rw 2>/dev/null || mount -o remount,rw / 2>/dev/null || true

[ -x /etc/init.d/$APP ] && /etc/init.d/$APP stop 2>/dev/null
update-rc.d -f $APP remove 2>/dev/null || true
rm -f /etc/init.d/$APP
rm -f /opt/$APP
rm -rf "$INSTALL_DIR"
rm -f /tmp/$APP.log /var/run/$APP.pid

echo "[uninstall] removed $APP. Reboot for a fully clean state."
df -h /mnt/nv 2>/dev/null || df -h
