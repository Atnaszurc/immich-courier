package tray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Start-at-login uses each OS's per-user mechanism, no admin rights needed:
//   Windows: HKCU\Software\Microsoft\Windows\CurrentVersion\Run
//   macOS:   ~/Library/LaunchAgents/<label>.plist
//   Linux:   ~/.config/autostart/immich-courier.desktop (XDG, desktop sessions)

const autostartName = "immich-courier"

// launchCommand is the tray executable plus its arguments.
func launchCommand(args []string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return append([]string{exe}, args...), nil
}

func autostartEnabled() bool {
	switch runtime.GOOS {
	case "windows":
		return winAutostartEnabled()
	case "darwin":
		_, err := os.Stat(launchAgentPath())
		return err == nil
	default:
		_, err := os.Stat(xdgAutostartPath())
		return err == nil
	}
}

func setAutostart(on bool, args []string) error {
	cmd, err := launchCommand(args)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "windows":
		return winSetAutostart(on, cmd)
	case "darwin":
		p := launchAgentPath()
		if !on {
			return removeIfExists(p)
		}
		var argXML strings.Builder
		for _, a := range cmd {
			fmt.Fprintf(&argXML, "\t\t<string>%s</string>\n", xmlEscape(a))
		}
		plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + argXML.String() + `	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`
		return writeFile(p, plist)
	default:
		p := xdgAutostartPath()
		if !on {
			return removeIfExists(p)
		}
		quoted := make([]string, len(cmd))
		for i, a := range cmd {
			quoted[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(a) + `"`
		}
		desktop := "[Desktop Entry]\nType=Application\nName=immich-courier\n" +
			"Comment=Upload new photos and videos to Immich\n" +
			"Exec=" + strings.Join(quoted, " ") + "\nX-GNOME-Autostart-enabled=true\nTerminal=false\n"
		return writeFile(p, desktop)
	}
}

const launchAgentLabel = "io.github.immich-courier"

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func xdgAutostartPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", autostartName+".desktop")
}

func writeFile(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// openPath opens a file or folder with the OS default handler.
func openPath(p string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", p).Start()
	case "darwin":
		return exec.Command("open", p).Start()
	default:
		return exec.Command("xdg-open", p).Start()
	}
}
