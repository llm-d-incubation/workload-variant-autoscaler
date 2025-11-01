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

package e2eopenshift

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
)

var lowLoad = numPrompts <= 2000 && requestRate <= 8

var _ = Describe("ShareGPT Scale-Up Test", Ordered, func() {
	var (
		ctx                  context.Context
		jobName              string
		initialReplicas      int32
		initialOptimized     int32
		scaledReplicas       int32
		scaledOptimized      int32
		jobCompletionTimeout = 10 * time.Minute
	)

	BeforeAll(func() {
		ctx = context.Background()
		jobName = "vllm-bench-sharegpt-e2e"

		By("recording initial state of the deployment")
		deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to get vLLM deployment")
		initialReplicas = deploy.Status.ReadyReplicas
		_, _ = fmt.Fprintf(GinkgoWriter, "Initial ready replicas: %d\n", initialReplicas)

		By("recording initial VariantAutoscaling state")
		va := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: llmDNamespace,
			Name:      deployment,
		}, va)
		Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")
		// In single-variant architecture, check that optimization has run by verifying NumReplicas > 0
		Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0), "DesiredOptimizedAlloc should have replicas set")
		initialOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
		_, _ = fmt.Fprintf(GinkgoWriter, "Initial optimized replicas: %d\n", initialOptimized)

		By("verifying HPA exists and is configured correctly")
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).Get(ctx, "vllm-deployment-hpa", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "HPA should exist")
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(deployment), "HPA should target the correct deployment")
		Expect(hpa.Spec.Metrics).To(HaveLen(1), "HPA should have one metric")
		Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use external metrics")
		Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.InfernoDesiredReplicas), "HPA should use inferno_desired_replicas metric")
	})

	It("should verify external metrics API is accessible", func() {
		By("querying external metrics API for inferno_desired_replicas")
		Eventually(func(g Gomega) {
			// Use raw API client to query external metrics
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + llmDNamespace + "/" + constants.InfernoDesiredReplicas).
				DoRaw(ctx)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
			g.Expect(string(result)).To(ContainSubstring(constants.InfernoDesiredReplicas), "Metric should be available")
			g.Expect(string(result)).To(ContainSubstring(deployment), "Metric should be for the correct variant")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("should create and run ShareGPT load generation job", func() {
		By("cleaning up any existing job")
		_ = k8sClient.BatchV1().Jobs(llmDNamespace).Delete(ctx, jobName, metav1.DeleteOptions{})
		// Wait a bit for cleanup
		time.Sleep(2 * time.Second)

		By("creating ShareGPT load generation job")
		job := createShareGPTJob(jobName, llmDNamespace, requestRate, numPrompts)
		_, err := k8sClient.BatchV1().Jobs(llmDNamespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to create load generation job")

		By("waiting for job pod to be running")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(llmDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("job-name=%s", jobName),
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list job pods")
			g.Expect(podList.Items).NotTo(BeEmpty(), "Job pod should exist")

			pod := podList.Items[0]
			// Check if pod is running or has completed initialization
			g.Expect(pod.Status.Phase).To(Or(
				Equal(corev1.PodRunning),
				Equal(corev1.PodSucceeded),
			), fmt.Sprintf("Job pod should be running or succeeded, but is in phase: %s", pod.Status.Phase))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "Load generation job is running\n")
	})

	It("should detect increased load and recommend scale-up", func() {
		By("waiting for load generation to ramp up (30 seconds)")
		time.Sleep(30 * time.Second)

		By("monitoring VariantAutoscaling for scale-up recommendation")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: llmDNamespace,
				Name:      deployment,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			// In single-variant architecture, check that optimization has run by verifying NumReplicas > 0
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0), "DesiredOptimizedAlloc should have replicas set")
			scaledOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
			_, _ = fmt.Fprintf(GinkgoWriter, "Current optimized replicas: %d (initial: %d)\n",
				scaledOptimized, initialOptimized)

			// Expect scale-up recommendation (more than initial)
			if !lowLoad {
				g.Expect(scaledOptimized).To(BeNumerically(">", initialOptimized),
					fmt.Sprintf("WVA should recommend more replicas under load (current: %d, initial: %d)", scaledOptimized, initialOptimized))
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping scale-up recommendation check\n")
			}

		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "WVA detected load and recommended %d replicas (up from %d)\n", scaledOptimized, initialOptimized)
	})

	It("should trigger HPA to scale up the deployment", func() {
		By("monitoring HPA for scale-up action")
		Eventually(func(g Gomega) {
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).Get(ctx, "vllm-deployment-hpa", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get HPA")

			// Check if HPA has processed the new metric value
			g.Expect(hpa.Status.CurrentMetrics).NotTo(BeEmpty(), "HPA should have current metrics")

			// The HPA should show a target value > 1 (indicating scale-up needed)
			if !lowLoad {
				for _, metric := range hpa.Status.CurrentMetrics {
					if metric.External != nil && metric.External.Metric.Name == constants.InfernoDesiredReplicas {
						currentValue := metric.External.Current.AverageValue
						g.Expect(currentValue).NotTo(BeNil(), "Current metric value should not be nil")

						currentReplicas := currentValue.AsApproximateFloat64()
						_, _ = fmt.Fprintf(GinkgoWriter, "HPA current metric value: %.2f\n", currentReplicas)
						g.Expect(currentReplicas).To(BeNumerically(">", float64(initialOptimized)),
							"HPA should see increased replica recommendation")
					}
				}
				// Check desired replicas
				g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", initialReplicas),
					fmt.Sprintf("HPA should desire more replicas (current: %d, initial: %d)", hpa.Status.DesiredReplicas, initialReplicas))
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping HPA scale-up check\n")
			}
		}, 3*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "HPA triggered scale-up\n")
	})

	It("should scale deployment to match recommended replicas", func() {
		By("monitoring deployment for actual scale-up")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")

			scaledReplicas = deploy.Status.ReadyReplicas
			_, _ = fmt.Fprintf(GinkgoWriter, "Current ready replicas: %d (initial: %d, desired: %d)\n",
				scaledReplicas, initialReplicas, scaledOptimized)

			// Verify that deployment has scaled up
			if !lowLoad {
				// Only expect scaling when load is high
				g.Expect(deploy.Status.Replicas).To(BeNumerically(">", initialReplicas),
					"Deployment should have more total replicas under high load")
				g.Expect(scaledReplicas).To(BeNumerically(">=", scaledOptimized),
					fmt.Sprintf("Deployment should have at least %d ready replicas to match optimizer recommendation", scaledOptimized))
			} else {
				// Under low load, scaling up is optional
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping scale-up check\n")
			}

		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "Deployment scaled to %d replicas (up from %d, target was %d)\n", scaledReplicas, initialReplicas, scaledOptimized)
	})

	It("should maintain scaled state while load is active", func() {
		By("verifying deployment stays scaled for at least 30 seconds")
		Consistently(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", scaledOptimized),
				fmt.Sprintf("Deployment should maintain at least %d replicas while job is running", scaledOptimized))
		}, 30*time.Second, 5*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "Deployment maintained %d replicas under load (target: %d)\n", scaledReplicas, scaledOptimized)
	})

	It("should complete the load generation job successfully", func() {
		By("waiting for job to complete")
		Eventually(func(g Gomega) {
			job, err := k8sClient.BatchV1().Jobs(llmDNamespace).Get(ctx, jobName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get job")

			_, _ = fmt.Fprintf(GinkgoWriter, "Job status - Active: %d, Succeeded: %d, Failed: %d\n",
				job.Status.Active, job.Status.Succeeded, job.Status.Failed)

			g.Expect(job.Status.Succeeded).To(BeNumerically(">=", 1), "Job should have succeeded")
		}, jobCompletionTimeout, 15*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "Load generation job completed successfully\n")
	})

	AfterAll(func() {
		By("cleaning up load generation job")
		err := k8sClient.BatchV1().Jobs(llmDNamespace).Delete(ctx, jobName, metav1.DeleteOptions{
			PropagationPolicy: func() *metav1.DeletionPropagation {
				policy := metav1.DeletePropagationBackground
				return &policy
			}(),
		})
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete job: %v\n", err)
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "Test completed - scaled from %d to %d replicas\n", initialReplicas, scaledReplicas)
	})
})

// createShareGPTJob creates a Kubernetes Job that runs vLLM bench with ShareGPT dataset
func createShareGPTJob(name, namespace string, requestRate, numPrompts int) *batchv1.Job {
	backoffLimit := int32(2)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"experiment": "sharegpt-e2e",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name:    "download-dataset",
							Image:   "busybox:latest",
							Command: []string{"/bin/sh", "-c"},
							Args: []string{
								`wget -O /data/ShareGPT_V3_unfiltered_cleaned_split.json \
https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json`,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "dataset-volume",
									MountPath: "/data",
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "vllm-bench-serve-container",
							Image:           "vllm/vllm-openai:latest",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{
									Name:  "HF_HOME",
									Value: "/tmp",
								},
							},
							Command: []string{"/usr/bin/python3"},
							Args: []string{
								"-m",
								"vllm.entrypoints.cli.main",
								"bench",
								"serve",
								"--backend",
								"openai",
								"--base-url",
								fmt.Sprintf("http://%s:80", gatewayName),
								"--dataset-name",
								"sharegpt",
								"--dataset-path",
								"/data/ShareGPT_V3_unfiltered_cleaned_split.json",
								"--model",
								modelID,
								"--seed",
								"12345",
								"--num-prompts",
								fmt.Sprintf("%d", numPrompts),
								"--max-concurrency",
								"512",
								"--request-rate",
								fmt.Sprintf("%d", requestRate),
								"--sharegpt-output-len",
								"1024",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "dataset-volume",
									MountPath: "/data",
								},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("8Gi"),
									corev1.ResourceCPU:    resource.MustParse("2"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("4Gi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "dataset-volume",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
}
