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

// crossNUMAPenalty is added to PCIeHops when a GPU-NIC pair crosses NUMA
// boundaries, making cross-NUMA assignments immediately visible in output.
const crossNUMAPenalty = 100

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

	pairs, flat, strategy := buildPairs(gpus, nics)

	gpuList := make([]checks.GPUInfo, len(gpus))
	for i, g := range gpus {
		gpuList[i] = checks.GPUInfo{ID: g.id, Name: g.name, NUMA: g.numa, PCIAddr: g.pciAddr}
	}
	nicList := make([]checks.NICInfo, len(nics))
	for i, n := range nics {
		nicList[i] = checks.NICInfo{Dev: n.dev, NUMA: n.numa, PCIAddr: n.pciAddr, LinkLayer: n.linkLayer}
	}

	topo := &checks.NodeTopology{
		GPUCount:        len(gpus),
		NICCount:        len(nics),
		IsFlat:          flat,
		PairingStrategy: strategy,
		GPUList:         gpuList,
		NICList:         nicList,
		Pairs:           pairs,
	}

	r.Details = topo

	switch {
	case len(nics) == 0:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%d GPU(s), 0 NIC(s): no RDMA NICs found matching rdma_type=%q", len(gpus), c.rdmaMode)
		return r
	case len(gpus) > len(nics):
		r.Status = checks.StatusWarn
	default:
		r.Status = checks.StatusPass
	}

	var pairDescs []string
	for _, p := range pairs {
		pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA%d)", p.GPUID, p.NICDev, p.NUMAID))
	}
	r.Message = fmt.Sprintf("%d GPU(s), %d NIC(s), strategy=%s: %s",
		len(gpus), len(nics), strategy, strings.Join(pairDescs, ", "))

	return r
}

type gpuInfo struct {
	id       int
	name     string
	numa     int
	pciAddr  string
	pciePath []string // full PCIe hierarchy from sysfs
}

type nicInfo struct {
	dev       string
	numa      int
	pciAddr   string
	linkLayer string   // "InfiniBand" or "Ethernet"
	pciePath  []string // full PCIe hierarchy from sysfs
}

// sysfsExec runs a command directly (no chroot). Privileged containers
// see the host's /sys natively, so chroot is unnecessary for sysfs reads.
func sysfsExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// ---------------------------------------------------------------------------
// GPU discovery
// ---------------------------------------------------------------------------

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
		numaOutput, err := sysfsExec(ctx, "cat",
			fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", sysfsAddr))
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		pciePath := discoverPCIePath(ctx, sysfsAddr)

		gpus = append(gpus, gpuInfo{id: id, name: name, numa: numa, pciAddr: sysfsAddr, pciePath: pciePath})
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

	for i, g := range gpus {
		if g.pciAddr == "" {
			continue
		}
		numaOutput, err := sysfsExec(ctx, "cat",
			fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", g.pciAddr))
		if err == nil {
			gpus[i].numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		} else {
			gpus[i].numa = -1
		}
		gpus[i].pciePath = discoverPCIePath(ctx, g.pciAddr)
	}

	return gpus, nil
}

// ---------------------------------------------------------------------------
// NIC discovery
// ---------------------------------------------------------------------------

// discoverNICs finds RDMA devices with PCIe addresses and link layer type.
// rdmaMode filters by link type: "ib" keeps InfiniBand, "roce" keeps Ethernet,
// empty keeps all.
func discoverNICs(ctx context.Context, rdmaMode string) ([]nicInfo, error) {
	output, err := sysfsExec(ctx, "ls", "/sys/class/infiniband/")
	if err != nil {
		return nil, fmt.Errorf("no infiniband devices: %w", err)
	}

	var nics []nicInfo
	for _, dev := range strings.Fields(strings.TrimSpace(string(output))) {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}

		numaPath := filepath.Join("/sys/class/infiniband", dev, "device/numa_node")
		numaOutput, err := sysfsExec(ctx, "cat", numaPath)
		numa := -1
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		// readlink -f gives the full sysfs path: PCI address is the last segment,
		// and the full set of segments is the PCIe hierarchy.
		pciAddr := ""
		var pciePath []string
		fullPathOutput, err := sysfsExec(ctx, "readlink", "-f",
			filepath.Join("/sys/class/infiniband", dev, "device"))
		if err == nil {
			fullPath := strings.TrimSpace(string(fullPathOutput))
			pciAddr = filepath.Base(fullPath)
			pciePath = parsePCIePath(fullPath)
		}

		linkLayer := ""
		llOutput, err := sysfsExec(ctx, "cat",
			filepath.Join("/sys/class/infiniband", dev, "ports/1/link_layer"))
		if err == nil {
			linkLayer = strings.TrimSpace(string(llOutput))
		}

		if rdmaMode == "ib" && linkLayer != "InfiniBand" {
			continue
		}
		if rdmaMode == "roce" && linkLayer != "Ethernet" {
			continue
		}

		nics = append(nics, nicInfo{dev: dev, numa: numa, pciAddr: pciAddr, linkLayer: linkLayer, pciePath: pciePath})
	}

	sort.Slice(nics, func(i, j int) bool { return nics[i].dev < nics[j].dev })

	return nics, nil
}

// ---------------------------------------------------------------------------
// PCIe hierarchy helpers
// ---------------------------------------------------------------------------

// discoverPCIePath resolves the full sysfs path for a PCI device and returns
// the hierarchy as path segments.
func discoverPCIePath(ctx context.Context, pciAddr string) []string {
	output, err := sysfsExec(ctx, "readlink", "-f",
		fmt.Sprintf("/sys/bus/pci/devices/%s", pciAddr))
	if err != nil {
		return nil
	}
	return parsePCIePath(strings.TrimSpace(string(output)))
}

// parsePCIePath extracts PCI address segments from a sysfs device path.
// Input:  /sys/devices/pci0000:37/0000:37:00.0/0000:38:00.0/0000:3b:00.0
// Output: ["0000:37:00.0", "0000:38:00.0", "0000:3b:00.0"]
func parsePCIePath(sysfsPath string) []string {
	parts := strings.Split(sysfsPath, "/")
	var segments []string
	foundRoot := false
	for _, p := range parts {
		if strings.HasPrefix(p, "pci") {
			foundRoot = true
			continue
		}
		if foundRoot && strings.Contains(p, ":") {
			segments = append(segments, p)
		}
	}
	return segments
}

// pcieSharedPrefix returns the length of the longest shared prefix between
// two PCIe paths.
func pcieSharedPrefix(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// pcieDistance computes the number of hops between two devices in the PCIe tree.
// distance = hops up to common ancestor + hops down.
func pcieDistance(a, b []string) int {
	shared := pcieSharedPrefix(a, b)
	return (len(a) - shared) + (len(b) - shared)
}

// isFlat returns true when all device PCIe paths have at most 1 segment,
// indicating a flat/virtual PCI bus with no hierarchy.
func isFlat(gpus []gpuInfo, nics []nicInfo) bool {
	for _, g := range gpus {
		if len(g.pciePath) > 1 {
			return false
		}
	}
	for _, n := range nics {
		if len(n.pciePath) > 1 {
			return false
		}
	}
	return true
}

// hasPCIePaths returns true if at least some devices have multi-segment
// PCIe path information suitable for distance calculations.
func hasPCIePaths(gpus []gpuInfo, nics []nicInfo) bool {
	for _, g := range gpus {
		if len(g.pciePath) >= 2 {
			return true
		}
	}
	for _, n := range nics {
		if len(n.pciePath) >= 2 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Pairing strategies
// ---------------------------------------------------------------------------

// buildPairs dispatches to the appropriate pairing strategy based on
// topology shape and GPU:NIC ratio. Returns pairs, isFlat flag, and
// the strategy name.
func buildPairs(gpus []gpuInfo, nics []nicInfo) ([]checks.GPUNICPair, bool, string) {
	if len(nics) == 0 || len(gpus) == 0 {
		return nil, true, "numa_affinity"
	}

	flat := isFlat(gpus, nics)
	if flat || !hasPCIePaths(gpus, nics) {
		return numaAffinityPairing(gpus, nics), flat, "numa_affinity"
	}

	if len(gpus) == len(nics) {
		return pcieDistancePairing(gpus, nics), false, "pcie_distance"
	}

	return numaLoadBalancePairing(gpus, nics), false, "numa_load_balance"
}

// numaAffinityPairing pairs GPUs to NICs by NUMA affinity with ordered matching.
// Within each NUMA group, GPUs are sorted by ID and NICs by device name, then
// zipped 1:1 (with round-robin when GPUs > NICs within a NUMA). Used as the
// fallback strategy for flat topologies or when PCIe paths are unavailable.
func numaAffinityPairing(gpus []gpuInfo, nics []nicInfo) []checks.GPUNICPair {
	gpusByNuma := groupGPUsByNuma(gpus)
	nicsByNuma := groupNICsByNuma(nics)

	// Sort within groups
	for numa := range gpusByNuma {
		sortGPUsByID(gpusByNuma[numa])
	}
	for numa := range nicsByNuma {
		sortNICsByDev(nicsByNuma[numa])
	}

	assignedGPU := make(map[int]bool)
	var pairs []checks.GPUNICPair

	// Phase 1: within-NUMA ordered zip
	for numa, numaGPUs := range gpusByNuma {
		numaNICs := nicsByNuma[numa]
		if len(numaNICs) == 0 {
			continue
		}
		for i, g := range numaGPUs {
			nic := numaNICs[i%len(numaNICs)]
			pairs = append(pairs, makePair(g, nic, 0))
			assignedGPU[g.id] = true
		}
	}

	// Phase 2: cross-NUMA fallback for GPUs on NUMA nodes with no NICs
	if len(pairs) < len(gpus) && len(nics) > 0 {
		allNICs := sortedNICsCopy(nics)
		fallbackIdx := 0
		for _, g := range gpus {
			if assignedGPU[g.id] {
				continue
			}
			nic := allNICs[fallbackIdx%len(allNICs)]
			pairs = append(pairs, makePair(g, nic, crossNUMAPenalty))
			fallbackIdx++
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPUID < pairs[j].GPUID })
	return pairs
}

// pcieDistancePairing matches GPUs to NICs 1:1 using PCIe tree distance.
// Phase 1: within each NUMA group, greedily assign by shortest PCIe distance.
// Phase 2: match remaining GPUs and NICs cross-NUMA by PCIe distance.
func pcieDistancePairing(gpus []gpuInfo, nics []nicInfo) []checks.GPUNICPair {
	gpusByNuma := groupGPUsByNuma(gpus)
	nicsByNuma := groupNICsByNuma(nics)

	assignedGPU := make(map[int]bool)
	assignedNIC := make(map[string]bool)
	var pairs []checks.GPUNICPair

	// Phase 1: within-NUMA greedy matching
	for numa, numaGPUs := range gpusByNuma {
		numaNICs := nicsByNuma[numa]
		if len(numaNICs) == 0 {
			continue
		}

		candidates := buildCandidates(numaGPUs, numaNICs)
		for _, c := range candidates {
			if assignedGPU[c.gpu.id] || assignedNIC[c.nic.dev] {
				continue
			}
			pairs = append(pairs, makePair(c.gpu, c.nic, c.dist))
			assignedGPU[c.gpu.id] = true
			assignedNIC[c.nic.dev] = true
		}
	}

	// Phase 2: cross-NUMA for remaining
	var remainGPUs []gpuInfo
	var remainNICs []nicInfo
	for _, g := range gpus {
		if !assignedGPU[g.id] {
			remainGPUs = append(remainGPUs, g)
		}
	}
	for _, n := range nics {
		if !assignedNIC[n.dev] {
			remainNICs = append(remainNICs, n)
		}
	}

	if len(remainGPUs) > 0 && len(remainNICs) > 0 {
		candidates := buildCandidates(remainGPUs, remainNICs)
		for _, c := range candidates {
			if assignedGPU[c.gpu.id] || assignedNIC[c.nic.dev] {
				continue
			}
			pairs = append(pairs, makePair(c.gpu, c.nic, c.dist+crossNUMAPenalty))
			assignedGPU[c.gpu.id] = true
			assignedNIC[c.nic.dev] = true
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPUID < pairs[j].GPUID })
	return pairs
}

// numaLoadBalancePairing distributes GPUs across NICs within NUMA groups
// when GPU and NIC counts differ. Uses PCIe distance for preference when
// paths are available, with even per-NIC capacity within each NUMA group.
func numaLoadBalancePairing(gpus []gpuInfo, nics []nicInfo) []checks.GPUNICPair {
	gpusByNuma := groupGPUsByNuma(gpus)
	nicsByNuma := groupNICsByNuma(nics)

	assignedGPU := make(map[int]bool)
	var pairs []checks.GPUNICPair

	// Phase 1: within-NUMA load balance
	for numa, numaGPUs := range gpusByNuma {
		numaNICs := nicsByNuma[numa]
		if len(numaNICs) == 0 {
			continue
		}

		sortGPUsByID(numaGPUs)
		sortNICsByDev(numaNICs)

		capacity := (len(numaGPUs) + len(numaNICs) - 1) / len(numaNICs)
		nicLoad := make(map[string]int)

		candidates := buildCandidates(numaGPUs, numaNICs)
		for _, c := range candidates {
			if assignedGPU[c.gpu.id] || nicLoad[c.nic.dev] >= capacity {
				continue
			}
			pairs = append(pairs, makePair(c.gpu, c.nic, c.dist))
			assignedGPU[c.gpu.id] = true
			nicLoad[c.nic.dev]++
		}
	}

	// Phase 2: cross-NUMA fallback
	if len(pairs) < len(gpus) && len(nics) > 0 {
		allNICs := sortedNICsCopy(nics)
		fallbackIdx := 0
		for _, g := range gpus {
			if assignedGPU[g.id] {
				continue
			}
			nic := allNICs[fallbackIdx%len(allNICs)]
			dist := 0
			if len(g.pciePath) >= 2 && len(nic.pciePath) >= 2 {
				dist = pcieDistance(g.pciePath, nic.pciePath)
			}
			pairs = append(pairs, makePair(g, nic, dist+crossNUMAPenalty))
			fallbackIdx++
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPUID < pairs[j].GPUID })
	return pairs
}

// ---------------------------------------------------------------------------
// Shared helpers for pairing strategies
// ---------------------------------------------------------------------------

type pairCandidate struct {
	gpu  gpuInfo
	nic  nicInfo
	dist int
}

// buildCandidates computes all GPU-NIC pair candidates sorted by PCIe
// distance (ascending), with deterministic tie-breaking by GPU ID then
// NIC device name.
func buildCandidates(gpus []gpuInfo, nics []nicInfo) []pairCandidate {
	var candidates []pairCandidate
	for _, g := range gpus {
		for _, n := range nics {
			dist := 0
			if len(g.pciePath) >= 2 && len(n.pciePath) >= 2 {
				dist = pcieDistance(g.pciePath, n.pciePath)
			}
			candidates = append(candidates, pairCandidate{gpu: g, nic: n, dist: dist})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		if candidates[i].gpu.id != candidates[j].gpu.id {
			return candidates[i].gpu.id < candidates[j].gpu.id
		}
		return candidates[i].nic.dev < candidates[j].nic.dev
	})
	return candidates
}

func makePair(g gpuInfo, n nicInfo, hops int) checks.GPUNICPair {
	return checks.GPUNICPair{
		GPUID:      g.id,
		GPUName:    g.name,
		GPUPCIAddr: g.pciAddr,
		NUMAID:     g.numa,
		NICDev:     n.dev,
		NICNuma:    n.numa,
		NICPCIAddr: n.pciAddr,
		PCIeHops:   hops,
	}
}

func groupGPUsByNuma(gpus []gpuInfo) map[int][]gpuInfo {
	m := make(map[int][]gpuInfo)
	for _, g := range gpus {
		m[g.numa] = append(m[g.numa], g)
	}
	return m
}

func groupNICsByNuma(nics []nicInfo) map[int][]nicInfo {
	m := make(map[int][]nicInfo)
	for _, n := range nics {
		m[n.numa] = append(m[n.numa], n)
	}
	return m
}

func sortGPUsByID(gpus []gpuInfo) {
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].id < gpus[j].id })
}

func sortNICsByDev(nics []nicInfo) {
	sort.Slice(nics, func(i, j int) bool { return nics[i].dev < nics[j].dev })
}

func sortedNICsCopy(nics []nicInfo) []nicInfo {
	cp := make([]nicInfo, len(nics))
	copy(cp, nics)
	sortNICsByDev(cp)
	return cp
}
