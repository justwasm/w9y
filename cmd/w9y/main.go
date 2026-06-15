package main

import (
	"context"
	"os"

	"charm.land/fang/v2"

	"github.com/btwiuse/w9y"
)

func main() {
	if err := fang.Execute(context.Background(), w9y.NewRootCommand()); err != nil {
		os.Exit(1)
	}
}
