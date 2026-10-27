# Building and releasing

## Prerequisites

| | Command-line tool | Desktop app |
| --- | --- | --- |
| All platforms | [Go 1.26+](https://go.dev/dl/) | Go 1.26+ |
| macOS | — | Xcode Command Line Tools (`xcode-select --install`) |
| Windows | — | Nothing else (WebView2 is part of Windows 10/11) |
| Linux | — | `gcc`, `pkg-config`, GTK 3 and WebKitGTK development packages (experimental) |

The desktop app is built with the [Wails v2](https://wails.io) CLI. You do not need to install it: the
Makefile runs a pinned version with `go run github.com/wailsapp/wails/v2/cmd/wails@v2.17.0`. There is no
Node.js or npm step; the frontend in `gui/frontend/dist` is plain HTML, CSS and JavaScript.

## Command-line tool

```sh
make build            # bin/veilfold
make test             # unit and integration tests
make race             # tests with the race detector
make lint             # gofmt check + go vet for linux, windows and darwin
make cross            # dist/veilfold-<os>-<arch> for all release targets
```

Without make:

```sh
go build -o bin/ ./cmd/veilfold
go test ./...
```

The CLI is pure Go (`CGO_ENABLED=0` works) and cross-compiles from any platform.

## Desktop app

```sh
make gui              # gui/build/bin/Veilfold.app (macOS) or Veilfold.exe (Windows)
make gui-dev          # development mode with live reload of the frontend
```

Equivalent commands:

```sh
cd gui
go run github.com/wailsapp/wails/v2/cmd/wails@v2.17.0 build -clean -skipbindings -trimpath
```

Useful variations:

- macOS universal binary: add `-platform darwin/universal`.
- Windows from macOS or Linux: add `-platform windows/amd64` (Windows builds need no C compiler).
- Linux (experimental): install the GTK/WebKitGTK headers, e.g. on Debian/Ubuntu
  `sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev`, then add
  `-tags webkit2_41`.

To compile the app without the Wails CLI (for a quick check), use the build tags Wails sets:

```sh
cd gui
# macOS needs one extra framework when linking without the Wails CLI:
CGO_LDFLAGS="-framework UniformTypeIdentifiers" go build -tags desktop,production .
GOOS=windows go build -tags desktop,production -ldflags "-H windowsgui" .
```

The `gui/` directory is its own Go module (it depends on the root module through a `replace` directive),
so `go test ./...` at the repository root never needs GUI dependencies.

### Icons

The application icon is rendered by `scripts/gen_icons.py` (Python 3, no dependencies), which writes
`gui/build/appicon.png` and `gui/build/windows/icon.ico`. The vector logo is `assets/logo.svg`. Run
`make icons` after changing either.

### Running against a test data directory

Set `VEILFOLD_DATA_DIR` to keep experiments away from your real vaults:

```sh
VEILFOLD_DATA_DIR=/tmp/veilfold-test ./bin/veilfold init
VEILFOLD_DATA_DIR=/tmp/veilfold-test gui/build/bin/Veilfold.app/Contents/MacOS/Veilfold
```

## Versioning

Veilfold follows [Semantic Versioning](https://semver.org). The version is injected at build time:

```sh
go build -ldflags "-X github.com/shenyizg/veilfold/internal/version.Version=v0.2.0" ./cmd/veilfold
```

## Releasing

1. Update `CHANGELOG.md` and the `productVersion` in `gui/wails.json`.
2. Tag the commit and push the tag:

   ```sh
   git tag -a v0.2.0 -m "Veilfold 0.2.0"
   git push origin v0.2.0
   ```

3. The [release workflow](../.github/workflows/release.yml) builds the CLI for Linux, macOS and Windows
   (amd64 and arm64), the desktop app for macOS (universal) and Windows (amd64), computes
   `SHA256SUMS.txt`, and publishes a draft GitHub release. Review it and publish.

Code signing and notarization are not set up yet. To add them, sign `Veilfold.app` with `codesign` and
notarize it with `xcrun notarytool` on macOS, and sign `Veilfold.exe` with `signtool` on Windows, using
certificates stored as repository secrets.
