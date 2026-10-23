# Sapo — perintah pengembangan.
# Semua nilai yang bisa berubah ada di variabel di bawah, bukan di dalam resep.

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
BIN       := bin
DEV_DIR   := .dev
DEV_SOCK  := $(DEV_DIR)/sapod.sock
DEV_LISTEN ?= 127.0.0.1:5000
LDFLAGS   := -s -w -X main.version=$(VERSION)
ARCH      ?= $(shell dpkg --print-architecture 2>/dev/null || go env GOARCH)

.PHONY: all build go web deb iso check test lint dev clean

all: build

build: go web ## Build sapod, sapo-api, dan panel

go: ## Build binary Go
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sapod ./cmd/sapod
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sapo-api ./cmd/sapo-api

web: ## Build panel ke web/dist
	cd web && npm ci && npm run build

deb: web ## Bangun paket .deb ke dist/ (ARCH=amd64|arm64)
	ARCH=$(ARCH) packaging/deb/build.sh

# ISO live dev (butuh Debian dengan live-build; build ISO memakai sudo). Arsitektur
# ISO diatur di packaging/iso/iso.conf; paket .deb dibangun untuk arsitektur itu.
iso: ## Bangun ISO live dev ke dist/
	$(MAKE) deb ARCH=$$(. packaging/iso/iso.conf && echo $$ARCH)
	sudo packaging/iso/build.sh

check: lint test ## Lint + test (wajib sebelum pull request)

test:
	go test ./...

lint:
	@test -z "$$(gofmt -l cmd internal tools)" || (echo "Jalankan gofmt:"; gofmt -l cmd internal tools; exit 1)
	go vet ./...
	cd web && npm run typecheck

# Mode pengembangan: sapod tanpa root (hanya operasi baca), sapo-api di
# localhost tanpa HTTPS (-no-tls), dan panel Vite dengan hot reload di
# http://localhost:5173.
dev: go
	@mkdir -p $(DEV_DIR)
	@echo "Panel: http://localhost:5173  (Ctrl+C untuk berhenti)"
	@trap 'kill 0' INT TERM; \
	  $(BIN)/sapod -config config/sapo.yaml -socket $(DEV_SOCK) -features features & \
	  sleep 1; \
	  $(BIN)/sapo-api -config config/sapo.yaml -listen $(DEV_LISTEN) -socket $(DEV_SOCK) -web web/dist -branding branding.yaml -wizards wizards -data $(DEV_DIR)/data -no-tls & \
	  cd web && npm run dev; \
	  wait

clean:
	rm -rf $(BIN) $(DEV_DIR) web/dist dist build
