package benchmark

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DesiredEPPConfig is the EndpointPickerConfig YAML with flowControl enabled
// and scorer weights: queue=2, kv-cache=2, prefix-cache=3.
const DesiredEPPConfig = `apiVersion: inference.networking.x-k8s.io/v1alpha1
kind: EndpointPickerConfig
featureGates:
- flowControl
plugins:
- type: queue-scorer
- type: kv-cache-utilization-scorer
- type: prefix-cache-scorer
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: queue-scorer
    weight: 2
  - pluginRef: kv-cache-utilization-scorer
    weight: 2
  - pluginRef: prefix-cache-scorer
    weight: 3
`

// PatchEPPConfigMap updates the EPP's existing ConfigMap to include
// featureGates: [flowControl] and the desired scorer weights, then triggers
// a rollout restart. This avoids changing deployment args, volumes, or env
// vars — only the ConfigMap data is modified.
func PatchEPPConfigMap(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, eppDeploymentName string) error {
	dep, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, eppDeploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get EPP deployment %s: %w", eppDeploymentName, err)
	}

	// Find the ConfigMap name from the deployment's volumes
	var configMapName string
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			configMapName = v.ConfigMap.Name
			break
		}
	}
	if configMapName == "" {
		return fmt.Errorf("EPP deployment %s has no ConfigMap volume", eppDeploymentName)
	}

	// Find the config file key from --config-file arg
	configKey := "default-plugins.yaml"
	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--config-file=") {
			parts := strings.Split(a, "/")
			if len(parts) > 0 {
				configKey = parts[len(parts)-1]
			}
		}
	}

	// Update the ConfigMap with flowControl + weights 2/2/3
	cm, err := k8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get EPP ConfigMap %s: %w", configMapName, err)
	}

	if strings.Contains(cm.Data[configKey], "flowControl") {
		return nil
	}

	cm.Data[configKey] = DesiredEPPConfig
	_, err = k8sClient.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update EPP ConfigMap %s: %w", configMapName, err)
	}

	// Trigger rollout restart via annotation change so the EPP picks up the new config
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = make(map[string]string)
	}
	dep.Spec.Template.Annotations["benchmark/restart-trigger"] = time.Now().Format(time.RFC3339)
	_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to trigger EPP rollout restart: %w", err)
	}

	// Wait for rollout to complete
	deadline := time.After(5 * time.Minute)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for EPP deployment %s to roll out", eppDeploymentName)
		case <-tick.C:
			d, getErr := k8sClient.AppsV1().Deployments(namespace).Get(ctx, eppDeploymentName, metav1.GetOptions{})
			if getErr != nil {
				continue
			}
			if d.Status.UpdatedReplicas > 0 && d.Status.ReadyReplicas == d.Status.UpdatedReplicas &&
				d.Status.UnavailableReplicas == 0 && d.Status.ObservedGeneration >= d.Generation {
				return nil
			}
		}
	}
}

// FindEPPDeployment discovers the EPP deployment by looking for deployments
// containing "epp" in their name.
func FindEPPDeployment(ctx context.Context, k8sClient *kubernetes.Clientset, namespace string) (string, error) {
	deps, err := k8sClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list deployments: %w", err)
	}
	for i := range deps.Items {
		name := deps.Items[i].Name
		if containsAny(name, "epp", "inference-scheduler") {
			return name, nil
		}
	}
	return "", fmt.Errorf("no EPP deployment found in namespace %s", namespace)
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
