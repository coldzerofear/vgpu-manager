# Probes

Checks too slow or too dependent on the machine for `run_all_tests.sh`, which never
runs them. `make test-probes` builds and runs them; they need a GPU, take several
minutes, and on a bare host need root for the reclaim probe's pid list.

The results these were written for are in `docs/library_graph_memory_accounting.md`
(6.5 to 6.13).

| Probe | Question | Passes when |
|---|---|---|
| `probe_churn` | Does each graph cost, capture and pool table entry get reclaimed, or does a table grow for as long as the process runs? | The `table stats` line the library logs at exit (`LOGGER_LEVEL=4`) shows the expected capacity and count; `run_probes.sh` holds them |
| `probe_fork` | Do the hooks survive a fork while other threads change and grow the tables? | No child hangs or crashes |
| `probe_reclaim` | When the driver gives idle graph-pool pages back under memory pressure from another tenant, is the relaunch still checked? | The relaunch is refused. Skips if the driver did not reclaim |
| `probe_capture_apis` | Which stream queries does a capture survive on this driver? Run it on a new driver: the library's rule of never asking during a capture rests on this. Runs on one device too, with a second context | Prints the table; fails if a query that must be safe breaks a capture, or if the library changes any line of it |
| `probe_graph_throttle` | Is every graph charged to the core limit, however many live, while the table grows under load? | Read the rates: all about equal and far below the rate without the library |
| `probe_launch_device` | Is a graph launch charged to the exec's device with another device current, also after an update there? Do the gap events stay in their context, and does such a launch count as activity on its device? | Checked: no rate above 3x the first, a launch after an idle gap waits out its duty cycle, one right after a launch from the other device does not. Run with both limits at 100 for the rate to compare with |
| `torch_many_graphs.py` | Real `CUDAGraph` objects at the number vLLM's piecewise mode keeps | Completes, and `table stats` shows the cost table empty |

Interpreting `table stats`: capacity never shrinks, so it reads as the high-water mark.
A count above what the application holds is a leak. Two counts that look like leaks
and are not, because the application really does hold them: pools created and not
destroyed, which survive a device reset; and execs instantiated under one context from
a graph made under another, which belong to the first and survive the second.

To run the driver-API GPU tests against an instrumented library, see
`make test-gpu-sanitize`.
