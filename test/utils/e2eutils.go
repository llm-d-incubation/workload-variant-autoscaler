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

package utils

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	gink "github.com/onsi/ginkgo/v2"
	gom "github.com/onsi/gomega"
	promAPI "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	clusterName         = "kind-wva-gpu-cluster"
	prometheusHelmChart = "https://prometheus-community.github.io/helm-charts"
	monitoringNamespace = "workload-variant-autoscaler-monitoring"

	certmanagerVersion = "v1.16.3"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	kedaVersion   = "2.16.1"
	kedaHelmRepo  = "https://kedacore.github.io/charts"
	kedaNamespace = "keda-system"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(gink.GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(gink.GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(gink.GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return string(output), nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
// Includes TLS certificate generation and configuration for HTTPS support.
func InstallPrometheusOperator() error {
	cmd := exec.Command("kubectl", "create", "ns", monitoringNamespace)
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for namespace to be created and active
	cmd = exec.Command("kubectl", "wait", "--for=jsonpath={.status.phase}=Active", "namespace/"+monitoringNamespace, "--timeout=30s")
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to wait for namespace to be ready: %w", err)
	}

	// Generate TLS certificates for Prometheus
	if err := generateTLSCertificates(); err != nil {
		return fmt.Errorf("failed to generate TLS certificates: %w", err)
	}

	// Create Kubernetes secret for TLS certificates
	if err := createTLSCertificateSecret(); err != nil {
		return fmt.Errorf("failed to create TLS certificate secret: %w", err)
	}

	cmd = exec.Command("helm", "repo", "add", "prometheus-community", prometheusHelmChart)
	if _, err := Run(cmd); err != nil {
		return err
	}

	cmd = exec.Command("helm", "repo", "update")
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Install Prometheus with TLS configuration
	cmd = exec.Command("helm", "upgrade", "-i", "kube-prometheus-stack", "prometheus-community/kube-prometheus-stack",
		"-n", monitoringNamespace,
		"-f", "deploy/examples/vllm-emulator/prometheus-operator/prometheus-tls-values.yaml")
	if _, err := Run(cmd); err != nil {
		return err
	}
	return nil
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	cmd := exec.Command("helm", "uninstall", "kube-prometheus-stack", "-n", monitoringNamespace)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	cmd = exec.Command("kubectl", "delete", "ns", monitoringNamespace)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// generateTLSCertificates generates self-signed TLS certificates for Prometheus
func generateTLSCertificates() error {
	// Create TLS certificates directory
	cmd := exec.Command("mkdir", "-p", "hack/tls-certs")
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to create TLS certs directory: %w", err)
	}

	// Check if certificate already exists and is valid
	certFile := "hack/tls-certs/prometheus-cert.pem"
	keyFile := "hack/tls-certs/prometheus-key.pem"

	// Check if certificate is still valid (not expired)
	cmd = exec.Command("openssl", "x509", "-checkend", "86400", "-noout", "-in", certFile)
	if err := cmd.Run(); err == nil {
		// Certificate exists and is valid
		return nil
	}

	// Generate self-signed certificate
	cmd = exec.Command("openssl", "req", "-x509", "-newkey", "rsa:4096",
		"-keyout", keyFile,
		"-out", certFile,
		"-days", "3650",
		"-nodes",
		"-subj", "/CN=prometheus",
		"-addext", "subjectAltName=DNS:kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc,DNS:prometheus,DNS:localhost,IP:127.0.0.1")

	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to generate TLS certificate: %w", err)
	}
	return nil
}

// createTLSCertificateSecret creates a Kubernetes secret for TLS certificates
func createTLSCertificateSecret() error {
	certFile := "hack/tls-certs/prometheus-cert.pem"
	keyFile := "hack/tls-certs/prometheus-key.pem"

	cmd := exec.Command("kubectl", "create", "secret", "tls", "prometheus-tls",
		"--cert="+certFile,
		"--key="+keyFile,
		"-n", monitoringNamespace)

	if _, err := Run(cmd); err != nil {
		// Secret might already exist, which is fine
		fmt.Println("TLS secret already exists or creation failed (this is usually OK)")
	}

	return nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	_, _ = Run(cmd)
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// InstallKEDA installs KEDA using Helm
func InstallKEDA() error {
	// Add Helm repo
	cmd := exec.Command("helm", "repo", "add", "kedacore", kedaHelmRepo)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to add KEDA Helm repo: %w", err)
	}

	// Update Helm repos
	cmd = exec.Command("helm", "repo", "update")
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to update Helm repos: %w", err)
	}

	// Install KEDA
	cmd = exec.Command("helm", "install", "keda", "kedacore/keda",
		"--version", kedaVersion,
		"--namespace", kedaNamespace,
		"--create-namespace",
		"--wait",
		"--timeout", "5m",
	)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to install KEDA: %w", err)
	}

	// Wait for KEDA operator to be ready
	cmd = exec.Command("kubectl", "wait", "deployment/keda-operator",
		"--for", "condition=Available",
		"--namespace", kedaNamespace,
		"--timeout", "5m",
	)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("KEDA operator did not become ready: %w", err)
	}

	return nil
}

// UninstallKEDA uninstalls KEDA
func UninstallKEDA() {
	cmd := exec.Command("helm", "uninstall", "keda", "--namespace", kedaNamespace)
	if _, err := Run(cmd); err != nil {
		fmt.Printf("Failed to uninstall KEDA: %v\n", err)
	}

	// Delete namespace
	cmd = exec.Command("kubectl", "delete", "namespace", kedaNamespace, "--ignore-not-found=true")
	if _, err := Run(cmd); err != nil {
		fmt.Printf("Failed to delete KEDA namespace: %v\n", err)
	}
}

// IsKEDAInstalled checks if KEDA is already installed
func IsKEDAInstalled() bool {
	// Check if KEDA CRDs are installed
	cmd := exec.Command("kubectl", "get", "crd", "scaledobjects.keda.sh")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "scaledobjects.keda.sh")
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string, maxGPUs int) error {
	cluster, err := CheckIfClusterExistsOrCreate(maxGPUs)
	if err != nil {
		return err
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command("kind", kindOptions...)
	_, err = Run(cmd)
	return err
}

func CheckIfClusterExistsOrCreate(maxGPUs int) (string, error) {
	// Check if the kind cluster exists
	existsCmd := exec.Command("kind", "get", "clusters")
	output, err := Run(existsCmd)
	if err != nil {
		return "", err
	}
	clusterExists := false
	clusters := GetNonEmptyLines(output)
	for _, c := range clusters {
		if c == clusterName {
			clusterExists = true
			break
		}
	}

	// Create the kind cluster if it doesn't exist
	expectedVersion := os.Getenv("K8S_EXPECTED_VERSION")
	if !clusterExists {
		scriptCmd := exec.Command("bash", "deploy/kind-emulator/setup.sh", "-g", fmt.Sprintf("%d", maxGPUs), "K8S_VERSION="+expectedVersion)
		if _, err := Run(scriptCmd); err != nil {
			return "", fmt.Errorf("failed to create kind cluster: %v", err)
		}
	} else {
		checkKubernetesVersion(expectedVersion)
	}

	return clusterName, nil
}

// checkKubernetesVersion verifies that the cluster is running the expected Kubernetes version
func checkKubernetesVersion(expectedVersion string) {
	gink.By("checking Kubernetes cluster version")

	expectedVersionClean := strings.TrimPrefix(expectedVersion, "v")

	cmd := exec.Command("kubectl", "version")
	output, err := Run(cmd)
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to get Kubernetes version: %s\n", expectedVersion))
	}

	// Extract server version
	lines := strings.Split(string(output), "\n")
	var serverVersion string
	for _, line := range lines {
		if strings.HasPrefix(line, "Server Version: v") {
			serverVersion = strings.TrimPrefix(line, "Server Version: v")
			break
		}
	}

	// Parse expected version (e.g., "1.32.0" -> major=1, minor=32)
	expectedParts := strings.Split(expectedVersionClean, ".")

	expectedMajor, err := strconv.Atoi(expectedParts[0])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse expected major version: %v", err))
	}

	expectedMinor, err := strconv.Atoi(expectedParts[1])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse expected minor version: %v", err))
	}

	// Parse actual server version (e.g., "1.32.0" -> major=1, minor=32)
	serverParts := strings.Split(serverVersion, ".")

	serverMajor, err := strconv.Atoi(serverParts[0])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse server major version: %v", err))
	}

	serverMinor, err := strconv.Atoi(serverParts[1])
	if err != nil {
		gink.Fail(fmt.Sprintf("failed to parse server minor version: %v", err))
	}

	// Check if actual version is >= expected version
	if serverMajor < expectedMajor || (serverMajor == expectedMajor && serverMinor < expectedMinor) {
		gink.Fail(fmt.Sprintf("Kubernetes version v%s is below required minimum %s\n", serverVersion, expectedVersion))
	}
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %s to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		_, err := out.WriteString(strings.TrimPrefix(scanner.Text(), prefix))
		if err != nil {
			return err
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err := out.WriteString("\n"); err != nil {
			return err
		}
	}

	_, err = out.Write(content[idx+len(target):])
	if err != nil {
		return err
	}
	// false positive
	// nolint:gosec
	return os.WriteFile(filename, out.Bytes(), 0644)
}

// isPortAvailable checks if the specified port is available
func isPortAvailable(port int) (bool, error) {
	// Try to bind to the port to check if it's available
	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return false, err // Port is already in use
	}
	if err := listener.Close(); err != nil {
		gom.Expect(err).NotTo(gom.HaveOccurred(), "Failed to close listener")
	}
	return true, nil // Port is available
}

// StartPortForwarding sets up port forwarding to a Service on the specified port
func startPortForwarding(service *corev1.Service, namespace string, localPort, servicePort int) *exec.Cmd {
	// Check if the port is already in use
	if available, err := isPortAvailable(localPort); !available {
		gink.Fail(fmt.Sprintf("Port %d is already in use. Cannot start port forwarding for service: %s. Error: %v", localPort, service.Name, err))
	}

	portForwardCmd := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("service/%s", service.Name),
		fmt.Sprintf("%d:%d", localPort, servicePort), "-n", namespace)
	err := portForwardCmd.Start()
	gom.Expect(err).NotTo(gom.HaveOccurred(), fmt.Sprintf("Port-forward command should start successfully for service: %s", service.Name))

	// Check if the port-forward process is still running
	gom.Eventually(func() error {
		if portForwardCmd.ProcessState != nil && portForwardCmd.ProcessState.Exited() {
			return fmt.Errorf("port-forward process exited unexpectedly with code: %d", portForwardCmd.ProcessState.ExitCode())
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(gom.Succeed(), fmt.Sprintf("Port-forward to localPort %d should keep running for service: %s with servicePort %d", localPort, service.Name, servicePort))

	return portForwardCmd
}

// StartLoadGenerator sets up and launches a load generator with rate and content specified, targeting the specified model, to the specified port
func StartLoadGenerator(rate, contentLength int, port int, modelName string) *exec.Cmd {
	// Install the load generator requirements
	requirementsCmd := exec.Command("pip", "install", "-r", "tools/vllm-emulator/requirements.txt")
	_, err := Run(requirementsCmd)
	gom.Expect(err).NotTo(gom.HaveOccurred(), "Failed to install loadgen requirements")
	loadGenCmd := exec.Command("python",
		"tools/vllm-emulator/loadgen.py",
		"--url", fmt.Sprintf("http://localhost:%d/v1", port),
		"--rate", fmt.Sprintf("%d", rate),
		"--content", fmt.Sprintf("%d", contentLength),
		"--model", modelName)
	err = loadGenCmd.Start()
	gom.Expect(err).NotTo(gom.HaveOccurred(), fmt.Sprintf("Failed to start load generator sending requests to model: %s, at \"http://localhost:%d/v1\"", modelName, port))

	// Check if the loadgen process is still running
	gom.Eventually(func() error {
		if loadGenCmd.ProcessState != nil && loadGenCmd.ProcessState.Exited() {
			return fmt.Errorf("load generator exited unexpectedly with code: %d", loadGenCmd.ProcessState.ExitCode())
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(gom.Succeed(), fmt.Sprintf("Load generator sending requests to model: %s at \"http://localhost:%d/v1\" should keep running", modelName, port))

	return loadGenCmd
}

func StopCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("command or process is nil")
	}

	// Check if process has already exited
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		fmt.Printf("Warning: Process (PID %d) has already exited with code %d\n",
			cmd.Process.Pid, cmd.ProcessState.ExitCode())
		return nil
	}

	// Try graceful shutdown with SIGINT
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		// If we can't signal, the process might have already exited
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			fmt.Printf("Warning: Process (PID %d) exited before signal could be sent (exit code: %d)\n",
				cmd.Process.Pid, cmd.ProcessState.ExitCode())
			return nil
		}
		return fmt.Errorf("failed to send interrupt signal: %w", err)
	}

	// Wait for graceful shutdown with timeout
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		// Process exited, check if it was due to early termination
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				fmt.Printf("Warning: Process (PID %d) exited early with code %d\n",
					cmd.Process.Pid, exitErr.ExitCode())
				// Don't treat early exit as an error for cleanup purposes
				return nil
			}
			return fmt.Errorf("process exited with error: %w", err)
		}
		return nil
	case <-time.After(5 * time.Second):
		// Timeout - force kill
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}
		// Wait for the kill to complete
		<-done
		return nil
	}
}

func SetUpPortForward(k8sClient *kubernetes.Clientset, ctx context.Context, serviceName, namespace string, localPort, servicePort int) *exec.Cmd {
	service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	gom.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to fetch service")
	portForwardCmd := startPortForwarding(service, namespace, localPort, servicePort)
	return portForwardCmd
}

func VerifyPortForwardReadiness(ctx context.Context, localPort int, request string) error {
	var client *http.Client
	tr := &http.Transport{}
	// Prometheus uses a self-signed certificate, so we need to skip verification when accessing its HTTPS endpoint.
	if request == fmt.Sprintf("https://localhost:%d/api/v1/query?query=up", localPort) {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
		resp, err := client.Get(request)
		if err != nil {
			return false, nil // Retrying
		}
		defer func() {
			err := resp.Body.Close()
			gom.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to close response body")
		}()
		// Retry on 4xx and 5xx errors
		if resp.StatusCode >= 500 {
			fmt.Printf("Debug: Error - Returned status code: %d, retrying...\n", resp.StatusCode)
			return false, nil // Retry on client and server errors
		}

		return true, nil // Success
	})
	return err
}

// ValidateAppLabelUniqueness checks if the appLabel is already in use by other resources and fails if it's not unique
func ValidateAppLabelUniqueness(namespace, appLabel string, k8sClient *kubernetes.Clientset, crClient client.Client) {
	// Create a context with timeout to prevent hanging tests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check if any pods exist with the specified app label
	podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing pods for label uniqueness: %v", err))
	}

	// Check if any deployments exist with the specified app label
	deploymentList, err := k8sClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing deployments for label uniqueness: %v", err))
	}

	// Check if any services exist with the specified app label
	serviceList, err := k8sClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing services for label uniqueness: %v", err))
	}

	// Check if any ServiceMonitors exist with the specified app label
	serviceMonitorList := &unstructured.UnstructuredList{}
	serviceMonitorList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	})
	err = crClient.List(ctx, serviceMonitorList, client.InNamespace(namespace), client.MatchingLabels{"app": appLabel})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing ServiceMonitors for label uniqueness: %v", err))
	}

	// Collects conflicting resources to show in error logs
	var conflicting []string

	if len(podList.Items) > 0 {
		for _, pod := range podList.Items {
			conflicting = append(conflicting, fmt.Sprintf("Pod: %s", pod.Name))
		}
	}

	if len(deploymentList.Items) > 0 {
		for _, deployment := range deploymentList.Items {
			conflicting = append(conflicting, fmt.Sprintf("Deployment: %s", deployment.Name))
		}
	}

	if len(serviceList.Items) > 0 {
		for _, service := range serviceList.Items {
			conflicting = append(conflicting, fmt.Sprintf("Service: %s", service.Name))
		}
	}

	if len(serviceMonitorList.Items) > 0 {
		for _, serviceMonitor := range serviceMonitorList.Items {
			name, found, err := unstructured.NestedString(serviceMonitor.Object, "metadata", "name")
			if err != nil {
				gink.Fail(fmt.Sprintf("Wrong ServiceMonitor name: %v", err))
			} else if !found {
				gink.Fail("ServiceMonitor name not found")
			}
			conflicting = append(conflicting, fmt.Sprintf("ServiceMonitor: %s", name))
		}
	}

	// Fails if any conflicts are found
	if len(conflicting) > 0 {
		gink.Fail(fmt.Sprintf("App label '%s' is not unique in namespace '%s'. Make sure to delete conflicting resources: %s",
			appLabel, namespace, strings.Join(conflicting, ", ")))
	}
}

// ValidateVariantAutoscalingUniqueness checks if the VariantAutoscaling configuration is unique within the namespace
func ValidateVariantAutoscalingUniqueness(namespace, modelId, acc string, crClient client.Client) {
	// Create a context with timeout to prevent hanging tests
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	variantAutoscalingList := &v1alpha1.VariantAutoscalingList{}
	err := crClient.List(ctx, variantAutoscalingList, client.InNamespace(namespace), client.MatchingLabels{"inference.optimization/acceleratorName": acc})
	if err != nil {
		gink.Fail(fmt.Sprintf("Failed to check existing VariantAutoscalings for accelerator label uniqueness: %v", err))
	}

	// found VAs with the same accelerator
	if len(variantAutoscalingList.Items) > 0 {
		var conflicting []string
		for _, va := range variantAutoscalingList.Items {
			// check for same modelId
			if va.Spec.ModelID == modelId {
				conflicting = append(conflicting, fmt.Sprintf("VariantAutoscaling: %s", va.Name))
			}
		}
		// Fails if any conflicts are found
		if len(conflicting) > 0 {
			gink.Fail(fmt.Sprintf("VariantAutoscaling '%s' is not unique in namespace '%s'. Make sure to delete conflicting VAs: %s",
				modelId, namespace, strings.Join(conflicting, ", ")))
		}
	}
}

func LogVariantAutoscalingStatus(ctx context.Context, vaName, namespace string, crClient client.Client) error {
	variantAutoscaling := &v1alpha1.VariantAutoscaling{}
	err := crClient.Get(ctx, client.ObjectKey{Name: vaName, Namespace: namespace}, variantAutoscaling)
	if err != nil {
		return err
	}
	// Load profile is no longer stored in VA status - it's collected separately from Prometheus

	// In single-variant architecture, check if optimization has run by checking NumReplicas >= 0
	// VariantID and Accelerator are in spec, not in OptimizedAlloc
	if variantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas >= 0 {
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s (from spec)\n",
			variantAutoscaling.Name,
			variantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas,
			variantAutoscaling.Spec.Accelerator)
	}
	return nil
}

// creates a vllme deployment with the specified configuration
func CreateVllmeDeployment(namespace, deployName, modelName, appLabel string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                       appLabel,
					"llm-d.ai/inferenceServing": "true",
					"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                       appLabel,
						"llm-d.ai/inferenceServing": "true",
						"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "vllme",
							Image:           "quay.io/infernoautoscaler/vllme:0.2.3-multi-arch",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{ContainerPort: 80},
							},
							Env: []corev1.EnvVar{
								{Name: "MODEL_NAME", Value: modelName},
								{Name: "DECODE_TIME", Value: "20"},
								{Name: "PREFILL_TIME", Value: "20"},
								{Name: "MODEL_SIZE", Value: "25000"},
								{Name: "KVC_PER_TOKEN", Value: "2"},
								{Name: "MAX_SEQ_LEN", Value: "2048"},
								{Name: "MEM_SIZE", Value: "80000"},
								{Name: "AVG_TOKENS", Value: "128"},
								{Name: "TOKENS_DISTRIBUTION", Value: "deterministic"},
								{Name: "MAX_BATCH_SIZE", Value: "8"},
								{Name: "REALTIME", Value: "True"},
								{Name: "MUTE_PRINT", Value: "False"},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:                    resource.MustParse("500m"),
									corev1.ResourceMemory:                 resource.MustParse("1Gi"),
									corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:                    resource.MustParse("100m"),
									corev1.ResourceMemory:                 resource.MustParse("500Mi"),
									corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

// creates a service for the vllme deployment
func CreateVllmeService(namespace, serviceName, appLabel string, nodePort int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       appLabel,
					Port:       80,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(80),
					NodePort:   int32(nodePort),
				},
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}
}

// creates a VariantAutoscaling resource with owner reference to deployment
func CreateVariantAutoscalingResource(namespace, resourceName, modelId, acc string) *v1alpha1.VariantAutoscaling {
	// Performance parameters for different accelerators
	perfParams := map[string]v1alpha1.PerfParms{
		"A100": {
			DecodeParms:  map[string]string{"alpha": "20.58", "beta": "0.41"},
			PrefillParms: map[string]string{"gamma": "20.58", "delta": "0.041"},
		},
		"MI300X": {
			DecodeParms:  map[string]string{"alpha": "0.77", "beta": "0.15"},
			PrefillParms: map[string]string{"gamma": "0.77", "delta": "0.15"},
		},
		"G2": {
			DecodeParms:  map[string]string{"alpha": "17.15", "beta": "0.34"},
			PrefillParms: map[string]string{"gamma": "17.15", "delta": "0.34"},
		},
	}

	// Select perf params for the specified accelerator, default to A100 if not found
	perfParms, ok := perfParams[acc]
	if !ok {
		perfParms = perfParams["A100"]
	}

	// Create variant ID: modelID-accelerator-accCount
	variantID := fmt.Sprintf("%s-%s-%d", modelId, acc, 1)

	return &v1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespace,
			Labels: map[string]string{
				"inference.optimization/acceleratorName": acc,
			},
		},
		Spec: v1alpha1.VariantAutoscalingSpec{
			ModelID:          modelId,
			VariantID:        variantID,
			Accelerator:      acc,
			AcceleratorCount: 1,
			SLOClassRef: v1alpha1.ConfigMapKeyRef{
				Name: "premium",
				Key:  "slo",
			},
			VariantProfile: v1alpha1.VariantProfile{
				PerfParms:    perfParms,
				MaxBatchSize: 4,
			},
		},
	}
}

// creates a ServiceMonitor for vllme metrics collection
func CreateVllmeServiceMonitor(name, namespace, appLabel string) *unstructured.Unstructured {
	serviceMonitor := &unstructured.Unstructured{}
	serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	})
	serviceMonitor.SetName(name)
	serviceMonitor.SetNamespace(namespace)
	serviceMonitor.SetLabels(map[string]string{
		"app":     appLabel,
		"release": "kube-prometheus-stack",
	})

	spec := map[string]any{
		"selector": map[string]any{
			"matchLabels": map[string]any{
				"app": appLabel,
			},
		},
		"endpoints": []any{
			map[string]any{
				"port":     appLabel,
				"path":     "/metrics",
				"interval": "15s",
			},
		},
		"namespaceSelector": map[string]any{
			"any": true,
		},
	}
	serviceMonitor.Object["spec"] = spec

	return serviceMonitor
}

// creates an InferenceModel resource for the specified model
func CreateInferenceModel(name, namespace, modelName string) *unstructured.Unstructured {
	inferenceModel := &unstructured.Unstructured{}
	inferenceModel.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "inference.networking.x-k8s.io",
		Version: "v1alpha2",
		Kind:    "InferenceModel",
	})
	inferenceModel.SetName(name)
	inferenceModel.SetNamespace(namespace)

	spec := map[string]any{
		"modelName":   modelName,
		"criticality": "Critical",
		"poolRef": map[string]any{
			"name": "gaie-sim",
		},
		"targetModels": []any{
			map[string]any{
				"name":   modelName,
				"weight": 100,
			},
		},
	}
	inferenceModel.Object["spec"] = spec

	return inferenceModel
}

// PrometheusQueryResult represents the response from Prometheus API
type PrometheusQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// PrometheusClient wraps the official Prometheus client
type PrometheusClient struct {
	client promv1.API
}

// creates a new Prometheus client for e2e tests
func NewPrometheusClient(baseURL string, insecureSkipVerify bool) (*PrometheusClient, error) {
	config := promAPI.Config{
		Address: baseURL,
	}

	if insecureSkipVerify {
		roundTripper := promAPI.DefaultRoundTripper
		if rt, ok := roundTripper.(*http.Transport); ok {
			rt.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		config.RoundTripper = roundTripper
	}

	client, err := promAPI.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	return &PrometheusClient{
		client: promv1.NewAPI(client),
	}, nil
}

// QueryWithRetry queries Prometheus API with retries and returns the metric value
func (p *PrometheusClient) QueryWithRetry(ctx context.Context, query string) (float64, error) {
	var result float64

	// Define the backoff strategy
	backoff := wait.Backoff{
		Duration: 100 * time.Millisecond, // Initial delay
		Factor:   2.0,                    // Exponential factor
		Jitter:   0.25,                   // 25% jitter
		Steps:    5,                      // Max 5 attempts
		Cap:      5 * time.Second,        // Max delay cap
	}

	// Use wait.ExponentialBackoff for retries
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		value, queryErr := p.executeQuery(ctx, query)
		if queryErr == nil {
			result = value
			return true, nil // Success, stop retrying
		}

		// Check if this is a permanent error (don't retry)
		if isPermanentPrometheusError(queryErr) {
			return false, queryErr // Stop retrying, return error
		}

		// Log retry attempt
		fmt.Printf("Debug: Prometheus query failed, will retry: %v\n", queryErr)
		return false, nil // Continue retrying
	})

	if err != nil {
		return 0, err
	}

	return result, nil
}

// executeQuery performs a single query attempt using the official Prometheus API
func (p *PrometheusClient) executeQuery(ctx context.Context, query string) (float64, error) {
	result, warnings, err := p.client.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("prometheus query failed: %w", err)
	}

	// Log any warnings from Prometheus
	if len(warnings) > 0 {
		fmt.Printf("Debug: Prometheus warnings: %v\n", warnings)
	}

	return extractValueFromResult(result)
}

// extractValueFromResult extracts float64 value from Prometheus query result
func extractValueFromResult(result model.Value) (float64, error) {
	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, fmt.Errorf("no data returned from prometheus query")
		}
		return float64(v[0].Value), nil
	case *model.Scalar:
		return float64(v.Value), nil
	default:
		return 0, fmt.Errorf("unexpected result type: %T", result)
	}
}

// isPermanentPrometheusError determines if a Prometheus error should not be retried
func isPermanentPrometheusError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Permanent errors that shouldn't be retried
	permanentErrors := []string{
		"bad_data",          // Invalid query syntax
		"invalid parameter", // Bad parameters
		"parse error",       // Query parsing failed
		"unauthorized",      // Auth issues
		"forbidden",         // Permission issues
	}

	for _, permErr := range permanentErrors {
		if strings.Contains(strings.ToLower(errStr), permErr) {
			return true
		}
	}

	return false
}

// PromMetricResult represents a single Prometheus metric result with labels
type PromMetricResult struct {
	Metric map[string]string
	Value  float64
}

// QueryPromWithLabels queries Prometheus and returns all results with their labels
func (p *PrometheusClient) QueryPromWithLabels(ctx context.Context, query string) ([]PromMetricResult, error) {
	result, warnings, err := p.client.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query failed: %w", err)
	}

	// Log any warnings from Prometheus
	if len(warnings) > 0 {
		fmt.Printf("Debug: Prometheus warnings: %v\n", warnings)
	}

	// Extract results with labels
	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return nil, fmt.Errorf("no data returned from prometheus query")
		}
		results := make([]PromMetricResult, len(v))
		for i, sample := range v {
			labels := make(map[string]string)
			for k, v := range sample.Metric {
				labels[string(k)] = string(v)
			}
			results[i] = PromMetricResult{
				Metric: labels,
				Value:  float64(sample.Value),
			}
		}
		return results, nil
	default:
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}
}

// GetInfernoReplicaMetrics queries Prometheus for metrics emitted by the Inferno autoscaler
func GetInfernoReplicaMetrics(variantName, namespace, acceleratorType, variantID string) (currentReplicas, desiredReplicas, desiredRatio float64, err error) {

	client, err := NewPrometheusClient("https://localhost:9090", true)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	labels := fmt.Sprintf(`variant_name="%s",exported_namespace="%s",accelerator_type="%s",variant_id="%s"`, variantName, namespace, acceleratorType, variantID)

	// Query both metrics with retries
	currentQuery := fmt.Sprintf(`%s{%s}`, constants.InfernoCurrentReplicas, labels)
	currentReplicas, err = client.QueryWithRetry(ctx, currentQuery)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to query current replicas: %w", err)
	}

	desiredQuery := fmt.Sprintf(`%s{%s}`, constants.InfernoDesiredReplicas, labels)
	desiredReplicas, err = client.QueryWithRetry(ctx, desiredQuery)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to query desired replicas: %w", err)
	}

	desiredRatioQuery := fmt.Sprintf(`%s{%s}`, constants.InfernoDesiredRatio, labels)
	desiredRatio, err = client.QueryWithRetry(ctx, desiredRatioQuery)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to query desired ratio: %w", err)
	}

	return currentReplicas, desiredReplicas, desiredRatio, nil
}

// setupEnvironment sets up necessary environment variables for the E2E tests
func SetupTestEnvironment(image string, numNodes, gpusPerNode int, gpuTypes string) {
	// Set default environment variables for Kind cluster creation
	gom.Expect(os.Setenv("IMG", image)).To(gom.Succeed())
	gom.Expect(os.Setenv("CLUSTER_NAME", clusterName)).To(gom.Succeed())
	gom.Expect(os.Setenv("CLUSTER_NODES", fmt.Sprintf("%d", numNodes))).To(gom.Succeed())
	gom.Expect(os.Setenv("CLUSTER_GPUS", fmt.Sprintf("%d", gpusPerNode))).To(gom.Succeed())
	gom.Expect(os.Setenv("CLUSTER_TYPE", gpuTypes)).To(gom.Succeed())
	gom.Expect(os.Setenv("WVA_IMAGE_PULL_POLICY", "IfNotPresent")).To(gom.Succeed()) // The image is built locally by the tests
	gom.Expect(os.Setenv("CREATE_CLUSTER", "true")).To(gom.Succeed())                // Always create a new cluster for E2E tests

	// Enable components needed for the tests
	gom.Expect(os.Setenv("DEPLOY_LLM_D", "true")).To(gom.Succeed())
	gom.Expect(os.Setenv("DEPLOY_WVA", "true")).To(gom.Succeed())
	gom.Expect(os.Setenv("DEPLOY_PROMETHEUS", "true")).To(gom.Succeed())
	gom.Expect(os.Setenv("APPLY_VLLM_EMULATOR_FIXES", "true")).To(gom.Succeed())

	// Disable components not needed to be deployed by the script
	// Tests create their own vLLM deployments, but script still patches InferencePool
	gom.Expect(os.Setenv("DEPLOY_VLLM_EMULATOR", "false")).To(gom.Succeed())      // we deploy our own vLLM deployments in the tests
	gom.Expect(os.Setenv("DEPLOY_VA", "false")).To(gom.Succeed())                 // we create our own VariantAutoscaling resources in the tests
	gom.Expect(os.Setenv("DEPLOY_HPA", "false")).To(gom.Succeed())                // HPA is not needed for these tests
	gom.Expect(os.Setenv("DEPLOY_PROMETHEUS_ADAPTER", "false")).To(gom.Succeed()) // Prometheus Adapter is not needed for these tests
	gom.Expect(os.Setenv("VLLM_SVC_ENABLED", "false")).To(gom.Succeed())          // we deploy our own Service in the tests
	gom.Expect(os.Setenv("DEPLOY_INFERENCE_MODEL", "false")).To(gom.Succeed())    // we create our own InferenceModel resources in the tests
}
