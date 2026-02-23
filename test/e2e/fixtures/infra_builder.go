package fixtures

import (
	"context"
	"fmt"
	"time"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Note: InferencePool should already exist from infra-only deployment.
// Tests assume the infrastructure (Gateway, EPP, InferencePool) is already deployed.

// CreateModelService creates a model service deployment (vLLM or simulator)
// InferencePool compatibility via llm-d.ai/model-pool label.
// This function is idempotent: it will delete any existing deployment with the same name
// before creating a new one to handle leftover resources from previous test runs.
func CreateModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int) error {
	appLabel := name + "-decode"
	deploymentName := appLabel

	// Check if deployment already exists
	// Only delete if it's in a bad state (no ready replicas after a reasonable time)
	// This prevents unnecessary deletion of healthy deployments
	existingDeployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err == nil {
		// Deployment exists - check if it's healthy
		// If it has ready replicas, we can reuse it (idempotent behavior)
		if existingDeployment.Status.ReadyReplicas > 0 {
			// Deployment is healthy, no need to recreate
			return nil
		}

		// Deployment exists but has no ready replicas - delete and recreate
		propagationPolicy := metav1.DeletePropagationForeground
		deleteErr := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			// If it's already being deleted (Conflict), that's fine - we'll wait for it
			if errors.IsConflict(deleteErr) {
				// Deployment is already being deleted, just wait for it
			} else {
				return fmt.Errorf("failed to delete existing deployment %s: %w", deploymentName, deleteErr)
			}
		}
		// Wait for deletion to complete (with timeout)
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		for {
			_, checkErr := k8sClient.AppsV1().Deployments(namespace).Get(waitCtx, deploymentName, metav1.GetOptions{})
			if errors.IsNotFound(checkErr) {
				break // Deployment is fully deleted
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timeout waiting for deployment %s to be deleted", deploymentName)
			}
			time.Sleep(2 * time.Second)
		}
	} else if !errors.IsNotFound(err) {
		// If error is not "not found", return it
		return fmt.Errorf("failed to check for existing deployment %s: %w", deploymentName, err)
	}

	// Choose image based on simulator flag
	image := "ghcr.io/llm-d/llm-d-inference-sim:v0.6.1"
	if !useSimulator {
		image = "vllm/vllm-openai:latest"
	}

	// Build container args
	args := buildModelServerArgs(modelID, useSimulator, maxNumSeqs)

	labels := map[string]string{
		"app":                       appLabel,
		"llm-d.ai/inferenceServing": "true",
		"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
		"llm-d.ai/model-pool":       poolName, // Added for InferencePool compatibility
		"test-resource":             "true",
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appLabel,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                       appLabel,
					"llm-d.ai/inferenceServing": "true",
					"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
					"llm-d.ai/model-pool":       poolName, // Added for InferencePool compatibility
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            appLabel,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent, // Use cached images for faster subsequent tests
							Args:            args,
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: 8000,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "metadata.name",
										},
									},
								},
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "metadata.namespace",
										},
									},
								},
								{
									Name: "POD_IP",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "status.podIP",
										},
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("4Gi"),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}

	_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		// If it still exists (race condition), delete and retry once
		propagationPolicy := metav1.DeletePropagationForeground
		_ = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		time.Sleep(2 * time.Second)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	}
	return err
}

// buildModelServerArgs builds the argument list for the model server container
func buildModelServerArgs(modelID string, useSimulator bool, maxNumSeqs int) []string {
	if useSimulator {
		// Simulator arguments - (v0.6.1 uses integer milliseconds)
		// avgTTFT=100, avgITL=20 as defaults
		return []string{
			"--model", modelID,
			"--port", "8000",
			fmt.Sprintf("--time-to-first-token=%d", 100),
			fmt.Sprintf("--inter-token-latency=%d", 20),
			"--mode=random",
			"--enable-kvcache",
			"--kv-cache-size=1024",
			"--block-size=16",
			"--tokenizers-cache-dir=/tmp",
			"--max-num-seqs", fmt.Sprintf("%d", maxNumSeqs),
			"--max-model-len", "1024",
		}
	}

	// Real vLLM arguments
	return []string{
		"--model", modelID,
		"--max-num-seqs", fmt.Sprintf("%d", maxNumSeqs),
		"--max-model-len", "1024",
		"--served-model-name", modelID,
		"--disable-log-requests",
	}
}

// CreateService creates a Kubernetes Service for the model server
// for better compatibility with InferencePool routing.
// This function is idempotent: it will delete any existing service with the same name
// before creating a new one to handle leftover resources from previous test runs.
func CreateService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, appLabel string, port int) error {
	serviceName := name + "-service"

	// Check if service already exists and delete it to ensure clean state
	_, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err == nil {
		// Service exists, delete it first
		deleteErr := k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		if deleteErr != nil {
			return fmt.Errorf("failed to delete existing service %s: %w", serviceName, deleteErr)
		}
		// Wait a moment for deletion to propagate
		time.Sleep(500 * time.Millisecond)
	} else if !errors.IsNotFound(err) {
		// If error is not "not found", return it
		return fmt.Errorf("failed to check for existing service %s: %w", serviceName, err)
	}

	service := &corev1.Service{
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
				{
					Name:     "http",
					Port:     int32(port),
					Protocol: corev1.ProtocolTCP,
				},
			},
			// Use ClusterIP instead of NodePort for InferencePool compatibility
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		// If it still exists (race condition), delete and retry once
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		time.Sleep(1 * time.Second)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	}
	return err
}

// CreateServiceMonitor creates a ServiceMonitor for Prometheus to scrape metrics from the model service.
// This function is idempotent: it will delete any existing ServiceMonitor with the same name
// before creating a new one to handle leftover resources from previous test runs.
func CreateServiceMonitor(ctx context.Context, crClient client.Client, monitoringNamespace, targetNamespace, name, appLabel string) error {
	serviceMonitorName := name + "-monitor"

	// Check if ServiceMonitor already exists and delete it to ensure clean state
	existingSM := &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceMonitorName,
			Namespace: monitoringNamespace,
		},
	}
	err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: monitoringNamespace}, existingSM)
	if err == nil {
		// ServiceMonitor exists, delete it first
		deleteErr := crClient.Delete(ctx, existingSM)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("failed to delete existing ServiceMonitor %s: %w", serviceMonitorName, deleteErr)
		}
		// Wait for deletion to complete
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for {
			checkErr := crClient.Get(waitCtx, client.ObjectKey{Name: serviceMonitorName, Namespace: monitoringNamespace}, &promoperator.ServiceMonitor{})
			if errors.IsNotFound(checkErr) {
				break // ServiceMonitor is fully deleted
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timeout waiting for ServiceMonitor %s to be deleted", serviceMonitorName)
			}
			time.Sleep(1 * time.Second)
		}
	} else if !errors.IsNotFound(err) {
		// If error is not "not found", return it
		return fmt.Errorf("failed to check for existing ServiceMonitor %s: %w", serviceMonitorName, err)
	}
	serviceMonitor := &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceMonitorName,
			Namespace: monitoringNamespace,
			Labels: map[string]string{
				"app":           appLabel,
				"release":       "kube-prometheus-stack", // Required label for Prometheus to discover this ServiceMonitor
				"test-resource": "true",
			},
		},
		Spec: promoperator.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": appLabel, // Match the service labels
				},
			},
			Endpoints: []promoperator.Endpoint{
				{
					Port:     "http",
					Path:     "/metrics",
					Interval: promoperator.Duration("15s"),
				},
			},
			NamespaceSelector: promoperator.NamespaceSelector{
				MatchNames: []string{targetNamespace}, // Scrape from the target namespace
			},
		},
	}

	return crClient.Create(ctx, serviceMonitor)
}
