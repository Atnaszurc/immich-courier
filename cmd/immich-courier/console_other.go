//go:build !windows

package main

import "runtime"

func isWindows() bool { return false }
func isDarwin() bool  { return runtime.GOOS == "darwin" }
func hideOwnConsole() {}
