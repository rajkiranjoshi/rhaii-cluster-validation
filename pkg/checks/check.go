package checks

import (
	"context"
	"encoding/json"
	"time"
)

// Check is the interface all validation checks must implement.
type Check interface {
	Name() string
	Category() string
	Run(ctx context.Context) Result
}

// Result represents the outcome of a single validation check.
type Result struct {
	Node        string `json:"node,omitempty"`
	Category    string `json:"category"`
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
	Details     any    `json:"details,omitempty"`
}

// Status represents the result of a check.
type Status string

const (
	StatusPass Status = "PASS"
	StatusWarn Status = "WARN"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// GPUInfo describes a single GPU with its PCIe location.
type GPUInfo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	NUMA    int    `json:"numa"`
	PCIAddr string `json:"pci_addr"`
}

// NICInfo describes a single RDMA NIC (HCA) with its PCIe location.
type NICInfo struct {
	Dev       string `json:"dev"`
	NUMA      int    `json:"numa"`
	PCIAddr   string `json:"pci_addr"`
	LinkLayer string `json:"link_layer"` // "InfiniBand" or "Ethernet"
}

// GPUNICPair represents a GPU paired with its closest RDMA NIC.
type GPUNICPair struct {
	GPUID      int    `json:"gpu_id"`
	GPUName    string `json:"gpu_name,omitempty"`
	GPUPCIAddr string `json:"gpu_pci_addr,omitempty"`
	NUMAID     int    `json:"numa_id"`
	NICDev     string `json:"nic_dev"`
	NICNuma    int    `json:"nic_numa"`
	NICPCIAddr string `json:"nic_pci_addr,omitempty"`
}

// NodeTopology holds the GPU-NIC-NUMA mapping for a node.
type NodeTopology struct {
	GPUCount int          `json:"gpu_count"`
	NICCount int          `json:"nic_count"`
	GPUList  []GPUInfo    `json:"gpu_list,omitempty"`
	NICList  []NICInfo    `json:"nic_list,omitempty"`
	Pairs    []GPUNICPair `json:"pairs"`
}

// NodeReport is the complete output from an agent run on a single node.
type NodeReport struct {
	Node      string    `json:"node"`
	Timestamp time.Time `json:"timestamp"`
	Results   []Result  `json:"results"`
}

// ExtractTopology finds the gpu_nic_topology check result and deserializes
// its Details into a NodeTopology. Returns nil if not found.
func ExtractTopology(report NodeReport) *NodeTopology {
	for _, r := range report.Results {
		if r.Name != "gpu_nic_topology" || r.Details == nil {
			continue
		}
		// Details may be *NodeTopology (in-process) or map[string]any (from JSON)
		if topo, ok := r.Details.(*NodeTopology); ok {
			return topo
		}
		data, err := json.Marshal(r.Details)
		if err != nil {
			continue
		}
		var topo NodeTopology
		if json.Unmarshal(data, &topo) == nil && len(topo.Pairs) > 0 {
			return &topo
		}
	}
	return nil
}
