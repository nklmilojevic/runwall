package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/config"
	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/demo"
	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/hub"
	"github.com/nklmilojevic/runwall/internal/ingest"
	"github.com/nklmilojevic/runwall/internal/mcpserver"
	"github.com/nklmilojevic/runwall/internal/notify"
	"github.com/nklmilojevic/runwall/internal/scoring"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/syncer"
	"github.com/nklmilojevic/runwall/internal/web"
	"github.com/nklmilojevic/runwall/internal/webhook"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	demoMode := flag.Bool("demo", false, "serve sample data from an in-memory database; no GitHub App needed")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*demoMode); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(demoMode bool) error {
	cfg, err := config.Load(os.Getenv, !demoMode)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("LOG_LEVEL: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := cfg.DBPath
	if demoMode {
		dbPath = ":memory:"
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", dbPath, err)
	}
	defer st.Close()

	costs, err := cost.Load(cfg.CostRatesFile)
	if err != nil {
		return fmt.Errorf("COST_RATES_FILE: %w", err)
	}

	h := hub.New()
	mux := http.NewServeMux()
	srv := &web.Server{Store: st, Hub: h, StuckThreshold: cfg.StuckThreshold, Costs: costs, BaseURL: cfg.BaseURL, Log: log, Now: time.Now}

	if demoMode {
		if err := demo.Seed(ctx, st, time.Now()); err != nil {
			return fmt.Errorf("seed demo data: %w", err)
		}
		srv.Syncer, srv.Demo = demo.Syncer{At: time.Now()}, true
		log.Info("demo mode: serving sample data, GitHub is not contacted, everyone is signed in as 'demo'")
	} else {
		notifier, err := notify.New(cfg.Notifier, log)
		if err != nil {
			return err
		}
		app, err := ghapp.New(cfg.AppID, cfg.PrivateKey, cfg.APIURL)
		if err != nil {
			return fmt.Errorf("github app: %w", err)
		}
		admins := cfg.Admins
		if len(admins) == 0 {
			// Default to the App's owner.
			a, _, err := app.App().Apps.Get(ctx, "")
			if err != nil {
				return fmt.Errorf("read GitHub App (check GITHUB_APP_ID and the private key): %w", err)
			}
			admins = []string{a.GetOwner().GetLogin()}
		}
		authCfg := auth.Config{BaseURL: cfg.BaseURL, WebURL: cfg.WebURL, APIURL: cfg.APIURL,
			AllowedUsers: cfg.AllowedUsers, AllowedOwners: cfg.AllowedAccounts, Admins: admins}
		authCfg.ClientID, authCfg.ClientSecret, authCfg.Key = cfg.ClientID, cfg.ClientSecret, auth.ParseKey(cfg.SessionKey)
		authn, err := auth.New(authCfg, st, log)
		if err != nil {
			return err
		}
		scorer := &scoring.Scorer{GH: app, Store: st, Log: log, Now: time.Now}
		ing := &ingest.Ingester{Store: st, Hub: h, Notifier: notifier, Log: log, Since: time.Now()}
		syncCfg := syncer.Config{Interval: cfg.Reconcile, Backfill: cfg.Backfill, Retention: cfg.Retention,
			ColdInterval: cfg.ColdInterval, Concurrency: cfg.Concurrency, JobBackfill: cfg.JobBackfill,
			Scorer: scorer, AllowedAccounts: cfg.AllowedAccounts}
		sy := syncer.New(app, st, ing, syncCfg, log)
		wh := webhook.New(cfg.WebhookSecret, st, ing, sy, log)
		if len(cfg.AllowedAccounts) > 0 {
			wh.Allowed = syncCfg.Allowed
		}
		mux.Handle("POST /webhook", wh)
		mcpSrv := &mcpserver.Server{Store: st, Auth: authn, Logs: sy, Costs: costs, Log: log, Now: time.Now, Version: version}
		mux.Handle("/mcp", mcpSrv.Handler())
		srv.Syncer, srv.Scorer, srv.Auth = sy, scorer, authn
		go wh.Run(ctx)
		go sy.Run(ctx)
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := authn.PruneSessions(ctx); err != nil {
						log.Error("prune sessions", "err", err)
					}
				}
			}
		}()
		log.Info("sign-in", "callback", cfg.BaseURL+"/auth/callback", "admins", admins, "mcp", cfg.BaseURL+"/mcp")
	}
	srv.Routes(mux)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: /events is a long-lived stream.
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", "http://"+cfg.ListenAddr)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
