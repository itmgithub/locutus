// Locutus is a web application for managing BorgBackup: repositories,
// clients, backup and prune schedules and file restores.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"itmon.nl/locutus/internal/borg"
	"itmon.nl/locutus/internal/config"
	"itmon.nl/locutus/internal/controllers"
	"itmon.nl/locutus/internal/db"
	"itmon.nl/locutus/internal/i18n"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/scheduler"
	"itmon.nl/locutus/internal/secrets"
	"itmon.nl/locutus/internal/tlsutil"
	"itmon.nl/locutus/internal/views"
)

// Version is the semantic version of the application.
var Version = "v1.0"

// Build is the short git commit id, injected at build time via -ldflags.
var Build = "dev"

func main() {
	cfg := config.Load()
	log.Printf("Locutus %s (Build: %s) starting", Version, Build)

	if len(cfg.ExtraCAs) > 0 {
		pool, n, err := tlsutil.LoadPool(cfg.ExtraCAs)
		if err != nil {
			log.Fatalf("load LOCUTUS_EXTRA_CA_CERTS: %v", err)
		}
		if err := tlsutil.InstallDefault(pool); err != nil {
			log.Fatalf("install LOCUTUS_EXTRA_CA_CERTS: %v", err)
		}
		log.Printf("trusting %d extra CA certificate(s) from %v", n, cfg.ExtraCAs)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	box, err := secrets.Open(cfg.SecretKey, cfg.SecretKeyFile)
	if err != nil {
		log.Fatalf("load secret key: %v", err)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()
	store := models.NewStore(database, box)

	translator, err := i18n.New(cfg.DefaultLanguage)
	if err != nil {
		log.Fatalf("load translations: %v", err)
	}

	renderer, err := views.New(translator, Version, Build)
	if err != nil {
		log.Fatalf("load templates: %v", err)
	}

	borgSvc, err := borg.NewService(borg.Options{
		Binary:     cfg.BorgBinary,
		BaseDir:    cfg.BorgBaseDir,
		KnownHosts: cfg.KnownHostsFile,
		Protected:  cfg.ProtectedPaths(),
	})
	if err != nil {
		log.Fatalf("prepare borg: %v", err)
	}
	if v, err := borgSvc.LocalVersion(context.Background()); err != nil {
		log.Printf("warning: borg is not available: %v", err)
	} else {
		log.Printf("using %s", v)
	}

	sched := scheduler.New(store, borgSvc)
	ctx, stopScheduler := context.WithCancel(context.Background())
	go sched.Run(ctx)

	app := controllers.New(cfg, store, renderer, translator, borgSvc, sched)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		log.Fatalf("load TLS certificate: %v", err)
	}

	go func() {
		var err error
		if tlsConfig != nil {
			srv.TLSConfig = tlsConfig
			log.Printf("listening on %s (https)", cfg.ListenAddr)
			err = srv.ListenAndServeTLS("", "")
		} else {
			log.Printf("listening on %s (http)", cfg.ListenAddr)
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	stopScheduler()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	sched.Wait(30 * time.Second)
}
