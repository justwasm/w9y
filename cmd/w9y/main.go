package main

import (
	"log"
	"os"

	"github.com/btwiuse/w9y"
)

func main() {
	if err := w9y.Run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
