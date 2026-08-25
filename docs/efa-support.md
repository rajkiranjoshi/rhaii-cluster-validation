# Composite RDMA Tools Image

Status: **Validated on EKS (EFA/SRD) and CoreWeave (InfiniBand)**

A single container image that supports RDMA validation across all network fabrics: AWS EFA (SRD), InfiniBand, and RoCEv2. Used as the tools image for `rdma-ping` and `rdma-bw` jobs.

Image: `quay.io/rajjoshi/odh-rhaii-validator-tools`
Tags: `dev` (development), `efa-v1` (validated milestone)

---

## Why a Composite Image

The validator's tools image previously carried only `perftest` (for IB/RoCE). To support AWS EFA clusters, we need `libfabric` + `fabtests`. Rather than maintaining separate images per fabric type, we build a single image containing both toolsets — they share the same `rdma-core` base with no ABI conflicts:

```
fabtests (fi_rdm_pingpong, fi_rma_bw)          perftest (ib_write_bw, ibv_rc_pingpong)
    │                                               │
    ▼                                               ▼
libfabric.so.1 (EFA provider)              libibverbs.so.1 (verbs API)
    │                                               │
    └────────────────┬──────────────────────────────┘
                     ▼
              rdma-core v60.0
         ┌───────────┼───────────┐
         ▼           ▼           ▼
    libefa.so    libmlx5.so   librdmacm.so
    (EFA HW)    (IB/RoCE HW)  (connection mgmt)
         │           │
         ▼           ▼
      efa.ko      mlx5_core.ko  (kernel drivers, on host)
```

---

## Source Components

All versions pinned to **AWS EFA installer 1.46.0** (matches [`ghcr.io/llm-d/llm-d-cuda:v0.8.1`](https://github.com/llm-d/llm-d/pull/607)):

| Component | Version | Source | What it provides |
|-----------|---------|--------|------------------|
| **rdma-core** | v60.0 | `github.com/linux-rdma/rdma-core` | `libibverbs.so`, `librdmacm.so`, `libefa.so` (EFA), `libmlx5.so` (IB/RoCE), `ibv_devinfo`, `ibv_rc_pingpong` |
| **libfabric** | v2.3.1amzn4.0 | `github.com/aws/libfabric` | `libfabric.so.1` with EFA provider + CUDA HMEM support, `fi_info` |
| **fabtests** | (bundled in libfabric) | `github.com/aws/libfabric` | `fi_rdm_pingpong`, `fi_rma_bw`, and ~60 other test binaries |
| **perftest** | (existing submodule) | `github.com/linux-rdma/perftest` | `ib_write_bw`, `ib_read_bw`, `ib_send_bw`, `ib_write_lat`, etc. |

Sources are managed as git submodules under `tools/`:

```
tools/rdma-core    → github.com/linux-rdma/rdma-core     tag v60.0
tools/libfabric    → github.com/aws/libfabric            tag v2.3.1amzn4.0
tools/perftest     → github.com/linux-rdma/perftest      (existing)
```

---

## Build Process

### Overview

The Dockerfile uses a multi-stage build: a CUDA-devel builder stage compiles all four components from source, then only the required binaries and libraries are copied to a minimal UBI9 runtime image.

```
Builder (CUDA devel UBI9)              Runtime (vanilla UBI9)
┌─────────────────────────┐            ┌─────────────────────────┐
│ 1. rdma-core → /usr     │──libs───►  │ /usr/lib64/libibverbs*  │
│ 2. libfabric → /usr     │──libs───►  │ /usr/lib64/libfabric*   │
│ 3. fabtests  → /opt     │──bins───►  │ /usr/local/bin/fi_*     │
│ 4. perftest  → /opt     │──bins───►  │ /usr/local/bin/ib_*     │
│    CUDA toolkit         │──rt─────►  │ /usr/local/lib/libcudart│
└─────────────────────────┘            └─────────────────────────┘
```

### Step 1: rdma-core v60.0

```bash
cmake -GNinja -B build \
  -DCMAKE_INSTALL_PREFIX=/usr \
  -DCMAKE_INSTALL_LIBDIR=lib64 \
  -DNO_MAN_PAGES=1 \
  -DNO_PYVERBS=1 \
  -DENABLE_RESOLVE_NEIGH=0
ninja -C build -j$(nproc)
ninja -C build install && ldconfig
```

Key flags:
- `-DENABLE_RESOLVE_NEIGH=0`: Removes `libnl3` dependency. Only disables userspace neighbour resolution in librdmacm — unused by perftest which does raw verbs QP setup with kernel ARP.
- `-DNO_PYVERBS=1`: Skip Python bindings (not needed at runtime).

### Step 2: libfabric v2.3.1amzn4.0

```bash
./configure --prefix=/usr --libdir=/usr/lib64 \
  --with-cuda=/usr/local/cuda
make -j$(nproc) && make install && ldconfig
```

Builds with CUDA HMEM support (for GPUDirect RDMA via `fi_rma_bw -D cuda`).

After libfabric installs, CUDA stub symlinks are created so the linker resolves `libcuda.so.1` / `libnvidia-ml.so.1` (these are DT_NEEDED by `libfabric.so`). At runtime, real driver libs are injected by NVIDIA Container Toolkit.

### Step 3: fabtests

```bash
cd libfabric/fabtests/
./configure --prefix=/usr --libdir=/usr/lib64 \
  --with-cuda=/usr/local/cuda \
  --with-libfabric=/usr
make -j$(nproc) && make install DESTDIR=/opt/fabtests
```

### Step 4: perftest

```bash
./configure --prefix=/usr --enable-cudart
make -j$(nproc) && make install DESTDIR=/opt/perftest
```

Links against rdma-core installed in step 1. `--enable-cudart` enables `--use_cuda` flag for GPUDirect RDMA bandwidth tests.

### Runtime Stage

The runtime image installs system utilities via dnf (`ethtool`, `pciutils`, `numactl`, `iperf3`, etc.) and copies from the builder:

| From builder | To runtime | Contents |
|---|---|---|
| `/usr/lib64/libib*`, `librdmacm*`, `libefa*`, `libmlx*`, `libfabric*` | `/usr/lib64/` | All shared libraries (rdma-core + libfabric) |
| `/usr/lib64/libibverbs/` | `/usr/lib64/libibverbs/` | Hardware provider plugins dir |
| `/usr/bin/ibv_*`, `/usr/bin/fi_info`, `/opt/fabtests/usr/bin/*`, `/opt/perftest/usr/bin/*` | `/usr/local/bin/` | All RDMA & libfabric executables |
| `/usr/sbin/ib*`, `perfquery`, `saquery`, `smpquery`, `smpdump`, `vendstat`, `sminfo` | `/usr/sbin/` | infiniband-diags tools |
| `/usr/local/cuda/lib64/libcudart.so.*` | `/usr/local/lib/` | CUDA runtime + unversioned symlink |

**libcudart.so symlink**: Fabtests' CUDA HMEM calls `dlopen("libcudart.so")` (unversioned). The NVIDIA GPU plugin injects `libcudart.so.13` but not the unversioned symlink. The Dockerfile creates `ln -s libcudart.so.13 /usr/local/lib/libcudart.so`.

### Dockerfile.dev vs Dockerfile.konflux

Both Dockerfiles follow the same 4-step build pattern and install rdma-core/libfabric directly to `/usr` (no DESTDIR). They differ only due to base image capabilities:

| | `Dockerfile.dev` | `Dockerfile.konflux` |
|---|---|---|
| Builder base | `nvcr.io/nvidia/cuda:13.0.0-devel-ubi9` | `registry.redhat.io/rhai/base-image-cuda-...@sha256:...` |
| Runtime base | `registry.access.redhat.com/ubi9/ubi:latest` | `registry.redhat.io/ubi9/ubi@sha256:...` |
| rdma-core libnl3 | `-DENABLE_RESOLVE_NEIGH=0` (libnl3-devel unavailable) | libnl3-devel installed from RH repos |
| libfabric CUDA | `--with-cuda` (direct link, stubs available) | `--enable-cuda-dlopen` (no CUDA driver stubs in RH image) |
| CUDA stubs | Symlinks created for fabtests linking | Not needed (dlopen mode) |
| Extra deps | — | `cuda-nvml-devel-13-0` (nvml.h for libfabric) |
| Extra setup | — | `public-repos.sh` for vendor repos |
| Labels | — | RHEL component metadata |

Each divergence is documented with inline comments that cross-reference the other Dockerfile.

---

## Using the Tools

### EFA: Reachability (`fi_rdm_pingpong`)

Tests SRD datapath connectivity between two EFA-capable pods.

```bash
# Server
FI_PROVIDER=efa fi_rdm_pingpong -p efa -E

# Client
FI_PROVIDER=efa fi_rdm_pingpong -p efa -E <server_pod_ip>
```

### EFA: GPUDirect RDMA Bandwidth (`fi_rma_bw`)

Tests 1-sided RMA WRITE throughput to GPU memory over SRD. Emulates NIXL's `fi_read` pattern.

```bash
# Server
FI_PROVIDER=efa FI_EFA_USE_DEVICE_RDMA=1 \
  fi_rma_bw -p efa -o write -E -D cuda -S 4194304

# Client
FI_PROVIDER=efa FI_EFA_USE_DEVICE_RDMA=1 \
  fi_rma_bw -p efa -o write -E -D cuda -S 4194304 <server_pod_ip>
```

Key flags:
- `-E`: OOB (out-of-band) address exchange over TCP (required for EFA, see below)
- `-D cuda`: Use GPU memory buffers (GPUDirect RDMA)
- `-S 4194304`: 4 MiB message size (matches NIXL transfer size)
- `-o write`: 1-sided RDMA WRITE operation
- `FI_EFA_USE_DEVICE_RDMA=1`: Enable GPUDirect path in EFA provider

### IB/RoCE: Reachability (`ibv_rc_pingpong`)

Tests InfiniBand/RoCEv2 datapath connectivity via RC (Reliable Connection) QP.

```bash
# Server
ibv_rc_pingpong -d <device> -g <gid_index>

# Client
ibv_rc_pingpong -d <device> -g <gid_index> <server_pod_ip>
```

### IB/RoCE: GPUDirect RDMA Bandwidth (`ib_write_bw`)

Tests RDMA WRITE throughput with GPU memory buffers.

```bash
# Server
ib_write_bw -d <device> --report_gbits -x <gid_index> --use_cuda=<gpu_id>

# Client
ib_write_bw -d <device> --report_gbits -x <gid_index> --use_cuda=<gpu_id> <server_pod_ip>
```

### OOB Address Exchange (EFA-specific)

EFA endpoints use proprietary raw addresses (GID + QPN + ConnID, 32 bytes) that cannot be resolved from IP alone. The `-E` flag in fabtests tools:

1. Opens a TCP socket between client/server using the pod's primary IP (eth0, from VPC CNI)
2. Each side calls `fi_getname()` to get its local EFA raw address
3. Exchanges raw addresses over TCP
4. Each side calls `fi_av_insert()` to add the peer to its address vector
5. SRD traffic then flows directly over EFA devices

This is transparent to the validator — it just needs to pass `-E` and ensure pod-to-pod TCP connectivity exists (which it always does via the VPC CNI).

### Pod Requirements

**EFA (AWS EKS):**
```yaml
resources:
  limits:
    nvidia.com/gpu: "1"
    vpc.amazonaws.com/efa: "4"    # or DRA: efa.networking.k8s.aws
securityContext:
  capabilities:
    add: ["IPC_LOCK"]
```

**IB (CoreWeave, on-prem):**
```yaml
resources:
  limits:
    nvidia.com/gpu: "1"
    rdma/ib: "1"                  # shared device plugin, boolean
securityContext:
  capabilities:
    add: ["IPC_LOCK"]
```

Privileged mode is NOT required — `IPC_LOCK` suffices for memory registration.

---

## Validation Results

### AWS EKS — EFA (p5.48xlarge, 4x EFA NICs, H100 GPUs)

Tested with Konflux-built image (`odh-pr` tag).

| Test | Command | Result |
|------|---------|--------|
| EFA reachability (all 4 NICs) | `fi_rdm_pingpong -p efa -E` per domain | **PASS** — 16.8 usec avg, all 4 domains reachable |
| EFA GPUDirect BW (4 parallel flows) | `fi_rma_bw -p efa -o write -E -D cuda -S 4194304` × 4 NICs | **PASS** — 337 Gbps aggregate (4× ~85 Gbps/NIC) |
| Backward compat | `ibv_devinfo` | **PASS** — 4 EFA devices, PORT_ACTIVE |

### CoreWeave — InfiniBand (H200 8xGPU, CX7-400G, PCIe-topology-aware NIC selection)

Tested with Konflux-built image (`odh-pr` tag). NIC chosen via PCIe topology (PIX — same PCIe switch as GPU).

| Test | Command | Result |
|------|---------|--------|
| IB reachability | `ibv_rc_pingpong -d ibp0 -g 0` (PIX NIC) | **PASS** — 7.6 usec, 8.6 Gbit/sec |
| IB GPUDirect BW | `ib_write_bw -d ibp0 --use_cuda=0 -q 4` (PIX NIC, 4 QPs) | **PASS** — 394.9 Gb/sec (near line-rate) |

---

## Binaries Included in Image

### From rdma-core (verbs utilities + infiniband-diags)

| Binary | Purpose |
|--------|---------|
| `ibv_devinfo` | Show RDMA device attributes (port state, GID, MTU) |
| `ibv_devices` | List available RDMA devices |
| `ibv_rc_pingpong` | IB/RoCE reachability test (RC transport) |
| `ibstat` | IB port state, rate, link layer (infiniband-diags) |
| `iblinkinfo` | Subnet link status and errors |
| `ibnetdiscover` | IB subnet topology discovery |
| `perfquery` | Query IB port performance counters |
| `saquery` | Query IB Subnet Administrator |

### From perftest (IB/RoCE bandwidth + latency)

| Binary | Purpose |
|--------|---------|
| `ib_write_bw` | RDMA WRITE bandwidth (GPUDirect capable) |
| `ib_read_bw` | RDMA READ bandwidth |
| `ib_send_bw` | SEND bandwidth |
| `ib_write_lat` | RDMA WRITE latency |
| `ib_read_lat` | RDMA READ latency |
| `ib_send_lat` | SEND latency |
| `ib_atomic_bw` | Atomic operations bandwidth |
| `ib_atomic_lat` | Atomic operations latency |
| `raw_ethernet_bw` | Raw Ethernet bandwidth |

### From fabtests (EFA/libfabric tests)

| Binary | Purpose |
|--------|---------|
| `fi_rdm_pingpong` | EFA reachability (RDM endpoint, SRD transport) |
| `fi_rma_bw` | EFA bandwidth (1-sided RMA WRITE/READ, GPUDirect) |
| `fi_rdm_bw` | RDM message bandwidth |
| `fi_rdm_tagged_bw` | Tagged message bandwidth |
| `fi_msg_pingpong` | MSG endpoint pingpong |
| `fi_info` | Query available fabric providers and capabilities |
| ... | ~60 additional test binaries |

### From system packages (dnf)

| Binary | Package | Purpose |
|--------|---------|---------|
| `ethtool` | ethtool | NIC link speed, driver info, ring params |
| `lspci` | pciutils | PCIe device topology, NUMA locality |
| `ip` | iproute | Network interface state, addresses |
| `numactl` | numactl | NUMA topology |
| `iperf3` | iperf3 | TCP bandwidth |
| `tcpdump` | tcpdump | Packet capture |

---

## Tools Gap Analysis

Two images are involved: the **validator image** (runs per-node `rdma-node` checks) and the **tools image** (runs multi-node `rdma-ping` / `rdma-bw` jobs). The tools image is what this document covers.

### Tools image coverage (multi-node jobs)

| Check | IB/RoCE binary | EFA binary | Status |
|-------|---------------|------------|--------|
| `rdma-ping` | `ibv_rc_pingpong` | `fi_rdm_pingpong` | Both in image |
| `rdma-bw` | `ib_write_bw --use_cuda` | `fi_rma_bw -D cuda` | Both in image |
| Loopback BW probe | `ib_write_bw --out_json` | (not yet needed) | In image |
| WEP (whole-endpoint) | `ib_write_bw` (parallel) | (not yet needed) | In image |

Shell glue used by job scripts: `bash`, `timeout`, `grep`, `cat`, `head`, `tr` — all present (coreutils).

### rdma-node checks (validator image, for reference)

These run in the validator image, not the tools image. The validator container is privileged (host sysfs access):

| Check | What it invokes | EFA compatibility |
|-------|----------------|-------------------|
| `rdma_devices_detected` | `ibv_devices` + sysfs `/sys/class/infiniband/` | Works — EFA devices appear as verbs devices |
| `rdma_nic_status` | `ibstat` (parses port state, rate, link layer) | **Won't work** — `ibstat` is IB/Mellanox-specific |
| `gpu_nic_topology` | `nvidia-smi` + sysfs (`numa_node`, `readlink` PCIe path) | Works — EFA devices are under `/sys/class/infiniband/` |

**EFA gap in validator**: `ibstat` cannot work with EFA. The UMAD library's `is_ib_type()` filter only accepts `node_type` 1–3 (CA/Switch/Router), but EFA reports `node_type=4` (UNSPECIFIED). Additionally, EFA has no `pkeys/` directory, so `umad_get_port()` would fail with -EIO even if the filter passed. The fix is pure sysfs reads — `ibstat` itself is just a sysfs reader internally:

```
/sys/class/infiniband/<dev>/ports/<n>/state       → "4: ACTIVE"  → check contains "ACTIVE"
/sys/class/infiniband/<dev>/ports/<n>/rate        → "400 Gb/sec (4X HDR)" → first token = "400"
/sys/class/infiniband/<dev>/ports/<n>/link_layer  → "InfiniBand" or "Ethernet" (already read in devices.go)
```

The existing `hasRDMACapability()` in `devices.go` already reads `link_layer` and GID entries from this same directory tree. Adding `state` and `rate` is the same pattern.

### Debugging tools already in the tools image

| Tool | Package | Useful for |
|------|---------|-----------|
| `ibv_devinfo` | rdma-core | Verbose device attributes (port state, GID table, MTU) |
| `ibv_devices` | rdma-core | Quick device listing |
| `ibstat` | rdma-core (infiniband-diags) | IB port state, rate, link layer |
| `iblinkinfo` | rdma-core (infiniband-diags) | Subnet link status and errors |
| `fi_info` | libfabric | EFA provider query, capabilities, addressing |
| `ethtool` | dnf | Link speed, driver info, NIC stats |
| `lspci` | pciutils | PCIe topology, NUMA, device IDs |
| `numactl` | dnf | NUMA topology |
| `ip` | iproute (base) | Interface state, addresses |
| `tcpdump` | dnf | Packet capture (for OOB TCP debug) |

### Tools NOT in image (not needed currently)

| Tool | Why not needed |
|------|---------------|
| `rdma` (iproute2-rdma) | `rdma link show` is useful but not invoked by any check |
| `mlxlink` / `mst` / `mlnx_tune` | Mellanox firmware tools — not available outside MLNX OFED |
| `nccl-tests` | Would need separate NCCL image; out of scope for now |

---

## TODO

### Validator code (future)
- [ ] Implement output parsers for fabtests (`fi_rma_bw`, `fi_rdm_pingpong`)
- [ ] Add EFA platform config (`pkg/config/platforms/`)
- [ ] Handle EFA DRA (K8s 1.34+) vs device plugin resource naming
- [ ] Add EFA reachability job (`fi_rdm_pingpong` pairwise mesh via jobrunner)
- [ ] Add EFA bandwidth job (`fi_rma_bw` per GPU-NIC pair via jobrunner)
- [ ] Fix `rdma_nic_status` for EFA: replace `ibstat` with direct sysfs reads (state, rate, link_layer) — works for all fabrics including EFA where ibstat cannot enumerate devices (node_type=4 filtered out by UMAD)
- [ ] Consider adding `iproute2-rdma` (`rdma` tool) for richer device introspection

---

## References

- [libfabric EFA provider docs](https://github.com/ofiwg/libfabric/blob/main/prov/efa/docs/overview.md)
- [fi_efa(7) man page](https://ofiwg.github.io/libfabric/main/man/fi_efa.7.html)
- [fabtests README](https://github.com/ofiwg/libfabric/tree/main/fabtests)
- [GPUDirect RDMA bandwidth on EFA (blog)](https://le.qun.ch/en/blog/2024/12/25/libfabric-efa-3-fi_info/)
- [AWS EFA on EKS docs](https://docs.aws.amazon.com/eks/latest/userguide/node-efa.html)
- [EFA changelog](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-changelog.html)
- [aws/libfabric releases](https://github.com/aws/libfabric/releases)
- [llm-d EFA Dockerfile](https://github.com/llm-d/llm-d/blob/main/docker/Dockerfile.cuda)
- [NIXL with EFA announcement](https://aws.amazon.com/about-aws/whats-new/2026/03/aws-support-nixl-with-efa/)
