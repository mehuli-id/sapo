# Sapo — perintah pengembangan.
# Semua nilai yang bisa berubah ada di variabel di bawah, bukan di dalam resep.

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
BIN       := bin
DEV_DIR   := .dev
DEV_SOCK  := $(DEV_DIR)/sapod.sock
DEV_LISTEN ?= 127.0.0.1:5000
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: all build go web check test lint dev dev-agent dev-api dev-web clean

all: build

build: go web ## Build sapod, sapo-api, dan panel

go: ## Build binary Go
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sapod ./cmd/sapod
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/sapo-api ./cmd/sapo-api

web: ## Build panel ke web/dist
	cd web && npm ci && npm run build

check: lint test ## Lint + test (wajib sebelum pull request)

test:
	go test ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || (echo "Jalankan gofmt:"; gofmt -l cmd internal; exit 1)
	go vet ./...
	cd web && npm run typecheck

# Mode pengembangan: sapod tanpa root (hanya operasi baca), sapo-api di
# localhost, dan panel Vite dengan hot reload di http://localhost:5173.
dev: go
	@mkdir -p $(DEV_DIR)
	@echo "Panel: http://localhost:5173  (Ctrl+C untuk berhenti)"
	@trap 'kill 0' INT TERM; \
	  $(BIN)/sapod -config config/sapo.yaml -socket $(DEV_SOCK) -features features & \
	  sleep 1; \
	  $(BIN)/sapo-api -config config/sapo.yaml -listen $(DEV_LISTEN) -socket $(DEV_SOCK) -web web/dist -branding branding.yaml & \
	  cd web && npm run dev; \
	  wait

clean:
	rm -rf $(BIN) $(DEV_DIR) web/dist
