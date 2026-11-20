// Command winicon writes the application icon: the .ico embedded in the
// Windows executable (run via go generate in cmd/immich-courier), or with
// -iconset a macOS .iconset folder for `iconutil -c icns` (release builds).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"immich-courier/internal/tray"
)

func main() {
	out := flag.String("o", "app.ico", "output file")
	iconset := flag.String("iconset", "", "write a macOS .iconset folder here instead")
	flag.Parse()
	if *iconset != "" {
		writeIconset(*iconset)
		return
	}
	if err := os.WriteFile(*out, tray.AppIconICO(), 0o644); err != nil {
		log.Fatal(err)
	}
}

// writeIconset writes the sizes iconutil expects: 16–512 pt at 1x and 2x.
func writeIconset(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, pt := range []int{16, 32, 128, 256, 512} {
		for scale, suffix := range map[int]string{1: "", 2: "@2x"} {
			name := fmt.Sprintf("icon_%dx%d%s.png", pt, pt, suffix)
			if err := os.WriteFile(filepath.Join(dir, name), tray.AppIconPNG(pt*scale), 0o644); err != nil {
				log.Fatal(err)
			}
		}
	}
}
