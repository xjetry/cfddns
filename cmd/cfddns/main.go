package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
		log.Fatal(err)
	}
	if err := cfg.ModeConflict(); err != nil {
		log.Fatal(err)
	}

	switch {
	case cfg.Uninstall:
		if err := install.Uninstall(); err != nil {
			log.Fatal(err)
		}
		log.Println("uninstalled")
		return
	case cfg.Install:
		if err := cfg.ValidateInstall(); err != nil {
			log.Fatal(err)
		}
		self, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		if err := install.Install(cfg, self); err != nil {
			log.Fatal(err)
		}
		log.Println("installed and started")
		return
	}

	if err := cfg.ValidateRun(); err != nil {
		log.Fatal(err)
	}

	up := &updater.Updater{
		DomainV4: cfg.DomainV4,
		DomainV6: cfg.DomainV6,
		IP:       &ip.Detector{},
		DNS:      &cloudflare.Client{Token: cfg.Token},
	}

	runOnce := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := up.Run(ctx)
		for _, r := range results {
			if r.Error != "" {
				log.Printf("%s %s %s: %s", r.Family, r.Domain, r.Action, r.Error)
			} else {
				log.Printf("%s %s %s ip=%s", r.Family, r.Domain, r.Action, r.IP)
			}
		}
		return results, ok
	}

	if cfg.Once {
		_, ok := runOnce(context.Background())
		if !ok {
			os.Exit(1)
		}
		return
	}

	// Daemon: first cycle logs errors but never exits solely for IP/DNS failures.
	runOnce(context.Background())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Port > 0 {
		srv := &httpserver.Server{Passwd: cfg.Passwd, Run: runOnce}
		httpSrv := &http.Server{Addr: ":" + strconv.Itoa(cfg.Port), Handler: srv.Handler()}
		go func() {
			log.Printf("http listening on :%d", cfg.Port)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// Keep the DDNS ticker running even if the HTTP port is unavailable.
				log.Printf("http server error: %v", err)
			}
		}()
		go func() {
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
			return
		case <-t.C:
			runOnce(ctx)
		}
	}
}
