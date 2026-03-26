package main

import (
	"log"

	"github.com/opentalon/opentalon/pkg/plugin"
)

func main() {
	if err := plugin.Serve(&WeaviateHandler{}); err != nil {
		log.Fatal(err)
	}
}
