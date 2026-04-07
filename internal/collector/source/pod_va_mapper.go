package source

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// PodVAMapper maps pod names to their corresponding VariantAutoscaling objects.
type PodVAMapper struct {
	k8sClient client.Client
}

// NewPodVAMapper creates a new PodVAMapper.
func NewPodVAMapper(k8sClient client.Client) *PodVAMapper {
	return &PodVAMapper{
		k8sClient: k8sClient,
	}
}

// FindVAForPod finds the VariantAutoscaling object for a Pod by:
// 1. finding the Deployment/LWS owning the Pod
// 2. finding the VariantAutoscaling that targets that Deployment/LWS, using indexed lookups.
// Returns the VariantAutoscaling name if found, empty string otherwise.
func (m *PodVAMapper) FindVAForPod(
	ctx context.Context,
	podName string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) string {
	logger := ctrl.LoggerFrom(ctx)

	scaleTargetKind, scaleTargetName := m.findScaleTargetNameForPod(ctx, podName, namespace, scaleTargets)
	if scaleTargetKind == "" || scaleTargetName == "" {
		return ""
	}

	// Use indexed lookup for VariantAutoscaling targeting this Deployment/LWS
	var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	var err error
	switch scaleTargetKind {
	case "Deployment":
		va, err = indexers.FindVAForDeployment(ctx, m.k8sClient, scaleTargetName, namespace)
		if err != nil {
			logger.V(logging.DEBUG).Error(err, "failed to find VariantAutoscaling for scale target", "scaleTarget", scaleTargetName, "namespace", namespace)
			return ""
		}
	case "LeaderWorkerSet":
		va, err = indexers.FindVAForLeaderWorkerSet(ctx, m.k8sClient, scaleTargetName, namespace)
		if err != nil {
			logger.V(logging.DEBUG).Error(err, "failed to find VariantAutoscaling for scale target", "scaleTarget", scaleTargetName, "namespace", namespace)
			return ""
		}
	}

	if va == nil {
		logger.V(logging.DEBUG).Info("no VariantAutoscaling matched for scale target", "scaleTarget", scaleTargetName, "namespace", namespace)
		return ""
	}

	return va.Name
}

// findScaleTargetNameForPod finds which Deployment/LWS owns a Pod by traversing owner references.
func (m *PodVAMapper) findScaleTargetNameForPod(
	ctx context.Context,
	podName string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) (string, string) {
	logger := ctrl.LoggerFrom(ctx)

	pod := &corev1.Pod{}
	if err := m.k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); err != nil {
		logger.V(logging.DEBUG).Error(err, "failed to get pod", "pod", podName, "namespace", namespace)
		return "", ""
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		logger.V(logging.DEBUG).Info("Pod has no owner", "pod", podName, "namespace", namespace)
		return "", ""
	}

	var controllee metav1.Object
	switch owner.Kind {
	case "ReplicaSet":
		rs := &appsv1.ReplicaSet{}
		if err := m.k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: owner.Name}, rs); err != nil {
			logger.V(logging.DEBUG).Error(err, "failed to get ReplicaSet", "replicaset", owner.Name, "namespace", namespace)
			return "", ""
		}
		controllee = rs
	case "StatefulSet":
		rs := &appsv1.StatefulSet{}
		if err := m.k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: owner.Name}, rs); err != nil {
			logger.V(logging.DEBUG).Error(err, "failed to get StatefulSet", "statefulset", owner.Name, "namespace", namespace)
			return "", ""
		}
		controllee = rs
	default:
		logger.V(logging.DEBUG).Info("Pod has no ReplicaSet or StatefulSet owner", "pod", podName, "namespace", namespace)
		return "", ""
	}

	rsOwner := metav1.GetControllerOf(controllee)
	if rsOwner == nil || (rsOwner.Kind != "Deployment" && rsOwner.Kind != "LeaderWorkerSet") {
		logger.V(logging.DEBUG).Info("Either ReplicaSet has no Deployment owner or StatefulSet has no LeaderWorkerSet owner", "replicaset/statefulset", owner.Name, "namespace", namespace)
		return "", ""
	}

	// Verify the Deployment/LWS is in our map of tracked Deployments/LWSs
	key := namespace + "/" + rsOwner.Name
	if scaleTarget, ok := scaleTargets[key]; ok && scaleTarget != nil {
		if scaleTarget.GetNamespace() == namespace {
			return rsOwner.Kind, rsOwner.Name
		}
	}
	return "", ""
}
