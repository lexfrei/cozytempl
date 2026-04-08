// Package main is the entry point for cozytempl.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lexfrei/cozytempl/internal/api"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/static"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	shutdownTimeout = 10 * time.Second
	readTimeout     = 15 * time.Second
	writeTimeout    = 60 * time.Second
	idleTimeout     = 120 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	k8sCfg, err := loadKubeConfig()
	if err != nil {
		return err
	}

	oidcProvider, err := auth.NewOIDCProvider(ctx,
		cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL)
	if err != nil {
		return fmt.Errorf("initializing OIDC: %w", err)
	}

	sessionStore := auth.NewSessionStore(cfg.SessionSecret)
	authHandler := auth.NewHandler(oidcProvider, sessionStore, log)

	tenantSvc := k8s.NewTenantService(k8sCfg)
	schemaSvc := k8s.NewSchemaService(k8sCfg)
	appSvc := k8s.NewApplicationService(k8sCfg, schemaSvc)
	watcher := k8s.NewWatcher(k8sCfg, log)

	err = watcher.Start(ctx)
	if err != nil {
		log.Warn("failed to start watcher, SSE will be unavailable", "error", err)
	}

	tenantHandler := api.NewTenantHandler(tenantSvc, log)
	appHandler := api.NewApplicationHandler(appSvc, log)
	schemaHandler := api.NewSchemaHandler(schemaSvc, log)
	sseHandler := api.NewSSEHandler(watcher, log)

	shellHandler := func(writer http.ResponseWriter, req *http.Request) {
		usr := auth.UserFromContext(req.Context())
		username := ""

		if usr != nil {
			username = usr.Username
		}

		writer.Header().Set("Content-Type", "text/html; charset=utf-8")

		renderErr := view.Shell(username).Render(req.Context(), writer)
		if renderErr != nil {
			log.Error("rendering shell", "error", renderErr)
		}
	}

	router := api.Router(
		authHandler, sessionStore,
		tenantHandler, appHandler, schemaHandler, sseHandler,
		static.FS, shellHandler, log,
	)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	go func() {
		log.Info("starting server", "addr", cfg.ListenAddr)

		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			log.Error("server error", "error", listenErr)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down server")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	return srv.Shutdown(shutdownCtx) //nolint:wrapcheck // top-level, error logged by caller
}

func loadKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restCfg, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err //nolint:wrapcheck // caller wraps with context
	}

	return restCfg, nil
}
