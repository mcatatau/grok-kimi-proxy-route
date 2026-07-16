package main

import (
	"embed"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Log panics so a real crash leaves a trail (does not swallow them).
	defer func() {
		if r := recover(); r != nil {
			logCrash(fmt.Sprintf("panic: %v\n%s", r, debug.Stack()))
			panic(r)
		}
	}()
	setupFileLog()

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "Grok Desktop",
		Width:     1280,
		Height:    860,
		MinWidth:  960,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 5, G: 5, B: 5, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.Dark,
		},
	})
	if err != nil {
		log.Printf("wails.Run: %v", err)
		logCrash("wails.Run: " + err.Error())
		println("Error:", err.Error())
	}
}

func setupFileLog() {
	dir, err := defaultLogDir()
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "app.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("——— Grok Desktop start %s ———", time.Now().Format(time.RFC3339))
}

func logCrash(msg string) {
	dir, err := defaultLogDir()
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "crash.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), msg)
}

func defaultLogDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "GrokDesktop", "logs"), nil
}
