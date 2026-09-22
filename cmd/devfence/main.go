package main

import (
	"os"

	"github.com/winrarr/devfence/internal/devfence"
)

func main() {
	os.Exit(devfence.Main(os.Args[1:], os.Stdout, os.Stderr))
}
