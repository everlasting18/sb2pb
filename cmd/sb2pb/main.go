package main

import (
	"os"

	"github.com/everlasting18/sb2pb/internal/app"
)

// version is set at release time: -ldflags "-X main.version=v0.1.0"
var version = "dev"

func main() {
	os.Exit(app.Main(os.Args[1:], version))
}
