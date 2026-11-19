// Command winicon writes the application icon (.ico) embedded in the Windows
// executable. Run via go generate in cmd/immich-courier.
package main

import (
	"flag"
	"log"
	"os"

	"immich-courier/internal/tray"
)

func main() {
	out := flag.String("o", "app.ico", "output file")
	flag.Parse()
	if err := os.WriteFile(*out, tray.AppIconICO(), 0o644); err != nil {
		log.Fatal(err)
	}
}
