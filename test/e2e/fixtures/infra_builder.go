package fixtures

import (
	"context"
	"fmt"
	"time"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateService creates a Kubernetes Service for the model server. Fails if the service already exists.
func CreateService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, appLabel string, port int) error {
	service := buildService(namespace, name, appLabel, port)
	_, err := k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	return err
}

// DeleteService deletes the Kubernetes Service. Idempotent; ignores NotFound.
func DeleteService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	serviceName := name + "-service"
	err := k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete service %s: %w", serviceName, err)
	}
	return nil
}

// EnsureService creates or replaces the Service (idempotent for test setup).
func EnsureService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, appLabel string, port int) error {
	serviceName := name + "-service"
	_, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err == nil {
		deleteErr := k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing service %s: %w", serviceName, deleteErr)
		}
		if waitErr := WaitUntilServiceDeleted(ctx, k8sClient, namespace, serviceName, 30*time.Second); waitErr != nil {
			return fmt.Errorf("timeout waiting for service %s to be deleted: %w", serviceName, waitErr)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing service %s: %w", serviceName, err)
	}
	service := buildService(namespace, name, appLabel, port)
	_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		if waitErr := WaitUntilServiceDeleted(ctx, k8sClient, namespace, serviceName, 30*time.Second); waitErr != nil {
			return fmt.Errorf("timeout waiting for service %s to be deleted before recreate: %w", serviceName, waitErr)
		}
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	}
	return err
}

func buildService(namespace, name, appLabel string, port int) *corev1.Service {
	serviceName := name + "-service"
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
				"test-resource":             "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: int32(port), Protocol: corev1.ProtocolTCP},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// CreateServiceMonitor creates a ServiceMonitor for Prometheus. Fails if it already exists.
func CreateServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, targetNamespace, name, appLabel string) error {
	serviceMonitor := buildServiceMonitor(monitoringNamespace, targetNamespace, name, appLabel)
	return crClient.Create(ctx, serviceMonitor)
}

// DeleteServiceMonitor deletes the ServiceMonitor. Idempotent; ignores NotFound.
func DeleteServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, name string) error {
	serviceMonitorName := name + "-monitor"
	sm := &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: serviceMonitorName, Namespace: monitoringNamespace},
	}
	err := crClient.Delete(ctx, sm)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ServiceMonitor %s: %w", serviceMonitorName, err)
	}
	return nil
}

// EnsureServiceMonitor creates or replaces the ServiceMonitor (idempotent for test setup).
func EnsureServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, targetNamespace, name, appLabel string) error {
	serviceMonitorName := name + "-monitor"
	existingSM := &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: serviceMonitorName, Namespace: monitoringNamespace},
	}
	err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: monitoringNamespace}, existingSM)
	if err == nil {
		deleteErr := crClient.Delete(ctx, existingSM)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing ServiceMonitor %s: %w", serviceMonitorName, deleteErr)
		}
		if waitErr := WaitUntilServiceMonitorDeleted(ctx, crClient, monitoringNamespace, serviceMonitorName, 30*time.Second); waitErr != nil {
			return fmt.Errorf("timeout waiting for ServiceMonitor %s to be deleted: %w", serviceMonitorName, waitErr)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing ServiceMonitor %s: %w", serviceMonitorName, err)
	}
	serviceMonitor := buildServiceMonitor(monitoringNamespace, targetNamespace, name, appLabel)
	return crClient.Create(ctx, serviceMonitor)
}

func buildServiceMonitor(monitoringNamespace, targetNamespace, name, appLabel string) *promoperator.ServiceMonitor {
	serviceMonitorName := name + "-monitor"
	return &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceMonitorName,
			Namespace: monitoringNamespace,
			Labels: map[string]string{
				"app":           appLabel,
				"release":       "kube-prometheus-stack",
				"test-resource": "true",
			},
		},
		Spec: promoperator.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Endpoints: []promoperator.Endpoint{
				{Port: "http", Path: "/metrics", Interval: promoperator.Duration("15s")},
			},
			NamespaceSelector: promoperator.NamespaceSelector{MatchNames: []string{targetNamespace}},
		},
	}
}
