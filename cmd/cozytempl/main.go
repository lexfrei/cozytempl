// Package main is the entry point for cozytempl.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
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

// Version and Revision are stamped at build time via
// -ldflags "-X main.Version=... -X main.Revision=..." — see the
// Containerfile build stage. Left at the "development" default
// for local `go build` / `go run` so nothing crashes when the
// ldflags are absent. Surfaced in the startup log line and
// available to any future /version endpoint or profile-page
// footer that wants to display the running build.
var (
	Version  = "development"
	Revision = "development"
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

	log.Info("cozytempl starting", "version", Version, "revision", Revision)

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

	tenantSvc := k8s.NewTenantService(k8sCfg, cfg.AuthMode)
	schemaSvc := k8s.NewSchemaService(k8sCfg, cfg.AuthMode)
	appSvc := k8s.NewApplicationService(k8sCfg, schemaSvc, cfg.AuthMode)
	usageSvc := k8s.NewUsageService(k8sCfg, cfg.AuthMode)
	eventSvc := k8s.NewEventService(k8sCfg, cfg.AuthMode)
	logSvc := k8s.NewLogService(k8sCfg, cfg.AuthMode)
	capiSvc := k8s.NewCAPIService(k8sCfg, cfg.AuthMode)

	// Watcher runs as its OWN ServiceAccount so the main cozytempl
	// pod SA can stay at zero k8s permissions in passthrough and
	// byok modes. If COZYTEMPL_WATCHER_KUBECONFIG points at a
	// kubeconfig file (typically mounted from a projected SA token
	// by the Helm chart), we use that. Otherwise the watcher
	// reuses the process's primary kubeconfig — fine in
	// impersonation-legacy and dev modes where the main SA
	// already has list/watch rights.
	watcherCfg, err := loadWatcherKubeConfig(k8sCfg)
	if err != nil {
		return fmt.Errorf("loading watcher kubeconfig: %w", err)
	}

	watcher := k8s.NewWatcher(watcherCfg, log)

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
		CAPISvc:   capiSvc,
		BaseCfg:   k8sCfg,
		Audit:     auditLog,
		I18n:      i18nBundle,
		AuthMode:  cfg.AuthMode,
		DevMode:   cfg.DevMode,
		Log:       log,
	})

	routerCfg := &api.RouterConfig{
		TenantHandler:         api.NewTenantHandler(tenantSvc, log),
		AppHandler:            api.NewApplicationHandler(appSvc, log),
		SchemaHandler:         api.NewSchemaHandler(schemaSvc, log),
		SSEHandler:            api.NewSSEHandler(watcher, k8sCfg, cfg.AuthMode, log),
		PageHandler:           pageHandler,
		I18n:                  i18nBundle,
		StaticFS:              static.FS,
		Log:                   log,
		AuthMode:              cfg.AuthMode,
		DevMode:               cfg.DevMode,
		DevUsername:           "dev-admin",
		TrustForwardedHeaders: cfg.TrustForwardedHeaders,
	}

	switch cfg.AuthMode {
	case config.AuthModeDev:
		log.Warn("running in dev mode — authentication disabled")

	case config.AuthModeBYOK, config.AuthModeToken:
		// Both modes skip OIDC entirely; the upload / paste flow is
		// the only authentication surface. Session cookie encrypts
		// the uploaded kubeconfig (BYOK) or the pasted Bearer token
		// (Token). The token probe needs k8sCfg to build a rest.Config
		// pointing at the same apiserver cozytempl itself talks to.
		sessionStore := auth.NewSessionStore(cfg.SessionSecret)
		routerCfg.AuthHandler = auth.NewHandler(nil, sessionStore, log, cfg.AuthMode, k8sCfg)
		routerCfg.SessionStore = sessionStore

	case config.AuthModePassthrough, config.AuthModeImpersonationLegacy:
		issuerForBackend := cfg.InternalIssuerURL()

		oidcProvider, oidcErr := auth.NewOIDCProvider(ctx,
			issuerForBackend, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURL)
		if oidcErr != nil {
			return fmt.Errorf("initializing OIDC: %w", oidcErr)
		}

		sessionStore := auth.NewSessionStore(cfg.SessionSecret)
		routerCfg.AuthHandler = auth.NewHandler(oidcProvider, sessionStore, log, cfg.AuthMode, k8sCfg)
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

	pprofSrv := startDebugPprofServer(ctx, log, cfg.DebugPprofAddr)

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

	if pprofSrv != nil {
		shutdownErr := pprofSrv.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			log.Warn("debug pprof server shutdown", "error", shutdownErr)
		}
	}

	return srv.Shutdown(shutdownCtx) //nolint:wrapcheck // top-level, error logged by caller
}

// startDebugPprofServer starts a dedicated HTTP server exposing the
// standard net/http/pprof handlers when COZYTEMPL_DEBUG_PPROF_ADDR is
// non-empty. The listener is separate from the main public server so
// pprof is NEVER reachable on the production port — an operator
// typically points this at "localhost:6060" and accesses it through a
// kubectl port-forward.
//
// pprof handlers are registered on a local mux rather than importing
// net/http/pprof purely for its init side-effects, which would
// contaminate http.DefaultServeMux and leak debug endpoints onto any
// other caller that reuses that mux.
//
// Returns nil when the endpoint is disabled so the caller can safely
// dereference the result for shutdown.
func startDebugPprofServer(ctx context.Context, log *slog.Logger, addr string) *http.Server {
	if addr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readTimeout,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	log.Warn("debug pprof server enabled",
		"addr", addr,
		"note", "do not expose on a network an attacker can reach")

	go func() {
		listenErr := srv.ListenAndServe()
		if listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			log.Error("debug pprof server error", "error", listenErr)
		}
	}()

	return srv
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

// watcherKubeconfigEnv names the env var the Helm chart sets to point
// at a projected-token kubeconfig for the cozytempl-watcher SA. In
// production that file is mounted read-only in the pod filesystem;
// locally it is usually unset and the watcher falls back to fallback.
const watcherKubeconfigEnv = "COZYTEMPL_WATCHER_KUBECONFIG"

// loadWatcherKubeConfig returns the rest.Config that the HelmRelease
// watcher should use. In production, the Helm chart creates a
// dedicated cozytempl-watcher ServiceAccount whose kubeconfig is
// mounted and pointed at via COZYTEMPL_WATCHER_KUBECONFIG — this
// lets the main cozytempl SA stay at zero RBAC. When the env var
// is unset we reuse fallback (the process's primary kubeconfig),
// which is the right behaviour for impersonation-legacy and dev
// modes, and for local development with a single credential.
func loadWatcherKubeConfig(fallback *rest.Config) (*rest.Config, error) {
	path := os.Getenv(watcherKubeconfigEnv)
	if path == "" {
		return fallback, nil
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("loading watcher kubeconfig from %q: %w", path, err)
	}

	return cfg, nil
}
