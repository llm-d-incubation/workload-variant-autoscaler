package benchmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// WorkloadScenario defines the GuideLLM workload parameters loaded from scenarios/ YAML files.
type WorkloadScenario struct {
	Name         string `json:"name" yaml:"name"`
	Description  string `json:"description,omitempty" yaml:"description,omitempty"`
	PromptTokens int    `json:"promptTokens" yaml:"promptTokens"`
	OutputTokens int    `json:"outputTokens" yaml:"outputTokens"`
	Rate         int    `json:"rate" yaml:"rate"`
	MaxSeconds   int    `json:"maxSeconds" yaml:"maxSeconds"`
	Profile      string `json:"profile" yaml:"profile"`
	RequestType  string `json:"requestType" yaml:"requestType"`
}

// scenariosDir returns the absolute path to the scenarios/ directory relative to this source file.
func scenariosDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "scenarios")
}

// defaultScenario returns the fallback prefill_heavy defaults used when no
// scenario file is found or when YAML parsing fails.
func defaultScenario() WorkloadScenario {
	return WorkloadScenario{
		Name:         "Prefill Heavy (default)",
		PromptTokens: 4000,
		OutputTokens: 1000,
		Rate:         20,
		MaxSeconds:   600,
		Profile:      "poisson",
		RequestType:  "text_completions",
	}
}

// LoadScenario loads a WorkloadScenario from test/benchmark/scenarios/<name>.yaml.
// If the named file doesn't exist, it falls back to prefill_heavy defaults.
func LoadScenario(name string) WorkloadScenario {
	if name == "" {
		name = "prefill_heavy"
	}

	path := filepath.Join(scenariosDir(), name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// Fallback to prefill_heavy defaults (preserves backward compatibility)
		return defaultScenario()
	}

	var scenario WorkloadScenario
	if parseErr := yaml.Unmarshal(data, &scenario); parseErr != nil {
		// On parse error, return defaults
		return defaultScenario()
	}

	return scenario
}

// CreateGuideLLMJobWithArgs launches a GuideLLM Job with parameters from the given WorkloadScenario.
func CreateGuideLLMJobWithArgs(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, name, targetServiceURL, modelID string,
	scenario WorkloadScenario,
) error {
	image := "ghcr.io/vllm-project/guidellm:v0.5.4"

	dataArg := "prompt_tokens=" + strconv.Itoa(scenario.PromptTokens) + ",output_tokens=" + strconv.Itoa(scenario.OutputTokens)

	args := []string{
		"benchmark",
		"--target", targetServiceURL,
		"--model", modelID,
		"--profile", scenario.Profile,
		"--rate", strconv.Itoa(scenario.Rate),
		"--max-seconds", strconv.Itoa(scenario.MaxSeconds),
		"--random-seed", "42",
		"--request-type", scenario.RequestType,
		"--data", dataArg,
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
