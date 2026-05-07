package server

import (
	"fmt"
	"html/template"
	"io"
	"net/http"

	"github.com/cyverse-de/maintenance-page/internal/k8s"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// Template is a custom Echo renderer for HTML templates.
type Template struct {
	templates *template.Template
}

// Render renders a template document.
func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

// AdminApp represents the administration interface for toggling maintenance mode.
type AdminApp struct {
	k8sClient              k8s.K8sClient
	httpRouteName          string
	maintenanceServiceName string
	maintenanceServicePort int32
	deUIServiceName        string
	deUIServicePort        int32
	template               *template.Template
	log                    *logrus.Entry
}

// NewAdminApp creates a new AdminApp instance and parses the administration interface template.
func NewAdminApp(k8sClient k8s.K8sClient, routeName, maintSvc, deUISvc string, maintPort, deUIPort int32, templatePath string, log *logrus.Logger) (*AdminApp, error) {
	tmpl, err := template.New("maintenance_admin.html").ParseFiles(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}

	return &AdminApp{
		k8sClient:              k8sClient,
		httpRouteName:          routeName,
		maintenanceServiceName: maintSvc,
		maintenanceServicePort: maintPort,
		deUIServiceName:        deUISvc,
		deUIServicePort:        deUIPort,
		template:               tmpl,
		log:                    log.WithField("component", "admin-app"),
	}, nil
}

// Register registers the AdminApp routes with the provided Echo instance.
func (a *AdminApp) Register(e *echo.Echo) {
	e.Renderer = &Template{
		templates: a.template,
	}
	e.GET("/", a.HandleIndex)
	e.POST("/toggle", a.HandleToggle)
	e.GET("/maintenance", a.HandleStatus)
}

// HandleStatus handles requests for the current maintenance mode status.
func (a *AdminApp) HandleStatus(c echo.Context) error {
	ctx := c.Request().Context()
	isMaint, err := a.k8sClient.IsMaintenanceMode(ctx, a.httpRouteName, a.maintenanceServiceName)
	if err != nil {
		a.log.WithError(err).Error("failed to get maintenance mode status")
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	return c.JSON(http.StatusOK, map[string]bool{"maintenance": isMaint})
}

// HandleIndex serves the administration interface page.
func (a *AdminApp) HandleIndex(c echo.Context) error {
	ctx := c.Request().Context()
	isMaint, err := a.k8sClient.IsMaintenanceMode(ctx, a.httpRouteName, a.maintenanceServiceName)
	if err != nil {
		a.log.WithError(err).Error("failed to get maintenance mode status")
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// The CSRF token is set by Echo's CSRF middleware. It may be nil in tests
	// where the middleware is not active.
	csrfToken, _ := c.Get("csrf").(string)

	data := struct {
		IsMaintenance bool
		CSRFToken     string
	}{
		IsMaintenance: isMaint,
		CSRFToken:     csrfToken,
	}

	return c.Render(http.StatusOK, "maintenance_admin.html", data)
}

// HandleToggle toggles the maintenance mode by updating the HTTPRoute.
func (a *AdminApp) HandleToggle(c echo.Context) error {
	ctx := c.Request().Context()
	isMaint, err := a.k8sClient.IsMaintenanceMode(ctx, a.httpRouteName, a.maintenanceServiceName)
	if err != nil {
		a.log.WithError(err).Error("failed to get maintenance mode status for toggle")
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	a.log.WithField("currentMaintenance", isMaint).Info("toggling maintenance mode")

	if isMaint {
		err = a.k8sClient.DisableMaintenanceMode(ctx, a.httpRouteName, a.maintenanceServiceName, a.deUIServiceName, a.deUIServicePort)
	} else {
		err = a.k8sClient.EnableMaintenanceMode(ctx, a.httpRouteName, a.maintenanceServiceName, a.maintenanceServicePort)
	}
	if err != nil {
		a.log.WithError(err).Error("failed to toggle maintenance mode")
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	return c.Redirect(http.StatusSeeOther, "/")
}
