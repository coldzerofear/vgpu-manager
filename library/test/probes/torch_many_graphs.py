"""Many live torch.cuda.CUDAGraph objects, as vLLM's piecewise mode keeps: one graph
per submodule per batch size, by its source about 33 x 51 for a 32-layer model.

Captured, replayed, dropped, captured again, dropped. With LOGGER_LEVEL=4 the library's
"table stats" line at exit shows the graph cost table empty again.

Each graph holds about 2.2 MiB of device memory, so 5000 of them need a limit above
11 GiB. With backend "async" every graph carries an alloc node and the driver itself
stops at about 1010 of them, with or without the library.

Usage: torch_many_graphs.py {default|async} N
"""
import gc
import os
import sys

if sys.argv[1] == "async":
    os.environ["PYTORCH_CUDA_ALLOC_CONF"] = "backend:cudaMallocAsync"  # before importing torch

import torch

count = int(sys.argv[2])
device = torch.device("cuda:0")
x = torch.ones(1024, device=device)
y = torch.ones(1024, device=device)
out = torch.zeros(1024, device=device)
(x * y + x).sum()  # the warm-up a capture needs


def capture():
    graph = torch.cuda.CUDAGraph()
    with torch.cuda.graph(graph):
        out.copy_(x * y + x)
    return graph


graphs = []
try:
    for _ in range(count):
        graphs.append(capture())
except RuntimeError as e:
    print(f"failed at capture {len(graphs)}: {type(e).__name__}", flush=True)
    os._exit(3)
print("live graphs", len(graphs), flush=True)
for graph in graphs[::50] + graphs[-5:]:
    graph.replay()
torch.cuda.synchronize()
graphs.clear()
gc.collect()

graphs = [capture() for _ in range(count)]
print("captured again", len(graphs), flush=True)
for graph in graphs[::50]:
    graph.replay()
torch.cuda.synchronize()
graphs.clear()
gc.collect()
print("dropped", flush=True)
