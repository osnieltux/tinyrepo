// Command tinyrepo builds and serves Debian and Arch Linux package
// repositories. Everything it does lives in internal/tinyrepo; this is only
// the entry point.
package main

import (
	"os"

	"tinyrepo/internal/tinyrepo"
)

func main() {
	// Main returns the exit code so every deferred close still happens.
	os.Exit(tinyrepo.Main())
}
