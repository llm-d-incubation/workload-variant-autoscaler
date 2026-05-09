package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

type externalMetricValueList struct {
	Items []struct {
		MetricLabels map[string]string `json:"metricLabels"`
	} `json:"items"`
}

const secondaryControllerChartPathEnv = "WVA_E2E_CHART_PATH"

func splitImage(image string) (string, string) {
	lastColon := strings.LastIndex(image, ":")
	lastSlash := strings.LastIndex(image, "/")
	if lastColon == -1 || lastColon < lastSlash {
		return image, "latest"
	}
	return image[:lastColon], image[lastColon+1:]
}

// cleanupSmokeTestResources deletes all resources created by smoke tests to ensure clean state
func cleanupSmokeTestResources() {
	GinkgoWriter.Println("Cleaning up smoke test resources for clean state...")

	// Helper to check if resource name matches smoke test patterns
	isSmokeTestResource := func(name string) bool {
		return strings.HasPrefix(name, "smoke-test-") || strings.HasPrefix(name, "error-test-")
	}

	// Delete all VariantAutoscalings with smoke-test prefix
	vaList := &variantautoscalingv1alpha1.VariantAutoscalingList{}
	if err := crClient.List(ctx, vaList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
		for _, va := range vaList.Items {
			if isSmokeTestResource(va.Name) {
				GinkgoWriter.Printf("  Deleting VA: %s\n", va.Name)
				_ = crClient.Delete(ctx, &va)
			}
		}
	}

	// Delete all HPAs with smoke-test prefix
	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, hpa := range hpaList.Items {
			if isSmokeTestResource(hpa.Name) {
				GinkgoWriter.Printf("  Deleting HPA: %s\n", hpa.Name)
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all ScaledObjects with smoke-test prefix (KEDA)
	if cfg.ScalerBackend == scalerBackendKeda {
		soList := &unstructured.UnstructuredList{}
		soList.SetAPIVersion("keda.sh/v1alpha1")
		soList.SetKind("ScaledObjectList")
		if err := crClient.List(ctx, soList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
			for _, so := range soList.Items {
				if isSmokeTestResource(so.GetName()) {
					GinkgoWriter.Printf("  Deleting ScaledObject: %s\n", so.GetName())
					_ = crClient.Delete(ctx, &so)
				}
			}
		}
	}

	// Delete all Deployments with smoke-test prefix
	deployList, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, deploy := range deployList.Items {
			if isSmokeTestResource(deploy.Name) {
				GinkgoWriter.Printf("  Deleting Deployment: %s\n", deploy.Name)
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploy.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all LeaderWorkerSets with smoke-test prefix
	lwsList := &unstructured.UnstructuredList{}
	lwsList.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
	lwsList.SetKind("LeaderWorkerSetList")
	if err := crClient.List(ctx, lwsList, client.InNamespace(cfg.LLMDNamespace)); err == nil {
		for _, lws := range lwsList.Items {
			if isSmokeTestResource(lws.GetName()) {
				GinkgoWriter.Printf("  Deleting LeaderWorkerSet: %s\n", lws.GetName())
				_ = crClient.Delete(ctx, &lws)
			}
		}
	}

	// Delete all Services with smoke-test prefix
	svcList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, svc := range svcList.Items {
			if isSmokeTestResource(svc.Name) {
				GinkgoWriter.Printf("  Deleting Service: %s\n", svc.Name)
				_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			}
		}
	}

	// Delete all ServiceMonitors with smoke-test prefix in monitoring namespace
	smList := &promoperator.ServiceMonitorList{}
	if err := crClient.List(ctx, smList, client.InNamespace(cfg.MonitoringNS)); err == nil {
		for _, sm := range smList.Items {
			if isSmokeTestResource(sm.Name) {
				GinkgoWriter.Printf("  Deleting ServiceMonitor: %s\n", sm.Name)
				_ = crClient.Delete(ctx, &sm)
			}
		}
	}

	// Wait a moment for deletions to propagate
	time.Sleep(2 * time.Second)
	GinkgoWriter.Println("Cleanup completed")
}

var _ = Describe("Smoke Tests - Infrastructure Readiness", Label("smoke", "full"), func() {
	Context("Basic infrastructure validation", func() {
		It("should have WVA controller running and ready", func() {
			By("Checking WVA controller pods")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.WVANamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "WVA controller pod should exist")

				// At least one pod should be running and ready
				readyPods := 0
				for _, pod := range pods.Items {
					if pod.Status.Phase == corev1.PodRunning {
						for _, condition := range pod.Status.Conditions {
							if condition.Type == "Ready" && condition.Status == "True" {
								readyPods++
								break
							}
						}
					}
				}
				g.Expect(readyPods).To(BeNumerically(">", 0), "At least one WVA controller pod should be ready")
			}).Should(Succeed())
		})

		It("should have llm-d CRDs installed", func() {
			By("Checking for InferencePool CRD")
			_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("inference.networking.k8s.io/v1")
			Expect(err).NotTo(HaveOccurred(), "llm-d CRDs should be installed")
		})

		It("should have Prometheus running", func() {
			By("Checking Prometheus pods")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
					LabelSelector: "app.kubernetes.io/name=prometheus",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Prometheus pod should exist")
			}).Should(Succeed())
		})

		When("using Prometheus Adapter as scaler backend", func() {
			It("should have external metrics API available", func() {
				if cfg.ScalerBackend != "prometheus-adapter" {
					Skip("External metrics API check only applies to Prometheus Adapter backend")
				}
				By("Checking for external.metrics.k8s.io API group")
				Eventually(func(g Gomega) {
					_, err := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
					g.Expect(err).NotTo(HaveOccurred(), "External metrics API should be available")
				}).Should(Succeed())
			})
		})

		When("using KEDA as scaler backend", func() {
			It("should have KEDA operator ready", func() {
				if cfg.ScalerBackend != "keda" {
					Skip("KEDA readiness check only applies when SCALER_BACKEND=keda")
				}
				By("Checking KEDA operator pods in " + cfg.KEDANamespace)
				Eventually(func(g Gomega) {
					pods, err := k8sClient.CoreV1().Pods(cfg.KEDANamespace).List(ctx, metav1.ListOptions{
						LabelSelector: "app.kubernetes.io/name=keda-operator",
					})
					g.Expect(err).NotTo(HaveOccurred(), "Failed to list KEDA pods")
					g.Expect(pods.Items).NotTo(BeEmpty(), "At least one KEDA operator pod should exist")
					ready := 0
					for _, p := range pods.Items {
						if p.Status.Phase == corev1.PodRunning {
							for _, c := range p.Status.Conditions {
								if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
									ready++
									break
								}
							}
						}
					}
					g.Expect(ready).To(BeNumerically(">", 0), "At least one KEDA operator pod should be ready")
				}).Should(Succeed())
			})
		})
	})

	Context("External metrics namespace isolation", Serial, Ordered, func() {
		var (
			primaryNamespace   = "llm-d-sim"
			secondaryNamespace = "llm-d-sim-mt"
			sharedVAName       = "smoke-test-mt-shared-va"
			primaryHPAName     = "smoke-test-mt-primary-hpa"
			secondaryHPAName   = "smoke-test-mt-secondary-hpa"
			primaryModelName   = "smoke-test-mt-primary-ms"
			secondaryModelName = "smoke-test-mt-secondary-ms"
			poolName           = "smoke-test-mt-pool"
		)

		BeforeAll(func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				Skip("Namespace-isolation external metrics check is specific to Prometheus Adapter backend")
			}
			if cfg.Environment != envKindEmulator {
				Skip("Namespace-isolation smoke scenario currently targets kind-emulator setup")
			}

			By("Creating secondary namespace for isolation test")
			_, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: secondaryNamespace,
				},
			}, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred(), "Failed to create secondary namespace")
			}

			DeferCleanup(func() {
				propagation := metav1.DeletePropagationBackground
				_ = k8sClient.CoreV1().Namespaces().Delete(ctx, secondaryNamespace, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
			})

			By("Creating model services in both namespaces with overlapping VA name")
			err = fixtures.EnsureModelService(ctx, k8sClient, primaryNamespace, primaryModelName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary model service")
			err = fixtures.EnsureService(ctx, k8sClient, primaryNamespace, primaryModelName, primaryModelName+"-decode", 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary model service Service")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, primaryNamespace, primaryModelName, primaryModelName+"-decode")
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary ServiceMonitor")

			err = fixtures.EnsureModelService(ctx, k8sClient, secondaryNamespace, secondaryModelName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary model service")
			err = fixtures.EnsureService(ctx, k8sClient, secondaryNamespace, secondaryModelName, secondaryModelName+"-decode", 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary model service Service")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, secondaryNamespace, secondaryModelName, secondaryModelName+"-decode")
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary ServiceMonitor")

			By("Creating overlapping VariantAutoscaling names in both namespaces")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(ctx, crClient, primaryNamespace, sharedVAName, primaryModelName+"-decode", cfg.ModelID, "H100", "")
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary VariantAutoscaling")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(ctx, crClient, secondaryNamespace, sharedVAName, secondaryModelName+"-decode", cfg.ModelID, "H100", "")
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary VariantAutoscaling")

			By("Creating HPAs in both namespaces for the shared VA name")
			err = fixtures.EnsureHPA(ctx, k8sClient, primaryNamespace, primaryHPAName, primaryModelName+"-decode", sharedVAName, 1, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary HPA")
			err = fixtures.EnsureHPA(ctx, k8sClient, secondaryNamespace, secondaryHPAName, secondaryModelName+"-decode", sharedVAName, 1, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary HPA")
		})

		It("should return exactly one external metric item when exported_namespace is selected", func() {
			By("Waiting for both VAs to be reconciled")
			Eventually(func(g Gomega) {
				primaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: primaryNamespace}, primaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				primaryCondition := variantautoscalingv1alpha1.GetCondition(primaryVA, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(primaryCondition).NotTo(BeNil())
				g.Expect(primaryCondition.Status).To(Equal(metav1.ConditionTrue))

				secondaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: secondaryNamespace}, secondaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				secondaryCondition := variantautoscalingv1alpha1.GetCondition(secondaryVA, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(secondaryCondition).NotTo(BeNil())
				g.Expect(secondaryCondition.Status).To(Equal(metav1.ConditionTrue))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Waiting for Prometheus-backed metrics on both VAs (HPA/external-metrics need this)")
			Eventually(func(g Gomega) {
				primaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: primaryNamespace}, primaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				mc := variantautoscalingv1alpha1.GetCondition(primaryVA, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(mc).NotTo(BeNil())
				g.Expect(mc.Status).To(Equal(metav1.ConditionTrue))

				secondaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: secondaryNamespace}, secondaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				mc = variantautoscalingv1alpha1.GetCondition(secondaryVA, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(mc).NotTo(BeNil())
				g.Expect(mc.Status).To(Equal(metav1.ConditionTrue))
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Querying external metrics API with explicit namespace-aware label selector")
			var metricList externalMetricValueList
			Eventually(func(g Gomega) {
				raw, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/"+primaryNamespace+"/"+constants.WVADesiredReplicas).
					Param("labelSelector", "variant_name="+sharedVAName+",exported_namespace="+primaryNamespace).
					DoRaw(ctx)
				g.Expect(err).NotTo(HaveOccurred(), "External metrics API query should succeed")
				g.Expect(json.Unmarshal(raw, &metricList)).To(Succeed(), "Should decode external metric response")
				g.Expect(metricList.Items).To(HaveLen(1), "Expected exactly one metric series for selected namespace and variant")
				g.Expect(metricList.Items[0].MetricLabels["exported_namespace"]).To(Equal(primaryNamespace))
				g.Expect(metricList.Items[0].MetricLabels["variant_name"]).To(Equal(sharedVAName))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Verifying both HPAs report active metric scaling")
			Eventually(func(g Gomega) {
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(primaryNamespace).Get(ctx, primaryHPAName+"-hpa", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var scalingActive *autoscalingv2.HorizontalPodAutoscalerCondition
				for i := range hpa.Status.Conditions {
					if hpa.Status.Conditions[i].Type == autoscalingv2.ScalingActive {
						scalingActive = &hpa.Status.Conditions[i]
						break
					}
				}
				if scalingActive == nil || scalingActive.Status != corev1.ConditionTrue {
					GinkgoWriter.Printf("primary HPA %s/%s conditions: %+v\n", primaryNamespace, primaryHPAName+"-hpa", hpa.Status.Conditions)
				}
				g.Expect(scalingActive).NotTo(BeNil(), "Primary HPA should report ScalingActive condition")
				g.Expect(scalingActive.Status).To(Equal(corev1.ConditionTrue), "Primary HPA should have external metric available")
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(secondaryNamespace).Get(ctx, secondaryHPAName+"-hpa", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var scalingActive *autoscalingv2.HorizontalPodAutoscalerCondition
				for i := range hpa.Status.Conditions {
					if hpa.Status.Conditions[i].Type == autoscalingv2.ScalingActive {
						scalingActive = &hpa.Status.Conditions[i]
						break
					}
				}
				if scalingActive == nil || scalingActive.Status != corev1.ConditionTrue {
					GinkgoWriter.Printf("secondary HPA %s/%s conditions: %+v\n", secondaryNamespace, secondaryHPAName+"-hpa", hpa.Status.Conditions)
				}
				g.Expect(scalingActive).NotTo(BeNil(), "Secondary HPA should report ScalingActive condition")
				g.Expect(scalingActive.Status).To(Equal(corev1.ConditionTrue), "Secondary HPA should have external metric available")
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})
	})

	Context("Dual namespace-scoped controllers isolation", Serial, Ordered, func() {
		var (
			primaryNamespace     = "llm-d-sim"
			secondaryNamespace   = "llm-d-sim-dual"
			secondaryController  = "workload-variant-autoscaler-system-dual"
			secondaryReleaseName = "wva-dual-secondary"
			primaryHPAName       = "smoke-test-dual-primary-hpa"
			secondaryHPAName     = "smoke-test-dual-secondary-hpa"
			primaryModelName     = "smoke-test-dual-primary-ms"
			secondaryModelName   = "smoke-test-dual-secondary-ms"
			poolName             = "smoke-test-dual-pool"
			sharedVAName         = "smoke-test-dual-shared-va"
			controllerInstance   = "dual-secondary"
		)

		BeforeAll(func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				Skip("Dual-controller external metrics check is specific to Prometheus Adapter backend")
			}
			if cfg.Environment != envKindEmulator {
				Skip("Dual-controller smoke scenario currently targets kind-emulator setup")
			}

			By("Creating secondary workload namespace")
			_, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: secondaryNamespace},
			}, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred(), "Failed to create secondary workload namespace")
			}

			By("Installing secondary namespace-scoped controller via Helm")
			primaryController, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).Get(ctx, "workload-variant-autoscaler-controller-manager", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to read primary controller deployment image")
			Expect(primaryController.Spec.Template.Spec.Containers).NotTo(BeEmpty(), "Primary controller deployment should contain containers")
			imageRepo, imageTag := splitImage(primaryController.Spec.Template.Spec.Containers[0].Image)
			chartPath := os.Getenv(secondaryControllerChartPathEnv)
			Expect(chartPath).NotTo(BeEmpty(),
				"Missing %s; set it to the workload-variant-autoscaler chart directory (use an absolute path; go test cwd is the test package dir)", secondaryControllerChartPathEnv)
			_, statErr := os.Stat(chartPath)
			Expect(statErr).NotTo(HaveOccurred(), "Invalid %s path: %s", secondaryControllerChartPathEnv, chartPath)

			helmArgs := []string{
				"upgrade", "-i", secondaryReleaseName, chartPath,
				"-n", secondaryController, "--create-namespace",
				"--set", "controller.enabled=true",
				"--set", "va.enabled=false",
				"--set", "hpa.enabled=false",
				"--set", "vllmService.enabled=false",
				"--set", "wva.namespaceScoped=true",
				"--set", "wva.controllerInstance=" + controllerInstance,
				"--set", "llmd.namespace=" + secondaryNamespace,
				"--set", "wva.image.repository=" + imageRepo,
				"--set", "wva.image.tag=" + imageTag,
				"--set", "wva.imagePullPolicy=IfNotPresent",
				"--set", "wva.prometheus.baseURL=https://kube-prometheus-stack-prometheus." + cfg.MonitoringNS + ".svc.cluster.local:9090",
				"--set", "wva.prometheus.tls.insecureSkipVerify=true",
			}
			cmd := exec.Command("helm", helmArgs...)
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Secondary controller helm install failed: %s", string(out))

			DeferCleanup(func() {
				uninstall := exec.Command("helm", "uninstall", secondaryReleaseName, "-n", secondaryController)
				_, _ = uninstall.CombinedOutput()
			})

			By("Waiting for secondary controller to be ready")
			Eventually(func(g Gomega) {
				pods, listErr := k8sClient.CoreV1().Pods(secondaryController).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				g.Expect(listErr).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Expected secondary controller pod")
				ready := 0
				for _, pod := range pods.Items {
					if pod.Status.Phase != corev1.PodRunning {
						continue
					}
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							ready++
							break
						}
					}
				}
				g.Expect(ready).To(BeNumerically(">", 0), "Expected at least one ready secondary controller pod")
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating model services in both namespaces")
			err = fixtures.EnsureModelService(ctx, k8sClient, primaryNamespace, primaryModelName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary model service")
			err = fixtures.EnsureService(ctx, k8sClient, primaryNamespace, primaryModelName, primaryModelName+"-decode", 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary service")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, primaryNamespace, primaryModelName, primaryModelName+"-decode")
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary ServiceMonitor")

			err = fixtures.EnsureModelService(ctx, k8sClient, secondaryNamespace, secondaryModelName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary model service")
			err = fixtures.EnsureService(ctx, k8sClient, secondaryNamespace, secondaryModelName, secondaryModelName+"-decode", 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary service")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, secondaryNamespace, secondaryModelName, secondaryModelName+"-decode")
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary ServiceMonitor")

			By("Creating overlapping VA names for each controller namespace")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(ctx, crClient, primaryNamespace, sharedVAName, primaryModelName+"-decode", cfg.ModelID, "H100", "")
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary VA")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(ctx, crClient, secondaryNamespace, sharedVAName, secondaryModelName+"-decode", cfg.ModelID, "H100", controllerInstance)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary VA")

			By("Creating HPAs in both namespaces for the shared VA name")
			err = fixtures.EnsureHPA(ctx, k8sClient, primaryNamespace, primaryHPAName, primaryModelName+"-decode", sharedVAName, 1, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary HPA")
			err = fixtures.EnsureHPA(ctx, k8sClient, secondaryNamespace, secondaryHPAName, secondaryModelName+"-decode", sharedVAName, 1, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary HPA")
		})

		It("should expose isolated external metrics for each namespace-scoped controller", func() {
			By("Waiting for both VAs to be reconciled")
			Eventually(func(g Gomega) {
				primaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: primaryNamespace}, primaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				c := variantautoscalingv1alpha1.GetCondition(primaryVA, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Status).To(Equal(metav1.ConditionTrue))

				secondaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: secondaryNamespace}, secondaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				c = variantautoscalingv1alpha1.GetCondition(secondaryVA, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Waiting for Prometheus-backed metrics on both VAs (HPA/external-metrics need this)")
			Eventually(func(g Gomega) {
				primaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: primaryNamespace}, primaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				mc := variantautoscalingv1alpha1.GetCondition(primaryVA, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(mc).NotTo(BeNil())
				g.Expect(mc.Status).To(Equal(metav1.ConditionTrue))

				secondaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: secondaryNamespace}, secondaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				mc = variantautoscalingv1alpha1.GetCondition(secondaryVA, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(mc).NotTo(BeNil())
				g.Expect(mc.Status).To(Equal(metav1.ConditionTrue))
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Querying external metrics for primary namespace")
			Eventually(func(g Gomega) {
				raw, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/"+primaryNamespace+"/"+constants.WVADesiredReplicas).
					Param("labelSelector", "variant_name="+sharedVAName+",exported_namespace="+primaryNamespace).
					DoRaw(ctx)
				g.Expect(err).NotTo(HaveOccurred())
				var metricList externalMetricValueList
				g.Expect(json.Unmarshal(raw, &metricList)).To(Succeed())
				g.Expect(metricList.Items).To(HaveLen(1))
				g.Expect(metricList.Items[0].MetricLabels["exported_namespace"]).To(Equal(primaryNamespace))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Querying external metrics for secondary controller namespace")
			Eventually(func(g Gomega) {
				raw, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/"+secondaryNamespace+"/"+constants.WVADesiredReplicas).
					Param("labelSelector", "variant_name="+sharedVAName+",exported_namespace="+secondaryNamespace).
					DoRaw(ctx)
				g.Expect(err).NotTo(HaveOccurred())
				var metricList externalMetricValueList
				g.Expect(json.Unmarshal(raw, &metricList)).To(Succeed())
				g.Expect(metricList.Items).To(HaveLen(1))
				g.Expect(metricList.Items[0].MetricLabels["exported_namespace"]).To(Equal(secondaryNamespace))
				if ci, ok := metricList.Items[0].MetricLabels["controller_instance"]; ok {
					g.Expect(ci).To(Equal(controllerInstance))
				}
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Verifying both HPAs report active metric scaling")
			Eventually(func(g Gomega) {
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(primaryNamespace).Get(ctx, primaryHPAName+"-hpa", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var scalingActive *autoscalingv2.HorizontalPodAutoscalerCondition
				for i := range hpa.Status.Conditions {
					if hpa.Status.Conditions[i].Type == autoscalingv2.ScalingActive {
						scalingActive = &hpa.Status.Conditions[i]
						break
					}
				}
				if scalingActive == nil || scalingActive.Status != corev1.ConditionTrue {
					GinkgoWriter.Printf("primary HPA %s/%s conditions: %+v\n", primaryNamespace, primaryHPAName+"-hpa", hpa.Status.Conditions)
				}
				g.Expect(scalingActive).NotTo(BeNil(), "Primary HPA should report ScalingActive condition")
				g.Expect(scalingActive.Status).To(Equal(corev1.ConditionTrue), "Primary HPA should have external metric available")
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(secondaryNamespace).Get(ctx, secondaryHPAName+"-hpa", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var scalingActive *autoscalingv2.HorizontalPodAutoscalerCondition
				for i := range hpa.Status.Conditions {
					if hpa.Status.Conditions[i].Type == autoscalingv2.ScalingActive {
						scalingActive = &hpa.Status.Conditions[i]
						break
					}
				}
				if scalingActive == nil || scalingActive.Status != corev1.ConditionTrue {
					GinkgoWriter.Printf("secondary HPA %s/%s conditions: %+v\n", secondaryNamespace, secondaryHPAName+"-hpa", hpa.Status.Conditions)
				}
				g.Expect(scalingActive).NotTo(BeNil(), "Secondary HPA should report ScalingActive condition")
				g.Expect(scalingActive.Status).To(Equal(corev1.ConditionTrue), "Secondary HPA should have external metric available")
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})
	})

	Context("Basic VA lifecycle", Serial, Ordered, func() {
		var (
			poolName         = "smoke-test-pool"
			modelServiceName = "smoke-test-ms"
			deploymentName   = modelServiceName + "-decode"
			vaName           = "smoke-test-va"
			hpaName          = "smoke-test-hpa"
			minReplicas      = int32(1) // Store minReplicas for stabilization check
		)

		BeforeAll(func() {
			By("Cleaning up any existing smoke test resources")
			cleanupSmokeTestResources()

			By("Creating model service deployment")
			err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

			// Register cleanup for deployment (runs even if test fails)
			DeferCleanup(func() {
				cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentName,
					func() error {
						return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			By("Creating service to expose model server")
			err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, deploymentName, 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create service")

			// Register cleanup for service
			DeferCleanup(func() {
				serviceName := modelServiceName + "-service"
				cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
					func() error {
						return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			By("Creating ServiceMonitor for metrics scraping")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, deploymentName)
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

			By("Waiting for model service to be ready")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(
				ctx, crClient, cfg.LLMDNamespace, vaName,
				deploymentName, cfg.ModelID, cfg.AcceleratorType,
				cfg.ControllerInstance,
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Creating scaler for the deployment (HPA or ScaledObject per backend)")
			if cfg.ScaleToZeroEnabled {
				minReplicas = 0
			}
			if cfg.ScalerBackend == scalerBackendKeda {
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
				err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, deploymentName, vaName, minReplicas, 10, cfg.MonitoringNS)
				Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
			} else {
				err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, deploymentName, vaName, minReplicas, 10)
				Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")
			}
		})

		AfterAll(func() {
			By("Cleaning up test resources")
			// Delete in reverse dependency order: scaler (HPA or ScaledObject) -> VA
			// Load Job, Service, Deployment, and ServiceMonitor cleanup is handled by DeferCleanup registered in BeforeAll and test

			if cfg.ScalerBackend == scalerBackendKeda {
				err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
				Expect(err).NotTo(HaveOccurred())
			} else {
				hpaNameFull := hpaName + "-hpa"
				cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameFull,
					func() error {
						return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			}

			// Delete VA
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				},
			}
			cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaName,
				func() error {
					return crClient.Delete(ctx, va)
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
					return errors.IsNotFound(err)
				})
		})

		It("should reconcile the VA successfully", func() {
			By("Checking VA status conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")

				// Check for TargetResolved condition
				targetResolved := false
				for _, cond := range va.Status.Conditions {
					if cond.Type == variantautoscalingv1alpha1.TypeTargetResolved && cond.Status == metav1.ConditionTrue {
						targetResolved = true
						break
					}
				}
				g.Expect(targetResolved).To(BeTrue(), "VA should have TargetResolved=True condition")
			}).Should(Succeed())
		})

		It("should expose external metrics for the VA", func() {
			By("Waiting for VA to be reconciled (TargetResolved condition)")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				// Verify VA is reconciled (has TargetResolved condition)
				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
			}).Should(Succeed())

			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists (KEDA backend; external metric name is KEDA-generated)")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			} else {
				By("Querying external metrics API for wva_desired_replicas")
				// Note: The metric may not exist until Engine has run and emitted metrics to Prometheus,
				// which Prometheus Adapter then queries. This can take time.
				result, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + cfg.LLMDNamespace + "/" + constants.WVADesiredReplicas).
					DoRaw(ctx)
				if err != nil {
					if errors.IsNotFound(err) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
					}
				} else {
					if strings.Contains(string(result), `"items":[]`) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(string(result)).To(ContainSubstring(constants.WVADesiredReplicas), "Metric response should contain metric name")
						GinkgoWriter.Printf("External metrics API returned metric: %s\n", constants.WVADesiredReplicas)
					}
				}
			}

			By("Verifying DesiredOptimizedAlloc is eventually populated (if Engine has run)")
			// This is a best-effort check - DesiredOptimizedAlloc is populated by the Engine
			// which may not run immediately. We check if it's populated, but don't fail if it's not yet.
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(getErr).NotTo(HaveOccurred())
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				// If populated, verify it's valid
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				// If not populated yet, that's okay - Engine may not have run yet
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should have MetricsAvailable condition set when pods are ready", func() {
			By("Waiting for MetricsAvailable condition to be set")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
				// MetricsAvailable can be True (metrics found) or False (metrics missing/stale)
				// For smoke tests, we just verify the condition exists and has a valid status
				g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
					"MetricsAvailable condition should have a valid status")
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should have scaling controlled by backend", func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists and KEDA has created an HPA")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")
				// KEDA creates an HPA for the ScaledObject; name pattern is often keda-hpa-<scaledobject> or from status
				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						h := &hpaList.Items[i]
						if h.Spec.ScaleTargetRef.Name == deploymentName {
							kedaHPA = h
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the deployment")
					g.Expect(kedaHPA.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			} else {
				By("Verifying HPA exists and is configured")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.WVADesiredReplicas), "HPA should use wva_desired_replicas metric")

				By("Waiting for HPA to read the metric and update status")
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", 0), "HPA should have current replicas set")
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			}
		})

		It("should verify Prometheus is scraping vLLM metrics", func() {
			By("Checking that deployment pods are ready and reporting metrics")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one pod")

				// At least one pod should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(BeNumerically(">", 0), "At least one pod should be ready for metrics scraping")
			}).Should(Succeed())

			// Note: Direct Prometheus query would require port-forwarding or in-cluster access
			// For smoke tests, we verify pods are ready (which is a prerequisite for metrics)
			// Full Prometheus query validation is in the full test suite
		})

		It("should collect saturation metrics without triggering scale-up", func() {
			By("Verifying VA is reconciled and has conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
			}).Should(Succeed())

			By("Verifying MetricsAvailable condition indicates metrics collection")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			// For smoke tests, we verify the condition exists
			// In ideal case, it should be True with ReasonMetricsFound, but False is also valid
			// if metrics are temporarily unavailable (smoke tests don't apply load)
			Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			if condition.Status == metav1.ConditionTrue {
				Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
					"When metrics are available, reason should be MetricsFound")
			}

			By("Checking if DesiredOptimizedAlloc is populated (best-effort)")
			// DesiredOptimizedAlloc is populated by the Engine, which may not run immediately
			// This is a best-effort check - we verify it's valid if populated, but don't fail if not
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
				GinkgoWriter.Printf("DesiredOptimizedAlloc is populated: accelerator=%s, replicas=%d\n",
					va.Status.DesiredOptimizedAlloc.Accelerator, *va.Status.DesiredOptimizedAlloc.NumReplicas)
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})
	})

	Context("Basic VA lifecycle with LeaderWorkerSet", Serial, Ordered, func() {
		var (
			poolName         = "smoke-test-lws-pool"
			modelServiceName = "smoke-test-lws-ms"
			lwsName          = modelServiceName + "-decode"
			vaName           = "smoke-test-lws-va"
			hpaName          = "smoke-test-lws-hpa"
			minReplicas      = int32(1)
			lwsGroupSize     = int32(2) // 1 leader + 1 worker
		)

		BeforeAll(func() {
			By("Cleaning up any existing smoke test resources")
			cleanupSmokeTestResources()

			By("Creating model service LeaderWorkerSet")
			err := fixtures.EnsureModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs, lwsGroupSize)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service LWS")

			// Register cleanup for LWS (runs even if test fails)
			DeferCleanup(func() {
				cleanupResource(ctx, "LeaderWorkerSet", cfg.LLMDNamespace, lwsName,
					func() error {
						return fixtures.DeleteModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName)
					},
					func() bool {
						lws := &unstructured.Unstructured{}
						lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
						lws.SetKind("LeaderWorkerSet")
						err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
						return errors.IsNotFound(err)
					})
			})

			By("Creating service to expose LWS model server")
			err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, lwsName, 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create service")

			// Register cleanup for service
			DeferCleanup(func() {
				serviceName := modelServiceName + "-service"
				cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
					func() error {
						return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			By("Creating ServiceMonitor for LWS metrics scraping")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, lwsName)
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

			By("Waiting for LWS to be ready")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				readyReplicas, found, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
				g.Expect(found).To(BeTrue(), "LWS should have status.readyReplicas")
				g.Expect(readyReplicas).To(Equal(int64(1)), "LWS should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource for LWS")
			err = fixtures.EnsureVariantAutoscaling(
				ctx, crClient, cfg.LLMDNamespace, vaName,
				lwsName, cfg.ModelID, cfg.AcceleratorType,
				30.0, cfg.ControllerInstance,
				fixtures.WithScaleTargetKind("LeaderWorkerSet"),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Creating scaler for the LWS (HPA or ScaledObject per backend)")
			if cfg.ScaleToZeroEnabled {
				minReplicas = 0
			}
			if cfg.ScalerBackend == scalerBackendKeda {
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
				err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10, cfg.MonitoringNS,
					fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
			} else {
				err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10,
					fixtures.WithScaleTargetRefKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")
			}
		})

		AfterAll(func() {
			By("Cleaning up LWS test resources")
			if cfg.ScalerBackend == scalerBackendKeda {
				err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
				Expect(err).NotTo(HaveOccurred())
			} else {
				hpaNameFull := hpaName + "-hpa"
				cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameFull,
					func() error {
						return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			}

			// Delete VA
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				},
			}
			cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaName,
				func() error {
					return crClient.Delete(ctx, va)
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
					return errors.IsNotFound(err)
				})
		})

		It("should reconcile the VA successfully with LWS", func() {
			By("Checking VA status conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")

				// Check for TargetResolved condition
				targetResolved := false
				for _, cond := range va.Status.Conditions {
					if cond.Type == variantautoscalingv1alpha1.TypeTargetResolved && cond.Status == metav1.ConditionTrue {
						targetResolved = true
						break
					}
				}
				g.Expect(targetResolved).To(BeTrue(), "VA should have TargetResolved=True condition")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should expose external metrics for the VA with LWS", func() {
			By("Waiting for VA to be reconciled (TargetResolved condition)")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
			}).Should(Succeed())

			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists (KEDA backend; external metric name is KEDA-generated)")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			} else {
				By("Querying external metrics API for wva_desired_replicas")
				result, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + cfg.LLMDNamespace + "/" + constants.WVADesiredReplicas).
					DoRaw(ctx)
				if err != nil {
					if errors.IsNotFound(err) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
					}
				} else {
					if strings.Contains(string(result), `"items":[]`) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(string(result)).To(ContainSubstring(constants.WVADesiredReplicas), "Metric response should contain metric name")
						GinkgoWriter.Printf("External metrics API returned metric: %s\n", constants.WVADesiredReplicas)
					}
				}
			}

			By("Verifying DesiredOptimizedAlloc is eventually populated (if Engine has run)")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(getErr).NotTo(HaveOccurred())
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should verify LWS structure with correct group size", func() {
			By("Checking LWS has correct group size")
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			Expect(err).NotTo(HaveOccurred())

			size, found, _ := unstructured.NestedInt64(lws.Object, "spec", "leaderWorkerTemplate", "size")
			Expect(found).To(BeTrue(), "LWS should have spec.leaderWorkerTemplate.size")
			Expect(size).To(Equal(int64(lwsGroupSize)), fmt.Sprintf("LWS should have group size %d", lwsGroupSize))
		})

		It("should have MetricsAvailable condition set when LWS pods are ready", func() {
			By("Waiting for MetricsAvailable condition to be set")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
				g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
					"MetricsAvailable condition should have a valid status")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have scaling controlled by backend with LWS", func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists and KEDA has created an HPA for LWS")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")

				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						h := &hpaList.Items[i]
						if h.Spec.ScaleTargetRef.Name == lwsName {
							kedaHPA = h
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the LWS")
					g.Expect(kedaHPA.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			} else {
				By("Verifying HPA exists and is configured for LWS")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.WVADesiredReplicas), "HPA should use wva_desired_replicas metric")
				Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("LeaderWorkerSet"), "HPA should target LeaderWorkerSet")

				By("Waiting for HPA to read the metric and update status")
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", 0), "HPA should have current replicas set")
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			}
		})

		It("should verify Prometheus is scraping LWS metrics", func() {
			By("Checking that LWS pods are ready and reporting metrics")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one pod")

				// At least one pod should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(BeNumerically(">", 0), "At least one pod should be ready for metrics scraping")
			}).Should(Succeed())
		})

		It("should collect saturation metrics without triggering scale-up", func() {
			By("Verifying VA is reconciled and has conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
			}).Should(Succeed())

			By("Verifying MetricsAvailable condition indicates metrics collection")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			if condition.Status == metav1.ConditionTrue {
				Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
					"When metrics are available, reason should be MetricsFound")
			}

			By("Checking if DesiredOptimizedAlloc is populated (best-effort)")
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
				GinkgoWriter.Printf("DesiredOptimizedAlloc is populated: accelerator=%s, replicas=%d\n",
					va.Status.DesiredOptimizedAlloc.Accelerator, *va.Status.DesiredOptimizedAlloc.NumReplicas)
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should verify LWS pods are created with leader and workers", func() {
			By("Checking that LWS created StatefulSet pods")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				// With 1 replica and group size 2, we expect 2 pods total (1 leader + 1 worker)
				g.Expect(pods.Items).To(HaveLen(int(lwsGroupSize)), fmt.Sprintf("Should have %d pods (1 replica × group size %d)", lwsGroupSize, lwsGroupSize))

				// At least the leader should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(BeNumerically(">=", 1), "At least the leader pod should be ready")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should scale up LWS under load", func() {
			// HPA name: with Prometheus Adapter we create HPA named <hpaName>-hpa; with KEDA, KEDA creates HPA named keda-hpa-<scaledobject-name>
			effectiveHpaName := hpaName + "-hpa"
			if cfg.ScalerBackend == scalerBackendKeda {
				effectiveHpaName = "keda-hpa-" + hpaName + "-so"
			}

			// wait for VA to stabilize at minReplicas before starting load
			By("Waiting for VA to stabilize at minReplicas")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				var optimized int32
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					optimized = *va.Status.DesiredOptimizedAlloc.NumReplicas
				}
				GinkgoWriter.Printf("Waiting for VA to be ready: optimized=%d, minReplicas=%d\n", optimized, minReplicas)
				g.Expect(optimized).To(BeNumerically(">=", minReplicas), "VA should have optimized >= minReplicas")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// wait for LWS to be fully stable
			By("Waiting for LWS to stabilize (no pods in transition)")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				specReplicas, _, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
				statusReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
				readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")

				GinkgoWriter.Printf("Waiting for LWS stability: spec=%d, status=%d, ready=%d\n",
					specReplicas, statusReplicas, readyReplicas)
				g.Expect(statusReplicas).To(Equal(specReplicas), "Status replicas should match spec")
				g.Expect(readyReplicas).To(Equal(specReplicas), "Ready replicas should match spec")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// Prefer starting from minReplicas so we reliably detect scale-up
			By("Waiting for VA to settle at minReplicas before recording initial state (best-effort)")
			settled := false
			for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				if err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va); err != nil {
					break
				}
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil && *va.Status.DesiredOptimizedAlloc.NumReplicas == minReplicas {
					settled = true
					break
				}
				var current int32
				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					current = *va.Status.DesiredOptimizedAlloc.NumReplicas
				}
				GinkgoWriter.Printf("Waiting for VA to settle: optimized=%d, minReplicas=%d\n", current, minReplicas)
				time.Sleep(10 * time.Second)
			}
			if !settled {
				GinkgoWriter.Printf("VA did not settle at minReplicas within 5m; will use current value as initial\n")
			}

			// Record initial state after stabilization
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())
			var initialOptimized int32
			if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
				initialOptimized = *va.Status.DesiredOptimizedAlloc.NumReplicas
			}
			GinkgoWriter.Printf("Initial optimized replicas (after stabilization): %d (settled=%v)\n", initialOptimized, settled)

			By("Starting burst load generation to trigger scale-up")
			scaleUpPrompts := 2400
			if cfg.NumPrompts > scaleUpPrompts {
				scaleUpPrompts = cfg.NumPrompts
			}
			loadCfg := fixtures.LoadConfig{
				Strategy:     cfg.LoadStrategy,
				RequestRate:  0,
				NumPrompts:   scaleUpPrompts,
				InputTokens:  cfg.InputTokens,
				OutputTokens: 400,
				ModelID:      cfg.ModelID,
			}

			targetURL := fmt.Sprintf("http://%s-service.%s.svc.cluster.local:8000/v1/completions", modelServiceName, cfg.LLMDNamespace)
			err = fixtures.EnsureBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, "smoke-lws-scaleup-load", targetURL, loadCfg)
			Expect(err).NotTo(HaveOccurred(), "Failed to create burst load generation job")

			jobName := "smoke-lws-scaleup-load-load"

			DeferCleanup(func() {
				cleanupResource(ctx, "Job", cfg.LLMDNamespace, jobName,
					func() error {
						propagation := metav1.DeletePropagationBackground
						return k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &propagation})
					},
					func() bool {
						_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			loadStartTime := time.Now()

			By("Verifying load job was created")
			Eventually(func(g Gomega) {
				job, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "Load job should exist")
				GinkgoWriter.Printf("Load job status: Active=%d, Succeeded=%d, Failed=%d\n",
					job.Status.Active, job.Status.Succeeded, job.Status.Failed)
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("Waiting for load job pod to start")
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "job-name=" + jobName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				if len(podList.Items) == 0 {
					job, jobErr := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
					if jobErr == nil {
						GinkgoWriter.Printf("Job exists but no pods yet. Job status: Active=%d, Succeeded=%d, Failed=%d\n",
							job.Status.Active, job.Status.Succeeded, job.Status.Failed)
					}
					g.Expect(podList.Items).NotTo(BeEmpty(), "Load job pod should exist")
				}

				pod := podList.Items[0]
				if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
					reason := pod.Status.Reason
					var messages []string

					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
							if reason == "" {
								reason = condition.Reason
							}
							if condition.Message != "" {
								messages = append(messages, "PodScheduled: "+condition.Message)
							}
						} else if condition.Status == corev1.ConditionFalse {
							messages = append(messages, fmt.Sprintf("%s: %s", condition.Type, condition.Reason))
							if condition.Message != "" {
								messages = append(messages, "  "+condition.Message)
							}
						}
					}

					for _, containerStatus := range pod.Status.ContainerStatuses {
						if containerStatus.State.Waiting != nil {
							if reason == "" {
								reason = containerStatus.State.Waiting.Reason
							}
							if containerStatus.State.Waiting.Message != "" {
								messages = append(messages, fmt.Sprintf("Container %s: %s", containerStatus.Name, containerStatus.State.Waiting.Message))
							}
						}
					}

					if reason == "" {
						reason = "Unknown (check pod events for details)"
					}

					GinkgoWriter.Printf("Load job pod status: Phase=%s, Reason=%s\n", pod.Status.Phase, reason)
					if len(messages) > 0 {
						for _, msg := range messages {
							GinkgoWriter.Printf("  %s\n", msg)
						}
					}
				}
				g.Expect(pod.Status.Phase).To(Or(
					Equal(corev1.PodRunning),
					Equal(corev1.PodSucceeded),
				), fmt.Sprintf("Load job pod should be running or succeeded (current: %s)", pod.Status.Phase))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			GinkgoWriter.Println("Load generation job is running")

			By("Waiting for load generation to ramp up (30 seconds)")
			time.Sleep(30 * time.Second)

			By("Waiting for VA to detect saturation and recommend scale-up")
			var desiredReplicas int
			checkCount := 0
			scaleUpTimeout := 7 * time.Minute
			loadConfig := loadCfg
			Eventually(func(g Gomega) {
				checkCount++
				elapsed := time.Since(loadStartTime)
				remaining := scaleUpTimeout - elapsed

				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
					desiredReplicas = int(*va.Status.DesiredOptimizedAlloc.NumReplicas)
				}
				metricsAvailable := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				metricsStatus := "Unknown"
				metricsReason := ""
				if metricsAvailable != nil {
					metricsStatus = string(metricsAvailable.Status)
					metricsReason = metricsAvailable.Reason
				}

				hpa, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, effectiveHpaName, metav1.GetOptions{})
				hpaDesired := int32(0)
				hpaCurrent := int32(0)
				if hpaErr == nil {
					hpaDesired = hpa.Status.DesiredReplicas
					hpaCurrent = hpa.Status.CurrentReplicas
				}

				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				lwsErr := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				lwsSpec := int64(0)
				lwsReady := int64(0)
				if lwsErr == nil {
					lwsSpec, _, _ = unstructured.NestedInt64(lws.Object, "spec", "replicas")
					lwsReady, _, _ = unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
				}

				job, jobErr := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Get(ctx, jobName, metav1.GetOptions{})
				jobSucceeded := int32(0)
				jobFailed := int32(0)
				jobActive := int32(0)
				loadStatus := "Unknown"
				loadReason := ""
				if jobErr == nil {
					jobSucceeded = job.Status.Succeeded
					jobFailed = job.Status.Failed
					jobActive = job.Status.Active
				}

				podList, podErr := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "job-name=" + jobName,
				})
				if podErr == nil && len(podList.Items) > 0 {
					pod := podList.Items[0]
					loadStatus = string(pod.Status.Phase)
					if pod.Status.Phase == corev1.PodPending {
						for _, condition := range pod.Status.Conditions {
							if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
								loadReason = condition.Reason
								break
							}
						}
						if loadReason == "" {
							for _, containerStatus := range pod.Status.ContainerStatuses {
								if containerStatus.State.Waiting != nil {
									loadReason = containerStatus.State.Waiting.Reason
									break
								}
							}
						}
					}
					if loadReason == "" {
						loadReason = "Running"
					}
				}

				var expectedDuration time.Duration
				if loadConfig.RequestRate > 0 {
					expectedDuration = time.Duration(loadConfig.NumPrompts/loadConfig.RequestRate+10) * time.Second
				} else {
					numBatches := (loadConfig.NumPrompts + 9) / 10
					expectedDuration = time.Duration(numBatches*1+10) * time.Second
				}
				loadProgress := ""
				if expectedDuration.Seconds() > 0 && elapsed < expectedDuration {
					progressPct := int((elapsed.Seconds() / expectedDuration.Seconds()) * 100)
					if progressPct > 100 {
						progressPct = 100
					}
					loadProgress = fmt.Sprintf(" (~%d%% of expected %v)", progressPct, expectedDuration.Round(time.Second))
				} else if elapsed >= expectedDuration {
					loadProgress = " (expected duration exceeded)"
				}

				scaleUpDetected := desiredReplicas > int(initialOptimized)
				statusIndicator := "⏳"
				if scaleUpDetected {
					statusIndicator = "✓"
				}
				GinkgoWriter.Printf("[%s Progress %d] %v elapsed | %v remaining\n", statusIndicator, checkCount, elapsed.Round(time.Second), remaining.Round(time.Second))
				GinkgoWriter.Printf("  VA: %d replicas (initial: %d) | Metrics: %s/%s | LastRun: %v\n",
					desiredReplicas, initialOptimized, metricsStatus, metricsReason, va.Status.DesiredOptimizedAlloc.LastRunTime.Format("15:04:05"))
				GinkgoWriter.Printf("  HPA: Desired=%d | Current=%d | LWS: Spec=%d | Ready=%d\n",
					hpaDesired, hpaCurrent, lwsSpec, lwsReady)

				loadConfigDisplay := ""
				if loadConfig.RequestRate > 0 {
					loadConfigDisplay = fmt.Sprintf("%d req/s", loadConfig.RequestRate)
				} else {
					loadConfigDisplay = "burst pattern"
				}
				GinkgoWriter.Printf("  Load: Phase=%s", loadStatus)
				if loadReason != "" && loadReason != "Running" {
					GinkgoWriter.Printf(" (Reason: %s)", loadReason)
				}
				GinkgoWriter.Printf(" | Config: %s, %d prompts | Active=%d | Succeeded=%d | Failed=%d%s\n",
					loadConfigDisplay, loadConfig.NumPrompts, jobActive, jobSucceeded, jobFailed, loadProgress)

				if checkCount%3 == 0 || scaleUpDetected {
					if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
						GinkgoWriter.Printf("  └─ Accelerator: %s", va.Status.DesiredOptimizedAlloc.Accelerator)
					}
					if metricsAvailable != nil && metricsAvailable.Message != "" {
						GinkgoWriter.Printf(" | Metrics: %s", metricsAvailable.Message)
					}
					if hpaErr == nil && len(hpa.Status.Conditions) > 0 {
						for _, cond := range hpa.Status.Conditions {
							if cond.Type == autoscalingv2.AbleToScale {
								GinkgoWriter.Printf(" | HPA: %s/%s", cond.Status, cond.Reason)
								break
							}
						}
					}
					GinkgoWriter.Println()
				}

				if settled {
					g.Expect(desiredReplicas).To(BeNumerically(">", int(initialOptimized)),
						fmt.Sprintf("VA should recommend more replicas than initial under load (current: %d, initial: %d, elapsed: %v)", desiredReplicas, initialOptimized, elapsed))
				} else {
					g.Expect(desiredReplicas).To(BeNumerically(">=", 2),
						fmt.Sprintf("VA should recommend at least 2 replicas under load when initial was %d (current: %d, elapsed: %v)", initialOptimized, desiredReplicas, elapsed))
					g.Expect(desiredReplicas).To(BeNumerically(">=", int(minReplicas)),
						fmt.Sprintf("VA should recommend at least minReplicas under load (current: %d, minReplicas: %d)", desiredReplicas, minReplicas))
				}
			}, scaleUpTimeout, 10*time.Second).Should(Succeed())

			GinkgoWriter.Printf("✓ VA detected saturation and recommended %d replicas (took %v)\n", desiredReplicas, time.Since(loadStartTime))
			GinkgoWriter.Printf("  → VA scale-up detected! Now verifying HPA and LWS scaling...\n")

			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying KEDA HPA exists and has valid status (skipping desired-replicas check)")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, effectiveHpaName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", minReplicas),
					"KEDA HPA should report current replicas >= minReplicas")
				GinkgoWriter.Printf("✓ KEDA HPA exists: Desired=%d, Current=%d (VA recommended %d)\n",
					hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas, desiredReplicas)
			} else {
				By("Verifying HPA reads the metric and updates desired replicas")
				hpaCheckStart := time.Now()
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, effectiveHpaName, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					elapsed := time.Since(hpaCheckStart)
					GinkgoWriter.Printf("  HPA check: Desired=%d | Current=%d (elapsed: %v)\n",
						hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas, elapsed.Round(time.Second))
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", 1),
						"HPA should have desired replicas > 1 after reading scale-up metric")
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
				GinkgoWriter.Printf("✓ HPA updated desired replicas to > 1 (took %v)\n", time.Since(hpaCheckStart))

				By("Waiting for LWS to scale up and new pods to be ready")
				lwsCheckStart := time.Now()
				Eventually(func(g Gomega) {
					lws := &unstructured.Unstructured{}
					lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
					lws.SetKind("LeaderWorkerSet")
					err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
					g.Expect(err).NotTo(HaveOccurred())

					elapsed := time.Since(lwsCheckStart)
					specReplicas, _, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
					statusReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "replicas")
					readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")

					GinkgoWriter.Printf("  LWS check: Spec=%d | Replicas=%d | Ready=%d | VA recommended=%d (elapsed: %v)\n",
						specReplicas, statusReplicas, readyReplicas, desiredReplicas, elapsed.Round(time.Second))

					g.Expect(statusReplicas).To(BeNumerically(">", int64(minReplicas)),
						fmt.Sprintf("LWS should have more total replicas than minReplicas under load (current: %d, min: %d)", statusReplicas, minReplicas))
					g.Expect(readyReplicas).To(BeNumerically(">=", int64(desiredReplicas)),
						fmt.Sprintf("LWS should have at least %d ready replicas to match VA recommendation (current: %d)", desiredReplicas, readyReplicas))
				}, 10*time.Minute, 10*time.Second).Should(Succeed())

				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				_ = crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				readyReplicas, _, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
				GinkgoWriter.Printf("✓ LWS successfully scaled up under load (took %v)\n", time.Since(lwsCheckStart))
				GinkgoWriter.Printf("  Final state: VA recommended %d replicas, LWS has %d ready replicas\n", desiredReplicas, readyReplicas)

				By("Verifying at least one additional LWS replica group becomes ready")
				Eventually(func(g Gomega) {
					pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
						LabelSelector: "app=" + modelServiceName + "-decode",
					})
					g.Expect(err).NotTo(HaveOccurred())
					// With group size 2, 2 replicas means 4 pods total
					g.Expect(len(pods.Items)).To(BeNumerically(">", int(lwsGroupSize)), "Should have more pods after scale-up")

					readyCount := 0
					for _, pod := range pods.Items {
						for _, condition := range pod.Status.Conditions {
							if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
								readyCount++
								break
							}
						}
					}
					// At least 2 replica groups (4 pods with group size 2) should be ready
					g.Expect(readyCount).To(BeNumerically(">", int(lwsGroupSize)), "At least 2 replica groups should be ready after scale-up")
				}, 5*time.Minute, 10*time.Second).Should(Succeed())
			}

			GinkgoWriter.Printf("LWS successfully scaled up under load\n")
		})
	})

	Context("Basic VA lifecycle with LeaderWorkerSet (single-node)", Serial, Ordered, func() {
		var (
			poolName         = "smoke-test-lws-single-pool"
			modelServiceName = "smoke-test-lws-single-ms"
			lwsName          = modelServiceName + "-decode"
			vaName           = "smoke-test-lws-single-va"
			hpaName          = "smoke-test-lws-single-hpa"
			minReplicas      = int32(1)
			lwsGroupSize     = int32(1) // 1 leader + 0 workers
		)

		BeforeAll(func() {
			By("Cleaning up any existing smoke test resources")
			cleanupSmokeTestResources()

			By("Creating model service LeaderWorkerSet with single-node (leader only)")
			err := fixtures.EnsureModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName, poolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs, lwsGroupSize)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service LWS")

			// Register cleanup for LWS (runs even if test fails)
			DeferCleanup(func() {
				cleanupResource(ctx, "LeaderWorkerSet", cfg.LLMDNamespace, lwsName,
					func() error {
						return fixtures.DeleteModelServiceLWS(ctx, crClient, cfg.LLMDNamespace, modelServiceName)
					},
					func() bool {
						lws := &unstructured.Unstructured{}
						lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
						lws.SetKind("LeaderWorkerSet")
						err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
						return errors.IsNotFound(err)
					})
			})

			By("Creating service to expose single-node LWS model server")
			err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelServiceName, lwsName, 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create service")

			// Register cleanup for service
			DeferCleanup(func() {
				serviceName := modelServiceName + "-service"
				cleanupResource(ctx, "Service", cfg.LLMDNamespace, serviceName,
					func() error {
						return k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			By("Creating ServiceMonitor for single-node LWS metrics scraping")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelServiceName, lwsName)
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

			By("Waiting for single-node LWS to be ready")
			Eventually(func(g Gomega) {
				lws := &unstructured.Unstructured{}
				lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
				lws.SetKind("LeaderWorkerSet")
				err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
				g.Expect(err).NotTo(HaveOccurred())

				readyReplicas, found, _ := unstructured.NestedInt64(lws.Object, "status", "readyReplicas")
				g.Expect(found).To(BeTrue(), "LWS should have status.readyReplicas")
				g.Expect(readyReplicas).To(Equal(int64(1)), "LWS should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource for single-node LWS")
			err = fixtures.EnsureVariantAutoscaling(
				ctx, crClient, cfg.LLMDNamespace, vaName,
				lwsName, cfg.ModelID, cfg.AcceleratorType,
				30.0, cfg.ControllerInstance,
				fixtures.WithScaleTargetKind("LeaderWorkerSet"),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Creating scaler for the single-node LWS (HPA or ScaledObject per backend)")
			if cfg.ScaleToZeroEnabled {
				minReplicas = 0
			}
			if cfg.ScalerBackend == scalerBackendKeda {
				_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaName+"-hpa", metav1.DeleteOptions{})
				err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10, cfg.MonitoringNS,
					fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject")
			} else {
				err = fixtures.EnsureHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName, lwsName, vaName, minReplicas, 10,
					fixtures.WithScaleTargetRefKind("LeaderWorkerSet"))
				Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")
			}
		})

		AfterAll(func() {
			By("Cleaning up single-node LWS test resources")
			if cfg.ScalerBackend == scalerBackendKeda {
				err := fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
				Expect(err).NotTo(HaveOccurred())
			} else {
				hpaNameFull := hpaName + "-hpa"
				cleanupResource(ctx, "HPA", cfg.LLMDNamespace, hpaNameFull,
					func() error {
						return k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			}

			// Delete VA
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				},
			}
			cleanupResource(ctx, "VA", cfg.LLMDNamespace, vaName,
				func() error {
					return crClient.Delete(ctx, va)
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: cfg.LLMDNamespace}, va)
					return errors.IsNotFound(err)
				})
		})

		It("should reconcile the VA successfully with single-node LWS", func() {
			By("Checking VA status conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")

				// Check for TargetResolved condition
				targetResolved := false
				for _, cond := range va.Status.Conditions {
					if cond.Type == variantautoscalingv1alpha1.TypeTargetResolved && cond.Status == metav1.ConditionTrue {
						targetResolved = true
						break
					}
				}
				g.Expect(targetResolved).To(BeTrue(), "VA should have TargetResolved=True condition")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should expose external metrics for the VA with single-node LWS", func() {
			By("Waiting for VA to be reconciled (TargetResolved condition)")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "VA should have TargetResolved condition")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "TargetResolved should be True")
			}).Should(Succeed())

			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists (KEDA backend; external metric name is KEDA-generated)")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject %s should exist", soName)
			} else {
				By("Querying external metrics API for wva_desired_replicas")
				result, err := k8sClient.RESTClient().
					Get().
					AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + cfg.LLMDNamespace + "/" + constants.WVADesiredReplicas).
					DoRaw(ctx)
				if err != nil {
					if errors.IsNotFound(err) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
					}
				} else {
					if strings.Contains(string(result), `"items":[]`) {
						GinkgoWriter.Printf("External metrics API is accessible, but metric %s doesn't exist yet (Engine may not have run)\n", constants.WVADesiredReplicas)
						_, discoveryErr := k8sClient.Discovery().ServerResourcesForGroupVersion("external.metrics.k8s.io/v1beta1")
						Expect(discoveryErr).NotTo(HaveOccurred(), "External metrics API should be accessible")
					} else {
						Expect(string(result)).To(ContainSubstring(constants.WVADesiredReplicas), "Metric response should contain metric name")
						GinkgoWriter.Printf("External metrics API returned metric: %s\n", constants.WVADesiredReplicas)
					}
				}
			}

			By("Verifying DesiredOptimizedAlloc is eventually populated (if Engine has run)")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			getErr := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(getErr).NotTo(HaveOccurred())
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should verify single-node LWS structure with group size 1", func() {
			By("Checking single-node LWS has group size 1")
			lws := &unstructured.Unstructured{}
			lws.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
			lws.SetKind("LeaderWorkerSet")
			err := crClient.Get(ctx, client.ObjectKey{Name: lwsName, Namespace: cfg.LLMDNamespace}, lws)
			Expect(err).NotTo(HaveOccurred())

			size, found, _ := unstructured.NestedInt64(lws.Object, "spec", "leaderWorkerTemplate", "size")
			Expect(found).To(BeTrue(), "LWS should have spec.leaderWorkerTemplate.size")
			Expect(size).To(Equal(int64(lwsGroupSize)), fmt.Sprintf("LWS should have group size %d (leader only)", lwsGroupSize))
		})

		It("should have MetricsAvailable condition set when single-node LWS pods are ready", func() {
			By("Waiting for MetricsAvailable condition to be set")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
				g.Expect(condition.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
					"MetricsAvailable condition should have a valid status")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have scaling controlled by backend with single-node LWS", func() {
			if cfg.ScalerBackend == scalerBackendKeda {
				By("Verifying ScaledObject exists and KEDA has created an HPA for single-node LWS")
				soName := hpaName + "-so"
				so := &unstructured.Unstructured{}
				so.SetAPIVersion("keda.sh/v1alpha1")
				so.SetKind("ScaledObject")
				err := crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: soName}, so)
				Expect(err).NotTo(HaveOccurred(), "ScaledObject should exist")

				Eventually(func(g Gomega) {
					hpaList, listErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
					g.Expect(listErr).NotTo(HaveOccurred())
					var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
					for i := range hpaList.Items {
						h := &hpaList.Items[i]
						if h.Spec.ScaleTargetRef.Name == lwsName {
							kedaHPA = h
							break
						}
					}
					g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the single-node LWS")
					g.Expect(kedaHPA.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			} else {
				By("Verifying HPA exists and is configured for single-node LWS")
				hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "HPA should exist")
				Expect(hpa.Spec.Metrics).NotTo(BeEmpty(), "HPA should have metrics configured")
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use External metric type")
				Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.WVADesiredReplicas), "HPA should use wva_desired_replicas metric")
				Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("LeaderWorkerSet"), "HPA should target LeaderWorkerSet")

				By("Waiting for HPA to read the metric and update status")
				Eventually(func(g Gomega) {
					hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx, hpaName+"-hpa", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(hpa.Status.CurrentReplicas).To(BeNumerically(">=", 0), "HPA should have current replicas set")
					g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">=", 0), "HPA should have desired replicas set")
				}).Should(Succeed())
			}
		})

		It("should verify Prometheus is scraping single-node LWS metrics", func() {
			By("Checking that single-node LWS pods are ready and reporting metrics")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one pod")

				// At least one pod should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(BeNumerically(">", 0), "At least one pod should be ready for metrics scraping")
			}).Should(Succeed())
		})

		It("should collect saturation metrics without triggering scale-up", func() {
			By("Verifying VA is reconciled and has conditions")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
			}).Should(Succeed())

			By("Verifying MetricsAvailable condition indicates metrics collection")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      vaName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())

			condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
			Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")
			if condition.Status == metav1.ConditionTrue {
				Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
					"When metrics are available, reason should be MetricsFound")
			}

			By("Checking if DesiredOptimizedAlloc is populated (best-effort)")
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
				GinkgoWriter.Printf("DesiredOptimizedAlloc is populated: accelerator=%s, replicas=%d\n",
					va.Status.DesiredOptimizedAlloc.Accelerator, *va.Status.DesiredOptimizedAlloc.NumReplicas)
			} else {
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run yet)\n")
			}
		})

		It("should verify single-node LWS pods are created (leader only)", func() {
			By("Checking that single-node LWS created pods with leader only")
			Eventually(func(g Gomega) {
				pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + modelServiceName + "-decode",
				})
				g.Expect(err).NotTo(HaveOccurred())
				// With 1 replica and group size 1, we expect 1 pod total (1 leader + 0 workers)
				g.Expect(pods.Items).To(HaveLen(int(lwsGroupSize)), fmt.Sprintf("Should have %d pod (1 replica × group size %d)", lwsGroupSize, lwsGroupSize))

				// The leader should be ready
				readyCount := 0
				for _, pod := range pods.Items {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyCount++
							break
						}
					}
				}
				g.Expect(readyCount).To(Equal(1), "The leader pod should be ready")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Error handling and graceful degradation", Label("smoke", "full"), Ordered, func() {
		var (
			errorTestPoolName         = "error-test-pool"
			errorTestModelServiceName = "error-test-ms"
			errorTestVAName           = "error-test-va"
		)

		BeforeAll(func() {
			By("Cleaning up any existing smoke test resources")
			cleanupSmokeTestResources()

			deploymentName := errorTestModelServiceName + "-decode"

			By("Creating model service deployment for error handling tests")
			err := fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

			// Register cleanup for deployment
			DeferCleanup(func() {
				cleanupResource(ctx, "Deployment", cfg.LLMDNamespace, deploymentName,
					func() error {
						return k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
					},
					func() bool {
						_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
						return errors.IsNotFound(err)
					})
			})

			By("Waiting for model service to be ready")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)), "Model service should have 1 ready replica")
			}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating VariantAutoscaling resource")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(
				ctx, crClient, cfg.LLMDNamespace, errorTestVAName,
				deploymentName, cfg.ModelID, cfg.AcceleratorType,
				cfg.ControllerInstance,
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

			By("Waiting for VA to reconcile initially")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty(), "VA should have status conditions")
			}).Should(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up error handling test resources")
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				},
			}
			cleanupResource(ctx, "VA", cfg.LLMDNamespace, errorTestVAName,
				func() error {
					return crClient.Delete(ctx, va)
				},
				func() bool {
					err := crClient.Get(ctx, client.ObjectKey{Name: errorTestVAName, Namespace: cfg.LLMDNamespace}, va)
					return errors.IsNotFound(err)
				})
		})

		It("should handle deployment deletion gracefully", func() {
			deploymentName := errorTestModelServiceName + "-decode"

			By("Verifying deployment exists before deletion")
			_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Deployment should exist before deletion")

			By("Deleting the deployment")
			err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to delete deployment")

			By("Waiting for deployment to be fully deleted")
			Eventually(func(g Gomega) {
				_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).To(HaveOccurred(), "Deployment should be deleted")
				g.Expect(errors.IsNotFound(err)).To(BeTrue(), "Error should be NotFound")
			}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).Should(Succeed())

			By("Verifying VA continues to exist after deployment deletion")
			// The VA should continue to exist even when the deployment is deleted
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Name:      errorTestVAName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred(), "VA should continue to exist after deployment deletion")

			// Note: The controller may not immediately detect deployment deletion due to caching.
			// This spec focuses on delete/recreate resilience (VA stays, TargetResolved recovers).
			// An explicit TargetResolved=False assertion on a permanently missing target is optional coverage.

			By("Recreating the deployment")
			err = fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, errorTestModelServiceName, errorTestPoolName, cfg.ModelID, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to recreate model service")

			By("Waiting for deployment to be created and progressing")
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "Deployment should be created")
				// Verify deployment exists and is progressing (may not be ready yet)
				g.Expect(deployment.Status.Replicas).To(BeNumerically(">=", 0), "Deployment should have replica status")
			}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Waiting for deployment to be ready (with extended timeout for recreation)")
			// When recreating, pods may take longer to start (image pull, etc.)
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)),
					"Model service should have 1 ready replica after recreation")
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).Should(Succeed())

			By("Verifying VA automatically resumes operation")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(condition).NotTo(BeNil(), "TargetResolved condition should exist")
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue),
					"TargetResolved should be True when deployment is recreated")
			}).Should(Succeed())
		})

		It("should handle metrics unavailability gracefully", func() {
			By("Verifying MetricsAvailable condition exists and reflects metrics state")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      errorTestVAName,
					Namespace: cfg.LLMDNamespace,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())

				condition := variantautoscalingv1alpha1.GetCondition(va, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should exist")

				// MetricsAvailable can be True or False depending on metrics availability
				// The important thing is that the condition exists and has a valid reason
				switch condition.Status {
				case metav1.ConditionFalse:
					// If metrics are unavailable, reason should indicate why
					g.Expect(condition.Reason).To(BeElementOf(
						variantautoscalingv1alpha1.ReasonMetricsMissing,
						variantautoscalingv1alpha1.ReasonMetricsStale,
						variantautoscalingv1alpha1.ReasonPrometheusError,
						variantautoscalingv1alpha1.ReasonMetricsUnavailable,
					), "When metrics are unavailable, reason should indicate the cause")
				case metav1.ConditionTrue:
					// If metrics are available, reason should be MetricsFound
					g.Expect(condition.Reason).To(Equal(variantautoscalingv1alpha1.ReasonMetricsFound),
						"When metrics are available, reason should be MetricsFound")
				}
			}).Should(Succeed())

			By("Verifying VA continues to reconcile even if metrics are temporarily unavailable")
			// The VA should continue to reconcile and have status conditions even if metrics are unavailable
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      errorTestVAName,
				Namespace: cfg.LLMDNamespace,
			}, va)
			Expect(err).NotTo(HaveOccurred())
			// VA should have status conditions (indicating it's reconciling)
			Expect(va.Status.Conditions).NotTo(BeEmpty(),
				"VA should have status conditions even if metrics are unavailable")
			// DesiredOptimizedAlloc may not be populated if Engine hasn't run due to missing metrics
			// This is acceptable - the important thing is that the VA continues to reconcile
			if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
				// If populated, verify it's valid
				Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be set")
				Expect(*va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0),
					"If DesiredOptimizedAlloc is populated, NumReplicas should be >= 0")
			} else {
				// If not populated, that's okay - Engine may not have run yet
				GinkgoWriter.Printf("DesiredOptimizedAlloc not yet populated (Engine may not have run due to missing metrics)\n")
			}
		})
	})
})
