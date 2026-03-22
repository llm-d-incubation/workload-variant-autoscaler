package scaletarget

import (
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type lwsAccessor struct {
	lws *lwsv1.LeaderWorkerSet
}

func NewLWSAccessor(lws *lwsv1.LeaderWorkerSet) ScaleTargetAccessor {
	accessor := lwsAccessor{
		lws: lws,
	}
	return &accessor
}

func (r *lwsAccessor) GetReplicas() *int32 {
	if r.lws == nil {
		return nil
	}
	return r.lws.Spec.Replicas
}

func (r *lwsAccessor) GetStatusReplicas() int32 {
	if r.lws == nil {
		return 1 // K8S fallback?
	}
	return r.lws.Status.Replicas
}

func (r *lwsAccessor) GetStatusReadyReplicas() int32 {
	if r.lws == nil {
		return 1 // K8S fallback?
	}
	return r.lws.Status.ReadyReplicas
}

// leader_GPUs + (Size - 1) * worker_GPUs.
func (r *lwsAccessor) GetTotalGPUsPerReplica() int {
	if r.lws == nil {
		return 1
	}

	leaderGPUs := 0
	if r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderGPUs = GetContainersGPUs(r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers)
	}
	// Default to 1 GPU if no explicit requests found
	// (common for inference workloads that may not have resource requests)
	if leaderGPUs == 0 {
		leaderGPUs = 1
	}

	workerGPUs := GetContainersGPUs(r.lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers)
	return leaderGPUs + (int(r.GetGroupSize())-1)*workerGPUs
}

func (r *lwsAccessor) GetDeletionTimestamp() *v1.Time {
	if r.lws == nil {
		return nil
	}
	return r.lws.DeletionTimestamp
}

func (r *lwsAccessor) GetLeaderPodTemplateSpec() corev1.PodTemplateSpec {
	if r.lws == nil || r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate == nil {
		return corev1.PodTemplateSpec{}
	}
	return *r.lws.Spec.LeaderWorkerTemplate.LeaderTemplate
}

func (r *lwsAccessor) GetWorkerPodTemplateSpec() corev1.PodTemplateSpec {
	if r.lws == nil {
		return corev1.PodTemplateSpec{}
	}
	return r.lws.Spec.LeaderWorkerTemplate.WorkerTemplate
}

func (r *lwsAccessor) GetGroupSize() int32 {
	if r.lws == nil || r.lws.Spec.LeaderWorkerTemplate.Size == nil {
		return 1 // K8S fallback
	}
	return *r.lws.Spec.LeaderWorkerTemplate.Size
}

func (r *lwsAccessor) GetObject() client.Object {
	return r.lws
}
