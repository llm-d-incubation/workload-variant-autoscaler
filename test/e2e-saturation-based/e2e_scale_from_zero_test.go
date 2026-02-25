/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2esaturation

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils/resources"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Scale-from-zero test constants
const (
	scaleFromZeroTestTimeout  = 15 * time.Minute
	scaleUpFromZeroTimeout    = 5 * time.Minute
	scaleFromZeroRequestCount = 10
)

// Test workload-variant-autoscaler - Scale-From-Zero Feature
// This test validates that the scale-from-zero engine correctly detects pending requests
// in the EPP flow control queue and scales up deployments from zero replicas.
// Note: This test assumes scale-to-zero is already configured and the deployment starts at 0 replicas.
var _ = Describe("Test workload-variant-autoscaler - Scale-From-Zero Feature", Ordered, func() {
	var (
		name            string
		namespace       string
		deployName      string
		serviceName     string
		gatewayService  string
		appLabel        string
		initialReplicas int32
		port            int
		modelName       string
		ctx             context.Context
	)

	BeforeAll(func() {
		ctx = context.Background()
		name = "llm-d-sim-sfz" // Scale-from-zero test
		deployName = name + "-deployment"
		serviceName = name + "-service"

		appLabel = name
		namespace = llmDNamespace
		port = 8000
		modelName = llamaModelId
		gatewayService = "infra-sim-inference-gateway"

		// Start with 0 replicas to test scale-from-zero
		initialReplicas = 0

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

		// Skip scalefromzero test if feature gate is not enabled
		if !utils.IsHPAScaleToZeroEnabled(ctx, k8sClient, GinkgoWriter) {
			Skip("HPAScaleToZero feature gate is not enabled; skipping scale from zero test")
		}

		By("verifying saturation-scaling ConfigMap exists before creating VA")
		Eventually(func(g Gomega) {
			cm, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, saturationConfigMapName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("saturation ConfigMap %s should exist in namespace %s", saturationConfigMapName, controllerNamespace))
			g.Expect(cm.Data).To(HaveKey("default"), "saturation ConfigMap should have 'default' configuration")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("ensuring unique app label for deployment and service")
		utils.ValidateAppLabelUniqueness(namespace, appLabel, k8sClient, crClient)
		utils.ValidateVariantAutoscalingUniqueness(namespace, modelName, a100Acc, crClient)

		By("creating llm-d-sim deployment with 0 replicas")
		deployment := resources.CreateLlmdSimDeployment(namespace, deployName, modelName, appLabel, fmt.Sprintf("%d", port), avgTTFT, avgITL, initialReplicas)
		_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", deployName))

		By("creating service to expose llm-d-sim deployment")
		service := resources.CreateLlmdSimService(namespace, serviceName, appLabel, 30005, port)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Service: %s", serviceName))

		By("creating VariantAutoscaling resource (deployment starts at 0 replicas)")
		variantAutoscaling := utils.CreateVariantAutoscalingResource(namespace, name, deployName, modelName, a100Acc, 10.0)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create VariantAutoscaling: %s", name))

		_, _ = fmt.Fprintf(GinkgoWriter, "Scale-from-zero test setup complete\n")
	})

	Context("Initial state verification", func() {
		It("should have VariantAutoscaling resource created", func() {
			By("verifying VariantAutoscaling exists")
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      name,
			}, va)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling: %s", name))
			Expect(va.Spec.ModelID).To(Equal(modelName))

			_, _ = fmt.Fprintf(GinkgoWriter, "VariantAutoscaling resource verified: %s\n", name)
		})

		It("should verify deployment starts at zero replicas", func() {
			By("checking deployment has 0 replicas")
			deploy, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			specReplicas := int32(1)
			if deploy.Spec.Replicas != nil {
				specReplicas = *deploy.Spec.Replicas
			}

			Expect(specReplicas).To(Equal(int32(0)), "Deployment should start with 0 replicas")
			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment verified at 0 replicas\n")
		})
	})

	Context("Scale-from-zero with pending requests", func() {
		var scaleFromZeroJobName string

		BeforeAll(func() {
			scaleFromZeroJobName = fmt.Sprintf("scale-from-zero-trigger-%d", time.Now().Unix())
		})

		It("should detect pending requests and trigger scale-from-zero", func() {
			By("creating a job to send requests while deployment is at zero")
			job := createScaleFromZeroTriggerJob(scaleFromZeroJobName, namespace, gatewayService, modelName, scaleFromZeroRequestCount)

			_, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create scale-from-zero trigger job")

			_, _ = fmt.Fprintf(GinkgoWriter, "Created scale-from-zero trigger job: %s\n", scaleFromZeroJobName)

			By("waiting for job pod to be running and sending requests")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("job-name=%s", scaleFromZeroJobName),
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(podList.Items)).To(BeNumerically(">", 0), "Job pod should exist")

				pod := podList.Items[0]
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), "Job pod should be running")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Job pod is running and sending requests\n")

			By("monitoring VariantAutoscaling for scale-from-zero decision")
			Eventually(func(g Gomega) {
				va := &v1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Namespace: namespace,
					Name:      name,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				optimized := int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
				_, _ = fmt.Fprintf(GinkgoWriter, "VA DesiredOptimizedAlloc.NumReplicas: %d (waiting for > 0)\n", optimized)

				// Scale-from-zero engine should detect pending requests and recommend scaling up
				g.Expect(optimized).To(BeNumerically(">", 0),
					"VariantAutoscaling should recommend scaling up from zero due to pending requests")

				// // Check for scale-from-zero condition
				// condition := findCondition(va.Status.Conditions, v1alpha1.TypeOptimizationReady)
				// if condition != nil {
				// 	_, _ = fmt.Fprintf(GinkgoWriter, "OptimizationReady condition: status=%s, reason=%s, message=%s\n",
				// 		condition.Status, condition.Reason, condition.Message)

				// 	// Verify the condition indicates scale-from-zero mode
				// 	g.Expect(condition.Reason).To(Equal("ScaleFromZero"),
				// 		"Condition reason should indicate scale-from-zero mode")
				// }
			}, scaleUpFromZeroTimeout, 10*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Scale-from-zero engine detected pending requests and recommended scale-up\n")
		})

		It("should scale deployment up from zero", func() {
			By("monitoring deployment for actual scale-up from zero")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				currentReplicas := deploy.Status.Replicas
				readyReplicas := deploy.Status.ReadyReplicas
				_, _ = fmt.Fprintf(GinkgoWriter, "Current replicas: %d, ready: %d (waiting for > 0)\n",
					currentReplicas, readyReplicas)

				g.Expect(currentReplicas).To(BeNumerically(">", 0),
					"Deployment should have scaled up from zero")
				g.Expect(readyReplicas).To(BeNumerically(">", 0),
					"Deployment should have at least one ready replica")
			}, scaleUpFromZeroTimeout, 10*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment successfully scaled up from zero\n")
		})

		It("should process the pending requests successfully", func() {
			By("waiting for the trigger job to complete")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, scaleFromZeroJobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				_, _ = fmt.Fprintf(GinkgoWriter, "Job status: succeeded=%d, failed=%d, active=%d\n",
					job.Status.Succeeded, job.Status.Failed, job.Status.Active)

				// Job should complete successfully (requests were processed after scale-up)
				g.Expect(job.Status.Succeeded).To(BeNumerically(">=", 1),
					"Job should complete successfully after deployment scaled up")
			}, scaleFromZeroTestTimeout, 15*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Pending requests were processed successfully after scale-up\n")
		})

		It("should verify scale-from-zero metrics in VariantAutoscaling status", func() {
			By("checking VA status for scale-from-zero indicators")
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      name,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			// Verify the decision was recorded
			Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0),
				"VA should show scaled-up state")

			// condition := findCondition(va.Status.Conditions, v1alpha1.TypeOptimizationReady)
			// Expect(condition).NotTo(BeNil(), "OptimizationReady condition should exist")
			// Expect(condition.Status).To(Equal(metav1.ConditionTrue), "Condition should be True")
			// Expect(condition.Reason).To(Equal("ScaleFromZero"), "Reason should indicate scale-from-zero")

			_, _ = fmt.Fprintf(GinkgoWriter, "VA status correctly reflects scale-from-zero decision\n")
		})

		AfterAll(func() {
			By("cleaning up scale-from-zero trigger job")
			propagationPolicy := metav1.DeletePropagationBackground
			_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, scaleFromZeroJobName, metav1.DeleteOptions{
				PropagationPolicy: &propagationPolicy,
			})
		})
	})

	Context("Multiple concurrent scale-from-zero requests", func() {
		var (
			concurrentJobBaseName string
			numConcurrentJobs     = 3
			requestsPerJob        = 5
		)

		BeforeAll(func() {
			concurrentJobBaseName = fmt.Sprintf("scale-from-zero-concurrent-%d", time.Now().Unix())
		})

		It("should handle multiple concurrent requests triggering scale-from-zero", func() {
			By("manually scaling deployment back to 0 for concurrent test")
			deploy, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicas := int32(0)
			deploy.Spec.Replicas = &replicas
			_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to reach 0 replicas")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deploy.Status.Replicas).To(Equal(int32(0)))
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment at 0 replicas, starting concurrent test\n")

			By("creating multiple concurrent jobs to trigger scale-from-zero")
			for i := 1; i <= numConcurrentJobs; i++ {
				jobName := fmt.Sprintf("%s-%d", concurrentJobBaseName, i)
				job := createScaleFromZeroTriggerJob(jobName, namespace, gatewayService, modelName, requestsPerJob)

				_, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, _ = fmt.Fprintf(GinkgoWriter, "Created concurrent job %d: %s\n", i, jobName)
			}

			By("verifying scale-from-zero handles concurrent load")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				_, _ = fmt.Fprintf(GinkgoWriter, "Current ready replicas: %d (waiting for > 0)\n", deploy.Status.ReadyReplicas)

				g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">", 0),
					"Deployment should scale up to handle concurrent requests")
			}, scaleUpFromZeroTimeout, 10*time.Second).Should(Succeed())

			By("waiting for all concurrent jobs to complete")
			Eventually(func(g Gomega) {
				completedCount := 0
				for i := 1; i <= numConcurrentJobs; i++ {
					jobName := fmt.Sprintf("%s-%d", concurrentJobBaseName, i)
					job, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
					if err == nil && job.Status.Succeeded >= 1 {
						completedCount++
					}
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "Completed jobs: %d / %d\n", completedCount, numConcurrentJobs)
				g.Expect(completedCount).To(Equal(numConcurrentJobs),
					"All concurrent jobs should complete successfully")
			}, scaleFromZeroTestTimeout, 15*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Scale-from-zero successfully handled concurrent requests\n")
		})

		AfterAll(func() {
			By("cleaning up concurrent test jobs")
			propagationPolicy := metav1.DeletePropagationBackground
			for i := 1; i <= numConcurrentJobs; i++ {
				jobName := fmt.Sprintf("%s-%d", concurrentJobBaseName, i)
				_ = k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
					PropagationPolicy: &propagationPolicy,
				})
			}
		})
	})

	AfterAll(func() {
		By("cleaning up scale-from-zero test resources")

		// Delete VariantAutoscaling resource
		va := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, va)
		if err == nil {
			err = crClient.Delete(ctx, va)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling: %s", name))
		}

		// Delete Service
		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", serviceName))

		// Delete Deployment
		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", deployName))

		_, _ = fmt.Fprintf(GinkgoWriter, "Cleanup completed for scale-from-zero test\n")
	})
})

// createScaleFromZeroTriggerJob creates a job that sends requests to trigger scale-from-zero
// The job sends requests slowly to allow time for the scale-from-zero engine to detect them
func createScaleFromZeroTriggerJob(name, namespace, gatewayService, modelID string, numRequests int) *batchv1.Job {
	backoffLimit := int32(3)

	script := fmt.Sprintf(`#!/bin/sh
echo "Scale-from-zero trigger job starting..."
echo "Sending %d requests to gateway %s:80"
echo "Model ID: %s"

# Wait for gateway to be reachable
MAX_RETRIES=30
RETRY_DELAY=5
CONNECTED=false

# for i in $(seq 1 $MAX_RETRIES); do
#   if curl -s -o /dev/null -w "%%{http_code}" http://%s:80/v1/models 2>/dev/null | grep -q 200; then
#     echo "Gateway is reachable on attempt $i"
#     CONNECTED=true
#     break
#   fi
#   echo "Attempt $i failed, retrying in ${RETRY_DELAY}s..."
#   echo "Gateway address: http://%s-istio:80/v1/models"
#   sleep $RETRY_DELAY
# done

# if [ "$CONNECTED" != "true" ]; then
#   echo "ERROR: Cannot connect to gateway after $MAX_RETRIES attempts"
#   exit 1
# fi

# Send requests with delays to allow scale-from-zero engine to detect them
SENT=0
SUCCESS=0
FAILED=0

while [ $SENT -lt %d ]; do
	 echo "Sending request $((SENT + 1)) / %d..."

	 RESPONSE=$(curl -s -w "\n%%{http_code}" --max-time 180 -X POST http://%s-istio:80/v1/completions \
	   -H "Content-Type: application/json" \
	   -d '{"model":"%s","prompt":"Test prompt for scale-from-zero","max_tokens":50}' 2>&1)

	 HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

	 if [ "$HTTP_CODE" = "200" ]; then
	   SUCCESS=$((SUCCESS + 1))
	   echo "Request $((SENT + 1)) succeeded (HTTP $HTTP_CODE)"
	 else
	   FAILED=$((FAILED + 1))
	   echo "Request $((SENT + 1)) failed (HTTP $HTTP_CODE)"
	 fi

	 SENT=$((SENT + 1))

	 # Small delay between requests to allow scale-from-zero engine to detect pending requests
	 sleep 2
done

echo "Job completed: sent=$SENT, success=$SUCCESS, failed=$FAILED"

# Consider job successful if at least some requests succeeded
if [ $SUCCESS -gt 0 ]; then
	 exit 0
else
	 exit 1
fi
`, numRequests, gatewayService, modelID, gatewayService, gatewayService, numRequests, numRequests, gatewayService, modelID)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"test-type": "scale-from-zero",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"test-type": "scale-from-zero",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "scale-from-zero-trigger",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{script},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
									corev1.ResourceCPU:    resource.MustParse("500m"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

// // findCondition finds a condition by type in the conditions list
// func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
// 	for i := range conditions {
// 		if conditions[i].Type == conditionType {
// 			return &conditions[i]
// 		}
// 	}
// 	return nil
// }
