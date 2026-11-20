//go:build windows

package daemon

import "os"

// ownedByMe: on Windows the socket lives under the user's profile (config
// dir or %TEMP%), which other users can't write to by default.
func ownedByMe(os.FileInfo) bool { return true }
