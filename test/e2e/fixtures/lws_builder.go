package fixtures

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// EnsureModelServiceLWS creates or replaces a LeaderWorkerSet for model service (idempotent for test setup).
func EnsureModelServiceLWS(ctx context.Context, crClient client.Client, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int, groupSize int32) error {
	lwsName := name + "-decode"
	desiredLWS := buildModelServiceLWS(namespace, name, poolName, modelID, useSimulator, maxNumSeqs, groupSize)

	// Check if LWS already exists
	existingLWS := &lwsv1.LeaderWorkerSet{}
	err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: namespace}, existingLWS)
	if err == nil {
		// LWS exists, check if it's ready
		if existingLWS.Status.ReadyReplicas > 0 && modelServiceLWSMatchesDesired(*existingLWS, *desiredLWS) {
			return nil
		}
		// Not ready, delete and recreate
		propagationPolicy := metav1.DeletePropagationForeground
		deleteErr := crClient.Delete(ctx, existingLWS, &client.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) && !errors.IsConflict(deleteErr) {
			return fmt.Errorf("delete existing LWS %s: %w", lwsName, deleteErr)
		}
		// Wait for deletion
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		for {
			checkErr := crClient.Get(waitCtx, client.ObjectKey{Name: lwsName, Namespace: namespace}, &lwsv1.LeaderWorkerSet{})
			if errors.IsNotFound(checkErr) {
				break
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timeout waiting for LWS %s to be deleted", lwsName)
			}
			time.Sleep(2 * time.Second)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing LWS %s: %w", lwsName, err)
	}

	err = crClient.Create(ctx, desiredLWS)
	if err != nil && errors.IsAlreadyExists(err) {
		propagationPolicy := metav1.DeletePropagationForeground
		_ = crClient.Delete(ctx, desiredLWS, &client.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		time.Sleep(2 * time.Second)
		err = crClient.Create(ctx, desiredLWS)
	}
	return err
}

func modelServiceLWSMatchesDesired(existing, desired lwsv1.LeaderWorkerSet) bool {
	return reflect.DeepEqual(existing.Spec, desired.Spec) &&
		reflect.DeepEqual(existing.Labels, desired.Labels) &&
		reflect.DeepEqual(existing.Annotations, desired.Annotations)
}

// DeleteModelServiceLWS deletes the LeaderWorkerSet for model service. Idempotent; ignores NotFound.
func DeleteModelServiceLWS(ctx context.Context, crClient client.Client, namespace, name string) error {
	lwsName := name + "-decode"
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lwsName,
			Namespace: namespace,
		},
	}
	err := crClient.Delete(ctx, lws)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete LeaderWorkerSet %s: %w", lwsName, err)
	}
	return nil
}

func buildModelServiceLWS(namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int, groupSize int32) *lwsv1.LeaderWorkerSet {
	appLabel := name + "-decode"
	image := "ghcr.io/llm-d/llm-d-inference-sim:v0.7.1"
	if !useSimulator {
		image = "ghcr.io/llm-d/llm-d-cuda-dev:latest"
	}
	args := buildModelServerArgs(modelID, useSimulator, maxNumSeqs)
	labels := map[string]string{
		"app":                       appLabel,
		"llm-d.ai/inferenceServing": "true",
		"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
		"llm-d.ai/model-pool":       poolName,
		"test-resource":             "true",
	}

	envVars := []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
	}
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	if !useSimulator {
		envVars = append(envVars,
			corev1.EnvVar{Name: "HF_HOME", Value: "/model-cache"},
			corev1.EnvVar{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "llm-d-hf-token"},
					Key:                  "HF_TOKEN",
				},
			}},
		)
		volumes = []corev1.Volume{
			{Name: "model-storage", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourcePtr("100Gi")}}},
			{Name: "torch-compile-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "metrics-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "triton-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
		volumeMounts = []corev1.VolumeMount{
			{Name: "model-storage", MountPath: "/model-cache"},
			{Name: "torch-compile-cache", MountPath: "/.cache"},
			{Name: "metrics-volume", MountPath: "/.config"},
			{Name: "triton-cache", MountPath: "/.triton"},
		}
	}

	// Leader template (runs vLLM API server)
	leaderTemplate := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            appLabel,
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            args,
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
					},
					Env:          envVars,
					Resources:    buildModelServiceResources(useSimulator),
					VolumeMounts: volumeMounts,
				},
			},
			Volumes:       volumes,
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}

	// Worker template (can be same as leader for simple cases, or different for distributed inference)
	workerTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            appLabel + "-worker",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            args,
					Env:             envVars,
					Resources:       buildModelServiceResources(useSimulator),
					VolumeMounts:    volumeMounts,
				},
			},
			Volumes:       volumes,
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}

	startupPolicy := lwsv1.LeaderReadyStartupPolicy

	return &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appLabel,
			Namespace: namespace,
		},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas:      ptr.To(int32(1)),
			StartupPolicy: startupPolicy,
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size:           ptr.To(groupSize),
				LeaderTemplate: leaderTemplate,
				WorkerTemplate: workerTemplate,
			},
		},
	}
}
