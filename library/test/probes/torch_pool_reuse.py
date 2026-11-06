# Real torch graphs. argv: scenario N MIB. cudaMallocAsync backend, so temporaries are
# stream-ordered and captured as alloc/free nodes.
import sys, time, torch
scen, n, mib = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
elems = (mib << 20) // 4
x = torch.ones(elems, device="cuda", dtype=torch.float32)
s = torch.cuda.Stream()
import ctypes
_cu = ctypes.CDLL("libcuda.so.1")
def pool():
    v = ctypes.c_ulonglong(0)
    _cu.cuDeviceGetGraphMemAttribute(0, 2, ctypes.byref(v))  # RESERVED_MEM_CURRENT
    return v.value >> 20
def body():
    # one temporary at a time, each released before the next
    y = x
    for _ in range(n):
        y = y * 1.0001
    return y.sum()
def temp():
    y = x * 1.0001
    y.sum()
def run(g, tag):
    t = time.time()
    try:
        g.replay(); torch.cuda.synchronize()
        print(f"{tag}: replay ok, graph pool {pool()} MiB", flush=True)
    except Exception as e:
        print(f"{tag}: replay FAILED {str(e)[:110]}", flush=True); return 1
    return 0
rc = 0
if scen == "chain":
    g = torch.cuda.CUDAGraph()
    with torch.cuda.graph(g): body()
    print(f"one graph, {n} temporaries of {mib} MiB captured", flush=True)
    rc = run(g, "graph")
elif scen == "recapture":
    gs = []
    for i in range(n):
        g = torch.cuda.CUDAGraph()
        try:
            with torch.cuda.graph(g): temp()
        except Exception as e:
            print(f"capture {i}: FAILED {str(e)[:110]}", flush=True); sys.exit(1)
        rc |= run(g, f"capture+replay {i}"); gs.append(g)
print("graph pool at the end", pool(), "MiB", flush=True)
sys.exit(rc)
