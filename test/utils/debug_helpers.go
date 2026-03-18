package utils

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
)

// DumpControllerLogs fetches and prints the controller manager logs for debugging.
// Call this in AfterEach or DeferCleanup to capture logs on test failure.
func DumpControllerLogs(ctx context.Context, k8sClient *kubernetes.Clientset, controllerNamespace string, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Controller Manager Logs ===\n")

	pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list controller pods: %v\n", err)
		return
	}

	if len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(w, "No controller pods found in namespace %s\n", controllerNamespace)
		return
	}

	for _, pod := range pods.Items {
		_, _ = fmt.Fprintf(w, "\n--- Logs from pod %s ---\n", pod.Name)
		logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: ptr.To(int64(200)),
		}).DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Failed to get logs: %v\n", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", string(logs))
	}
}

// DumpVAStatus fetches and prints all VariantAutoscaling resources for debugging.
func DumpVAStatus(ctx context.Context, crClient client.Client, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== VariantAutoscaling Status ===\n")

	vaList := &v1alpha1.VariantAutoscalingList{}
	if err := crClient.List(ctx, vaList); err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list VAs: %v\n", err)
		return
	}

	for _, va := range vaList.Items {
		_, _ = fmt.Fprintf(w, "\nVA: %s/%s\n", va.Namespace, va.Name)
		_, _ = fmt.Fprintf(w, "  ModelID: %s\n", va.Spec.ModelID)
		_, _ = fmt.Fprintf(w, "  Labels: %v\n", va.Labels)
		_, _ = fmt.Fprintf(w, "  DesiredOptimizedAlloc:\n")
		_, _ = fmt.Fprintf(w, "    Accelerator: %s\n", va.Status.DesiredOptimizedAlloc.Accelerator)
		replicas := "<nil>"
		if nr := va.Status.DesiredOptimizedAlloc.NumReplicas; nr != nil {
			replicas = fmt.Sprintf("%d", *nr)
		}
		_, _ = fmt.Fprintf(w, "    NumReplicas: %s\n", replicas)
		_, _ = fmt.Fprintf(w, "    LastRunTime: %v\n", va.Status.DesiredOptimizedAlloc.LastRunTime)
		_, _ = fmt.Fprintf(w, "  Conditions:\n")
		for _, cond := range va.Status.Conditions {
			_, _ = fmt.Fprintf(w, "    - Type: %s, Status: %s, Reason: %s, Message: %q\n", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
	}
}
