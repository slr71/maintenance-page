package k8s

import (
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

// newTestClient builds a Client backed by the given (optional) HTTPRoutes.
func newTestClient(t *testing.T, namespace string, routes ...*gatewayv1.HTTPRoute) (*Client, *gatewayfake.Clientset) {
	t.Helper()
	objs := make([]runtime.Object, 0, len(routes))
	for _, r := range routes {
		objs = append(objs, r)
	}
	gwClient := gatewayfake.NewClientset(objs...)
	log := logrus.New()
	return &Client{
		clientset:     fake.NewSimpleClientset(),
		gatewayClient: gwClient,
		namespace:     namespace,
		log:           log.WithField("component", "k8s-client"),
	}, gwClient
}

// ruleWithBackend builds a single-backend HTTPRouteRule for use in tests.
func ruleWithBackend(name string) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(name),
					},
				},
			},
		},
	}
}

func TestEnsureService(t *testing.T) {
	client, _ := newTestClient(t, "test-ns")
	clientset := client.clientset

	ctx := context.Background()
	name := "test-service"
	port := int32(80)
	targetPort := int32(8080)
	labels := map[string]string{"app": "test"}

	// Test creation
	err := client.EnsureService(ctx, name, port, targetPort, labels)
	require.NoError(t, err)

	svc, err := clientset.CoreV1().Services("test-ns").Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, name, svc.Name)
	assert.Equal(t, port, svc.Spec.Ports[0].Port)
	assert.Equal(t, targetPort, svc.Spec.Ports[0].TargetPort.IntVal)

	// Test already exists
	err = client.EnsureService(ctx, name, port, targetPort, labels)
	require.NoError(t, err)
}

func TestIsMaintenanceMode(t *testing.T) {
	namespace := "test-ns"
	routeName := "test-route"
	maintSvcName := "maint-svc"

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{ruleWithBackend(maintSvcName)},
		},
	}

	client, _ := newTestClient(t, namespace, route)
	ctx := context.Background()

	// Test ON
	isMaint, err := client.IsMaintenanceMode(ctx, routeName, maintSvcName)
	require.NoError(t, err)
	assert.True(t, isMaint)

	// Test OFF (different service name)
	isMaint, err = client.IsMaintenanceMode(ctx, routeName, "other-svc")
	require.NoError(t, err)
	assert.False(t, isMaint)

	// Test Route Not Found
	_, err = client.IsMaintenanceMode(ctx, "non-existent", maintSvcName)
	require.Error(t, err)
}

func TestEnableMaintenanceMode(t *testing.T) {
	namespace := "test-ns"
	maintSvc := "maintenance-page"
	maintPort := int32(80)

	t.Run("redirects every rule and snapshots originals", func(t *testing.T) {
		// Mirror the production HTTPRoute shape: several distinct backends behind
		// different path prefixes. The bug being fixed was that only the rule
		// pointing at the catch-all (sonora) was redirected.
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "discoenv-routes", Namespace: namespace},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					ruleWithBackend("kifshare"),
					ruleWithBackend("terrain"),
					ruleWithBackend("job-status-listener"),
					ruleWithBackend("sonora"),
				},
			},
		}
		client, gw := newTestClient(t, namespace, route)
		ctx := context.Background()

		require.NoError(t, client.EnableMaintenanceMode(ctx, "discoenv-routes", maintSvc, maintPort))

		got, err := gw.GatewayV1().HTTPRoutes(namespace).Get(ctx, "discoenv-routes", metav1.GetOptions{})
		require.NoError(t, err)

		// Every rule should now point at the maintenance service.
		for i, rule := range got.Spec.Rules {
			require.Lenf(t, rule.BackendRefs, 1, "rule %d", i)
			assert.Equalf(t, gatewayv1.ObjectName(maintSvc), rule.BackendRefs[0].Name, "rule %d", i)
		}

		// And the snapshot annotation should preserve the originals so they can be restored.
		snap, ok := got.Annotations[AnnotationOriginalBackends]
		require.True(t, ok, "snapshot annotation should be present")
		assert.Contains(t, snap, "kifshare")
		assert.Contains(t, snap, "terrain")
		assert.Contains(t, snap, "job-status-listener")
		assert.Contains(t, snap, "sonora")
	})

	t.Run("idempotent when annotation already present", func(t *testing.T) {
		// Simulate a route already in maintenance: every rule points at maintenance-page
		// and the annotation holds the real originals. Calling Enable again must not
		// overwrite that snapshot with the all-maintenance state.
		const presetSnapshot = `[[{"name":"sonora"}]]`
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "already-on",
				Namespace:   namespace,
				Annotations: map[string]string{AnnotationOriginalBackends: presetSnapshot},
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{ruleWithBackend(maintSvc)},
			},
		}
		client, gw := newTestClient(t, namespace, route)
		ctx := context.Background()

		require.NoError(t, client.EnableMaintenanceMode(ctx, "already-on", maintSvc, maintPort))

		got, err := gw.GatewayV1().HTTPRoutes(namespace).Get(ctx, "already-on", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, presetSnapshot, got.Annotations[AnnotationOriginalBackends])
	})

	t.Run("empty rules returns ErrNoSuitableRules", func(t *testing.T) {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: namespace},
			Spec:       gatewayv1.HTTPRouteSpec{Rules: nil},
		}
		client, _ := newTestClient(t, namespace, route)
		err := client.EnableMaintenanceMode(context.Background(), "empty", maintSvc, maintPort)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNoSuitableRules))
	})

	t.Run("route not found", func(t *testing.T) {
		client, _ := newTestClient(t, namespace)
		err := client.EnableMaintenanceMode(context.Background(), "missing", maintSvc, maintPort)
		require.Error(t, err)
	})
}

func TestDisableMaintenanceMode(t *testing.T) {
	namespace := "test-ns"
	maintSvc := "maintenance-page"
	fallbackSvc := "sonora"
	port := int32(80)

	t.Run("restores per-rule backends from annotation", func(t *testing.T) {
		// Round-trip: Enable populates the annotation, Disable consumes it.
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "discoenv-routes", Namespace: namespace},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					ruleWithBackend("kifshare"),
					ruleWithBackend("terrain"),
					ruleWithBackend("sonora"),
				},
			},
		}
		client, gw := newTestClient(t, namespace, route)
		ctx := context.Background()

		require.NoError(t, client.EnableMaintenanceMode(ctx, "discoenv-routes", maintSvc, port))
		require.NoError(t, client.DisableMaintenanceMode(ctx, "discoenv-routes", maintSvc, fallbackSvc, port))

		got, err := gw.GatewayV1().HTTPRoutes(namespace).Get(ctx, "discoenv-routes", metav1.GetOptions{})
		require.NoError(t, err)

		want := []string{"kifshare", "terrain", "sonora"}
		require.Len(t, got.Spec.Rules, len(want))
		for i, name := range want {
			require.Lenf(t, got.Spec.Rules[i].BackendRefs, 1, "rule %d", i)
			assert.Equalf(t, gatewayv1.ObjectName(name), got.Spec.Rules[i].BackendRefs[0].Name, "rule %d", i)
		}
		_, ok := got.Annotations[AnnotationOriginalBackends]
		assert.False(t, ok, "annotation should be cleared after restore")
	})

	t.Run("legacy fallback when annotation missing", func(t *testing.T) {
		// Simulates a route that was put into maintenance by the previous code path,
		// which only redirected the catch-all rule and didn't write the annotation.
		// The fallback should redirect just that rule back to sonora.
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: namespace},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					ruleWithBackend("terrain"),
					ruleWithBackend(maintSvc),
				},
			},
		}
		client, gw := newTestClient(t, namespace, route)
		ctx := context.Background()

		require.NoError(t, client.DisableMaintenanceMode(ctx, "legacy", maintSvc, fallbackSvc, port))

		got, err := gw.GatewayV1().HTTPRoutes(namespace).Get(ctx, "legacy", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, gatewayv1.ObjectName("terrain"), got.Spec.Rules[0].BackendRefs[0].Name)
		assert.Equal(t, gatewayv1.ObjectName(fallbackSvc), got.Spec.Rules[1].BackendRefs[0].Name)
	})

	t.Run("legacy fallback with no maintenance-backed rule errors", func(t *testing.T) {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "nothing-to-do", Namespace: namespace},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{ruleWithBackend("terrain")},
			},
		}
		client, _ := newTestClient(t, namespace, route)
		err := client.DisableMaintenanceMode(context.Background(), "nothing-to-do", maintSvc, fallbackSvc, port)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNoSuitableRules))
	})

	t.Run("malformed annotation surfaces decode error", func(t *testing.T) {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "garbled",
				Namespace:   namespace,
				Annotations: map[string]string{AnnotationOriginalBackends: "not json"},
			},
			Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{ruleWithBackend(maintSvc)}},
		}
		client, _ := newTestClient(t, namespace, route)
		err := client.DisableMaintenanceMode(context.Background(), "garbled", maintSvc, fallbackSvc, port)
		require.Error(t, err)
	})
}
