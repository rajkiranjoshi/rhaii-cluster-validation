package config

import (
	"embed"
	"fmt"

	imagereferences "github.com/opendatahub-io/rhaii-cluster-validation/manifests/image-references"
	"gopkg.in/yaml.v3"
)

//go:embed platforms/*.yaml
var platformFS embed.FS

// Platform represents a detected cloud platform.
type Platform string

const (
	PlatformAKS       Platform = "AKS"
	PlatformEKS       Platform = "EKS"
	PlatformCoreWeave Platform = "CoreWeave"
	PlatformOCP       Platform = "OCP"
	PlatformUnknown   Platform = "Unknown"
)

// PlatformConfig holds platform-specific defaults for validation checks.
type PlatformConfig struct {
	Platform Platform `yaml:"platform" json:"platform"`

	Agent      ResourceConfig `yaml:"agent" json:"agent"`
	Jobs       ResourceConfig `yaml:"jobs" json:"jobs"`
	GPU        GPUConfig      `yaml:"gpu" json:"gpu"`
	CRDs       CRDConfig      `yaml:"crds" json:"crds"`
	Operators  OperatorConfig `yaml:"operators" json:"operators"`
	Thresholds ThresholdConfig `yaml:"thresholds" json:"thresholds"`
	Images     ImageConfig    `yaml:"images" json:"images"`
}

// CRDConfig holds minimum version requirements for required CRDs.
type CRDConfig struct {
	// Map of CRD name to minimum required API version.
	// Empty string means any version is accepted.
	MinAPIVersions map[string]string `yaml:"min_api_versions,omitempty" json:"min_api_versions,omitempty"`
	// Map of CRD name to minimum required release version (semver).
	// Empty string means report only, don't enforce.
	MinReleaseVersions map[string]string `yaml:"min_release_versions,omitempty" json:"min_release_versions,omitempty"`
}

// OperatorConfig holds platform-specific namespace overrides for operator health checks.
type OperatorConfig struct {
	// Map of operator name (e.g. "cert-manager", "istio", "lws") to namespaces to check.
	Namespaces map[string][]string `yaml:"namespaces,omitempty" json:"namespaces,omitempty"`
}

// ResourceConfig holds resource requests, limits, and annotations for pods.
type ResourceConfig struct {
	Requests    map[string]string `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits      map[string]string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

// GPUVendor represents the GPU hardware vendor.
type GPUVendor string

const (
	GPUVendorNVIDIA  GPUVendor = "nvidia"
	GPUVendorAMD     GPUVendor = "amd"
	GPUVendorUnknown GPUVendor = ""
)

// GPUConfig holds GPU-related configuration.
// min_driver_version is configurable per platform. Other vendor-specific settings
// (node selector, device plugin, taint) are auto-detected from node labels at runtime.
type GPUConfig struct {
	MinDriverVersion string `yaml:"min_driver_version" json:"min_driver_version"`
}

// ThresholdConfig holds network performance thresholds.
type ThresholdConfig struct {
	TCPBandwidth     BandwidthThreshold `yaml:"tcp_bandwidth_gbps" json:"tcp_bandwidth_gbps"`
	RDMABandwidthPD  BandwidthThreshold `yaml:"rdma_bandwidth_pd_gbps" json:"rdma_bandwidth_pd_gbps"`
	RDMABandwidthWEP BandwidthThreshold `yaml:"rdma_bandwidth_wep_gbps" json:"rdma_bandwidth_wep_gbps"`
}

// BandwidthThreshold defines pass/warn/fail thresholds for bandwidth.
type BandwidthThreshold struct {
	Pass float64 `yaml:"pass" json:"pass"`
	Warn float64 `yaml:"warn" json:"warn"`
	Fail float64 `yaml:"fail" json:"fail"`
}


// ImageConfig holds container images for multi-node test jobs.
// Allows per-job customization while providing a default fallback.
type ImageConfig struct {
	// Default image for all jobs (if job-specific image not set)
	Default string `yaml:"default" json:"default"`

	// Per-job image overrides (empty string means use Default)
	Jobs JobImages `yaml:"jobs" json:"jobs"`
}

// JobImages maps job types to their container images.
type JobImages struct {
	Iperf3 string `yaml:"iperf3,omitempty" json:"iperf3,omitempty"`
	RDMA   string `yaml:"rdma,omitempty" json:"rdma,omitempty"`
	NCCL   string `yaml:"nccl,omitempty" json:"nccl,omitempty"`
}

// GetJobImage returns the appropriate image for a job type, falling back to default.
func (ic *ImageConfig) GetJobImage(jobType string) string {
	var jobImage string
	switch jobType {
	case "iperf3":
		jobImage = ic.Jobs.Iperf3
	case "rdma":
		jobImage = ic.Jobs.RDMA
	case "nccl":
		jobImage = ic.Jobs.NCCL
	}

	// If job-specific image is empty, use default
	if jobImage == "" {
		return ic.Default
	}
	return jobImage
}

// platformFileMap maps platform names to their embedded config files.
var platformFileMap = map[Platform]string{
	PlatformAKS:       "platforms/aks.yaml",
	PlatformEKS:       "platforms/eks.yaml",
	PlatformCoreWeave: "platforms/coreweave.yaml",
	PlatformOCP:       "platforms/ocp.yaml",
}

// loadImageConfig reads the embedded image configuration from manifests/image-references/jobs.yaml.
func loadImageConfig() (ImageConfig, error) {
	// The YAML file contains just an "images" key, so we need a wrapper struct
	var wrapper struct {
		Images ImageConfig `yaml:"images"`
	}
	if err := yaml.Unmarshal([]byte(imagereferences.JobsYAML), &wrapper); err != nil {
		return ImageConfig{}, fmt.Errorf("failed to parse embedded image config: %w", err)
	}

	return wrapper.Images, nil
}

// GetConfig returns the embedded platform config for the given platform.
// Image configuration is loaded from manifests/image-references/jobs.yaml (shared across platforms).
func GetConfig(platform Platform) (PlatformConfig, error) {
	filename, ok := platformFileMap[platform]
	if !ok {
		// Fall back to AKS defaults for unknown platforms
		filename = platformFileMap[PlatformAKS]
	}

	data, err := platformFS.ReadFile(filename)
	if err != nil {
		return PlatformConfig{}, fmt.Errorf("failed to read embedded config for %s: %w", platform, err)
	}

	var cfg PlatformConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return PlatformConfig{}, fmt.Errorf("failed to parse embedded config for %s: %w", platform, err)
	}

	// Load shared image configuration
	imgCfg, err := loadImageConfig()
	if err != nil {
		return PlatformConfig{}, err
	}
	cfg.Images = imgCfg

	return cfg, nil
}
