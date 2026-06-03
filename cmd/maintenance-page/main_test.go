package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestMaintenanceDir creates a temporary maintenance page directory containing
// the index page and a sample asset, returning its absolute path.
func newTestMaintenanceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance_index.html"), []byte("<html>maintenance</html>"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance_index.css"), []byte("body{}"), 0o600))
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	return abs
}

// TestHealthEndpoint verifies the probe endpoint returns 200 even though the
// maintenance catch-all middleware would otherwise turn a kube-probe request
// (Accept: */*, no Sec-Fetch headers) into a 503.
func TestHealthEndpoint(t *testing.T) {
	absDir := newTestMaintenanceDir(t)
	e := echo.New()
	e.Use(maintenanceMiddleware(absDir))
	e.GET(healthPath, func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, healthPath, nil)
	req.Header.Set("User-Agent", "kube-probe/1.29")
	req.Header.Set("Accept", "*/*")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("X-Maintenance-Mode"))
}

func TestMaintenanceMiddleware(t *testing.T) {
	absDir := newTestMaintenanceDir(t)
	handler := maintenanceMiddleware(absDir)(func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	tests := []struct {
		name           string
		path           string
		headers        map[string]string
		wantStatus     int
		wantBody       string
		wantMaintainer bool // expect X-Maintenance-Mode marker header
	}{
		{
			name:       "document navigation serves the maintenance page",
			path:       "/apps",
			headers:    map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"},
			wantStatus: http.StatusOK,
			wantBody:   "<html>maintenance</html>",
		},
		{
			name:           "xhr api call gets a 503 with the maintenance marker",
			path:           "/terrain/apps",
			headers:        map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty", "Accept": "application/json"},
			wantStatus:     http.StatusServiceUnavailable,
			wantMaintainer: true,
		},
		{
			name:       "existing asset is always served even with a non-document dest",
			path:       "/maintenance_index.css",
			headers:    map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "style"},
			wantStatus: http.StatusOK,
			wantBody:   "body{}",
		},
		{
			name:       "no Sec-Fetch headers with html accept serves the page",
			path:       "/dashboard",
			headers:    map[string]string{"Accept": "text/html,application/xhtml+xml"},
			wantStatus: http.StatusOK,
			wantBody:   "<html>maintenance</html>",
		},
		{
			name:           "no Sec-Fetch headers with json accept gets a 503",
			path:           "/terrain/apps",
			headers:        map[string]string{"Accept": "application/json, text/plain, */*"},
			wantStatus:     http.StatusServiceUnavailable,
			wantMaintainer: true,
		},
		{
			name:       "root path navigation serves the page",
			path:       "/",
			headers:    map[string]string{"Sec-Fetch-Mode": "navigate"},
			wantStatus: http.StatusOK,
			wantBody:   "<html>maintenance</html>",
		},
		{
			name:       "path traversal attempt falls back to the maintenance page",
			path:       "/../../../etc/passwd",
			headers:    map[string]string{"Sec-Fetch-Mode": "navigate"},
			wantStatus: http.StatusOK,
			wantBody:   "<html>maintenance</html>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)

			require.NoError(t, handler(c))
			assert.Equal(t, tt.wantStatus, rec.Code)

			if tt.wantBody != "" {
				assert.Equal(t, tt.wantBody, rec.Body.String())
			}

			if tt.wantMaintainer {
				assert.Equal(t, "true", rec.Header().Get("X-Maintenance-Mode"))
				assert.Equal(t, "300", rec.Header().Get("Retry-After"))
				assert.Contains(t, rec.Body.String(), `"maintenance":true`)
			} else {
				assert.Empty(t, rec.Header().Get("X-Maintenance-Mode"))
			}
		})
	}
}
