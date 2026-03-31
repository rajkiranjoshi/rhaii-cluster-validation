package rdma

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// DevicesCheck validates RDMA device presence and accessibility.
type DevicesCheck struct {
	nodeName string
}

func NewDevicesCheck(nodeName string) *DevicesCheck {
	return &DevicesCheck{nodeName: nodeName}
}

func (c *DevicesCheck) Name() string     { return "rdma_devices_detected" }
func (c *DevicesCheck) Category() string { return "networking_rdma" }

func (c *DevicesCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	if _, err := os.Stat("/dev/infiniband"); os.IsNotExist(err) {
		r.Status = checks.StatusFail
		r.Message = "/dev/infiniband not found"
		r.Remediation = "Enable RDMA networking on node pool"
		return r
	}

	output, err := exec.CommandContext(ctx, "ibv_devices").Output()
	var verbsDevices []string
	if err != nil {
		sysOutput, sysErr := exec.CommandContext(ctx, "ls", "/sys/class/infiniband/").Output()
		if sysErr != nil {
			r.Status = checks.StatusFail
			r.Message = fmt.Sprintf("ibv_devices failed: %v; sysfs fallback also failed", err)
			r.Remediation = "Check RDMA device plugin and network operator installation"
			return r
		}
		verbsDevices = strings.Fields(strings.TrimSpace(string(sysOutput)))
	} else {
		verbsDevices = parseIBVDevices(string(output))
	}
	if len(verbsDevices) == 0 {
		r.Status = checks.StatusFail
		r.Message = "No RDMA devices found via ibv_devices or sysfs"
		r.Remediation = "Check RDMA device plugin and network operator installation"
		return r
	}

	var rdmaCapable, verbsOnly []string
	for _, dev := range verbsDevices {
		if hasRDMACapability(ctx, dev) {
			rdmaCapable = append(rdmaCapable, dev)
		} else {
			verbsOnly = append(verbsOnly, dev)
		}
	}

	r.Details = map[string]any{
		"verbs_devices": verbsDevices,
		"rdma_devices":  rdmaCapable,
		"verbs_only":    verbsOnly,
	}

	if len(rdmaCapable) > 0 {
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("%d RDMA-capable device(s): %s", len(rdmaCapable), strings.Join(rdmaCapable, ", "))
		if len(verbsOnly) > 0 {
			r.Message += fmt.Sprintf(" (%d verbs-only: %s)", len(verbsOnly), strings.Join(verbsOnly, ", "))
		}
	} else {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%d verbs device(s) found (%s) but 0 are RDMA-capable (no GIDs)",
			len(verbsOnly), strings.Join(verbsOnly, ", "))
	}
	return r
}

const zeroGID = "0000:0000:0000:0000:0000:0000:0000:0000"

// hasRDMACapability checks if a device has at least one non-zero GID,
// indicating real RDMA transport (IB or RoCE) vs an SR-IOV VF with no RDMA.
func hasRDMACapability(_ context.Context, dev string) bool {
	gid0 := fmt.Sprintf("/sys/class/infiniband/%s/ports/1/gids/0", dev)
	if _, err := os.Stat(gid0); os.IsNotExist(err) {
		return false
	}
	data, err := os.ReadFile(gid0)
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(data))
	return trimmed != "" && trimmed != zeroGID
}

func parseIBVDevices(output string) []string {
	var devices []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "device") || strings.Contains(line, "---") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			devices = append(devices, fields[0])
		}
	}
	return devices
}
