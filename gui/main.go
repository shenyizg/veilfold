// Command Veilfold is the desktop app for macOS and Windows (Linux builds
// work too, but are not officially supported).
package main

import (
	"embed"
	"log"
	"runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/shenyizg/veilfold/internal/version"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var icon []byte

func main() {
	app, err := NewApp()
	if err != nil {
		log.Fatal(err)
	}
	var appMenu *menu.Menu
	if runtime.GOOS == "darwin" {
		// Without an Edit menu, copy and paste do not work in text fields.
		appMenu = menu.NewMenuFromItems(menu.AppMenu(), menu.EditMenu(), menu.WindowMenu())
	}
	err = wails.Run(&options.App{
		Title:            "Veilfold",
		Width:            980,
		Height:           680,
		MinWidth:         760,
		MinHeight:        540,
		BackgroundColour: &options.RGBA{R: 245, G: 246, B: 250, A: 255},
		AssetServer:      &assetserver.Options{Assets: assets},
		Menu:             appMenu,
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		ErrorFormatter:   formatError,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "io.github.shenyizg.veilfold",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { app.secondInstance(nil) },
		},
		DragAndDrop: &options.DragAndDrop{EnableFileDrop: true, DisableWebViewDrop: true},
		Mac: &mac.Options{
			// The standard title bar: web content never sits under it, so every
			// control receives clicks (an inset toolbar swallows them).
			TitleBar: mac.TitleBarDefault(),
			About: &mac.AboutInfo{
				Title:   "Veilfold " + version.Version,
				Message: "Hide and lock private folders.\nMIT License · github.com/shenyizg/veilfold",
				Icon:    icon,
			},
		},
		Windows: &windows.Options{Theme: windows.SystemDefault},
	})
	if err != nil {
		log.Fatal(err)
	}
}
