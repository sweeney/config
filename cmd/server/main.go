package main

import (
	"log"
	"os"
)

var version = "dev"

func main() {
	if err := runServer(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
