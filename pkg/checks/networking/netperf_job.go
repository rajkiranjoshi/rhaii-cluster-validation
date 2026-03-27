package networking

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

// NetperfJob implements the Job interface for TCP latency testing via netperf TCP_RR.
type NetperfJob struct {
	Duration      int                  // test duration in seconds (default 10)
	PassThreshold float64              // milliseconds pass threshold (lower is better)
	WarnThreshold float64              // milliseconds warn threshold
	PodCfg        *jobrunner.PodConfig // optional pod configuration
	ServerImage   string               // optional custom server image (empty = use default)
	ClientImage   string               // optional custom client image (empty = use default)
}

// NewNetperfJob creates a netperf TCP latency job.
func NewNetperfJob(pass, warn float64, podCfg *jobrunner.PodConfig) *NetperfJob {
	return &NetperfJob{
		Duration:      10,
		PassThreshold: pass,
		WarnThreshold: warn,
		PodCfg:        podCfg,
	}
}

// NewNetperfJobWithImages creates a netperf job with custom images.
func NewNetperfJobWithImages(pass, warn float64, podCfg *jobrunner.PodConfig, serverImage, clientImage string) *NetperfJob {
	return &NetperfJob{
		Duration:      10,
		PassThreshold: pass,
		WarnThreshold: warn,
		PodCfg:        podCfg,
		ServerImage:   serverImage,
		ClientImage:   clientImage,
	}
}

func (j *NetperfJob) Name() string { return "netperf-tcp" }

// Implement ImageConfigurable interface
func (j *NetperfJob) GetServerImage() string { return j.ServerImage }
func (j *NetperfJob) GetClientImage() string { return j.ClientImage }

// Setters for controller to apply config
func (j *NetperfJob) SetServerImage(img string) { j.ServerImage = img }
func (j *NetperfJob) SetClientImage(img string) { j.ClientImage = img }

func (j *NetperfJob) SetPodConfig(cfg *jobrunner.PodConfig) { j.PodCfg = cfg }
func (j *NetperfJob) SetThreshold(pass, warn float64) {
	j.PassThreshold = pass
	j.WarnThreshold = warn
}

func (j *NetperfJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		[]string{"netserver", "-D"})
}

func (j *NetperfJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		[]string{"netperf", "-H", serverIP, "-t", "TCP_RR", "-l", fmt.Sprintf("%d", j.Duration), "--", "-o", "mean_latency"})
}

func (j *NetperfJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	// netperf TCP_RR output with -o mean_latency returns a single line with the mean latency in microseconds
	// Example output: "54.32" (microseconds)

	lines := strings.Split(strings.TrimSpace(logs), "\n")

	// Get last non-empty line (netperf output)
	var latencyStr string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			latencyStr = line
			break
		}
	}

	if latencyStr == "" {
		return nil, fmt.Errorf("no latency value found in netperf output")
	}

	// Parse latency in microseconds
	latencyUs, err := strconv.ParseFloat(latencyStr, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse netperf latency %q: %w", latencyStr, err)
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
