package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Scale-from-zero test validates that the WVA controller correctly detects pending requests
// and scales up deployments from zero replicas when the HPAScaleToZero feature gate is enabled.
var _ = Describe("Scale-From-Zero Feature", Label("smoke", "full"), Ordered, func() {
	var (
		poolName         = "scale-from-zero-pool"
		modelServiceName = "scale-from-zero-ms"
		vaName           = "scale-from-zero-va"
		hpaName          = "scale-from-zero-hpa"
	)

	BeforeAll(func() {
		// Skip if HPAScaleToZero is not enabled
		// if !cfg.ScaleToZeroEnabled {
		// 	Skip("HPAScaleToZero feature gate is not enabled; skipping scale-from-zero test")
		// }

		// Note: InferencePool should already exist from infra-only deployment
		// We no longer create InferencePools in individual tests

		// Wait for InferencePool to be reconciled and registered in the datastore
		// The scale-from-zero engine needs the InferencePool to be in the datastore
		// to find the EPP and query flow control queue metrics.
		// The InferencePool reconciler should have already reconciled it as part of infrastructure.
		// check for EPP service by name and pods by inferencepool label.
		By("Waiting for InferencePool to be reconciled (allows time for controller to register it in datastore)")
		// Wait for the EPP service to exist
		Eventually(func(g Gomega) {
			_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, "gaie-sim-epp", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "EPP service should exist")
		}, 2*time.Minute, 5*time.Second).Should(Succeed(), "EPP service should exist")

		// Wait for EPP pods to be ready (using correct label: inferencepool=gaie-sim-epp)
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "inferencepool=gaie-sim-epp",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list pods")
			g.Expect(len(podList.Items)).To(BeNumerically(">", 0), "EPP pods should exist")

			// Check that at least one pod is ready
			hasReadyPod := false
			for _, pod := range podList.Items {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						hasReadyPod = true
						break
					}
				}
				if hasReadyPod {
					break
				}
			}
			g.Expect(hasReadyPod).To(BeTrue(), "At least one EPP pod should be ready")
		}, 2*time.Minute, 5*time.Second).Should(Succeed(), "EPP pods should be ready")

		// Additional delay to ensure the datastore is fully populated after EPP is ready
		time.Sleep(5 * time.Second)

		By("Creating model service deployment with 0 initial replicas")
		// Create deployment with 0 replicas using the fixture
		err := fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		// Immediately scale deployment to 0 (with retry to handle race conditions)
		By("Scaling deployment to 0 replicas")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			deployment.Spec.Replicas = ptr.To(int32(0))
			_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to update deployment to 0 replicas")
		}, 30*time.Second, 2*time.Second).Should(Succeed(), "Should successfully scale deployment to 0 replicas")

		By("Creating service to expose model server")
		err = fixtures.CreateService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, modelServiceName+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		By("Creating ServiceMonitor for metrics scraping")
		err = fixtures.CreateServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, modelServiceName+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		// Register cleanup for ServiceMonitor
		DeferCleanup(func() {
			serviceMonitorName := modelServiceName + "-monitor"
			cleanupResource(ctx, "ServiceMonitor", cfg.MonitoringNS, serviceMonitorName,
				func() error {
					return crClient.Delete(ctx, &promoperator.ServiceMonitor{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceMonitorName,
							Namespace: cfg.MonitoringNS,
						},
					})
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: serviceMonitorName, Namespace: cfg.MonitoringNS}, &promoperator.ServiceMonitor{})
					return errors.IsNotFound(err)
				})
		})

		By("Verifying deployment is at 0 replicas")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deploy.Status.Replicas).To(Equal(int32(0)), "Deployment should be scaled to 0")
		}, 1*time.Minute, 5*time.Second).Should(Succeed())

		By("Creating VariantAutoscaling resource")
		err = fixtures.CreateVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			modelServiceName+"-decode", cfg.ModelID, cfg.AcceleratorType, 30.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		// Wait for VA to be marked as inactive (replicas == 0) and for InferencePool to be available
		// The scale-from-zero engine checks for inactive VAs, so we need to ensure:
		// 1. The deployment is at 0 replicas (already verified above)
		// 2. The InferencePool is registered in the datastore (controller should have reconciled it)
		// 3. The VA is created and can be found by the scale-from-zero engine
		By("Waiting for VA to be ready and InferencePool to be available in datastore")
		// Note: The InferencePool should already be reconciled as part of infrastructure setup.
		// However, if the controller was restarted, the datastore might be empty until the
		// InferencePool reconciler runs again. We wait here to allow time for reconciliation.
		// We wait longer to ensure the InferencePool reconciler has had time to register the pool
		// in the datastore before the scale-from-zero engine runs.
		time.Sleep(30 * time.Second) // Allow time for VA reconciliation and InferencePool registration

		GinkgoWriter.Println("Scale-from-zero test setup complete with deployment at 0 replicas")
	})

	AfterAll(func() {
		By("Cleaning up scale-from-zero test resources")

		// Delete HPA
		_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})

		// Delete VA
		va := &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			},
		}
		_ = crClient.Delete(ctx, va)

		// Delete service
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, modelServiceName+"-service", metav1.DeleteOptions{})

		// Delete deployment
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelServiceName+"-decode", metav1.DeleteOptions{})

	})

	Context("Initial state verification", func() {
		It("should have VariantAutoscaling resource created", func() {
			By("Verifying VariantAutoscaling exists")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace,
				Name:      vaName,
			}, va)
			Expect(err).NotTo(HaveOccurred())
			Expect(va.Spec.ModelID).To(Equal(cfg.ModelID))

			GinkgoWriter.Printf("VariantAutoscaling resource verified: %s\n", vaName)
		})

		It("should verify deployment starts at zero replicas", func() {
			By("Checking deployment has 0 replicas")
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			specReplicas := int32(1)
			if deploy.Spec.Replicas != nil {
				specReplicas = *deploy.Spec.Replicas
			}

			Expect(specReplicas).To(Equal(int32(0)), "Deployment should start with 0 replicas")
			GinkgoWriter.Println("Deployment verified at 0 replicas")
		})

	})

	Context("Scale-from-zero with pending requests", func() {
		var triggerJobName string

		AfterAll(func() {
			if triggerJobName != "" {
				By("Cleaning up trigger job")
				propagation := metav1.DeletePropagationBackground
				err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
				if err != nil && !errors.IsNotFound(err) {
					GinkgoWriter.Printf("Warning: failed to delete trigger job %s: %v\n", triggerJobName, err)
				}
			}
		})

		It("should detect pending requests and trigger scale-from-zero", func() {
			By("Discovering inference gateway service")
			// Discover the inference gateway service
			gatewayServiceName := ""
			serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to list services")

			for _, svc := range serviceList.Items {
				if strings.Contains(svc.Name, "inference-gateway") {
					gatewayServiceName = svc.Name
					GinkgoWriter.Printf("Found inference gateway service: %s\n", gatewayServiceName)
					break
				}
			}
			Expect(gatewayServiceName).NotTo(BeEmpty(), "Inference gateway service should exist")

			By("Creating a job to send requests while deployment is at zero")
			triggerJobName = fmt.Sprintf("scale-from-zero-trigger-%d", time.Now().Unix())

			// Create a job that sends requests to the gateway service (which routes through EPP)
			// This allows EPP to queue requests and expose the flow control queue size metric
			job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, gatewayServiceName, cfg.ModelID)
			_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to create scale-from-zero trigger job")

			GinkgoWriter.Printf("Created scale-from-zero trigger job: %s\n", triggerJobName)

			By("Waiting for job pod to be running and sending requests")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("job-name=%s", triggerJobName),
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(podList.Items)).To(BeNumerically(">", 0), "Job pod should exist")

				pod := podList.Items[0]
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodRunning),
					Equal(corev1.PodSucceeded),
				), "Job pod should be running or succeeded")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			GinkgoWriter.Println("Job pod is running and sending requests")

			// Give requests time to queue up in EPP before checking for scale-up
			By("Waiting for requests to queue up in EPP flow control queue")
			time.Sleep(10 * time.Second)

			By("Monitoring VariantAutoscaling for scale-from-zero decision")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Namespace: cfg.LLMDNamespace,
					Name:      vaName,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				optimized := va.Status.DesiredOptimizedAlloc.NumReplicas

				GinkgoWriter.Printf("VA DesiredOptimizedAlloc.NumReplicas: %d (waiting for > 0)\n", optimized)

				// Scale-from-zero engine should detect pending requests and recommend scaling up
				g.Expect(optimized).To(BeNumerically(">", 0),
					"VariantAutoscaling should recommend scaling up from zero due to pending requests")

			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			GinkgoWriter.Println("Scale-from-zero engine detected pending requests and recommended scale-up")
		})

		It("should scale deployment up from zero", func() {
			By("Monitoring deployment for actual scale-up from zero")
			Eventually(func(g Gomega) {
				deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelServiceName+"-decode", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				currentReplicas := deploy.Status.Replicas
				readyReplicas := deploy.Status.ReadyReplicas

				GinkgoWriter.Printf("Current replicas: %d, ready: %d (waiting for > 0)\n",
					currentReplicas, readyReplicas)

				// Deployment should have scaled up from 0
				g.Expect(currentReplicas).To(BeNumerically(">", 0),
					"Deployment should have scaled up from zero")

				// At least one pod should be ready
				g.Expect(readyReplicas).To(BeNumerically(">", 0),
					"At least one pod should be ready after scale-up")

			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			GinkgoWriter.Println("Deployment successfully scaled up from zero")
		})

		It("should successfully process requests after scaling up", func() {
			By("Verifying the trigger job completes successfully")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, triggerJobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				// Job should eventually succeed
				g.Expect(job.Status.Succeeded).To(BeNumerically(">", 0),
					"Job should complete successfully after deployment scales up")

			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			GinkgoWriter.Println("Requests processed successfully after scale-from-zero")
		})

	})
})

// createScaleFromZeroTriggerJob creates a job that sends requests to the inference gateway to trigger scale-from-zero
// Requests go through the gateway (port 80) which routes through EPP, creating the flow control queue
// that the scale-from-zero engine monitors via the inference_extension_flow_control_queue_size metric
func createScaleFromZeroTriggerJob(name, namespace, gatewayService, modelID string) *batchv1.Job {
	backoffLimit := int32(3)
	numRequests := 10

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
				"test-resource": "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"test-resource": "true",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "curl-trigger",
							Image: "curlimages/curl:latest",
							Command: []string{
								"sh", "-c", script,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
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
