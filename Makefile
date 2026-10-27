# Common development tasks. Windows users without make can run the
# commands shown here directly.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/shenyizg/veilfold/internal/version.Version=$(VERSION)
WAILS   := go run github.com/wailsapp/wails/v2/cmd/wails@v2.17.0

.PHONY: all build test race vet fmt lint cross gui gui-dev icons clean

all: vet test build

## build: build the veilfold command-line tool into bin/
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/veilfold

## test: run all tests
test:
	go test ./...

## race: run all tests with the race detector
race:
	go test -race ./...

## vet: run go vet for every supported operating system
vet:
	go vet ./...
	GOOS=linux go vet ./...
	GOOS=windows go vet ./...
	GOOS=darwin go vet ./...

## fmt: format all Go code
fmt:
	gofmt -w .

## lint: check formatting and run vet
lint:
	@test -z "$$(gofmt -l .)" || (echo "Run 'make fmt' on:" && gofmt -l . && exit 1)
	$(MAKE) vet

## cross: build the CLI for every release target into dist/
cross:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/veilfold-$$os-$$arch$$ext ./cmd/veilfold || exit 1; \
	done

## gui: build the desktop app for this platform (gui/build/bin/)
gui:
	cd gui && $(WAILS) build -clean -skipbindings -trimpath -ldflags "-X github.com/shenyizg/veilfold/internal/version.Version=$(VERSION)"

## gui-dev: run the desktop app in development mode with live reload
gui-dev:
	cd gui && $(WAILS) dev -skipbindings

## icons: regenerate the application icons from scripts/gen_icons.py
icons:
	python3 scripts/gen_icons.py
	cp assets/logo.svg gui/frontend/dist/logo.svg

clean:
	rm -rf bin dist gui/build/bin gui/frontend/wailsjs
