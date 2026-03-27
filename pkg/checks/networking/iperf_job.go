package networking

import (
	"encoding/json"
	"fmt"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

// IperfJob implements the Job interface for TCP bandwidth testing via iperf3.
type IperfJob struct {
	Duration      int                  // test duration in seconds (default 10)
	PassThreshold float64              // Gbps pass threshold
	WarnThreshold float64              // Gbps warn threshold
	PodCfg        *jobrunner.PodConfig // optional pod configuration
	ServerImage   string               // optional custom server image (empty = use default)
	ClientImage   string               // optional custom client image (empty = use default)
}

// NewIperfJob creates an iperf3 TCP bandwidth job.
func NewIperfJob(pass, warn float64, podCfg *jobrunner.PodConfig) *IperfJob {
	return &IperfJob{
		Duration:      10,
		PassThreshold: pass,
		WarnThreshold: warn,
		PodCfg:        podCfg,
	}
}

// NewIperfJobWithImages creates an iperf3 job with custom images.
func NewIperfJobWithImages(pass, warn float64, podCfg *jobrunner.PodConfig, serverImage, clientImage string) *IperfJob {
	return &IperfJob{
		Duration:      10,
		PassThreshold: pass,
		WarnThreshold: warn,
		PodCfg:        podCfg,
		ServerImage:   serverImage,
		ClientImage:   clientImage,
	}
}

func (j *IperfJob) Name() string { return "iperf3-tcp" }

// Implement ImageConfigurable interface
func (j *IperfJob) GetServerImage() string { return j.ServerImage }
func (j *IperfJob) GetClientImage() string { return j.ClientImage }

// Setters for controller to apply config
func (j *IperfJob) SetServerImage(img string) { j.ServerImage = img }
func (j *IperfJob) SetClientImage(img string) { j.ClientImage = img }

func (j *IperfJob) SetPodConfig(cfg *jobrunner.PodConfig) { j.PodCfg = cfg }
func (j *IperfJob) SetThreshold(pass, warn float64) {
	j.PassThreshold = pass
	j.WarnThreshold = warn
}

func (j *IperfJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		[]string{"iperf3", "-s"})
}

func (j *IperfJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		[]string{"iperf3", "-c", serverIP, "-t", fmt.Sprintf("%d", j.Duration), "-J"})
}

func (j *IperfJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	var result iperfJobOutput
	if err := json.Unmarshal([]byte(logs), &result); err != nil {
		return nil, fmt.Errorf("failed to parse iperf3 JSON: %w", err)
	}

	gbps := result.End.SumSent.BitsPerSecond / 1e9
	retransmits := result.End.SumSent.Retransmits

	r := &jobrunner.JobResult{
		Details: map[string]any{
			"bandwidth_gbps": fmt.Sprintf("%.1f", gbps),
			"retransmits":    retransmits,
		},
	}

	switch {
	case gbps >= j.PassThreshold:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (>= %.0f Gbps pass threshold)", gbps, j.PassThreshold)
	case gbps >= j.WarnThreshold:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (>= %.0f Gbps warn, < %.0f Gbps pass)", gbps, j.WarnThreshold, j.PassThreshold)
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (< %.0f Gbps warn threshold)", gbps, j.WarnThreshold)
	}

	return r, nil
}

type iperfJobOutput struct {
	End struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int     `json:"retransmits"`
		} `json:"sum_sent"`
	} `json:"end"`
}
