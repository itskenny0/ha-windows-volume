# Build targets for ha-volume.
#
#   make            cross-compile the Windows binary (stripped, no console).
#   make windows    same as `make`.
#   make linux      build a Linux test binary (uses the audio stub).
#   make test       run unit + integration tests.
#   make clean      remove bin/.
#
# Cross-compiling from Linux needs `x86_64-w64-mingw32-gcc`; on Debian/Ubuntu
# that's `apt install gcc-mingw-w64`. We don't actually use CGO in any of the
# Windows-only deps, so building without it (CGO_ENABLED=0) keeps things
# simple and reproducible.

GO ?= go
OUT := bin/ha-volume.exe
LDFLAGS := -H=windowsgui -s -w

.PHONY: all windows linux test vet clean

all: windows

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -ldflags="$(LDFLAGS)" -o $(OUT) ./cmd/ha-volume
	@printf "→ "; ls -lh $(OUT) | awk '{print $$NF " (" $$5 ")"}'

linux:
	$(GO) build -o bin/ha-volume-linux ./cmd/ha-volume

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	GOOS=windows GOARCH=amd64 $(GO) vet ./...

clean:
	rm -rf bin
