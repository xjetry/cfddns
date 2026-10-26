package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/config"
	"cfddns/internal/httpserver"
	"cfddns/internal/install"
	"cfddns/internal/ip"
	"cfddns/internal/updater"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cfddns ")

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

	up := &updater.Updater{
		DomainV4: cfg.DomainV4,
		DomainV6: cfg.DomainV6,
		IP:       &ip.Detector{V6Iface: cfg.V6Iface},
		DNS:      &cloudflare.Client{Token: cfg.Token},
	}
	rep := newReporter(log.Printf)
	runOnce := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := up.Run(ctx)
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

	// The signal context exists before the first cycle so a SIGTERM during
	// it cancels the in-flight requests instead of killing the process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Daemon: first cycle logs errors but never exits solely for IP/DNS failures.
	runOnce(ctx)

	var wg sync.WaitGroup
	if cfg.Port > 0 {
		srv := &httpserver.Server{Passwd: cfg.Passwd, Run: runOnce}
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
			runOnce(ctx)
		}
	}
}
