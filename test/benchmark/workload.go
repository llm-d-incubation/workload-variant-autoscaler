package benchmark

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// CreateGuideLLMJobWithArgs launches a GuideLLM Job with the specified arguments.
func CreateGuideLLMJobWithArgs(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL, modelID string,
) error {
	image := "ghcr.io/vllm-project/guidellm:v0.5.4"

	args := []string{
		"benchmark",
		"--target", targetServiceURL,
		"--model", modelID,
		"--profile", "poisson",
		"--rate", "20",
		"--max-seconds", "600",
		"--random-seed", "42",
		"--request-type", "text_completions",
		"--data", "prompt_tokens=4000,output_tokens=1000",
		"--output-path", "/tmp/benchmarks.json",
		"--backend-kwargs", `'{"validate_backend": false}'`,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-load",
			Namespace: namespace,
			Labels: map[string]string{
				"app":           name + "-load",
				"test-resource": "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":           name + "-load",
						"test-resource": "true",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "load-gen",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c"},
							Args: []string{
								fmt.Sprintf("echo 'Waiting 30s for gateway routing to propagate...' && sleep 30 && guidellm %s && echo '=== BENCHMARK JSON ===' && cat /tmp/benchmarks.json", strings.Join(args, " ")),
							},
							Env: []corev1.EnvVar{
								{Name: "HF_HOME", Value: "/tmp"},
								{
									Name: "HF_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "llm-d-hf-token"},
											Key:                  "HF_TOKEN",
											Optional:             ptr.To(true),
										},
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, name+"-load", metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})

	_, createErr := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	return createErr
}
