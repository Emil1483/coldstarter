package main

import (
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	// startLocks holds one buffered‐1 channel per service for TryLock
	startLocks = make(map[string]chan struct{})
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

	// initialize maps
	svcMap := make(map[string]Service, len(cfg.Services))
	for _, s := range cfg.Services {
		svcMap[s.Name] = s
		// make a channel of capacity 1 for each service
		startLocks[s.Name] = make(chan struct{}, 1)
	}

	for _, s := range cfg.Services {
		go startProxyHTTP(s, svcMap, cfg.Settings.AliveSeconds)
	}

	select {}
}

type serviceControl struct {
	mu        sync.Mutex
	isRunning bool
	timer     *time.Timer
	deps      []string
}

// controls maps service name → its control struct
var controls = map[string]*serviceControl{}

func startProxyHTTP(svc Service, svcMap map[string]Service, alive int) {
	// prepare control for this service
	ctrl := &serviceControl{deps: collectDeps(svc.Name, svcMap)}
	controls[svc.Name] = ctrl

	// target backend URL
	targetURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(svc.Host, strconv.Itoa(svc.Port)),
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// our handler wraps the ReverseProxy
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1) startService _synchronously_ on first hit after a stop
		ctrl.mu.Lock()
		if !ctrl.isRunning {
			ctrl.isRunning = true
			ctrl.mu.Unlock()

			log.Printf("→ startService(%q)", svc.Name)
			startService(svc.Name, ctrl.deps)
		} else {
			ctrl.mu.Unlock()
		}

		// 2) reset (or create) the stop timer
		ctrl.mu.Lock()
		if ctrl.timer == nil {
			// first time: create
			ctrl.timer = time.AfterFunc(time.Duration(alive)*time.Second, func() {
				log.Printf("← stopService(%q)", svc.Name)
				stopService(svc.Name, ctrl.deps)

				ctrl.mu.Lock()
				ctrl.isRunning = false
				ctrl.mu.Unlock()
			})
		} else {
			// reuse: stop old and start new
			ctrl.timer.Stop()
			ctrl.timer.Reset(time.Duration(alive) * time.Second)
		}
		ctrl.mu.Unlock()

		// 3) now proxy the request
		proxy.ServeHTTP(w, r)
	})

	listenAddr := ":" + strconv.Itoa(svc.ExposedPort)
	log.Printf("HTTP proxy %q listening on %s → %s", svc.Name, listenAddr, targetURL.Host)
	if err := http.ListenAndServe(listenAddr, handler); err != nil {
		log.Fatalf("proxy %q failed: %v", svc.Name, err)
	}
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
	log.Printf("%s started", name)
}

func stopService(name string, deps []string) {
	log.Printf("Stopping %s with deps %v", name, deps)
	time.Sleep(2 * time.Second)
	log.Printf("%s stopped", name)
}
