package main

import (
	"log/slog"
	"os"

	"github.com/btwiuse/w9y"
)

func main() {
	if err := w9y.Run(os.Args[1:]); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}
