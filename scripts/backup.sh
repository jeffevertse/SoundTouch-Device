#!/bin/sh
# Phase-0 rollback snapshot — run from the Mac BEFORE anything else.
# Usage: scripts/backup.sh <speaker-ip>
set -e
HOST="$1"
[ -z "$HOST" ] && { echo "usage: $0 <speaker-ip>"; exit 1; }

SSHOPT="-o HostKeyAlgorithms=+ssh-rsa"
DIR="backup/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$DIR"

echo "Snapshotting $HOST -> $DIR"
ssh $SSHOPT "root@$HOST" '
  echo "== uname =="; uname -a
  echo "== os-release =="; cat /etc/os-release 2>/dev/null
  echo "== mount =="; mount
  echo "== df =="; df -h
  echo "== /mnt/nv =="; ls -la /mnt/nv
  echo "== /etc/init.d =="; ls -la /etc/init.d
  echo "== /opt =="; ls -la /opt
' > "$DIR/device-state.txt" 2>&1

# Copy off the config files OTHER tools modify, so we can prove we did not touch
# them (and could restore them if ever needed).
scp -O $SSHOPT "root@$HOST:/opt/Bose/etc/SoundTouchSdkPrivateCfg.xml" "$DIR/" 2>/dev/null || echo "(no SoundTouchSdkPrivateCfg.xml)"
scp -O $SSHOPT "root@$HOST:/mnt/nv/BoseApp-Persistence/1/Sources.xml" "$DIR/" 2>/dev/null || echo "(no Sources.xml)"

echo "Done. Review $DIR/device-state.txt before installing anything."
