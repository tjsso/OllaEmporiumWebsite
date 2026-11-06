package main

import (
	"log"

	"ollaemporium/internal/config"
	"ollaemporium/internal/site"
)

func main() {
	if err := config.LoadEnv(); err != nil {
		log.Fatal(err)
	}
	site.Run()
}
