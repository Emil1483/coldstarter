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

// stopTimers holds one timer per service name
var (
	stopTimers   = make(map[string]*time.Timer)
	stopTimersMu sync.Mutex
)

func main() {
	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		log.Fatal("CONFIG env var not set")
	}

	var cfg Config
	if _, err := toml.DecodeFile(cfgPath, &cfg); err != nil {
		log.Fatalf("failed to decode TOML: %v", err)
	}

	svcMap := make(map[string]Service, len(cfg.Services))
	for _, s := range cfg.Services {
		svcMap[s.Name] = s
		go startProxy(s, svcMap, cfg.Settings.AliveSeconds)
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
			log.Printf("accept %s: %v", listenAddr, err)
			continue
		}
		go handleClient(client, svc, svcMap, alive)
	}
}

func handleClient(client net.Conn, svc Service, svcMap map[string]Service, alive int) {
	defer client.Close()

	// 1) Gather deps and start (always)
	log.Printf("Handling client for %s", svc.Name)
	deps := collectDeps(svc.Name, svcMap)
	startService(svc.Name, deps)

	// 2) Reset the stop timer for this service
	resetStopTimer(svc.Name, deps, alive)

	// 3) Proxy the connection
	log.Printf("Proxying connection for %s to %s:%d", svc.Name, svc.Host, svc.Port)
	remoteAddr := net.JoinHostPort(svc.Host, strconv.Itoa(svc.Port))
	remote, err := net.Dial("tcp", remoteAddr)
	if err != nil {
		log.Printf("dial %s: %v", remoteAddr, err)
		return
	}
	defer remote.Close()

	log.Printf("Connected to remote %s", remoteAddr)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(remote, client) }()
	go func() { defer wg.Done(); io.Copy(client, remote) }()
	log.Printf("Started proxying for %s", svc.Name)
	wg.Wait()
	log.Printf("Finished proxying for %s", svc.Name)
}

// resetStopTimer stops any existing timer for `name`, then makes a new one.
func resetStopTimer(name string, deps []string, alive int) {
	log.Printf("Resetting stop timer for %s with deps %v", name, deps)
	stopTimersMu.Lock()
	defer stopTimersMu.Unlock()

	if t, exists := stopTimers[name]; exists {
		// Stop and drain
		if !t.Stop() {
			<-t.C
		}
	}

	// Create a new timer that will call stopService after `alive` seconds
	stopTimers[name] = time.AfterFunc(time.Duration(alive)*time.Second, func() {
		stopService(name, deps)
	})
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
