// Command ss-server receives agent reports and serves the status dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lightmani/serverstatus/internal/server"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file (reloaded on SIGHUP)")
	flag.Parse()

	logger := log.New(os.Stderr, "ss-server: ", log.LstdFlags|log.Lmsgprefix)
	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		logger.Fatal(err)
	}
	if len(cfg.Clients) == 0 {
		logger.Print("warning: no clients configured; all reports will be rejected")
	}

	srv := server.New(cfg, logger)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          logger,
	}

	go func() {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		for range hup {
			next, err := server.LoadConfig(*configPath)
			if err != nil {
				logger.Printf("reload failed, keeping current config: %v", err)
				continue
			}
			srv.Reload(next)
			logger.Printf("config reloaded: %d clients", len(next.Clients))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s (%d clients)", cfg.Listen, len(cfg.Clients))
	if cfg.TLSCert != "" {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}
