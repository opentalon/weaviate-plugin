package main

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"

	"github.com/opentalon/opentalon/pkg/plugin"
)

func main() {
	log.SetOutput(os.Stderr)
	log.Println("weaviate-plugin: process starting")

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/opentalon/opentalon" {
				log.Printf("weaviate-plugin: opentalon SDK %s", dep.Version)
			}
		}
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("weaviate-plugin: PANIC: %v\n%s", r, debug.Stack())
			fmt.Fprintf(os.Stderr, "weaviate-plugin: PANIC: %v\n", r)
			os.Exit(1)
		}
	}()

	log.Println("weaviate-plugin: calling plugin.Serve")
	if err := plugin.Serve(&WeaviateHandler{}); err != nil {
		log.Fatalf("weaviate-plugin: plugin.Serve error: %v", err)
	}
}
