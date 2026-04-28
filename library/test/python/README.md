# vgpu-manager Python framework smoke tests

Each script does a single allocation + compute round trip through one AI
framework, so that when run under `LD_PRELOAD=libvgpu-control.so` you get
hook coverage on the framework's CUDA path.

Ported as-is from HAMi-core/test/python. Framework dependencies are NOT
installed by the library build; install whichever framework you need into
the test environment separately.

## Running

```sh
LD_PRELOAD=/path/to/libvgpu-control.so python3 limit_pytorch.py \
    --device 0 --tensor_shape 1024,1024,1024
```

## Scripts

| File                   | Framework                                    | Install                                      |
|------------------------|----------------------------------------------|----------------------------------------------|
| `limit_pytorch.py`     | PyTorch (eager)                              | `pip install torch`                          |
| `limit_tensorflow.py`  | TensorFlow 1.x (Session API, graph mode)     | `pip install 'tensorflow<2'`                 |
| `limit_tensorflow2.py` | TensorFlow 2.x (eager)                       | `pip install tensorflow`                     |
| `limit_mxnet.py`       | Apache MXNet                                 | `pip install mxnet-cu112` (or matching CUDA) |

All four are optional - framework availability is checked by `import` at
script start. If a framework is not installed in the test environment
the corresponding script is skipped by `run_all_tests.sh`.
