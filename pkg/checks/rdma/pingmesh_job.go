package rdma

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

const (
	pingmeshPortBase       = 18515
	defaultPingTimeout     = 10
	defaultPingIterations  = 1
	defaultServerBufSec    = 30
)

// PingMeshJob implements jobrunner.Job for pairwise RDMA connectivity testing
// using ibv_rc_pingpong. A single job type handles both RoCEv2 and InfiniBand.
type PingMeshJob struct {
	ServerDevices []string // RDMA devices on the server (destination) node
	ClientDevices []string // RDMA devices on the client (source) node
	RDMAType      config.RDMAType
	GIDIndex      int // -1 = auto-discover; >= 0 = fixed
	Iterations    int
	Timeout       int // per-test timeout in seconds
	PodCfg        *jobrunner.PodConfig
	ServerImage   string
	ClientImage   string
}

func NewPingMeshJob(serverDevs, clientDevs []string, rdmaType config.RDMAType, gidIndex, iterations, timeout int) *PingMeshJob {
	if iterations <= 0 {
		iterations = defaultPingIterations
	}
	if timeout <= 0 {
		timeout = defaultPingTimeout
	}
	return &PingMeshJob{
		ServerDevices: serverDevs,
		ClientDevices: clientDevs,
		RDMAType:      rdmaType,
		GIDIndex:      gidIndex,
		Iterations:    iterations,
		Timeout:       timeout,
	}
}

func (j *PingMeshJob) Name() string { return "pingmesh" }

func (j *PingMeshJob) SetPodConfig(cfg *jobrunner.PodConfig) {
	if cfg == nil {
		cfg = &jobrunner.PodConfig{
			ResourceRequests: make(map[string]string),
			ResourceLimits:   make(map[string]string),
		}
	}
	if cfg.ResourceLimits == nil {
		cfg.ResourceLimits = make(map[string]string)
	}
	for k, v := range cfg.ResourceRequests {
		if k == "cpu" || k == "memory" {
			continue
		}
		if _, ok := cfg.ResourceLimits[k]; !ok {
			cfg.ResourceLimits[k] = v
		}
	}
	cfg.Privileged = true
	j.PodCfg = cfg
}

func (j *PingMeshJob) GetServerImage() string    { return j.ServerImage }
func (j *PingMeshJob) GetClientImage() string    { return j.ClientImage }
func (j *PingMeshJob) SetServerImage(img string) { j.ServerImage = img }
func (j *PingMeshJob) SetClientImage(img string) { j.ClientImage = img }

func (j *PingMeshJob) serverTimeout() int {
	tests := len(j.ServerDevices) * len(j.ClientDevices)
	return tests*j.Timeout + defaultServerBufSec
}

// gidDiscoveryFunc returns the bash function for RoCEv2 GID auto-discovery.
func gidDiscoveryFunc() string {
	return `find_rocev2_gid() {
  local dev=$1
  for i in $(seq 0 255); do
    gtype=$(cat /sys/class/infiniband/$dev/ports/1/gid_attrs/types/$i 2>/dev/null)
    [ -z "$gtype" ] && break
    if [ "$gtype" = "RoCE v2" ]; then
      gid=$(cat /sys/class/infiniband/$dev/ports/1/gids/$i 2>/dev/null)
      if echo "$gid" | grep -q "0000:0000:0000:0000:0000:ffff:"; then
        echo "$i"; return 0
      fi
    fi
  done
  for i in $(seq 0 255); do
    gtype=$(cat /sys/class/infiniband/$dev/ports/1/gid_attrs/types/$i 2>/dev/null)
    [ -z "$gtype" ] && break
    if [ "$gtype" = "RoCE v2" ]; then
      echo "$i"; return 0
    fi
  done
  echo "-1"; return 1
}`
}

// gidFlagExpr returns the bash expression for the -g flag on a per-device basis.
// For RoCE with auto-discover: "-g $(find_rocev2_gid $dev)"
// For RoCE with fixed index:   "-g N"
// For IB:                      "" (empty, no flag)
func (j *PingMeshJob) gidFlagExpr(devVar string) string {
	if j.RDMAType != config.RDMATypeRoCE {
		return ""
	}
	if j.GIDIndex >= 0 {
		return fmt.Sprintf(" -g %d", j.GIDIndex)
	}
	return fmt.Sprintf(" -g $(find_rocev2_gid %s)", devVar)
}

func (j *PingMeshJob) needsGIDDiscovery() bool {
	return j.RDMAType == config.RDMATypeRoCE && j.GIDIndex < 0
}

func (j *PingMeshJob) serverScript() []string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp\nexec 2>/tmp/pm_server_err.log\n\n")

	if j.needsGIDDiscovery() {
		sb.WriteString(gidDiscoveryFunc())
		sb.WriteString("\nexport -f find_rocev2_gid\n\n")
	}

	sb.WriteString(fmt.Sprintf("timeout %d bash -c '\nidx=0\n", j.serverTimeout()))
	for _, sdev := range j.ServerDevices {
		if !checks.ValidDeviceName.MatchString(sdev) {
			continue
		}
		gidFlag := j.gidFlagExpr("$sdev")
		sb.WriteString(fmt.Sprintf("sdev=%s\n", sdev))
		sb.WriteString(fmt.Sprintf("for cslot in $(seq 0 %d); do\n", len(j.ClientDevices)-1))
		sb.WriteString(fmt.Sprintf("  ibv_rc_pingpong -d $sdev%s -p $((18515 + idx)) -n %d > /dev/null 2>&1 &\n", gidFlag, j.Iterations))
		sb.WriteString("  idx=$((idx + 1))\ndone\n")
	}
	sb.WriteString("wait\n' > /dev/null 2>&1 || true\n")

	return []string{"bash", "-c", sb.String()}
}

func (j *PingMeshJob) clientScript(serverIP string) []string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp/pm\nexec 2>/tmp/pm/script_stderr.log\n\n")

	if j.needsGIDDiscovery() {
		sb.WriteString(gidDiscoveryFunc())
		sb.WriteString("\n\n")
	}

	sb.WriteString("idx=0\n")
	for _, sdev := range j.ServerDevices {
		if !checks.ValidDeviceName.MatchString(sdev) {
			continue
		}
		for _, cdev := range j.ClientDevices {
			if !checks.ValidDeviceName.MatchString(cdev) {
				continue
			}
			gidFlag := j.gidFlagExpr(fmt.Sprintf("%s", cdev))
			sb.WriteString(fmt.Sprintf(
				"timeout %d ibv_rc_pingpong -d %s%s -p $((18515 + idx)) -n %d %s > /tmp/pm/out_${idx}.txt 2>&1\n",
				j.Timeout, cdev, gidFlag, j.Iterations, serverIP,
			))
			sb.WriteString(fmt.Sprintf("echo '%s:%s:'$? >> /tmp/pm/results.txt\n", cdev, sdev))
			sb.WriteString("idx=$((idx + 1))\n")
		}
	}

	// Assemble JSON from results file
	sb.WriteString(`
printf '['
first=1
idx=0
while IFS=: read -r cdev sdev rc; do
  [ $first -eq 0 ] && printf ','
  first=0
  if [ "$rc" -eq 0 ]; then
    printf '{"src_dev":"%s","dst_dev":"%s","pass":true}' "$cdev" "$sdev"
  else
    err=$(head -c 200 /tmp/pm/out_${idx}.txt 2>/dev/null | tr '"' "'" | tr '\n' ' ')
    printf '{"src_dev":"%s","dst_dev":"%s","pass":false,"error":"%s"}' "$cdev" "$sdev" "$err"
  fi
  idx=$((idx + 1))
done < /tmp/pm/results.txt
printf ']'
`)

	return []string{"bash", "-c", sb.String()}
}

func (j *PingMeshJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		j.serverScript())
}

func (j *PingMeshJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		j.clientScript(serverIP))
}

// ParseResult parses the client pod JSON array into a JobResult.
func (j *PingMeshJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	// Defensive extraction: find the JSON array bounds
	start := strings.Index(logs, "[")
	end := strings.LastIndex(logs, "]")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found in pingmesh client output")
	}
	jsonStr := logs[start : end+1]

	var results []PingMeshPairResult
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		return nil, fmt.Errorf("failed to parse pingmesh JSON: %w", err)
	}

	passed := 0
	for _, r := range results {
		if r.Pass {
			passed++
		}
	}

	status := checks.StatusPass
	if passed == 0 && len(results) > 0 {
		status = checks.StatusFail
	} else if passed < len(results) {
		status = checks.StatusFail
	}

	return &jobrunner.JobResult{
		Status:  status,
		Message: fmt.Sprintf("Pingmesh: %d/%d NIC pairs passed", passed, len(results)),
		Details: results,
	}, nil
}
