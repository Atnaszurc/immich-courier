// Package tray shows the daemon's state in the system tray / menu bar and
// offers the everyday actions. It runs in the same process as the daemon.
package tray

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"fyne.io/systray"

	"immich-courier/internal/daemon"
)

const (
	maxAccounts = 8  // menu lines are created up front and hidden when unused
	maxFolders  = 12 // (systray menus can't be rebuilt cheaply)
)

// Options configures the tray.
type Options struct {
	ConfigPath string
	LogPath    string
	// LaunchArgs are the arguments to start the tray with at login (e.g. --config).
	LaunchArgs []string
	// OpenSettings opens the settings page in the browser.
	OpenSettings func()
	// FirstRun opens the settings page right away (nothing configured yet).
	FirstRun bool
}

// Run shows the tray and blocks until the user quits or d stops. It must be
// called from the main goroutine (macOS requirement).
func Run(ctx context.Context, d *daemon.Daemon, opt Options) {
	systray.Run(func() { onReady(ctx, d, opt) }, func() {})
}

func onReady(ctx context.Context, d *daemon.Daemon, opt Options) {
	systray.SetTitle("")
	systray.SetTooltip("immich-courier")
	systray.SetIcon(iconBytes(iconIdle))

	mStatus := systray.AddMenuItem("Starting…", "")
	mStatus.Disable()
	var mAccts []*systray.MenuItem
	for i := 0; i < maxAccounts; i++ {
		m := systray.AddMenuItem("", "")
		m.Disable()
		m.Hide()
		mAccts = append(mAccts, m)
	}
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings…", "Accounts and watched folders")
	mPause := systray.AddMenuItemCheckbox("Pause uploads", "Keep watching and queuing, but don't upload", false)
	mRescan := systray.AddMenuItem("Check folders now", "Rescan watched folders and retry waiting uploads")
	mFolders := systray.AddMenuItem("Open folder", "")
	var mFolder []*systray.MenuItem
	var folderPaths = make([]string, maxFolders)
	for i := 0; i < maxFolders; i++ {
		m := mFolders.AddSubMenuItem("", "")
		m.Hide()
		mFolder = append(mFolder, m)
		go func(i int) {
			for range m.ClickedCh {
				if p := folderPaths[i]; p != "" {
					openPath(p)
				}
			}
		}(i)
	}
	systray.AddSeparator()
	mConfig := systray.AddMenuItem("Open config file", opt.ConfigPath)
	mLog := systray.AddMenuItem("Open log", opt.LogPath)
	mAuto := systray.AddMenuItemCheckbox("Start at login", "", autostartEnabled())
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit immich-courier", "Stop watching and uploading")

	if opt.FirstRun && opt.OpenSettings != nil {
		go opt.OpenSettings()
	}

	refresh := make(chan struct{}, 1)
	poke := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-mPause.ClickedCh:
				d.SetPaused(!mPause.Checked())
				poke()
			case <-mSettings.ClickedCh:
				if opt.OpenSettings != nil {
					opt.OpenSettings()
				}
			case <-mRescan.ClickedCh:
				d.Rescan()
			case <-mConfig.ClickedCh:
				openPath(opt.ConfigPath)
			case <-mLog.ClickedCh:
				openPath(opt.LogPath)
			case <-mAuto.ClickedCh:
				on := !mAuto.Checked()
				if err := setAutostart(on, opt.LaunchArgs); err != nil {
					d.Log.Error("couldn't change start at login", "err", err)
				} else if on {
					mAuto.Check()
				} else {
					mAuto.Uncheck()
				}
			case <-mQuit.ClickedCh:
				d.Quit()
				return
			}
		}
	}()

	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		last := iconState(-1)
		for {
			s := d.Status()
			sum := summarize(s)
			if sum.icon != last {
				systray.SetIcon(iconBytes(sum.icon))
				last = sum.icon
			}
			systray.SetTooltip("immich-courier: " + sum.text)
			mStatus.SetTitle(sum.text)
			if s.Paused {
				mPause.Check()
			} else {
				mPause.Uncheck()
			}
			for i, m := range mAccts {
				if i < len(sum.accounts) && len(s.Accounts) > 1 {
					m.SetTitle(sum.accounts[i])
					m.Show()
				} else {
					m.Hide()
				}
			}
			for i, m := range mFolder {
				if i < len(s.Folders) {
					folderPaths[i] = s.Folders[i].Path
					m.SetTitle(filepath.Base(s.Folders[i].Path) + "  (" + s.Folders[i].Account + ")")
					m.SetTooltip(s.Folders[i].Path)
					m.Show()
				} else {
					folderPaths[i] = ""
					m.Hide()
				}
			}
			if len(s.Folders) == 0 {
				mFolders.Disable()
			} else {
				mFolders.Enable()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-refresh:
			}
		}
	}()
}

type summary struct {
	icon     iconState
	text     string   // one line for the menu header and tooltip
	accounts []string // one line per account
}

// summarize turns daemon status into what the tray shows. Most urgent first:
// problems, paused, offline, busy, idle.
func summarize(s daemon.Status) summary {
	var out summary
	waiting, offline, uploading := 0, 0, ""
	var authFailed []string
	for _, a := range s.Accounts {
		waiting += a.Queued
		line := a.Name + ": "
		switch {
		case a.AuthFailed:
			authFailed = append(authFailed, a.Name)
			line += "login expired"
		case !a.Online:
			offline++
			line += "server unreachable"
		case a.Uploading != "":
			line += "uploading"
		default:
			line += "online"
		}
		if a.Queued > 0 {
			line += fmt.Sprintf(", %d waiting", a.Queued)
		}
		if a.Uploading != "" && uploading == "" {
			uploading = filepath.Base(a.Uploading)
		}
		out.accounts = append(out.accounts, line)
	}

	switch {
	case s.Problem != "":
		out.icon, out.text = iconProblem, "Not uploading: "+s.Problem
	case len(authFailed) > 0:
		out.icon, out.text = iconProblem, "Log in again: "+authFailed[0]
	case s.Paused:
		out.icon, out.text = iconPaused, "Paused"
		if waiting > 0 {
			out.text += fmt.Sprintf(", %d waiting", waiting)
		}
	case offline > 0 && offline == len(s.Accounts):
		out.icon, out.text = iconOffline, "Server unreachable"
		if waiting > 0 {
			out.text += fmt.Sprintf(", %d waiting", waiting)
		}
	case uploading != "":
		out.icon, out.text = iconBusy, "Uploading "+uploading
		if waiting > 1 {
			out.text += fmt.Sprintf(" (%d left)", waiting)
		}
	case waiting > 0:
		out.icon, out.text = iconBusy, fmt.Sprintf("%d waiting", waiting)
		if offline > 0 {
			out.text += ", some servers unreachable"
		}
	case offline > 0:
		out.icon, out.text = iconOffline, "Some servers unreachable"
	default:
		out.icon, out.text = iconIdle, "Up to date"
	}
	return out
}
