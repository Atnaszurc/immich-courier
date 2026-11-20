package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"fyne.io/systray"

	"immich-courier/internal/auth"
	"immich-courier/internal/config"
	"immich-courier/internal/daemon"
	"immich-courier/internal/tray"
	"immich-courier/internal/webui"
)

// cmdTray runs the uploader with a tray icon. It's what runs when the
// program is started without arguments (double-click, start at login).
func cmdTray(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tray", flag.ExitOnError)
	cfgPath := configFlag(fs)
	fs.Parse(args)

	hideOwnConsole()

	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o700); err != nil {
		tray.ShowError(err.Error())
		return err
	}
	logPath := daemon.LogPath(*cfgPath)
	logFile := openLog(logPath)
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(logFile, nil))

	d, err := daemon.Open(*cfgPath, version, log)
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		fmt.Fprintln(os.Stderr, "immich-courier is already running.")
		return nil
	}
	if err != nil {
		log.Error("can't start", "err", err)
		tray.ShowError("immich-courier can't start:\n\n" + err.Error())
		return err
	}
	log.Info("immich-courier started with tray", "version", version, "config", *cfgPath)
	fmt.Fprintln(os.Stderr, "immich-courier is running in the system tray. Log:", logPath)

	ui := webui.New(d)
	defer ui.Close()
	d.OpenSettings = ui.URL // before Run, so IPC never sees it unset

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := d.Run(ctx); err != nil {
			log.Error("stopped with error", "err", err)
			tray.ShowError("immich-courier stopped because of an error:\n\n" + err.Error())
		}
		log.Info("immich-courier stopped", "reason", stopReason(ctx))
		systray.Quit()
	}()

	openSettings := func() {
		u, err := ui.URL()
		if err != nil {
			log.Error("can't open settings", "err", err)
			tray.ShowError("Can't open settings:\n\n" + err.Error())
			return
		}
		if err := auth.OpenBrowser(u); err != nil {
			log.Error("can't open browser", "err", err)
		}
	}
	firstRun := false
	if cfg, err := config.Load(*cfgPath); err == nil && len(cfg.Accounts) == 0 {
		firstRun = true
	}

	launchArgs := []string{"tray"}
	if *cfgPath != defaultConfigPath() {
		launchArgs = append(launchArgs, "--config", *cfgPath)
	}
	tray.Run(ctx, d, tray.Options{
		ConfigPath: *cfgPath, LogPath: logPath, LaunchArgs: launchArgs,
		OpenSettings: openSettings, FirstRun: firstRun,
	})
	// Tray closed (Quit, or the session is ending): stop the daemon and wait,
	// so the state DB and socket are closed cleanly.
	d.Quit()
	<-runDone
	return nil
}

// guiAvailable reports whether a tray can be shown, so that running the
// program without arguments on a headless server prints help instead.
func guiAvailable() bool {
	switch {
	case isWindows(), isDarwin():
		return true
	default:
		return os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" &&
			(os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "")
	}
}
