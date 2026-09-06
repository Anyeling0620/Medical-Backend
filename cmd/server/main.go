package main

import (
	"Medical-Web-Backend/internal/bootstrap"
	"Medical-Web-Backend/internal/config"
	"log"
	"os"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("load configuration: %v", err)
		os.Exit(1)
	}

	app, err := bootstrap.NewApp(cfg)
	if err != nil {
		log.Printf("initialize application: %v", err)
		os.Exit(1)
	}

	if err := app.Run(); err != nil {
		log.Printf("run application: %v", err)
		os.Exit(1)
	}

}
