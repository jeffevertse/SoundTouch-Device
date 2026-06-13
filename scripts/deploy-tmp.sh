#!/bin/sh
# Safe test: copy the binary to /tmp (tmpfs) and run it in the foreground.
# Nothing is persisted — a reboot fully reverts the speaker. Do this before
# `make install`. Usage: scripts/deploy-tmp.sh <speaker-ip>   (run `make armv7` first)
set -e
HOST="$1"
[ -z "$HOST" ] && { echo "usage: $0 <speaker-ip>"; exit 1; }
[ -f dist/soundtouchd ] || { echo "build first: make armv7"; exit 1; }

SSHOPT="-o HostKeyAlgorithms=+ssh-rsa"
scp -O $SSHOPT dist/soundtouchd "root@$HOST:/tmp/soundtouchd"
scp -O $SSHOPT packaging/config.example.json "root@$HOST:/tmp/config.json"

echo "Running from /tmp on $HOST (Ctrl-C to stop; nothing persisted)."
ssh $SSHOPT "root@$HOST" '/tmp/soundtouchd -config /tmp/config.json'
