# SM throttle ablation pipeline

A small measurement harness for vgpu-manager's algorithmic SM throttle work:
**delta** (stock symmetric proportional) vs **aimd** (additive-increase /
multiplicative-decrease, ported from
[midokura/HAMi-core `ablation/orig-aimd-v5`](https://github.com/midokura/HAMi-core/tree/ablation/orig-aimd-v5)).
See [docs/sm_controller_aimd.md](../../../docs/sm_controller_aimd.md) for the
controller design and [docs/sm_core_limit_gap_throttle_design.md](../../../docs/sm_core_limit_gap_throttle_design.md)
for the orthogonal GAP-path piece.

## What this measures

For each variant: launch a sustained compute workload under
`LD_PRELOAD=libvgpu-control.so`, concurrently sample real SM utilization via
`nvidia-smi`, then compute mean absolute error (MAE) of the observed
utilization against the configured `hard_core` target.

```
data/<datestamp>-<gpu_safe>/
├── delta/
│   ├── samples.csv     elapsed_ms, sm_util_pct
│   ├── workload.log    workload stdout/stderr
│   └── meta.json       variant, target, controller, gpu, …
├── aimd/
│   └── …
└── compare.png         4-panel comparison (time series, histogram, MAE bar, stats)
```

### What we measure, what we don't

The GAP path in `cuda_hook.c` is always-on (gated by per-device `core_limit`,
no env switch). So every variant here measures **"controller + GAP path"** as
shipped -- not the controller in isolation. That matches what users actually
deploy, but it means a delta-vs-aimd comparison here is conservative: the GAP
path already trims the worst spikes both ways. If you need pure controller
A/B you'd have to compile a build with GAP stripped, which is out of scope
for this harness.

## Prerequisites

| Tool | Version | Why |
|---|---|---|
| NVIDIA driver | any recent | nvidia-smi sampling, libcuda |
| CUDA toolkit | matching driver | nvcc to build the workload |
| `libvgpu-control.so` | this repo | built via `cd library && make build` |
| Python 3 + matplotlib + numpy | recent | plotting |

A real GPU is required; nothing here mocks. CI without a GPU should treat
this dir as documentation only.

## Quick start

```bash
# 1. Build the library (one-time)
cd library
make build                                # produces build/libvgpu-control.so

# 2. Run the full ablation (delta + aimd, default 30s each)
export VGPU_SO=$PWD/build/libvgpu-control.so
test/ablation/run_ablation.sh

# 3. Plot the result (path printed by step 2)
python3 test/ablation/plot_compare.py test/ablation/data/<datestamp>-<gpu>/
```

You'll get a `compare.png` and a stdout table:

```
variant           samples     mean      MAE      P50      P95      P99
------------------------------------------------------------------------
delta                 300    47.21%   18.34%   16.00%   42.00%   55.00%
aimd                  300    30.85%    2.91%    2.00%    7.00%   12.00%
```

(Numbers are illustrative; your hardware will differ.)

## Knobs

All passed via env to `run_ablation.sh` (which forwards to `collect.sh`):

| Env | Default | Effect |
|---|---|---|
| `VGPU_SO` | (required) | absolute path to libvgpu-control.so |
| `CUDA_CORE_LIMIT` | 30 | **vgpu-manager's** hard_core target percent (the library reads this name; HAMi-core's `CUDA_DEVICE_SM_LIMIT` is **not** recognized) |
| `ABLATION_DURATION_S` | 30 | wall time per variant |
| `ABLATION_GPU_ID` | 0 | GPU index for sampling + UUID lookup |
| `ABLATION_SAMPLE_MS` | 100 | nvidia-smi sample interval |
| `VARIANTS` | "delta aimd" | space-separated list of variant labels |
| `CUDA_SM_AIMD_MD_DIVISOR` | (library default 3) | only used when controller=aimd |
| `CUDA_SM_AIMD_EFF_RATIO` | (library default 875) | only used when controller=aimd |
| `CUDA_SM_AIMD_AI_BASE_DIV` | (library default 400) | only used when controller=aimd |
| `NVCC_ARCH` | (nvcc default) | e.g. `-arch=sm_80` for A100, `-arch=sm_90` for H100 |
| `OUT_BASE` | `./data` | where the dated output dir is created |

`NVIDIA_VISIBLE_DEVICES` is set automatically by `collect.sh` from the GPU's
UUID (looked up via `nvidia-smi -i $ABLATION_GPU_ID`). Without it the library
activates no device and the controller is never invoked, so every variant
would report identical unthrottled samples -- the script fails fast if the
UUID cannot be read.

### Parameter sweeps

Free-form variant names work; `run_ablation.sh` forwards the parent shell's
env to `collect.sh` unchanged, and the library reads its own `CUDA_SM_AIMD_*`
envs only when `controller=aimd`. Use the library's actual env names:

```bash
# Compare three MD factors on the same hardware
for md in 2 3 4; do
  CUDA_SM_AIMD_MD_DIVISOR=$md CUDA_SM_CONTROLLER=aimd \
    ./collect.sh "aimd_md$md" data/sweep-$(date +%Y%m%d)/aimd_md$md
done
python3 plot_compare.py data/sweep-$(date +%Y%m%d)/
```

(That sweep pattern is informal; for a real CI-driven sweep you'd loop in a
shell over `VARIANTS` and parse `compare.png`s.)

## A100 / H100 parameter starting point

Midokura calibrated AIMD v5 on RTX 4080 (consumer, SM=76, 1536/SM). For
datacenter GPUs the SM count and thread granularity are larger by ~1.4-1.7x,
so the default `÷3` MD can over-cut. Starting points worth measuring:

| GPU | `CUDA_SM_AIMD_MD_DIVISOR` | `CUDA_SM_AIMD_AI_BASE_DIV` | Why |
|---|---|---|---|
| RTX 4080 | 3 | 400 | Midokura defaults (validated) |
| A100 | 2 or 3 | 800 | larger SM grid → smaller AI step |
| H100 | 2 | 800-1600 | as above, plus more aggressive MD softening |

These are starting points, not verified -- the harness exists so you can
verify them.

## How the workload works

[`workload.cu`](workload.cu) launches an fma loop kernel back-to-back with
one `cudaDeviceSynchronize` per launch and **no host-side sleeps**, sized so
one kernel runs ~50-200ms on a typical datacenter GPU. The watcher
(~80ms NVML cycle) thus stays inside its feedback regime the whole time and
the controller has continuous work to clamp utilization at the target. The
deterministic kernel shape (configurable via `ABLATION_GRID`, `ABLATION_BLOCK`,
`ABLATION_FMA_ITERS`) means a comparison run is reproducible at the GPU
level; only the throttle controller varies.

## How the sampler works

[`collect.sh`](collect.sh) uses `nvidia-smi --query-gpu=utilization.gpu -lms
<ms>` which spawns nvidia-smi **once** and lets it self-loop at the
requested cadence (no per-sample fork cost). Elapsed time is synthesized as
`sample_index * sample_interval_ms`, which is exact for `-lms`'s fixed
cadence -- jitter shows up as missing samples, not skewed timestamps.

## How the plot trims data

[`plot_compare.py`](plot_compare.py) drops:

- **warm-up**: samples before the first one above 5% util (gives the GPU a
  moment to ramp after the workload starts);
- **post-workload tail**: samples after `meta.wall_ms_actual` (workload's
  measured wall time), anchored to the workload-start sample so the active
  window is exactly the workload's lifetime.

MAE / P50 / P95 / P99 are computed over the surviving samples.

## Roadmap

- **Multi-pod (k8s)** measurement: scale to N concurrent pods, verify each
  variant's per-pod fairness; mirrors Midokura's `k3s_multi_collect.sh`.
- **Auto-baseline regression**: store one set of numbers under `data/baseline/`,
  fail CI if a PR regresses MAE by more than a configurable threshold.
- **Per-GPU profiles**: pre-set AIMD parameter recommendations under
  `profiles/<gpu_name>.env`, loaded by `run_ablation.sh`.

These are intentionally not in P0 -- this directory should stay small and
focused on "give me one number to compare two builds on one GPU".
