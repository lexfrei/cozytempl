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
	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/handler"
	"github.com/lexfrei/cozytempl/internal/i18n"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/tracing"
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
		// Log before returning so the operator sees the specific failure
		// reason in structured output, not just the generic "fatal" line
		// main() prints when run() returns.
		log.Error("loading configuration", "error", err)

		return fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// OpenTelemetry is opt-in: if OTEL_EXPORTER_OTLP_ENDPOINT is
	// unset, tracing.Init returns a no-op and the global tracer
	// provider stays at its zero-cost default. No config means no
	// span pipeline overhead — only operators who actually wire a
	// collector pay for the spans they read.
	tracingShutdown, err := tracing.Init(ctx)
	if err != nil {
		log.Error("initialising tracing", "error", err)

		return fmt.Errorf("initialising tracing: %w", err)
	}

	defer func() {
		if shutdownErr := tracingShutdown(context.Background()); shutdownErr != nil {
			log.Error("tracing shutdown", "error", shutdownErr)
		}
	}()

	k8sCfg, err := loadKubeConfig()
	if err != nil {
		return err
	}

	tenantSvc := k8s.NewTenantService(k8sCfg)
	schemaSvc := k8s.NewSchemaService(k8sCfg)
	appSvc := k8s.NewApplicationService(k8sCfg, schemaSvc)
	usageSvc := k8s.NewUsageService(k8sCfg)
	eventSvc := k8s.NewEventService(k8sCfg)
	logSvc := k8s.NewLogService(k8sCfg)
	watcher := k8s.NewWatcher(k8sCfg, log)

	err = watcher.Start(ctx)
	if err != nil {
		log.Warn("failed to start watcher, SSE will be unavailable", "error", err)
	}

	// Audit events share the same JSON log stream as everything
	// else. Pod logs are the append-only store; forward them to
	// Loki / ELK for long-term retention. Swap in a Kafka producer
	// here if the deployment eventually needs a dedicated sink.
	auditLog := audit.NewSlogLogger(log)

	// i18n bundle: every TOML file under internal/i18n/locales/
	// is loaded into a go-i18n bundle. Failure here is fatal
	// because a broken translation file silently degrades the
	// UI — better to crash at startup than render [message.id]
	// to real users.
	i18nBundle, err := i18n.NewBundle()
	if err != nil {
		return fmt.Errorf("loading i18n bundle: %w", err)
	}

	pageHandler := handler.NewPageHandler(handler.PageHandlerDeps{
		TenantSvc: tenantSvc,
		AppSvc:    appSvc,
		SchemaSvc: schemaSvc,
		UsageSvc:  usageSvc,
		EventSvc:  eventSvc,
		LogSvc:    logSvc,
		Audit:     auditLog,
		I18n:      i18nBundle,
		DevMode:   cfg.DevMode,
		Log:       log,
	})

	routerCfg := &api.RouterConfig{
		TenantHandler: api.NewTenantHandler(tenantSvc, log),
		AppHandler:    api.NewApplicationHandler(appSvc, log),
		SchemaHandler: api.NewSchemaHandler(schemaSvc, log),
		SSEHandler:    api.NewSSEHandler(watcher, k8sCfg, log),
		PageHandler:   pageHandler,
		I18n:          i18nBundle,
		StaticFS:      static.FS,
		Log:           log,
		AuthMode:      cfg.AuthMode,
		DevMode:       cfg.DevMode,
		DevUsername:   "dev-admin",
	}

	switch cfg.AuthMode {
	case config.AuthModeDev:
		log.Warn("running in dev mode — authentication disabled")

	case config.AuthModeBYOK:
		// BYOK mode skips OIDC entirely; the upload flow is the
		// only authentication surface. Session cookie encrypts the
		// uploaded kubeconfig.
		routerCfg.SessionStore = auth.NewSessionStore(cfg.SessionSecret)

	case config.AuthModePassthrough, config.AuthModeImpersonationLegacy:
		issuerForBackend := cfg.InternalIssuerURL()

		oidcProvider, oidcErr := auth.NewOIDCProvider(ctx,
			issuerForBackend, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL)
		if oidcErr != nil {
			return fmt.Errorf("initializing OIDC: %w", oidcErr)
		}

		sessionStore := auth.NewSessionStore(cfg.SessionSecret)
		routerCfg.AuthHandler = auth.NewHandler(oidcProvider, sessionStore, log)
		routerCfg.SessionStore = sessionStore
		routerCfg.OIDCProvider = oidcProvider

		if cfg.AuthMode == config.AuthModeImpersonationLegacy {
			log.Warn("impersonation-legacy auth mode is deprecated; migrate to passthrough")
		}
	}

	router := api.Router(routerCfg)

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
