VERSION ?= 0.1.0
SSHOPT  := -o HostKeyAlgorithms=+ssh-rsa
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet armv7 backup run-tmp install uninstall clean

## Local host build (for development on the Mac)
build:
	go build -ldflags "$(LDFLAGS)" -o dist/soundtouchd-host ./cmd/soundtouchd

test:
	go test ./...

vet:
	go vet ./...

## Cross-compile the static armv7 binary for the speaker
armv7:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o dist/soundtouchd ./cmd/soundtouchd
	@file dist/soundtouchd

## ── Device targets (require HOST=<speaker-ip>) ───────────────────────────────
## ALWAYS run `make backup HOST=…` first, then `make run-tmp`, then `make install`.

backup:
	@test -n "$(HOST)" || (echo "set HOST=<speaker-ip>"; exit 1)
	sh scripts/backup.sh $(HOST)

## Run from /tmp (nothing persisted; reboot reverts) — validate before installing
run-tmp: armv7
	@test -n "$(HOST)" || (echo "set HOST=<speaker-ip>"; exit 1)
	sh scripts/deploy-tmp.sh $(HOST)

## Persistent install into /mnt/nv with auto-start
install: armv7
	@test -n "$(HOST)" || (echo "set HOST=<speaker-ip>"; exit 1)
	ssh $(SSHOPT) root@$(HOST) 'mkdir -p /tmp/soundtouchd-stage'
	scp -O $(SSHOPT) dist/soundtouchd packaging/soundtouchd.initd packaging/config.example.json \
		packaging/install.sh packaging/uninstall.sh root@$(HOST):/tmp/soundtouchd-stage/
	ssh $(SSHOPT) root@$(HOST) 'sh /tmp/soundtouchd-stage/install.sh'

## Full rollback on the device
uninstall:
	@test -n "$(HOST)" || (echo "set HOST=<speaker-ip>"; exit 1)
	scp -O $(SSHOPT) packaging/uninstall.sh root@$(HOST):/tmp/uninstall.sh
	ssh $(SSHOPT) root@$(HOST) 'sh /tmp/uninstall.sh'

clean:
	rm -rf dist
