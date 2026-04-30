package main

import (
	"os"

	"github.com/Luew2/FreeCode/internal/adapters/cli"
)

func main() {
	os.Exit(cli.RunWithIO(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
