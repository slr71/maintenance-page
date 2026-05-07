package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// AnnotationOriginalBackends is the HTTPRoute annotation under which the per-rule
// BackendRefs snapshot is stored while maintenance mode is active. The value is a
// JSON-encoded slice (one entry per rule, indexed positionally) of HTTPBackendRef
// lists, captured by EnableMaintenanceMode and consumed by DisableMaintenanceMode.
const AnnotationOriginalBackends = "cyverse.org/maintenance-original-backends"

// ErrNoSuitableRules is returned when an HTTPRoute has no rules to redirect or
// restore — for example, when DisableMaintenanceMode's legacy fallback finds no
// rules currently pointing at the maintenance service.
var ErrNoSuitableRules = errors.New("no suitable rules found in HTTPRoute to update")

// K8sClient defines the interface for interacting with Kubernetes resources needed for maintenance mode.
type K8sClient interface {
	// EnsureService creates the specified service if it does not exist.
	EnsureService(ctx context.Context, name string, port, targetPort int32, labels map[string]string) error
	// IsMaintenanceMode returns true if the specified HTTPRoute points to the maintenance service.
	IsMaintenanceMode(ctx context.Context, routeName, maintenanceServiceName string) (bool, error)
	// EnableMaintenanceMode redirects every rule in the HTTPRoute to the maintenance service,
	// recording each rule's original BackendRefs in an annotation so they can be restored later.
	// Calling it on a route that is already in maintenance is a no-op.
	EnableMaintenanceMode(ctx context.Context, routeName, maintenanceServiceName string, maintenancePort int32) error
	// DisableMaintenanceMode restores the per-rule BackendRefs saved by EnableMaintenanceMode and
	// clears the annotation. If the annotation is missing (e.g., maintenance was enabled by an older
	// version of this code), it falls back to redirecting any rule still pointing at the maintenance
	// service to fallbackServiceName.
	DisableMaintenanceMode(ctx context.Context, routeName, maintenanceServiceName, fallbackServiceName string, fallbackPort int32) error
}

// Client is a Kubernetes client that implements the K8sClient interface.
type Client struct {
	clientset     kubernetes.Interface
	gatewayClient gatewayclient.Interface
	namespace     string
	log           *logrus.Entry
}

// Ensure Client implements K8sClient.
var _ K8sClient = &Client{}

// NewClient creates a new Client instance.
func NewClient(kubeconfig, namespace string, log *logrus.Logger) (*Client, error) {
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("error building kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating k8s client: %w", err)
	}

	gwClient, err := gatewayclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating gateway API client: %w", err)
	}

	return &Client{
		clientset:     clientset,
		gatewayClient: gwClient,
		namespace:     namespace,
		log:           log.WithField("component", "k8s-client"),
	}, nil
}

// EnsureService creates the service with the given name and labels if it doesn't already exist.
// It does not update the service if it already exists with different configuration; the assumption
// is that services are either created fresh or managed externally.
func (c *Client) EnsureService(ctx context.Context, name string, port, targetPort int32, labels map[string]string) error {
	log := c.log.WithFields(logrus.Fields{
		"service":    name,
		"port":       port,
		"targetPort": targetPort,
	})

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Port:       port,
					TargetPort: intstr.FromInt32(targetPort),
				},
			},
		},
	}

	_, err := c.clientset.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Info("creating service")
		_, err = c.clientset.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create service %s: %w", name, err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get service %s: %w", name, err)
	}

	return nil
}

// IsMaintenanceMode returns true if the specified HTTPRoute points to the maintenance service.
func (c *Client) IsMaintenanceMode(ctx context.Context, routeName, maintenanceServiceName string) (bool, error) {
	route, err := c.gatewayClient.GatewayV1().HTTPRoutes(c.namespace).Get(ctx, routeName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get HTTPRoute %s: %w", routeName, err)
	}

	for _, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			if string(backend.Name) == maintenanceServiceName {
				return true, nil
			}
		}
	}

	return false, nil
}

// EnableMaintenanceMode redirects every rule in the HTTPRoute to the maintenance service.
// The current per-rule BackendRefs are serialized as JSON into AnnotationOriginalBackends so
// that DisableMaintenanceMode can restore them. If the annotation is already present the
// route is considered to already be in maintenance and the call is a no-op.
func (c *Client) EnableMaintenanceMode(ctx context.Context, routeName, maintenanceServiceName string, maintenancePort int32) error {
	log := c.log.WithFields(logrus.Fields{
		"route":  routeName,
		"target": maintenanceServiceName,
	})

	route, err := c.gatewayClient.GatewayV1().HTTPRoutes(c.namespace).Get(ctx, routeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get HTTPRoute %s: %w", routeName, err)
	}

	if len(route.Spec.Rules) == 0 {
		return fmt.Errorf("HTTPRoute %s: %w", routeName, ErrNoSuitableRules)
	}

	// Idempotency guard: if the snapshot annotation already exists, assume maintenance
	// is already on. Re-snapshotting now would record maintenance-page as the "original"
	// backend for every rule, permanently destroying the real backends.
	if _, ok := route.Annotations[AnnotationOriginalBackends]; ok {
		log.Debug("maintenance already enabled; nothing to do")
		return nil
	}

	snapshot := make([][]gatewayv1.HTTPBackendRef, len(route.Spec.Rules))
	for i, rule := range route.Spec.Rules {
		snapshot[i] = rule.BackendRefs
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("failed to encode original backends for HTTPRoute %s: %w", routeName, err)
	}
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations[AnnotationOriginalBackends] = string(encoded)

	maintRef := []gatewayv1.HTTPBackendRef{newBackendRef(maintenanceServiceName, maintenancePort)}
	for i := range route.Spec.Rules {
		route.Spec.Rules[i].BackendRefs = maintRef
	}

	if _, err := c.gatewayClient.GatewayV1().HTTPRoutes(c.namespace).Update(ctx, route, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update HTTPRoute %s: %w", routeName, err)
	}

	log.Info("enabled maintenance mode")
	return nil
}

// DisableMaintenanceMode reverses EnableMaintenanceMode by reading the snapshot annotation
// and restoring each rule's original BackendRefs. If the annotation is absent, it falls back
// to legacy behavior: any rule whose backend is the maintenance service is redirected to
// fallbackServiceName. The legacy path returns ErrNoSuitableRules if it finds no such rule.
func (c *Client) DisableMaintenanceMode(ctx context.Context, routeName, maintenanceServiceName, fallbackServiceName string, fallbackPort int32) error {
	log := c.log.WithFields(logrus.Fields{
		"route":    routeName,
		"fallback": fallbackServiceName,
	})

	route, err := c.gatewayClient.GatewayV1().HTTPRoutes(c.namespace).Get(ctx, routeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get HTTPRoute %s: %w", routeName, err)
	}

	if encoded, ok := route.Annotations[AnnotationOriginalBackends]; ok && encoded != "" {
		var snapshot [][]gatewayv1.HTTPBackendRef
		if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
			return fmt.Errorf("failed to decode original backends annotation on HTTPRoute %s: %w", routeName, err)
		}

		// Restore positionally. If a rule was added during maintenance the snapshot
		// has no entry at that index; leave the new rule untouched.
		for i := range route.Spec.Rules {
			if i >= len(snapshot) {
				break
			}
			route.Spec.Rules[i].BackendRefs = snapshot[i]
		}
		delete(route.Annotations, AnnotationOriginalBackends)

		if _, err := c.gatewayClient.GatewayV1().HTTPRoutes(c.namespace).Update(ctx, route, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update HTTPRoute %s: %w", routeName, err)
		}

		log.Info("disabled maintenance mode (restored from annotation)")
		return nil
	}

	// Legacy fallback: the route was put into maintenance by a previous version that did
	// not write the snapshot annotation. Redirect any maintenance-backed rule to the
	// fallback service. This is one-shot — once restored, future toggles use the snapshot.
	log.Warn("snapshot annotation missing; using legacy fallback to disable maintenance")
	fallbackRef := []gatewayv1.HTTPBackendRef{newBackendRef(fallbackServiceName, fallbackPort)}
	updated := false
	for i, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			if string(backend.Name) == maintenanceServiceName {
				route.Spec.Rules[i].BackendRefs = fallbackRef
				updated = true
				break
			}
		}
	}
	if !updated {
		return fmt.Errorf("HTTPRoute %s: %w", routeName, ErrNoSuitableRules)
	}

	if _, err := c.gatewayClient.GatewayV1().HTTPRoutes(c.namespace).Update(ctx, route, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update HTTPRoute %s: %w", routeName, err)
	}

	log.Info("disabled maintenance mode (legacy fallback)")
	return nil
}

// newBackendRef builds a single-service HTTPBackendRef pointing at the given service and port.
func newBackendRef(name string, port int32) gatewayv1.HTTPBackendRef {
	p := gatewayv1.PortNumber(port)
	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &p,
			},
		},
	}
}
