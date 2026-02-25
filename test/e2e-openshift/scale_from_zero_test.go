/*scaleFromZeroTestTimeout
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

package e2eopenshift

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
)

// Scale-from-zero test configuration
const (
	scaleFromZeroTestTimeout = 15 * time.Minute
	// Time to wait for deployment to scale to zero after load stops
	scaleDownTimeout = 10 * time.Minute
	// Time to wait for scale-from-zero to detect pending requests and scale up
	scaleUpFromZeroTimeout = 5 * time.Minute
	// Number of requests to send to trigger scale-from-zero
	scaleFromZeroRequestCount = 10
)

var _ = Describe("Scale-From-Zero Test", Ordered, func() {
	var (
		ctx                   context.Context
		scaleToZeroEnabled    bool
		hpaScaleToZeroEnabled bool
		originalConfigExists  bool
		originalConfigData    map[string]string
		testDeploymentName    string
		testNamespace         string
		testGatewayService    string
		testModelID           string
	)

	BeforeAll(func() {
		ctx = context.Background()

		// Verify controller namespace matches what controller expects
		// The controller uses config.SystemNamespace() which checks POD_NAMESPACE env var
		// or defaults to "workload-variant-autoscaler-system"
		expectedSystemNamespace := config.SystemNamespace()
		if controllerNamespace != expectedSystemNamespace {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CONTROLLER_NAMESPACE (%s) does not match controller's expected namespace (%s)\n",
				controllerNamespace, expectedSystemNamespace)
			_, _ = fmt.Fprintf(GinkgoWriter, "  Controller uses POD_NAMESPACE env var if set, otherwise defaults to %s\n",
				config.DefaultNamespace)
			_, _ = fmt.Fprintf(GinkgoWriter, "  Ensure CONTROLLER_NAMESPACE matches the actual controller deployment namespace\n")
		}

		// Use the primary llm-d namespace for testing
		testNamespace = llmDNamespace
		testDeploymentName = getDeploymentName()
		testGatewayService = getGatewayName()
		testModelID = modelID

		// Check if scale-to-zero is enabled
		scaleToZeroEnabled = true // Scale-from-zero requires scale-to-zero to be enabled

		// Check if HPAScaleToZero feature gate is enabled
		hpaScaleToZeroEnabled = isHPAScaleToZeroEnabled(ctx)

		_, _ = fmt.Fprintf(GinkgoWriter, "\n========================================\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "Starting Scale-From-Zero Tests\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "  Scale-to-Zero Enabled: %v\n", scaleToZeroEnabled)
		_, _ = fmt.Fprintf(GinkgoWriter, "  HPA Scale-to-Zero Feature Gate: %v\n", hpaScaleToZeroEnabled)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Controller Namespace: %s\n", controllerNamespace)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Expected System Namespace: %s\n", expectedSystemNamespace)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Test Namespace: %s\n", testNamespace)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Test Deployment: %s\n", testDeploymentName)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Test Gateway: %s\n", testGatewayService)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Test Model ID: %s\n", testModelID)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Scale-to-Zero ConfigMap: %s\n", scaleToZeroConfigMapName)
		_, _ = fmt.Fprintf(GinkgoWriter, "========================================\n\n")

		if !hpaScaleToZeroEnabled {
			Skip("HPAScaleToZero feature gate is not enabled - scale-from-zero requires this feature")
		}

		// Backup existing scale-to-zero ConfigMap if it exists
		By("backing up existing scale-to-zero ConfigMap")
		existingCM, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, scaleToZeroConfigMapName, metav1.GetOptions{})
		if err == nil {
			originalConfigExists = true
			originalConfigData = existingCM.Data
			_, _ = fmt.Fprintf(GinkgoWriter, "Backed up existing scale-to-zero ConfigMap\n")
		} else {
			originalConfigExists = false
			_, _ = fmt.Fprintf(GinkgoWriter, "No existing scale-to-zero ConfigMap found\n")
		}
	})

	Context("Scale-from-zero with pending requests", Ordered, func() {
		var (
			vaName               string
			initialReplicas      int32
			loadJobName          string
			scaleFromZeroJobName string
		)

		AfterAll(func() {
			By("cleaning up test jobs")
			propagationPolicy := metav1.DeletePropagationBackground

			if scaleFromZeroJobName != "" {
				err := k8sClient.BatchV1().Jobs(testNamespace).Delete(ctx, scaleFromZeroJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagationPolicy,
				})
				if err != nil && !errors.IsNotFound(err) {
					_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete trigger job %s: %v\n", scaleFromZeroJobName, err)
				}
			}

			if loadJobName != "" {
				err := k8sClient.BatchV1().Jobs(testNamespace).Delete(ctx, loadJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagationPolicy,
				})
				if err != nil && !errors.IsNotFound(err) {
					_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete load job %s: %v\n", loadJobName, err)
				}
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "Scale-from-zero test completed\n")
		})

		BeforeAll(func() {
			By("configuring scale-to-zero as enabled with short retention period")
			scaleToZeroCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      scaleToZeroConfigMapName,
					Namespace: controllerNamespace,
				},
				Data: map[string]string{
					"default": `enable_scale_to_zero: true
retention_period: 2m`,
				},
			}

			// Delete existing ConfigMap if it exists
			_ = k8sClient.CoreV1().ConfigMaps(controllerNamespace).Delete(ctx, scaleToZeroConfigMapName, metav1.DeleteOptions{})
			time.Sleep(2 * time.Second)

			_, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Create(ctx, scaleToZeroCM, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create scale-to-zero ConfigMap")

			By("finding VariantAutoscaling for the deployment")
			vaList := &v1alpha1.VariantAutoscalingList{}
			err = crClient.List(ctx, vaList, client.InNamespace(testNamespace))
			Expect(err).NotTo(HaveOccurred(), "Should be able to list VariantAutoscalings")

			// Find VA that targets our deployment
			for _, va := range vaList.Items {
				if va.GetScaleTargetName() == testDeploymentName {
					vaName = va.Name
					break
				}
			}

			if vaName == "" {
				Skip("No VariantAutoscaling found for deployment " + testDeploymentName)
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "Found VariantAutoscaling: %s\n", vaName)

			By("recording initial deployment state")
			deploy, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, testDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			initialReplicas = deploy.Status.ReadyReplicas
			_, _ = fmt.Fprintf(GinkgoWriter, "Initial ready replicas: %d\n", initialReplicas)

			loadJobName = fmt.Sprintf("scale-from-zero-load-%d", time.Now().Unix())
			scaleFromZeroJobName = fmt.Sprintf("scale-from-zero-trigger-%d", time.Now().Unix())
		})

		It("should scale deployment to zero when idle", func() {
			By("waiting for deployment to scale to zero (no load)")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, testDeploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				_, _ = fmt.Fprintf(GinkgoWriter, "Current deployment replicas: %d (waiting for 0)\n", deploy.Status.Replicas)

				g.Expect(deploy.Status.Replicas).To(Equal(int32(0)),
					"Deployment should have scaled to 0 replicas when idle")
			}, scaleDownTimeout, 15*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Deployment successfully scaled to zero\n")
		})

		It("should verify VariantAutoscaling recommends zero replicas", func() {
			By("checking VA status shows zero replicas")
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: testNamespace,
				Name:      vaName,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			_, _ = fmt.Fprintf(GinkgoWriter, "VA DesiredOptimizedAlloc.NumReplicas: %d\n",
				va.Status.DesiredOptimizedAlloc.NumReplicas)

			Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(0),
				"VariantAutoscaling should recommend 0 replicas when idle")
		})

		It("should detect pending requests and trigger scale-from-zero", func() {
			By("creating a job to send requests while deployment is at zero")
			// This job will send requests that will queue up in EPP's flow control queue
			// The scale-from-zero engine should detect these pending requests and scale up
			job := createScaleFromZeroTriggerJob(scaleFromZeroJobName, testNamespace, testGatewayService, testModelID, scaleFromZeroRequestCount)

			_, err := k8sClient.BatchV1().Jobs(testNamespace).Create(ctx, job, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create scale-from-zero trigger job")

			_, _ = fmt.Fprintf(GinkgoWriter, "Created scale-from-zero trigger job: %s\n", scaleFromZeroJobName)

			By("waiting for job pod to be running and sending requests")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
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
					Namespace: testNamespace,
					Name:      vaName,
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
				// 	g.Expect(condition.Reason).To(Equal("ScaleFromZeroMode"),
				// 		"Condition reason should indicate scale-from-zero mode")
				// }
			}, scaleUpFromZeroTimeout, 10*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Scale-from-zero engine detected pending requests and recommended scale-up\n")
		})

		It("should scale deployment up from zero", func() {
			By("monitoring deployment for actual scale-up from zero")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, testDeploymentName, metav1.GetOptions{})
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
				job, err := k8sClient.BatchV1().Jobs(testNamespace).Get(ctx, scaleFromZeroJobName, metav1.GetOptions{})
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
				Namespace: testNamespace,
				Name:      vaName,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			// Verify the decision was recorded
			Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0),
				"VA should show scaled-up state")

			// Check condition
			condition := findCondition(va.Status.Conditions, v1alpha1.TypeOptimizationReady)
			Expect(condition).NotTo(BeNil(), "OptimizationReady condition should exist")
			Expect(condition.Status).To(Equal(metav1.ConditionTrue), "Condition should be True")
			Expect(condition.Reason).To(Equal("ScaleFromZeroMode"), "Reason should indicate scale-from-zero")

			_, _ = fmt.Fprintf(GinkgoWriter, "VA status correctly reflects scale-from-zero decision\n")
		})
	})

	Context("Scale-from-zero with multiple concurrent requests", Ordered, func() {
		var (
			vaName                string
			concurrentJobBaseName string
			numConcurrentJobs     = 3
			requestsPerJob        = 5
		)

		AfterAll(func() {
			if concurrentJobBaseName != "" {
				By("cleaning up concurrent test jobs")
				propagationPolicy := metav1.DeletePropagationBackground
				for i := 1; i <= numConcurrentJobs; i++ {
					jobName := fmt.Sprintf("%s-%d", concurrentJobBaseName, i)
					err := k8sClient.BatchV1().Jobs(testNamespace).Delete(ctx, jobName, metav1.DeleteOptions{
						PropagationPolicy: &propagationPolicy,
					})
					if err != nil && !errors.IsNotFound(err) {
						_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete concurrent job %s: %v\n", jobName, err)
					}
				}
			}
		})

		BeforeAll(func() {
			By("ensuring scale-to-zero is enabled")
			// ConfigMap should already be set from previous context

			By("finding VariantAutoscaling for the deployment")
			vaList := &v1alpha1.VariantAutoscalingList{}
			err := crClient.List(ctx, vaList, client.InNamespace(testNamespace))
			Expect(err).NotTo(HaveOccurred(), "Should be able to list VariantAutoscalings")

			for _, va := range vaList.Items {
				if va.GetScaleTargetName() == testDeploymentName {
					vaName = va.Name
					break
				}
			}

			Expect(vaName).NotTo(BeEmpty(), "VariantAutoscaling should exist")
			concurrentJobBaseName = fmt.Sprintf("scale-from-zero-concurrent-%d", time.Now().Unix())
		})

		It("should wait for deployment to scale to zero again", func() {
			By("waiting for deployment to scale back to zero")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, testDeploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deploy.Status.Replicas).To(Equal(int32(0)))
			}, scaleDownTimeout, 15*time.Second).Should(Succeed())
		})

		It("should handle multiple concurrent requests triggering scale-from-zero", func() {
			By("creating multiple concurrent jobs to trigger scale-from-zero")
			for i := 1; i <= numConcurrentJobs; i++ {
				jobName := fmt.Sprintf("%s-%d", concurrentJobBaseName, i)
				job := createScaleFromZeroTriggerJob(jobName, testNamespace, testGatewayService, testModelID, requestsPerJob)

				_, err := k8sClient.BatchV1().Jobs(testNamespace).Create(ctx, job, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, _ = fmt.Fprintf(GinkgoWriter, "Created concurrent job %d: %s\n", i, jobName)
			}

			By("verifying scale-from-zero handles concurrent load")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, testDeploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">", 0),
					"Deployment should scale up to handle concurrent requests")
			}, scaleUpFromZeroTimeout, 10*time.Second).Should(Succeed())

			By("waiting for all concurrent jobs to complete")
			Eventually(func(g Gomega) {
				completedCount := 0
				for i := 1; i <= numConcurrentJobs; i++ {
					jobName := fmt.Sprintf("%s-%d", concurrentJobBaseName, i)
					job, err := k8sClient.BatchV1().Jobs(testNamespace).Get(ctx, jobName, metav1.GetOptions{})
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
	})

	AfterAll(func() {
		By("restoring original scale-to-zero ConfigMap state")
		if originalConfigExists {
			existingCM, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, scaleToZeroConfigMapName, metav1.GetOptions{})
			if err == nil {
				existingCM.Data = originalConfigData
				_, err = k8sClient.CoreV1().ConfigMaps(controllerNamespace).Update(ctx, existingCM, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, _ = fmt.Fprintf(GinkgoWriter, "Restored original scale-to-zero ConfigMap\n")
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(controllerNamespace).Delete(ctx, scaleToZeroConfigMapName, metav1.DeleteOptions{})
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "\n========================================\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "Scale-From-Zero tests completed\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "========================================\n\n")
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

# Send requests with delays to allow scale-from-zero engine to detect them
SENT=0
SUCCESS=0
FAILED=0

while [ $SENT -lt %d ]; do
	 echo "Sending request $((SENT + 1)) / %d..."
	 
	 RESPONSE=$(curl -s -w "\n%%{http_code}" --max-time 180 -X POST http://%s:80/v1/completions \
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
`, numRequests, gatewayService, modelID, numRequests, numRequests, gatewayService, modelID)

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

// findCondition finds a condition by type in the conditions list
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
