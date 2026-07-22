package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/shared"
	"strconv"
	"time"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
	// Using config...
	log.Println("Connecting to:", cfg.DOCKER_API_URL)
	log.Println("User:", cfg.DOCKER_API_USERNAME)
	//test credentials
	pm := &shared.PortManager{}
	log.Println("project aims to acheieve creation, deletion, start, stop, pause of database containers")
	CreatePostgres("haki", "dddt", "t1est", pm)
	log.Println("done")
}

func CreatePostgres(user string, password string, name string, pm *shared.PortManager) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
	targeturl := fmt.Sprintf("%s/v1.44/containers/create?name=%s", cfg.DOCKER_API_URL, name)

	ranport, err := pm.GenerateUniquePort()
	if err != nil {
		log.Println("error generating port")
		return
	}
	body := shared.ContainerPayLoad{
		Image: "postgres:latest",
		Env: []string{
			fmt.Sprintf("POSTGRES_USER=%s", user),
			fmt.Sprintf("POSTGRES_PASSWORD=%s", password),
			fmt.Sprintf("POSTGRES_DB=%s", name),
		},
		HostConfig: shared.HostConfig{
			PortBindings: map[string][]shared.PortBinding{
				"5432/tcp": {
					{HostPort: strconv.Itoa(ranport)},
				},
			},
		},
	}
	jsondata, err := json.Marshal(body)
	if err != nil {
		panic(err.Error())
	}
	req, err := http.NewRequest(http.MethodPost, targeturl, bytes.NewBuffer(jsondata))
	if err != nil {
		panic("failed to make http request")
	}
	req.SetBasicAuth(cfg.DOCKER_API_USERNAME, cfg.DOCKER_API_PASSWORD)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) SparkDB-Engine/1.0")
	client := &http.Client{
		Timeout: 15 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		panic(err.Error())
	}
	defer resp.Body.Close()

	resbody, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("Http status: %s\n", resp.Status)
	fmt.Printf("Http response: %s\n", resbody)

	if resp.StatusCode != http.StatusCreated {
		return
	}

	startURL := fmt.Sprintf("%s/v1.44/containers/%s/start", cfg.DOCKER_API_URL, name)
	startReq, err := http.NewRequest(http.MethodPost, startURL, nil)
	if err != nil {
		panic("failed to make start http request")
	}
	startReq.SetBasicAuth(cfg.DOCKER_API_USERNAME, cfg.DOCKER_API_PASSWORD)
	startReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) SparkDB-Engine/1.0")

	startResp, err := client.Do(startReq)
	if err != nil {
		panic(err.Error())
	}
	defer startResp.Body.Close()

	startBody, err := io.ReadAll(startResp.Body)
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("Start Http status: %s\n", startResp.Status)
	fmt.Printf("Start Http response: %s\n", startBody)
}
