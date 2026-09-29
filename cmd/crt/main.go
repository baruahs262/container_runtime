// Command crt is a container runtime built on Linux namespaces, cgroups
// and overlayfs.
package main

import (
	"os"

	"github.com/baruahs262/container-runtime/internal/cli"
	// Importing the container package registers the re-exec entry points
	// (crt-init, crt-shim) that run before main.
	_ "github.com/baruahs262/container-runtime/internal/container"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
