package rdma

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// TopologyCheck discovers GPU-NIC-NUMA-PCIe mapping on the node.
type TopologyCheck struct {
	nodeName string
	rdmaMode string // "ib", "roce", or "" (all)
}

func NewTopologyCheck(nodeName, rdmaMode string) *TopologyCheck {
	return &TopologyCheck{nodeName: nodeName, rdmaMode: rdmaMode}
}

func (c *TopologyCheck) Name() string     { return "gpu_nic_topology" }
func (c *TopologyCheck) Category() string { return "networking_rdma" }

func (c *TopologyCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	vendor := os.Getenv("GPU_VENDOR")

	gpus, err := discoverGPUs(ctx, vendor)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("GPU discovery failed: %v", err)
		return r
	}

	nics, err := discoverNICs(ctx, c.rdmaMode)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("NIC discovery failed: %v", err)
		return r
	}

	pairs := buildPairs(gpus, nics)

	gpuList := make([]checks.GPUInfo, len(gpus))
	for i, g := range gpus {
		gpuList[i] = checks.GPUInfo{ID: g.id, Name: g.name, NUMA: g.numa, PCIAddr: g.pciAddr}
	}
	nicList := make([]checks.NICInfo, len(nics))
	for i, n := range nics {
		nicList[i] = checks.NICInfo{Dev: n.dev, NUMA: n.numa, PCIAddr: n.pciAddr, LinkLayer: n.linkLayer}
	}

	topo := &checks.NodeTopology{
		GPUCount: len(gpus),
		NICCount: len(nics),
		GPUList:  gpuList,
		NICList:  nicList,
		Pairs:    pairs,
	}

	r.Status = checks.StatusPass
	r.Details = topo

	var pairDescs []string
	for _, p := range pairs {
		pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA%d)", p.GPUID, p.NICDev, p.NUMAID))
	}
	r.Message = fmt.Sprintf("%d GPU(s), %d NIC(s): %s", len(gpus), len(nics), strings.Join(pairDescs, ", "))

	return r
}

type gpuInfo struct {
	id      int
	name    string
	numa    int
	pciAddr string
}

type nicInfo struct {
	dev       string
	numa      int
	pciAddr   string
	linkLayer string // "InfiniBand" or "Ethernet"
}

// discoverGPUs detects GPUs with PCIe addresses. Dispatches to NVIDIA or AMD
// based on vendor string (from GPU_VENDOR env var).
func discoverGPUs(ctx context.Context, vendor string) ([]gpuInfo, error) {
	switch vendor {
	case "amd":
		return discoverAMDGPUs(ctx)
	default:
		return discoverNVIDIAGPUs(ctx)
	}
}

func discoverNVIDIAGPUs(ctx context.Context) ([]gpuInfo, error) {
	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,gpu_name,pci.bus_id",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	var gpus []gpuInfo
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.SplitN(line, ",", 3)
		if len(fields) < 3 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || id < 0 || id > 255 {
			continue
		}
		name := strings.TrimSpace(fields[1])
		pciAddr := strings.TrimSpace(fields[2])

		// nvidia-smi returns 8-digit domain (00000000:19:00.0), sysfs uses 4-digit (0000:19:00.0)
		sysfsAddr := normalizePCIAddr(pciAddr)

		numa := -1
		numaOutput, err := hostExec(ctx, "cat",
			fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", sysfsAddr))
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		gpus = append(gpus, gpuInfo{id: id, name: name, numa: numa, pciAddr: sysfsAddr})
	}

	return gpus, nil
}

// normalizePCIAddr converts an nvidia-smi PCI address (00000000:3B:00.0)
// to the lowercase 4-digit domain format used by sysfs (0000:3b:00.0).
func normalizePCIAddr(addr string) string {
	addr = strings.ToLower(addr)
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) == 2 && len(parts[0]) > 4 {
		return parts[0][len(parts[0])-4:] + ":" + parts[1]
	}
	return addr
}

// discoverAMDGPUs uses amd-smi to list GPUs with PCIe addresses.
// Expected output format from "amd-smi list":
//
//	GPU: 0
//	    BDF: 0000:0c:00.0
//	    UUID: ...
func discoverAMDGPUs(ctx context.Context) ([]gpuInfo, error) {
	output, err := exec.CommandContext(ctx, "amd-smi", "list").Output()
	if err != nil {
		return nil, fmt.Errorf("amd-smi list failed: %w", err)
	}

	var gpus []gpuInfo
	var current *gpuInfo

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "GPU:") {
			if current != nil {
				gpus = append(gpus, *current)
			}
			idStr := strings.TrimSpace(strings.TrimPrefix(line, "GPU:"))
			id, err := strconv.Atoi(idStr)
			if err != nil {
				current = nil
				continue
			}
			current = &gpuInfo{id: id, name: "AMD GPU"}
		} else if strings.HasPrefix(line, "BDF:") && current != nil {
			current.pciAddr = strings.TrimSpace(strings.TrimPrefix(line, "BDF:"))
		}
	}
	if current != nil {
		gpus = append(gpus, *current)
	}

	// Read NUMA from sysfs for each GPU
	for i, g := range gpus {
		if g.pciAddr == "" {
			continue
		}
		numaOutput, err := hostExec(ctx, "cat",
			fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", g.pciAddr))
		if err == nil {
			gpus[i].numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		} else {
			gpus[i].numa = -1
		}
	}

	return gpus, nil
}

// discoverNICs finds RDMA devices with PCIe addresses and link layer type.
// rdmaMode filters by link type: "ib" keeps InfiniBand, "roce" keeps Ethernet,
// empty keeps all.
func discoverNICs(ctx context.Context, rdmaMode string) ([]nicInfo, error) {
	output, err := hostExec(ctx, "ls", "/sys/class/infiniband/")
	if err != nil {
		return nil, fmt.Errorf("no infiniband devices: %w", err)
	}

	var nics []nicInfo
	for _, dev := range strings.Fields(strings.TrimSpace(string(output))) {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}

		// NUMA node
		numaPath := filepath.Join("/sys/class/infiniband", dev, "device/numa_node")
		numaOutput, err := hostExec(ctx, "cat", numaPath)
		numa := -1
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		// PCIe address: readlink device symlink and take the last path component
		pciAddr := ""
		linkOutput, err := hostExec(ctx, "readlink", filepath.Join("/sys/class/infiniband", dev, "device"))
		if err == nil {
			pciAddr = filepath.Base(strings.TrimSpace(string(linkOutput)))
		}

		// Link layer from port 1
		linkLayer := ""
		llOutput, err := hostExec(ctx, "cat",
			filepath.Join("/sys/class/infiniband", dev, "ports/1/link_layer"))
		if err == nil {
			linkLayer = strings.TrimSpace(string(llOutput))
		}

		// Filter by rdmaMode
		if rdmaMode == "ib" && linkLayer != "InfiniBand" {
			continue
		}
		if rdmaMode == "roce" && linkLayer != "Ethernet" {
			continue
		}

		nics = append(nics, nicInfo{dev: dev, numa: numa, pciAddr: pciAddr, linkLayer: linkLayer})
	}

	sort.Slice(nics, func(i, j int) bool { return nics[i].dev < nics[j].dev })

	return nics, nil
}

// buildPairs matches GPUs to NICs based on NUMA affinity.
func buildPairs(gpus []gpuInfo, nics []nicInfo) []checks.GPUNICPair {
	nicsByNuma := make(map[int][]nicInfo)
	for _, nic := range nics {
		nicsByNuma[nic.numa] = append(nicsByNuma[nic.numa], nic)
	}

	nicIdx := make(map[int]int)

	var pairs []checks.GPUNICPair
	for _, gpu := range gpus {
		pair := checks.GPUNICPair{
			GPUID:      gpu.id,
			GPUName:    gpu.name,
			GPUPCIAddr: gpu.pciAddr,
			NUMAID:     gpu.numa,
		}

		if available, ok := nicsByNuma[gpu.numa]; ok && nicIdx[gpu.numa] < len(available) {
			nic := available[nicIdx[gpu.numa]]
			pair.NICDev = nic.dev
			pair.NICNuma = nic.numa
			pair.NICPCIAddr = nic.pciAddr
			nicIdx[gpu.numa]++
		} else if len(nics) > 0 {
			fallbackIdx := gpu.id % len(nics)
			pair.NICDev = nics[fallbackIdx].dev
			pair.NICNuma = nics[fallbackIdx].numa
			pair.NICPCIAddr = nics[fallbackIdx].pciAddr
		}

		pairs = append(pairs, pair)
	}

	return pairs
}
