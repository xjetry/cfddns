package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
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
	"cfddns/internal/install"
	"cfddns/internal/ip"
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

	det := &ip.Detector{V6Iface: cfg.V6Iface}
	dns := &cloudflare.Client{Token: cfg.Token}
	rep := newReporter(log.Printf)
	up := &updater.Updater{
		DomainV4: cfg.DomainV4,
		DomainV6: cfg.DomainV6,
		IP:       det,
		DNS:      dns,
		// The reporter keeps per-family state and is fed from the ticker
		// and from every HTTP request goroutine; Observe runs it inside the
		// cycle lock, which is what makes that safe.
		Observe: rep.report,
	}

	if cfg.Once {
		os.Exit(once(up))
	}

	// Between cycles the daemon only waits, so everything a cycle allocated
	// (TLS handshakes, JSON, pooled connections) is garbage until the next
	// period. Dropping the connections and returning freed heap pages to the
	// OS right away keeps the resident size at the idle minimum on routers
	// and other small hosts instead of waiting for the lazy scavenger.
	cycle := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := up.Run(ctx)
		det.CloseIdleConnections()
		dns.CloseIdleConnections()
		debug.FreeOSMemory()
		return results, ok
	}

	d := &daemon{period: cfg.Period, cycle: cycle, logf: log.Printf}
	if cfg.Port > 0 {
		srv := &httpserver.Server{Passwd: cfg.Passwd, Run: cycle}
		d.http = srv.HTTPServer(":" + strconv.Itoa(cfg.Port))
	}

	// The signal context exists before the first cycle so a SIGTERM during
	// it cancels the in-flight requests instead of killing the process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d.run(ctx)
}

// once runs a single cycle and returns the process exit code: 0 when every
// configured family ended in created, updated or unchanged, 1 otherwise.
func once(up *updater.Updater) int {
	if _, ok := up.Run(context.Background()); !ok {
		return 1
	}
	return 0
}

// daemon is the long-running mode: one cycle at start, then one per period,
// with the optional HTTP trigger running the same cycle function on request
// goroutines. Cycle and HTTP failures are logged and never end the daemon;
// only ctx does.
type daemon struct {
	period time.Duration
	cycle  func(context.Context) ([]updater.Result, bool)
	// http, when non-nil, is served on its Addr; nil means -port is unset.
	http *http.Server
	logf func(format string, args ...any)
}

// run returns once ctx is done and the HTTP server, if it was listening,
// has shut down.
func (d *daemon) run(ctx context.Context) {
	// First cycle before anything else, so a start right after -install
	// publishes the address at once; its errors are logged like any other
	// cycle's and do not stop the daemon.
	d.cycle(ctx)

	var wg sync.WaitGroup
	if d.http != nil {
		// Bind before announcing: "listening" is only ever logged for a port
		// that is open, and a bind failure leaves the ticker running.
		ln, err := net.Listen("tcp", d.http.Addr)
		if err != nil {
			d.logf("http listen %s: %v", d.http.Addr, err)
		} else {
			d.logf("http listening on %s", ln.Addr())
			wg.Add(2)
			go func() {
				defer wg.Done()
				if err := d.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					d.logf("http server error: %v", err)
				}
			}()
			go func() {
				defer wg.Done()
				<-ctx.Done()
				shctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = d.http.Shutdown(shctx)
			}()
		}
	}

	t := time.NewTicker(d.period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.logf("shutdown")
			wg.Wait()
			return
		case <-t.C:
			d.cycle(ctx)
		}
	}
}
