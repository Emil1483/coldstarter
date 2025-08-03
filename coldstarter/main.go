package main

import (
	"context"
	"errors"
	"io"
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
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

//—— CONFIG TYPES —————————————————————————————————————————————————————————

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

//—— SERVICE MANAGER ——————————————————————————————————————————————————————

type ServiceManager struct {
	deps     map[string][]string    // direct deps
	alive    map[string]bool        // desired state
	running  map[string]bool        // actual state
	timers   map[string]*time.Timer // per-service stop timers
	locks    map[string]*sync.Mutex // mutex guarding start/stop
	aliveDur time.Duration
	cfg      Config

	mu sync.Mutex // guards alive, running, timers
}

// NewServiceManager builds the graph and initializes maps
func NewServiceManager(cfg Config) *ServiceManager {
	sm := &ServiceManager{
		deps:     make(map[string][]string),
		alive:    make(map[string]bool),
		running:  make(map[string]bool),
		timers:   make(map[string]*time.Timer),
		locks:    make(map[string]*sync.Mutex),
		aliveDur: time.Duration(cfg.Settings.AliveSeconds) * time.Second,
		cfg:      cfg,
	}
	for _, svc := range cfg.Services {
		sm.deps[svc.Name] = svc.Dependencies
		sm.locks[svc.Name] = &sync.Mutex{}
		sm.alive[svc.Name] = false
		sm.running[svc.Name] = false
	}
	return sm
}

func (sm *ServiceManager) startDependenciesConcurrently(svcName string) {
	names := sm.dependencyOrder(svcName)

	var wg sync.WaitGroup
	wg.Add(len(names))

	for _, name := range names {
		// capture loop variable
		name := name
		go func() {
			defer wg.Done()
			sm.startIfNeeded(name)
		}()
	}

	// wait for all of them to finish
	wg.Wait()
}

// ServiceAlive is called on every incoming request to `svcName`
func (sm *ServiceManager) ServiceAlive(svcName string) {
	sm.mu.Lock()
	// 1) mark desired state
	if !sm.alive[svcName] {
		sm.alive[svcName] = true
	}
	// 2) reset its stop-timer
	if t, ok := sm.timers[svcName]; ok {
		t.Stop()
	}
	sm.timers[svcName] = time.AfterFunc(sm.aliveDur, func() {
		sm.ServiceDead(svcName)
	})
	sm.mu.Unlock()

	// 3) ensure svc+deps are running
	sm.startDependenciesConcurrently(svcName)

}

// ServiceDead is invoked once the timer fires for `svcName`
func (sm *ServiceManager) ServiceDead(svcName string) {
	sm.mu.Lock()
	// 1) mark dead
	sm.alive[svcName] = false
	sm.mu.Unlock()

	// 2) consider stopping svc + its deps (in reverse order)
	for i := len(sm.dependencyOrder(svcName)) - 1; i >= 0; i-- {
		name := sm.dependencyOrder(svcName)[i]
		// only stop if no alive service wants it
		if !sm.usedByAnyAlive(name) {
			sm.stopIfNeeded(name)
		}
	}
}

// dependencyOrder returns a list [dep1,depDeps…, svcName] in topological order
func (sm *ServiceManager) dependencyOrder(svcName string) []string {
	var order []string
	seen := map[string]bool{}
	var dfs func(string)
	dfs = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, d := range sm.deps[name] {
			dfs(d)
		}
		order = append(order, name)
	}
	dfs(svcName)
	return order
}

// usedByAnyAlive returns true if any currently-alive svc transitively depends on `name`
func (sm *ServiceManager) usedByAnyAlive(name string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for svc, isAlive := range sm.alive {
		if !isAlive {
			continue
		}
		// see if `name` appears in this svc's dep-closure (or is svc itself)
		for _, d := range sm.dependencyOrder(svc) {
			if d == name {
				return true
			}
		}
	}
	return false
}

func (sm *ServiceManager) getServiceFromName(name string) Service {
	for _, svc := range sm.cfg.Services {
		if svc.Name == name {
			return svc
		}
	}
	panic("unknown service: " + name)
}

// startIfNeeded calls startService(name) once, if not already running
func (sm *ServiceManager) startIfNeeded(name string) {
	sm.mu.Lock()
	already := sm.running[name]
	sm.mu.Unlock()
	if already {
		return
	}

	lock := sm.locks[name]
	lock.Lock()
	defer lock.Unlock()

	// re-check after acquiring
	sm.mu.Lock()
	if sm.running[name] {
		sm.mu.Unlock()
		return
	}
	sm.mu.Unlock()

	svc := sm.getServiceFromName(name)
	startService(svc)

	sm.mu.Lock()
	sm.running[name] = true
	sm.mu.Unlock()
}

// stopIfNeeded calls stopService(name) once, if currently running
func (sm *ServiceManager) stopIfNeeded(name string) {
	sm.mu.Lock()
	running := sm.running[name]
	sm.mu.Unlock()
	if !running {
		return
	}

	lock := sm.locks[name]
	lock.Lock()
	defer lock.Unlock()

	// re-check after locking
	sm.mu.Lock()
	if !sm.running[name] {
		sm.mu.Unlock()
		return
	}
	sm.mu.Unlock()

	stopService(name)

	sm.mu.Lock()
	sm.running[name] = false
	sm.mu.Unlock()
}

//—— STUBBED SERVICE LIFECYCLE HOOKS ——————————————————————————————————————

func startService(svc Service) {
	name := svc.Name
	log.Printf("🔼 startService(%q)", name)
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Printf("Error creating Docker client: %v\n", err)
	}
	cli.NegotiateAPIVersion(ctx)

	// Inspect the container by name to get its ID
	ctrJSON, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		log.Printf("Error inspecting container '%s': %v\n", name, err)
		return
	}

	cli.NegotiateAPIVersion(ctx)

	if err := cli.ContainerStart(ctx, ctrJSON.ID, container.StartOptions{}); err != nil {
		log.Printf("Error starting container '%s': %v\n", name, err)
		return
	}
	log.Printf("Container '%s' started (ID: %s)\n", name, ctrJSON.ID)

	url := "http://" + svc.Host + ":" + strconv.Itoa(svc.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			log.Printf("Timeout waiting for server at %s\n", url)
			return
		default:
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				log.Println("🎉 " + svc.Name + " is up")
				return
			}

			if errors.Is(err, io.EOF) {
				log.Println("🎉 Empty reply (EOF) received; " + svc.Name + " is up!")
				return
			}

			log.Printf("Waiting for service at %s… (%v)\n", url, err)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func stopService(name string) {
	log.Printf("🔽 stopService(%q)", name)
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Printf("Error creating Docker client: %v\n", err)
		return
	}
	cli.NegotiateAPIVersion(ctx)
	// Inspect the container by name to get its ID
	ctrJSON, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		log.Printf("Error inspecting container '%s': %v\n", name, err)
		return
	}
	cli.NegotiateAPIVersion(ctx)

	if err := cli.ContainerStop(ctx, ctrJSON.ID, container.StopOptions{}); err != nil {
		log.Printf("Error stopping container '%s': %v\n", name, err)
		return
	}
	log.Printf("Container '%s' stopped (ID: %s)\n", name, ctrJSON.ID)
}

//—— HTTP PROXY SETUP —————————————————————————————————————————————————

func main() {
	// 1) load config
	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		log.Fatal("CONFIG not set")
	}
	var cfg Config
	if _, err := toml.DecodeFile(cfgPath, &cfg); err != nil {
		log.Fatalf("decode TOML: %v", err)
	}

	for _, svc := range cfg.Services {
		for _, dep := range svc.Dependencies {
			var found bool = false
			for _, other := range cfg.Services {
				if other.Name == dep {
					found = true
					break
				}
			}
			if !found {
				panic("service " + svc.Name + " depends on unknown service " + dep)
			}
		}
	}

	// 2) build manager
	mgr := NewServiceManager(cfg)

	// 3) launch one HTTP reverse-proxy per service
	for _, svc := range cfg.Services {
		go func(s Service) {
			proxy := httputil.NewSingleHostReverseProxy(&url.URL{
				Scheme: "http",
				Host:   net.JoinHostPort(s.Host, strconv.Itoa(s.Port)),
			})

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// mark activity
				mgr.ServiceAlive(s.Name)
				// proxy the request
				proxy.ServeHTTP(w, r)
			})

			addr := ":" + strconv.Itoa(s.ExposedPort)
			log.Printf("➡️  [%s] http proxy on %s → %s", s.Name, addr, s.Host)
			log.Fatal(http.ListenAndServe(addr, handler))
		}(svc)
	}

	select {} // never exit
}
