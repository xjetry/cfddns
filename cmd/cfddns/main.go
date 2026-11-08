package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
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
		if err := runInspect(ctx, &insp, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	det := &ip.Detector{V6Iface: cfg.V6Iface}
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
	runOnce := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := up.Run(ctx)
		// Unlike the log reporter, status also records a lock-wait failure:
		// no detection ran, but the attempted execution belongs in the snapshot.
		reportStatus(statusRep, results, log.Printf)
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
	cycle := func(ctx context.Context) ([]updater.Result, bool) {
		results, ok := runOnce(ctx)
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

// runInspect writes the {ok,results} envelope; failed family queries are
// reported inside it, so only an encoding or write failure is returned.
func runInspect(ctx context.Context, insp *inspector.Inspector, w io.Writer) error {
	results, ok := insp.Inspect(ctx)
	return json.NewEncoder(w).Encode(struct {
		OK      bool               `json:"ok"`
		Results []inspector.Result `json:"results"`
	}{OK: ok, Results: results})
}

func reportStatus(rep *status.Reporter, results []updater.Result, logf func(string, ...any)) {
	if rep == nil {
		return
	}
	if err := rep.Report(results); err != nil {
		logf("status report failed: %v", err)
	}
}
