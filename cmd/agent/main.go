package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/gpu"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/controller"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/runner"

	"github.com/spf13/cobra"
)

const (
	CheckModeGPU          = "gpu"
	CheckModeNetworking   = "networking"
	CheckModeNetChecks    = "net-checks"
	CheckModeNetPing      = "net-ping"
	CheckModeNetBandwidth = "net-bandwidth"
	CheckModeAll          = "all"
)

var (
	version      = "dev"
	defaultImage = "ghcr.io/opendatahub-io/rhaii-cluster-validation/odh-rhaii-cluster-validator:latest"
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "kubectl-rhaii_validate",
		Short:         "RHAII cluster validation - GPU, RDMA, and network checks",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Validate GPU cluster readiness for AI/ML workloads.

  gpu          - GPU hardware checks (driver, ECC, memory)
  networking   - Network bandwidth tests (iperf3, RDMA)
  all          - Run all checks (deps + gpu + networking)
  deps         - Check CRDs and operator health (Gateway API, InferencePool, LWS, cert-manager, Istio)`,
	}

	rootCmd.AddCommand(newGPUCmd())
	rootCmd.AddCommand(newNetworkingCmd())
	rootCmd.AddCommand(newNetChecksCmd())
	rootCmd.AddCommand(newNetPingCmd())
	rootCmd.AddCommand(newNetBandwidthCmd())
	rootCmd.AddCommand(newAllCmd())
	rootCmd.AddCommand(newDepsCmd())
	rootCmd.AddCommand(newCleanCmd())

	// Internal: agent mode (used by per-node Job pods, not user-facing)
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newTCPLatServerCmd())
	rootCmd.AddCommand(newTCPLatClientCmd())

	if err := rootCmd.Execute(); err != nil {
		// TODO: surface error details via structured logging or JSON
		// instead of stderr to avoid interleaving with agent JSON output.
		os.Exit(1)
	}
}

// --- gpu subcommand ---

func newGPUCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "gpu",
		Short: "Run GPU hardware checks on all GPU nodes",
		Long:  `Deploys agents to GPU nodes and validates GPU driver version, ECC status, and GPU health.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = CheckModeGPU
			return runDeploy(opts, func(ctrl *controller.Controller) {
				// GPU checks only — no multi-node jobs
			})
		},
	}

	addDeployFlags(cmd, &opts)
	return cmd
}

// --- networking subcommand ---

func newNetworkingCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "networking",
		Short: "Run network bandwidth tests across GPU nodes",
		Long: `Runs per-node networking checks, RDMA connectivity mesh (pingmesh), and
bandwidth tests between GPU nodes. Requires 2+ GPU nodes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = CheckModeNetworking
			return runDeploy(opts, func(ctrl *controller.Controller) {
				ctrl.AddJob(networking.NewIperfJob(0, 0, nil))
				ctrl.AddJob(networking.NewTCPLatencyJob(0, 0, nil))
				ctrl.AddJob(rdma.NewRDMABandwidthJob(0, 0, nil))
			})
		},
	}

	addDeployFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Node to run server on (default: ring topology)")
	cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Specific client nodes (default: all other GPU nodes)")
	return cmd
}

// --- net-checks subcommand ---

func newNetChecksCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "net-checks",
		Short: "Run per-node networking checks (topology, RDMA devices, NIC status)",
		Long: `Runs per-node networking checks without bandwidth tests.
Discovers GPU-NIC-NUMA topology, validates RDMA device presence, and checks NIC link status.
Results are stored in the report ConfigMap for use by net-bandwidth.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = CheckModeNetChecks
			return runDeploy(opts, func(ctrl *controller.Controller) {})
		},
	}

	addDeployFlags(cmd, &opts)
	return cmd
}

// --- net-ping subcommand ---

func newNetPingCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "net-ping",
		Short: "Run RDMA data-plane connectivity mesh (pingmesh)",
		Long: `Tests RDMA data-plane connectivity between all GPU-paired NICs across nodes
using ibv_rc_pingpong. Requires topology from a previous net-checks run.
Reports rail-only and cross-rail connectivity status.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = CheckModeNetPing
			return runDeploy(opts, func(ctrl *controller.Controller) {})
		},
	}

	addDeployFlags(cmd, &opts)
	return cmd
}

// --- net-bandwidth subcommand ---

func newNetBandwidthCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "net-bandwidth",
		Short: "Run multi-node bandwidth tests (iperf3, RDMA)",
		Long: `Runs multi-node bandwidth tests using topology from a previous net-checks or networking run.
Requires 2+ GPU nodes. Uses stored report for GPU-NIC topology mapping.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = CheckModeNetBandwidth
			return runDeploy(opts, func(ctrl *controller.Controller) {
				ctrl.AddJob(networking.NewIperfJob(0, 0, nil))
				ctrl.AddJob(networking.NewTCPLatencyJob(0, 0, nil))
				ctrl.AddJob(rdma.NewRDMABandwidthJob(0, 0, nil))
			})
		},
	}

	addDeployFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Node to run server on (default: ring topology)")
	cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Specific client nodes (default: all other GPU nodes)")
	return cmd
}

// --- all subcommand ---

func newAllCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "all",
		Short: "Run all checks (gpu + networking)",
		Long:  `Full cluster validation: GPU hardware checks, RDMA connectivity mesh (pingmesh), and network bandwidth tests.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = CheckModeAll
			return runDeploy(opts, func(ctrl *controller.Controller) {
				ctrl.AddJob(networking.NewIperfJob(0, 0, nil))
				ctrl.AddJob(networking.NewTCPLatencyJob(0, 0, nil))
				ctrl.AddJob(rdma.NewRDMABandwidthJob(0, 0, nil))
			})
		},
	}

	addDeployFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Node to run server on (default: ring topology)")
	cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Specific client nodes (default: all other GPU nodes)")
	return cmd
}

// --- deps subcommand ---

func newDepsCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "deps",
		Short: "Check required CRDs and dependencies",
		Long: `Validates that required CRDs and operators are healthy:

CRDs:
  - gateways.gateway.networking.k8s.io              (Gateway API)
  - httproutes.gateway.networking.k8s.io             (Gateway API)
  - inferencepools.inference.networking.x-k8s.io     (InferencePool)
  - leaderworkersets.leaderworkerset.x-k8s.io        (LeaderWorkerSet)
  - certificates.cert-manager.io                     (cert-manager)

Operators:
  - cert-manager        (pods running in cert-manager namespace)
  - Istio               (pods running in istio-system namespace)
  - LeaderWorkerSet     (pods running in lws-system / openshift-lws-operator)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			ctrl, err := controller.New(opts, os.Stdout)
			if err != nil {
				return err
			}
			return ctrl.RunDeps(ctx)
		},
	}

	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&opts.ConfigFile, "config", "", "Path to config override file (for CRD min version overrides)")
	cmd.Flags().StringVarP(&opts.OutputFormat, "output", "o", "table", "Output format: table or json")
	return cmd
}

// --- shared helpers ---

// runDeploy creates a controller, applies the setup function, and runs validation.
func runDeploy(opts controller.Options, setup func(*controller.Controller)) error {
	if opts.Image == "" {
		opts.Image = defaultImage
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctrl, err := controller.New(opts, os.Stdout)
	if err != nil {
		return err
	}

	setup(ctrl)
	return ctrl.Run(ctx)
}

// addDeployFlags adds common flags for commands that deploy to a cluster.
func addDeployFlags(cmd *cobra.Command, opts *controller.Options) {
	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&opts.Namespace, "namespace", "rhaii-validation", "Namespace for validation pods")
	cmd.Flags().StringVar(&opts.Image, "image", "", "Agent container image")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Timeout for checks (default 5m)")
	cmd.Flags().StringVar(&opts.ConfigFile, "config", "", "Path to config override file")
	cmd.Flags().BoolVar(&opts.Debug, "debug", false, "Keep pods alive after run for debugging")
	cmd.Flags().StringVarP(&opts.OutputFormat, "output", "o", "table", "Output format: table or json")
	cmd.Flags().StringSliceVar(&opts.Nodes, "nodes", nil, "Restrict to specific GPU nodes (default: all GPU nodes)")
}

// --- clean subcommand ---

func newCleanCmd() *cobra.Command {
	var (
		kubeconfig string
		namespace  string
	)

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove validation resources from cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := controller.Options{
				Kubeconfig: kubeconfig,
				Namespace:  namespace,
				Image:      "unused",
			}
			ctrl, err := controller.New(opts, os.Stdout)
			if err != nil {
				return err
			}
			return ctrl.Cleanup()
		},
	}

	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&namespace, "namespace", "rhaii-validation", "Namespace to clean")
	return cmd
}

// --- run subcommand (internal, used by per-node check Jobs) ---

func newRunCmd() *cobra.Command {
	var (
		nodeName     string
		bandwidth    bool
		iperfServer  string
		tcpThreshold float64
		configFile   string
	)

	cmd := &cobra.Command{
		Use:           "run",
		Short:         "Run checks on current node (internal, used by check Jobs)",
		Hidden:        true,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeName == "" {
				nodeName = os.Getenv("NODE_NAME")
			}
			if nodeName == "" {
				hostname, _ := os.Hostname()
				nodeName = hostname
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			cfg, err := config.Load(config.PlatformUnknown, configFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to load config override: %v, using platform defaults\n", err)
				cfg, _ = config.GetConfig(config.PlatformUnknown)
			}
			fmt.Fprintf(os.Stderr, "Platform config: %s\n", cfg.Platform)

			r := runner.New(nodeName, os.Stdout)

			vendor := os.Getenv("GPU_VENDOR")
			checkMode := os.Getenv("CHECK_MODE")
			if checkMode == "" {
				checkMode = CheckModeAll
			}

			if checkMode == CheckModeGPU || checkMode == CheckModeAll {
				switch config.GPUVendor(vendor) {
				case config.GPUVendorAMD:
					r.AddCheck(gpu.NewAMDDriverCheck(nodeName, cfg.GPU.MinDriverVersion))
					r.AddCheck(gpu.NewAMDECCCheck(nodeName))
				default:
					r.AddCheck(gpu.NewDriverCheck(nodeName, cfg.GPU.MinDriverVersion))
					r.AddCheck(gpu.NewECCCheck(nodeName))
				}
			}

			if checkMode == CheckModeNetworking || checkMode == CheckModeNetChecks || checkMode == CheckModeAll {
				rdmaType, err := checks.NormalizeRDMAType(os.Getenv("RDMA_TYPE"))
				if err != nil {
					return err
				}
				r.AddCheck(rdma.NewDevicesCheck(nodeName))
				r.AddCheck(rdma.NewStatusCheck(nodeName, rdmaType))
				r.AddCheck(rdma.NewTopologyCheck(nodeName, rdmaType))
			}

			if bandwidth {
				threshold := cfg.Thresholds.TCPBandwidth.Pass
				if cmd.Flags().Changed("tcp-threshold") {
					threshold = tcpThreshold
				}
				r.AddCheck(networking.NewTCPBandwidthCheck(nodeName, iperfServer, threshold))
			}

			report, err := r.Run(ctx)
			if err != nil {
				return err
			}
			if runner.HasFailures(report) {
				return fmt.Errorf("one or more checks failed")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&nodeName, "node-name", "", "Node name (auto-detected)")
	cmd.Flags().BoolVar(&bandwidth, "bandwidth", false, "Run bandwidth tests")
	cmd.Flags().StringVar(&iperfServer, "iperf-server", "", "iperf3 server IP")
	cmd.Flags().Float64Var(&tcpThreshold, "tcp-threshold", 25.0, "TCP bandwidth pass threshold (Gbps)")
	cmd.Flags().StringVar(&configFile, "config", "", "Path to config file")

	return cmd
}
