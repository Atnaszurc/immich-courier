//go:build !windows

package tray

import (
	"fmt"
	"os"
)

func winAutostartEnabled() bool            { return false }
func winSetAutostart(bool, []string) error { return nil }
func ShowError(msg string)                 { fmt.Fprintln(os.Stderr, "immich-courier:", msg) }
