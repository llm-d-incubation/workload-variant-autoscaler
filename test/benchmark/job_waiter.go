package benchmark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// WaitForJobCompletion waits for a job to complete successfully
func WaitForJobCompletion(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, jobName string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == "True" {
				return true, nil
			}
			if cond.Type == batchv1.JobFailed && cond.Status == "True" {
				return false, fmt.Errorf("job failed: %s", cond.Message)
			}
		}

		return false, nil
	})
}

// GetJobPodLogs retrieves the logs from the first pod associated with a job
func GetJobPodLogs(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, jobName string) (string, error) {
	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods for job: %w", err)
	}

	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s", jobName)
	}

	podName := pods.Items[0].Name

	req := k8sClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("error in opening stream: %w", err)
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", fmt.Errorf("error in copy information from podLogs to buf: %w", err)
	}

	return buf.String(), nil
}

// VerifyGatewayConnectivity sends a small test request through the Gateway to verify
// the backend is accessible. Returns nil on success, error otherwise.
func VerifyGatewayConnectivity(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, gatewayURL, modelID string) error {
	jobName := "gateway-connectivity-check"

	_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})
	time.Sleep(3 * time.Second)

	curlCmd := fmt.Sprintf(
		`echo "=== Connectivity check to %s ===" && `+
			`echo "Attempting request with model=%s ..." && `+
			`HTTP_CODE=$(curl -v -sS -o /tmp/body.txt -w "%%{http_code}" -m 60 -X POST "%s/v1/completions" `+
			`-H "Content-Type: application/json" `+
			`-d '{"model":"%s","prompt":"Hello","max_tokens":5}' 2>/tmp/curl_debug.txt) && `+
			`echo "HTTP_CODE=$HTTP_CODE" && echo "--- Response Body ---" && cat /tmp/body.txt && echo "" && `+
			`echo "--- Curl Debug (last 20 lines) ---" && tail -20 /tmp/curl_debug.txt && `+
			`if [ "$HTTP_CODE" -ge 200 ] && [ "$HTTP_CODE" -lt 300 ]; then echo "OK"; else echo "FAIL: HTTP $HTTP_CODE"; exit 1; fi`,
		gatewayURL, modelID, gatewayURL, modelID,
	)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": "true"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "curl",
							Image:   "registry.access.redhat.com/ubi9/ubi-minimal:latest",
							Command: []string{"sh", "-c"},
							Args:    []string{curlCmd},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	if _, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create connectivity check job: %w", err)
	}

	if err := WaitForJobCompletion(ctx, k8sClient, namespace, jobName, 3*time.Minute); err != nil {
		logs, logErr := GetJobPodLogs(ctx, k8sClient, namespace, jobName)
		if logErr == nil && strings.TrimSpace(logs) != "" {
			return fmt.Errorf("connectivity check failed: %w\nLogs: %s", err, logs)
		}
		return fmt.Errorf("connectivity check failed: %w", err)
	}

	logs, _ := GetJobPodLogs(ctx, k8sClient, namespace, jobName)
	if !strings.Contains(logs, "OK") {
		return fmt.Errorf("connectivity check did not return OK: %s", logs)
	}

	_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})
	return nil
}
