package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/config"
	"cfddns/internal/httpserver"
	"cfddns/internal/inspector"
	"cfddns/internal/install"
	"cfddns/internal/ip"
	"cfddns/internal/status"
	"cfddns/internal/updater"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cfddns ")
	// One P is enough: a cycle is a handful of sequential HTTPS calls, and
	// every extra P costs its own allocation cache, GC worker and OS thread,
	// which on a multi-core router is memory spent on parallelism never used.
	runtime.GOMAXPROCS(1)

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatal(err)
	}
	if err := cfg.ModeConflict(); err != nil {
		log.Fatal(err)
	}

	switch {
	case cfg.Uninstall:
		sys, err := install.Select(cfg.Init, install.HostProbe())
		if err != nil {
			log.Fatal(err)
		}
		if err := install.RequireRoot("uninstall"); err != nil {
			log.Fatal(err)
		}
		if err := install.Uninstall(sys, install.Exec); err != nil {
			log.Fatal(err)
		}
		log.Printf("uninstalled (%s)", sys.Name)
		return
	case cfg.Install:
		if err := cfg.ValidateInstall(); err != nil {
			log.Fatal(err)
		}
		sys, err := install.Select(cfg.Init, install.HostProbe())
		if err != nil {
			log.Fatal(err)
		}
		if err := install.RequireRoot("install"); err != nil {
			log.Fatal(err)
		}
		self, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		if err := install.Install(sys, cfg, self, install.Exec); err != nil {
			log.Fatal(err)
		}
		log.Printf("installed and started (%s); logs: %s", sys.Name, sys.LogHint)
		return
	}

	if err := cfg.ValidateRun(); err != nil {
		log.Fatal(err)
	}

	dns := &cloudflare.Client{Token: cfg.Token}
	if cfg.Inspect {
		insp := inspector.Inspector{
			DomainV4: cfg.DomainV4,
			DomainV6: cfg.DomainV6,
			DNS:      dns,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		results, ok := insp.Inspect(ctx)
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			OK      bool               `json:"ok"`
			Results []inspector.Result `json:"results"`
		}{OK: ok, Results: results}); err != nil {
			log.Fatal(err)
		}
		return
	}

	det := &ip.Detector{V6Iface: cfg.V6Iface}
	up := &updater.Updater{
		DomainV4: cfg.DomainV4,
		DomainV6: cfg.DomainV6,
		IP:       det,
		DNS:      dns,
	}
	var statusRep *status.Reporter
	if cfg.StatusFile != "" {
		statusRep = status.New(status.Config{
			Path:     cfg.StatusFile,
			Period:   cfg.Period,
			DomainV4: cfg.DomainV4,
			DomainV6: cfg.DomainV6,
		})
		if err := statusRep.Clear(); err != nil {
			log.Printf("status clear failed: %v", err)
		}
	}
	rep := newReporter(log.Printf)
	runOnce := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := up.Run(ctx)
		reportStatus(statusRep, results, log.Printf)
		rep.report(results)
		return results, ok
	}

	if cfg.Once {
		_, ok := runOnce(context.Background())
		if !ok {
			os.Exit(1)
		}
		return
	}
	if statusRep != nil {
		defer func() {
			if err := statusRep.Clear(); err != nil {
				log.Printf("status clear failed: %v", err)
			}
		}()
	}

	// Between cycles the daemon only waits, so everything a cycle allocated
	// (TLS handshakes, JSON, pooled connections) is garbage until the next
	// period. Dropping the connections and returning freed heap pages to the
	// OS right away keeps the resident size at the idle minimum on routers
	// and other small hosts instead of waiting for the lazy scavenger.
	runCycle := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := runOnce(ctx)
		det.CloseIdleConnections()
		dns.CloseIdleConnections()
		debug.FreeOSMemory()
		return results, ok
	}

	// The signal context exists before the first cycle so a SIGTERM during
	// it cancels the in-flight requests instead of killing the process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Daemon: first cycle logs errors but never exits solely for IP/DNS failures.
	runCycle(ctx)

	var wg sync.WaitGroup
	if cfg.Port > 0 {
		srv := &httpserver.Server{Passwd: cfg.Passwd, Run: runCycle}
		httpSrv := srv.HTTPServer(":" + strconv.Itoa(cfg.Port))
		wg.Add(2)
		go func() {
			defer wg.Done()
			log.Printf("http listening on :%d", cfg.Port)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// Keep the DDNS ticker running even if the HTTP port is unavailable.
				log.Printf("http server error: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-ctx.Done()
			shctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shctx)
		}()
	}

	t := time.NewTicker(cfg.Period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("shutdown")
			wg.Wait()
			return
		case <-t.C:
			runCycle(ctx)
		}
	}
}

func reportStatus(rep *status.Reporter, results []updater.Result, logf func(string, ...any)) {
	if rep == nil {
		return
	}
	if err := rep.Report(results); err != nil {
		logf("status report failed: %v", err)
	}
}
