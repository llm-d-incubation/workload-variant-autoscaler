package scaletarget

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type deploymentAccessor struct {
	deployment *appsv1.Deployment
}

func NewDeploymentAccessor(deploy *appsv1.Deployment) ScaleTargetAccessor {
	accessor := deploymentAccessor{
		deployment: deploy,
	}
	return &accessor
}

func (r *deploymentAccessor) GetReplicas() *int32 {
	if r.deployment == nil {
		return nil
	}
	return r.deployment.Spec.Replicas
}

func (r *deploymentAccessor) GetStatusReplicas() int32 {
	// Caller must not pass nil r.deployment
	return r.deployment.Status.Replicas
}

func (r *deploymentAccessor) GetStatusReadyReplicas() int32 {
	// Caller must not pass nil r.deployment
	return r.deployment.Status.ReadyReplicas
}

func (r *deploymentAccessor) GetTotalGPUsPerReplica() int {
	// Caller must not pass nil r.deployment
	total := GetContainersGPUs(r.deployment.Spec.Template.Spec.Containers)
	// Default to 1 GPU if no explicit requests found
	// (common for inference workloads that may not have resource requests)
	if total == 0 {
		return 1
	}
	return total
}

func (r *deploymentAccessor) GetDeletionTimestamp() *v1.Time {
	if r.deployment == nil {
		return nil
	}
	return r.deployment.DeletionTimestamp
}

func (r *deploymentAccessor) GetLeaderPodTemplateSpec() corev1.PodTemplateSpec {
	if r.deployment == nil {
		return corev1.PodTemplateSpec{}
	}
	return r.deployment.Spec.Template
}

func (r *deploymentAccessor) GetWorkerPodTemplateSpec() corev1.PodTemplateSpec {
	return r.GetLeaderPodTemplateSpec()
}

func (r *deploymentAccessor) GetGroupSize() int32 {
	return 1
}

func (r *deploymentAccessor) GetObject() client.Object {
	return r.deployment
}
