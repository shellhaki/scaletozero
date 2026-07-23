package main

import (
	"log"
	"net/http"
	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/postgres"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	client := postgres.NewClient(cfg)
	handler := postgres.NewHandler(client)

	r := gin.Default()
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "api running"})
	})
	handler.Register(r)

	log.Println("scale-to-zero: provisioning postgres containers (create/start/stop/pause/delete)")
	if err := r.Run(":8080"); err != nil {
		log.Fatalf("server error: %v", err)
	}
}