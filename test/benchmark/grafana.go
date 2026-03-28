package benchmark

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

const (
	grafanaLocalPort = 3000
	grafanaSvcName   = "benchmark-grafana"
	grafanaSvcPort   = 3000
	dashboardUID     = "wva-benchmark-scaleup"
)

// GrafanaClient manages an ephemeral Grafana instance for benchmark snapshots.
type GrafanaClient struct {
	baseURL        string
	httpClient     *http.Client
	portForwardCmd *exec.Cmd
}

// NewGrafanaClient sets up port-forward to Grafana and returns a client.
// It first verifies the Grafana service exists to avoid a fatal assertion
// inside SetUpPortForward if the service is missing.
func NewGrafanaClient(k8sClient *kubernetes.Clientset, ctx context.Context, monitoringNS string) (*GrafanaClient, error) {
	// Pre-check: verify the Grafana service exists before calling SetUpPortForward,
	// which uses Expect() internally and would fail the entire suite.
	if _, err := k8sClient.CoreV1().Services(monitoringNS).Get(ctx, grafanaSvcName, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("grafana service %q not found in namespace %q: %w", grafanaSvcName, monitoringNS, err)
	}

	pfCmd := utils.SetUpPortForward(k8sClient, ctx, grafanaSvcName, monitoringNS, grafanaLocalPort, grafanaSvcPort)

	baseURL := fmt.Sprintf("http://localhost:%d", grafanaLocalPort)

	// Wait for Grafana to respond via port-forward
	attempts := 0
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		attempts++
		resp, httpErr := http.Get(baseURL + "/api/health") //nolint:noctx // simple health check in poll loop
		if httpErr != nil {
			if attempts%5 == 1 {
				fmt.Printf("Grafana health check attempt %d: %v\n", attempts, httpErr)
			}
			return false, nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			fmt.Printf("Grafana health check attempt %d: status %d\n", attempts, resp.StatusCode)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		fmt.Printf("Grafana health check failed after %d attempts\n", attempts)
		if pfCmd.Process != nil {
			_ = pfCmd.Process.Kill()
		}
		return nil, fmt.Errorf("grafana not ready: %w", err)
	}

	return &GrafanaClient{
		baseURL:        baseURL,
		portForwardCmd: pfCmd,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}, nil
}

// Close kills the port-forward process.
func (g *GrafanaClient) Close() {
	if g.portForwardCmd != nil && g.portForwardCmd.Process != nil {
		_ = g.portForwardCmd.Process.Kill()
	}
}

// snapshotRequest is the payload for Grafana's POST /api/snapshots endpoint.
type snapshotRequest struct {
	Dashboard json.RawMessage `json:"dashboard"`
	Name      string          `json:"name"`
	Expires   int             `json:"expires"`
}

// snapshotResponse is the response from Grafana's POST /api/snapshots.
type snapshotResponse struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	DeleteKey string `json:"deleteKey"`
	DeleteURL string `json:"deleteUrl"`
}

// SnapshotResult contains the key and URL from a created Grafana snapshot.
type SnapshotResult struct {
	Key string
	URL string
}

// CreateSnapshot creates a Grafana snapshot of the benchmark dashboard
// covering the time range from scenarioStart to now.
func (g *GrafanaClient) CreateSnapshot(scenarioStart time.Time) (*SnapshotResult, error) {
	dashURL := fmt.Sprintf("%s/api/dashboards/uid/%s", g.baseURL, dashboardUID)
	resp, err := g.httpClient.Get(dashURL)
	if err != nil {
		return nil, fmt.Errorf("fetch dashboard: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch dashboard returned %d: %s", resp.StatusCode, string(body))
	}

	var dashResp struct {
		Dashboard json.RawMessage `json:"dashboard"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dashResp); err != nil {
		return nil, fmt.Errorf("decode dashboard: %w", err)
	}

	var dashModel map[string]interface{}
	if err := json.Unmarshal(dashResp.Dashboard, &dashModel); err != nil {
		return nil, fmt.Errorf("unmarshal dashboard model: %w", err)
	}
	dashModel["time"] = map[string]string{
		"from": scenarioStart.UTC().Format(time.RFC3339),
		"to":   time.Now().UTC().Format(time.RFC3339),
	}
	modifiedDash, err := json.Marshal(dashModel)
	if err != nil {
		return nil, fmt.Errorf("marshal modified dashboard: %w", err)
	}

	snapReq := snapshotRequest{
		Dashboard: modifiedDash,
		Name:      fmt.Sprintf("Benchmark %s", time.Now().UTC().Format("2006-01-02T15:04:05Z")),
		Expires:   0,
	}
	payload, err := json.Marshal(snapReq)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot request: %w", err)
	}

	snapResp, err := g.httpClient.Post(g.baseURL+"/api/snapshots", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	defer func() { _ = snapResp.Body.Close() }()

	if snapResp.StatusCode != 200 {
		body, _ := io.ReadAll(snapResp.Body)
		return nil, fmt.Errorf("create snapshot returned %d: %s", snapResp.StatusCode, string(body))
	}

	var result snapshotResponse
	if err := json.NewDecoder(snapResp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode snapshot response: %w", err)
	}

	return &SnapshotResult{Key: result.Key, URL: result.URL}, nil
}

// ExportSnapshotJSON fetches the full snapshot data and writes it to outputPath.
// The exported JSON can be re-imported into any Grafana via POST /api/snapshots.
func (g *GrafanaClient) ExportSnapshotJSON(snapshotKey string, outputPath string) error {
	url := fmt.Sprintf("%s/api/snapshots/%s", g.baseURL, snapshotKey)
	resp, err := g.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fetch snapshot returned %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read snapshot body: %w", err)
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return os.WriteFile(outputPath, data, 0644)
	}

	return os.WriteFile(outputPath, buf.Bytes(), 0644)
}

// RenderPanel renders a single Grafana panel to PNG and saves it to outputPath.
func (g *GrafanaClient) RenderPanel(panelID int, from, to time.Time, width, height int, outputPath string) error {
	url := fmt.Sprintf("%s/render/d-solo/%s/benchmark?orgId=1&panelId=%d&from=%d&to=%d&width=%d&height=%d",
		g.baseURL, dashboardUID, panelID,
		from.UnixMilli(), to.UnixMilli(),
		width, height,
	)

	resp, err := g.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("render panel %d: %w", panelID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("render panel %d returned %d: %s", panelID, resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read render response: %w", err)
	}

	return os.WriteFile(outputPath, data, 0644)
}

// benchmarkPanel describes a panel in the benchmark dashboard for rendering.
type benchmarkPanel struct {
	ID    int
	Title string
	File  string
	Width int
}

// benchmarkPanels lists the panels to render from the benchmark dashboard.
var benchmarkPanels = []benchmarkPanel{
	{ID: 1, Title: "Deployment Replicas", File: "panel-replicas.png", Width: 1000},
	{ID: 2, Title: "WVA Desired Replicas", File: "panel-desired-replicas.png", Width: 1000},
	{ID: 3, Title: "KV Cache Usage", File: "panel-kv-cache.png", Width: 1000},
	{ID: 4, Title: "Queue Depth", File: "panel-queue-depth.png", Width: 1000},
	{ID: 5, Title: "Saturation Metrics", File: "panel-saturation.png", Width: 1400},
}

// RenderAllPanels renders all benchmark dashboard panels to PNG files in outputDir.
// Returns the list of file paths written, and any errors encountered (best-effort).
func (g *GrafanaClient) RenderAllPanels(from, to time.Time, outputDir string) ([]string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	var rendered []string
	var renderErrors []string

	for _, p := range benchmarkPanels {
		outPath := filepath.Join(outputDir, p.File)
		if err := g.RenderPanel(p.ID, from, to, p.Width, 500, outPath); err != nil {
			renderErrors = append(renderErrors, fmt.Sprintf("%s: %v", p.Title, err))
			continue
		}
		rendered = append(rendered, outPath)
	}

	if len(renderErrors) > 0 && len(rendered) == 0 {
		return nil, fmt.Errorf("all panel renders failed: %s", renderErrors[0])
	}

	return rendered, nil
}
