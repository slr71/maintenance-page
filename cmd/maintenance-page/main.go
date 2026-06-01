package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/cyverse-de/maintenance-page/internal/k8s"
	"github.com/cyverse-de/maintenance-page/internal/server"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sirupsen/logrus"
)

// isDocumentNavigation reports whether a request is a top-level browser page load
// (vs. an XHR/fetch API call). It relies on the Sec-Fetch-* headers that modern
// browsers send, falling back to the Accept header for clients that omit them.
// We use this to decide whether an unmatched path should receive the maintenance
// HTML page (navigation) or a 503 (API call), without needing a hardcoded path list.
func isDocumentNavigation(r *http.Request) bool {
	mode := r.Header.Get("Sec-Fetch-Mode")
	dest := r.Header.Get("Sec-Fetch-Dest")
	if mode != "" || dest != "" {
		return mode == "navigate" || dest == "document" || dest == "iframe" || dest == "frame"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveMaintenanceResponse serves the maintenance HTML page for browser navigations
// and a 503 for XHR/API calls. The 503 carries the X-Maintenance-Mode marker so the
// DE UI can distinguish gateway maintenance from an incidental backend 503 and force
// a hard reload (which lands the user on the maintenance page).
func serveMaintenanceResponse(c echo.Context, defaultPath string) error {
	if isDocumentNavigation(c.Request()) {
		return c.File(defaultPath)
	}
	h := c.Response().Header()
	h.Set("X-Maintenance-Mode", "true")
	h.Set("Retry-After", "300")
	h.Set("Cache-Control", "no-store")
	return c.JSON(http.StatusServiceUnavailable, map[string]any{
		"error_code":  "ERR_MAINTENANCE",
		"maintenance": true,
		"message":     "The Discovery Environment is currently under maintenance.",
	})
}

// maintenanceMiddleware serves static files from a directory for any base URL path. It works by inspecting the URL
// path. The base name is extracted from the URL; if a file with that base name exists in the directory then the
// contents of that file are returned (so the maintenance page's own assets always load). Otherwise the request is
// served by serveMaintenanceResponse, which returns the maintenance page for browser navigations and a 503 for
// XHR/API calls.
// The absDir parameter must be an absolute path to the maintenance page directory.
func maintenanceMiddleware(absDir string) echo.MiddlewareFunc {
	// Matches the last path segment in a URL path (e.g., "/foo" in "/a/b/foo").
	basenameRegex := regexp.MustCompile(`/[^/]*$`)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			urlPath := c.Request().URL.Path
			defaultPath := filepath.Join(absDir, "maintenance_index.html")

			// Extract the basename from the URL path, falling back to the default response if it's empty.
			basename := strings.TrimPrefix(basenameRegex.FindString(urlPath), "/")
			if basename == "" {
				return serveMaintenanceResponse(c, defaultPath)
			}

			// Resolve the full path and verify it's still within the maintenance page directory.
			filePath := filepath.Join(absDir, filepath.Clean(basename))
			resolved, err := filepath.Abs(filePath)
			if err != nil || !strings.HasPrefix(resolved, absDir+string(os.PathSeparator)) {
				return serveMaintenanceResponse(c, defaultPath)
			}

			// Return the file if it exists. This always serves the maintenance page's own assets with a 200,
			// regardless of request type.
			if info, err := os.Stat(resolved); err == nil && !info.IsDir() {
				return c.File(resolved)
			}

			// Fall back to the default response.
			return serveMaintenanceResponse(c, defaultPath)
		}
	}
}

func setupEcho(log *logrus.Logger) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true,
		LogURI:    true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			log.WithFields(logrus.Fields{
				"URI":    v.URI,
				"status": v.Status,
			}).Info("request")
			return nil
		},
	}))
	e.Use(middleware.Recover())
	return e
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func main() {
	log := logrus.New()

	var (
		maintenancePageService = flag.String("maintenance-page-service", getEnv("MAINTENANCE_PAGE_SERVICE", "maintenance-page"), "The name of the K8s Service for the loading page.")
		adminPageService       = flag.String("admin-page-service", getEnv("ADMIN_PAGE_SERVICE", "maintenance-page-admin"), "The name of the K8s Service for the admin page.")
		port                   = flag.Int("port", 8080, "The port to listen on for the maintenance page.")
		adminPort              = flag.Int("admin-port", 8081, "The port to listen on for the admin page.")
		kubeconfig             = flag.String("kubeconfig", getEnv("KUBECONFIG", ""), "Path to kubeconfig (empty for in-cluster)")
		namespace              = flag.String("namespace", getEnv("NAMESPACE", "prod"), "The namespace to operate in.")
		httpRouteName          = flag.String("sonora-route-name", getEnv("SONORA_ROUTE_NAME", "discoenv-routes"), "The name of the HTTPRoute to toggle.")
		deUIService            = flag.String("sonora-service", getEnv("SONORA_SERVICE", "sonora"), "The name of the normal DE UI service.")
		deUIPort               = flag.Int("sonora-port", 80, "The port of the normal DE UI service.")
		adminTemplate          = flag.String("admin-template", getEnv("ADMIN_TEMPLATE", "public/maintenance_admin.html"), "The path to the admin page template.")
	)
	flag.Parse()

	// Read auth credentials from environment variables only to avoid exposing them in process listings.
	basicAuthUsername := os.Getenv("BASIC_AUTH_USERNAME")
	basicAuthPassword := os.Getenv("BASIC_AUTH_PASSWORD")
	if basicAuthUsername == "" || basicAuthPassword == "" {
		log.Fatal("BASIC_AUTH_USERNAME and BASIC_AUTH_PASSWORD environment variables are required")
	}

	// Initialize K8s client
	k8sClient, err := k8s.NewClient(*kubeconfig, *namespace, log)
	if err != nil {
		log.Fatalf("failed to initialize k8s client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Labels for the services
	labels := map[string]string{"app": "maintenance-page"}

	// Ensure Maintenance Page Service
	ensureCtx1, ensureCancel1 := context.WithTimeout(ctx, 30*time.Second)
	defer ensureCancel1()

	if err := k8sClient.EnsureService(ensureCtx1, *maintenancePageService, 80, int32(*port), labels); err != nil {
		log.Errorf("failed to ensure maintenance page service: %v", err)
	}

	// Ensure Admin Page Service
	ensureCtx2, ensureCancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer ensureCancel2()

	if err := k8sClient.EnsureService(ensureCtx2, *adminPageService, 80, int32(*adminPort), labels); err != nil {
		log.Errorf("failed to ensure admin page service: %v", err)
	}

	// Resolve the maintenance page directory to an absolute path for path traversal protection.
	absDir, err := filepath.Abs("public")
	if err != nil {
		log.Fatalf("failed to resolve maintenance page directory: %v", err)
	}

	// Setup Maintenance Page Server
	maintenanceEcho := setupEcho(log)
	maintenanceEcho.Use(maintenanceMiddleware(absDir))

	// Setup Admin Page Server
	adminEcho := setupEcho(log)
	adminEcho.Use(middleware.BasicAuth(func(username, password string, c echo.Context) (bool, error) {
		usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(basicAuthUsername)) == 1
		passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(basicAuthPassword)) == 1
		return usernameMatch && passwordMatch, nil
	}))
	adminEcho.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
		TokenLookup: "form:_csrf",
	}))

	adminApp, err := server.NewAdminApp(k8sClient, *httpRouteName, *maintenancePageService, *deUIService, 80, int32(*deUIPort), *adminTemplate, log)
	if err != nil {
		log.Fatalf("failed to initialize admin app: %v", err)
	}
	adminApp.Register(adminEcho)

	// Start servers
	go func() {
		addr := fmt.Sprintf(":%d", *port)
		log.Infof("Starting maintenance page server on %s", addr)
		if err := maintenanceEcho.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("maintenance page server failed: %v", err)
		}
	}()

	go func() {
		addr := fmt.Sprintf(":%d", *adminPort)
		log.Infof("Starting admin page server on %s", addr)
		if err := adminEcho.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("admin page server failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	<-ctx.Done()
	log.Info("Shutting down servers...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := maintenanceEcho.Shutdown(shutdownCtx); err != nil {
		log.Errorf("maintenance page server shutdown error: %v", err)
	}
	if err := adminEcho.Shutdown(shutdownCtx); err != nil {
		log.Errorf("admin page server shutdown error: %v", err)
	}
}
