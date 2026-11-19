// immich-courier-tray runs the uploader with a tray icon. On Windows it is
// built as a GUI program (-ldflags -H=windowsgui) so no console window opens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"fyne.io/systray"

	"immich-courier/internal/config"
	"immich-courier/internal/daemon"
	"immich-courier/internal/tray"
)

var version = "dev"

const maxLogSize = 5 << 20

func main() {
	defPath, _ := config.DefaultPath()
	cfgPath := flag.String("config", defPath, "config file")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o700); err != nil {
		tray.ShowError(err.Error())
		os.Exit(1)
	}
	logPath := daemon.LogPath(*cfgPath)
	logFile := openLog(logPath)
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(logFile, nil))

	d, err := daemon.Open(*cfgPath, version, log)
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		return // the tray for this config is already running
	}
	if err != nil {
		log.Error("can't start", "err", err)
		tray.ShowError("immich-courier can't start:\n\n" + err.Error())
		os.Exit(1)
	}
	log.Info("immich-courier tray started", "version", version, "config", *cfgPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := d.Run(ctx); err != nil {
			log.Error("stopped with error", "err", err)
			tray.ShowError("immich-courier stopped because of an error:\n\n" + err.Error())
		}
		log.Info("immich-courier stopped")
		systray.Quit()
	}()

	var launchArgs []string
	if *cfgPath != defPath {
		launchArgs = []string{"--config", *cfgPath}
	}
	tray.Run(ctx, d, tray.Options{ConfigPath: *cfgPath, LogPath: logPath, LaunchArgs: launchArgs})
	// Tray closed (Quit, or the session is ending): stop the daemon and wait.
	d.Quit()
	<-runDone // let the daemon finish, so the state DB and socket are closed cleanly
}

// openLog appends to the log file, starting over (keeping one old copy)
// once it passes maxLogSize.
func openLog(p string) io.WriteCloser {
	if st, err := os.Stat(p); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't open log:", err)
		return nopCloser{os.Stderr}
	}
	return f
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
