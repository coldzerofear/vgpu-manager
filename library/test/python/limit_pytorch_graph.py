"""CUDA graphs under the memory limit with PyTorch's cudaMallocAsync backend.

With this backend a tensor allocated during torch.cuda.graph capture becomes a
graph allocation, mapped from the device's graph pool when the graph is
replayed (uploaded / launched). Checks, under LD_PRELOAD=libvgpu-control.so and
a memory limit:

  T1  capture with temporaries + replay works and computes the right result
      (HAMi-core #387: this failed when a pool query broke the capture);
  T2  many graphs captured before any replay, their temporaries summing past
      the limit, all capture and replay (the pool is reused across graphs, so
      nothing may be charged per graph ahead of the replay);
  T4  window B: after a capture, other memory takes the headroom the graph
      needs; the replay is refused instead of mapping past the limit, and goes
      through once that memory is freed.

An allocation inside a capture that exceeds the limit is not exercised here:
PyTorch cannot recover from an OOM during capture -- the capture is
invalidated and a later free of a captured block throws from a destructor and
terminates the process, with or without the library. The C test
test_alloc_async_accounting [D] covers that refusal.

Each check runs in its own process: the cudaMallocAsync allocator defers the
frees of captured blocks, and one check's leftovers landing inside the next
check's capture invalidates it -- PyTorch state, not the code under test.
"""
import argparse
import os
import subprocess
import sys

os.environ["PYTORCH_CUDA_ALLOC_CONF"] = "backend:cudaMallocAsync"
import torch  # noqa: E402

MiB = 1 << 20


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--device", type=int, default=0, help="Test device id")
    parser.add_argument("--tensor_shape", type=str, default=None,
                        help="Accepted for the harness; unused")
    parser.add_argument("--case", type=str, default=None, help="Run one check (internal)")
    return parser.parse_args(sys.argv[1:])


def free_bytes():
    free, _ = torch.cuda.mem_get_info()
    return free


def capture(fn):
    # Warm up outside the capture first, as PyTorch requires: the first run of
    # an op initializes the allocator pool and loads kernels, which a capture
    # cannot do.
    fn()
    torch.cuda.synchronize()
    g = torch.cuda.CUDAGraph()
    with torch.cuda.graph(g):
        out = fn()
    return g, out


def setup(args):
    dev = torch.device("cuda", args.device)
    torch.cuda.set_device(dev)
    backend = torch.cuda.memory.get_allocator_backend()
    if backend != "cudaMallocAsync":
        print("FAIL: cudaMallocAsync backend not active (%s)" % backend)
        return None
    return dev


def t1(dev):
    x = torch.randn(64 * MiB // 4, device=dev)
    s = torch.cuda.Stream()
    s.wait_stream(torch.cuda.current_stream())
    with torch.cuda.stream(s):
        for _ in range(2):
            (x * 2 + 1).sum()
    torch.cuda.current_stream().wait_stream(s)
    g1, out = capture(lambda: (x * 2 + 1).sum())
    for _ in range(5):
        g1.replay()
    torch.cuda.synchronize()
    ok = bool(torch.allclose(out, (x * 2 + 1).sum(), rtol=1e-3))
    print("T1 capture + replay, result matches: %s" % ok)
    return 0 if ok else 1


def t2(dev):
    limit = torch.cuda.mem_get_info()[1]
    per = 256 * MiB
    n = max(4, int(limit * 1.5) // per)
    graphs, outs = [], []
    for i in range(n):
        g, o = capture(lambda i=i: torch.empty(per // 4, device=dev).fill_(float(i)).sum())
        graphs.append(g)
        outs.append(o)
    for g in graphs:
        g.replay()
    torch.cuda.synchronize()
    ok = all(abs(o.item() - i * (per // 4)) <= 1e-3 * max(1, i * (per // 4)) for i, o in enumerate(outs))
    print("T2 %d graphs (%d MiB of temporaries, limit %d MiB) captured and replayed: %s"
          % (n, n * per // MiB, limit // MiB, ok))
    return 0 if ok else 1


def t4(dev):
    need = free_bytes() // 3
    # float32: a uint8 sum is computed in int64, an 8x temporary of its own.
    g4, _ = capture(lambda: torch.empty(need // 4, dtype=torch.float32, device=dev).fill_(1).sum())
    filler = torch.empty(free_bytes() - need // 2, dtype=torch.uint8, device=dev)
    refused = False
    try:
        g4.replay()
        torch.cuda.synchronize()
    except Exception as e:  # noqa: BLE001 - the refusal surfaces as a CUDA error
        refused = True
        print("T4 replay with the headroom taken refused: %s" % type(e).__name__)
    if not refused:
        print("T4 FAIL: replay mapped %d MiB past the limit" % (need // MiB))
    # The freed filler stays in the allocator's own pool, which NVML (rightly)
    # still counts and the graph pool cannot reuse: hand it back first.
    del filler
    torch.cuda.synchronize()
    torch.cuda.empty_cache()
    replayed = True
    try:
        g4.replay()
        torch.cuda.synchronize()
    except Exception as e:  # noqa: BLE001
        replayed = False
        print("T4 FAIL: replay after freeing the headroom: %s" % e)
    print("T4 replay after freeing the headroom: %s" % replayed)
    return (not refused) + (not replayed)


CASES = {"T1": t1, "T2": t2, "T4": t4}


def app(args):
    if args.case is not None:
        dev = setup(args)
        if dev is None:
            return 1
        return 1 if CASES[args.case](dev) else 0
    print("PyTorch %s" % torch.__version__)
    failures = 0
    for name in CASES:
        r = subprocess.run([sys.executable, os.path.abspath(__file__), "--device", str(args.device),
                            "--case", name])
        if r.returncode != 0:
            print("%s FAIL (exit %d)" % (name, r.returncode))
            failures += 1
    print("Result: %s" % ("PASS" if failures == 0 else "FAIL (%d)" % failures))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(app(parse_args()))
