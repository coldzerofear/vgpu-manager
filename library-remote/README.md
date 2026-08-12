# VGPU-Remote-Adapter

A remote vGPU adapter for implementing network-based remote VGPU hard isolation

## Project objectives:

- [ ] Suitable for use with Lupine Server
- [ ] Remote container GPU device quota configuration discovery
- [ ] Remote container device PID list discovery/maintenance
- [ ] Remote container level memory strict isolation
- [ ] Remote container level SM core strict isolation
- [ ] Container level multi process shared token bucket

> Note: Checking indicates that the function has been completed, while unchecking indicates that the function has not been completed or is planned to be implemented.

## Building a dynamic link library

```
./build.sh
```

## Environment variable


## Log level

Use environment variable `LOGGER_LEVEL` to set the visibility of logs

| LOGGER_LEVEL       | description                                 |
| ------------------ |---------------------------------------------|
| 0                  | fatal                                       |
| 1                  | errors,fatal                                |
| 2 (default)        | warnings,errors,fatal                       |
| 3                  | infos,warnings,errors,fatal                 |
| 4                  | verbose,infos,warnings,errors,fatal         |
| 5                  | details,verbose,infos,warnings,errors,fatal |

## CUDA/GPU support information

CUDA 13.1 and before are supporteds
