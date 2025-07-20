package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// top-level config
type Config struct {
	Settings Settings  `toml:"settings"`
	Services []Service `toml:"services"`
}

type Settings struct {
	AliveSeconds int `toml:"alive_seconds"`
}

type Service struct {
	Name         string   `toml:"name"`
	Host         string   `toml:"host"`
	Port         int      `toml:"port"`
	ExposedPort  int      `toml:"exposedPort"`
	Dependencies []string `toml:"dependencies"`
}

func main() {
	// load CONFIG env var
	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		log.Fatal("CONFIG environment variable not set")
	}

	// parse TOML
	var cfg Config
	if _, err := toml.DecodeFile(cfgPath, &cfg); err != nil {
		log.Fatalf("failed to decode TOML: %v", err)
	}

	// build lookup map
	svcMap := make(map[string]Service, len(cfg.Services))
	for _, s := range cfg.Services {
		svcMap[s.Name] = s
	}

	// grab alive timeout from TOML settings
	alive := cfg.Settings.AliveSeconds
	if alive <= 0 {
		log.Fatalf("invalid alive_seconds in config: %d", alive)
	}

	// launch proxies
	for _, svc := range cfg.Services {
		go startProxy(svc, svcMap, alive)
	}

	select {}
}

func startProxy(svc Service, svcMap map[string]Service, alive int) {
	listenAddr := fmt.Sprintf(":%d", svc.ExposedPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}
	log.Printf("Proxying %s on %s → %s", svc.Name,
		listenAddr, net.JoinHostPort(svc.Host, strconv.Itoa(svc.Port)))

	for {
		client, err := ln.Accept()
		if err != nil {
			log.Printf("accept error on %s: %v", listenAddr, err)
			continue
		}
		go handleClient(client, svc, svcMap, alive)
	}
}

func handleClient(client net.Conn, svc Service, svcMap map[string]Service, alive int) {
	defer client.Close()

	deps := collectDeps(svc.Name, svcMap)
	startService(svc.Name, deps)

	// schedule stopService
	go func() {
		time.Sleep(time.Duration(alive) * time.Second)
		stopService(svc.Name, deps)
	}()

	remoteAddr := net.JoinHostPort(svc.Host, strconv.Itoa(svc.Port))
	remote, err := net.Dial("tcp", remoteAddr)
	if err != nil {
		log.Printf("dial %s: %v", remoteAddr, err)
		return
	}
	defer remote.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(remote, client)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, remote)
	}()
	wg.Wait()
}

func collectDeps(name string, svcMap map[string]Service) []string {
	seen := make(map[string]bool)
	var ordered []string

	var dfs func(string)
	dfs = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		s, ok := svcMap[n]
		if !ok {
			log.Printf("warning: unknown service %q", n)
			return
		}
		for _, d := range s.Dependencies {
			dfs(d)
			ordered = append(ordered, d)
		}
	}

	dfs(name)
	return ordered
}

func startService(name string, deps []string) {
	log.Printf("Starting %s with deps %v", name, deps)
	time.Sleep(2 * time.Second)
}

func stopService(name string, deps []string) {
	log.Printf("Stopping %s with deps %v", name, deps)
	time.Sleep(2 * time.Second)
}
