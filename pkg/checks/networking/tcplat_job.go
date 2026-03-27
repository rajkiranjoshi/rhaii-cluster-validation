package networking

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

// TCPLatencyJob implements the Job interface for TCP latency testing.
// Uses built-in tcp-lat-server/client commands (custom Go implementation).
type TCPLatencyJob struct {
	Duration      int                  // test duration in seconds (default 5)
	PassThreshold float64              // milliseconds pass threshold (lower is better)
	WarnThreshold float64              // milliseconds warn threshold
	PodCfg        *jobrunner.PodConfig // optional pod configuration
	ServerImage   string               // optional custom server image (empty = use default)
	ClientImage   string               // optional custom client image (empty = use default)
}

// NewTCPLatencyJob creates a TCP latency job.
func NewTCPLatencyJob(pass, warn float64, podCfg *jobrunner.PodConfig) *TCPLatencyJob {
	return &TCPLatencyJob{
		Duration:      5,
		PassThreshold: pass,
		WarnThreshold: warn,
		PodCfg:        podCfg,
	}
}

// NewTCPLatencyJobWithImages creates a TCP latency job with custom images.
func NewTCPLatencyJobWithImages(pass, warn float64, podCfg *jobrunner.PodConfig, serverImage, clientImage string) *TCPLatencyJob {
	return &TCPLatencyJob{
		Duration:      5,
		PassThreshold: pass,
		WarnThreshold: warn,
		PodCfg:        podCfg,
		ServerImage:   serverImage,
		ClientImage:   clientImage,
	}
}

func (j *TCPLatencyJob) Name() string { return "tcp-latency" }

// Implement ImageConfigurable interface
func (j *TCPLatencyJob) GetServerImage() string { return j.ServerImage }
func (j *TCPLatencyJob) GetClientImage() string { return j.ClientImage }

// Setters for controller to apply config
func (j *TCPLatencyJob) SetServerImage(img string) { j.ServerImage = img }
func (j *TCPLatencyJob) SetClientImage(img string) { j.ClientImage = img }

func (j *TCPLatencyJob) SetPodConfig(cfg *jobrunner.PodConfig) { j.PodCfg = cfg }
func (j *TCPLatencyJob) SetThreshold(pass, warn float64) {
	j.PassThreshold = pass
	j.WarnThreshold = warn
}

func (j *TCPLatencyJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		[]string{"rhaii-validator", "tcp-lat-server"})
}

func (j *TCPLatencyJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		[]string{"rhaii-validator", "tcp-lat-client", "--server", serverIP, "--duration", fmt.Sprintf("%d", j.Duration)})
}

func (j *TCPLatencyJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	// tcp-lat-client outputs mean latency in microseconds on the first line
	// Example output: "54.32" (microseconds)

	lines := strings.Split(strings.TrimSpace(logs), "\n")

	// Get first non-empty line (mean latency from tcp-lat-client)
	var latencyStr string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			latencyStr = line
			break
		}
	}

	if latencyStr == "" {
		return nil, fmt.Errorf("no latency value found in tcp-lat output")
	}

	// Parse latency in microseconds
	latencyUs, err := strconv.ParseFloat(latencyStr, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse TCP latency %q: %w", latencyStr, err)
	}

	// Convert to milliseconds
	latencyMs := latencyUs / 1000.0

	r := &jobrunner.JobResult{
		Details: map[string]any{
			"latency_ms": fmt.Sprintf("%.2f", latencyMs),
			"latency_us": fmt.Sprintf("%.0f", latencyUs),
		},
	}

	switch {
	case latencyMs <= j.PassThreshold:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("TCP latency: %.2f ms (<= %.1f ms pass threshold)", latencyMs, j.PassThreshold)
	case latencyMs <= j.WarnThreshold:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("TCP latency: %.2f ms (<= %.1f ms warn, > %.1f ms pass)", latencyMs, j.WarnThreshold, j.PassThreshold)
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("TCP latency: %.2f ms (> %.1f ms warn threshold)", latencyMs, j.WarnThreshold)
	}

	return r, nil
}
