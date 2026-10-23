/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

#ifndef VGPU_TEST_KMIX_MODEL_H
#define VGPU_TEST_KMIX_MODEL_H

/* Kernel-mix model: realistic GPUs x typical PyTorch kernels, roofline timing.
 * Parameters are public specs (dense FP16 tensor peak, memory bandwidth) and
 * the launch shapes PyTorch/cuBLAS/FlashAttention typically use. Modelled,
 * not measured. */
#include <math.h>
typedef struct { const char *n; int sm, th, maxb; double tflops, gbps; } gpu_t;
static const gpu_t GPUS[] = {
  {"T4",       40, 1024, 16,   65,  320},
  {"RTX3090",  82, 1536, 16,   71,  936},
  {"A100",    108, 2048, 32,  312, 1555},
  {"L4",       58, 1536, 24,  121,  300},
  {"RTX4090", 128, 1536, 24,  165, 1008},
  {"H100",    132, 2048, 32,  989, 3350},
  {"B200",    148, 2048, 32, 2250, 8000},
  {"FUT",     512, 4096, 64, 8000, 20000},
};
#define NGPU (int)(sizeof(GPUS)/sizeof(GPUS[0]))

/* kind: 0 compute (tensor), 1 memory-bound */
typedef struct { const char *n; int kind; double blocks, tpb, flops, bytes; double eff; } kern_t;
/* Shapes: LLM-ish training/inference at hidden 4096, seq 2048, batch 8 (tokens 16384). */
static const kern_t KERNS[] = {
  /* cuBLAS/cutlass GEMM 16384x4096x4096, 128x256 tiles, 256 threads */
  {"gemm_big",      0, (16384/128.0)*(4096/256.0), 256, 2.0*16384*4096*4096, 0, 0.70},
  /* decode GEMM 8x4096x4096 (inference, memory bound on weights), split-K-ish 128 blocks x 128 thr */
  {"gemm_decode",   1, 128, 128, 2.0*8*4096*4096, 2.0*4096*4096, 0.60},
  /* small GEMM 1024^3, 64 blocks x 128 */
  {"gemm_small",    0, 64, 128, 2.0*1024*1024*1024, 0, 0.35},
  /* FlashAttention fwd: batch 8, heads 32, seq 2048, d 128: blocks b*h*(s/128), 128 thr */
  {"flash_attn",    0, 8*32*(2048/128.0), 128, 4.0*8*32*2048.0*2048*128, 0, 0.50},
  /* elementwise (add/gelu) fp16 on 16384x4096: PyTorch vectorized, 128 thr, 4 elems/thr */
  {"elementwise",   1, 16384.0*4096/(128*4), 128, 0, 3*2.0*16384*4096, 0.80},
  /* layernorm/softmax rows: one block per row, 512 thr */
  {"layernorm",     1, 16384, 512, 0, 2*2.0*16384*4096, 0.75},
  /* reduction to scalar (loss/grad-norm): 1 block 1024 thr over 16M fp32 */
  {"reduce_1blk",   1, 1, 1024, 0, 4.0*16384*4096/64, 0.30},
  /* optimizer step (AdamW multi-tensor apply) over 64M params, 512 thr, chunked */
  {"adam_mta",      1, 64e6/(512*4*16), 512, 0, 16.0*64e6, 0.70},
  /* embedding gather 16384 tokens x 4096: one block per 4 rows, 256 thr */
  {"embedding",     1, 16384/4.0, 256, 0, 2*2.0*16384*4096, 0.60},
  /* tiny launch-bound kernels (scalar ops, fills), 1 block, 128 thr */
  {"tiny",          1, 1, 128, 0, 4096, 0.10},
};
#define NKERN (int)(sizeof(KERNS)/sizeof(KERNS[0]))

/* GPU busy time of one launch, microseconds. */
static inline double kern_us(const gpu_t *g, const kern_t *k) {
  double us;
  if (k->kind == 0) {
    /* tile kernels: partial waves do not use the whole GPU */
    double resident = (double)g->sm;          /* ~1 big tile CTA per SM */
    double fill = k->blocks < resident ? k->blocks / resident : 1.0;
    double waves = ceil(k->blocks / resident);
    double full = k->flops / (g->tflops * 1e12 * k->eff) * 1e6;
    us = fill < 1.0 ? full / fill : full * (waves * resident / k->blocks);
  } else {
    us = k->bytes / (g->gbps * 1e9 * k->eff) * 1e6;
  }
  if (k->flops > 0 && k->kind == 1) {
    double c = k->flops / (g->tflops * 1e12 * 0.3) * 1e6;
    if (c > us) us = c;
  }
  return us < 4.0 ? 4.0 : us;                 /* launch/latency floor */
}
/* tokens per launch: unit 0 = grids (blocks), 1 = grids*blocks (threads) */
static inline double kern_tokens(const kern_t *k, int unit) {
  return unit ? k->blocks * k->tpb : k->blocks;
}
/* tokens per 1% util on one GPU = tokens per launch / (busy us per launch / 1000us)
 * (1% of a 100 ms cycle = 1000 us). */
static inline double kern_k(const gpu_t *g, const kern_t *k, int unit) {
  return kern_tokens(k, unit) / (kern_us(g, k) / 1000.0);
}

/* Workload phases: launches of each kernel per "step" (order of KERNS). */
typedef struct { const char *n; double cnt[16]; } phase_t;
static const phase_t PHASES[] = {
  /*                    gemmB  gemmD gemmS flash  elem  ln   red1  adam  emb  tiny */
  {"train_fwd_bwd",    {12,    0,    0,    3,     20,   6,   0,    0,    0.03, 10}},
  {"train_optimizer",  {0,     0,    0,    0,     0,    0,   2,    1,    0,    20}},
  {"infer_prefill",    {4,     0,    0,    1,     6,    2,   0,    0,    0.03, 4}},
  {"infer_decode",     {0,     4,    1,    0,     0,    0,   0,    0,    0,    14}},
  {"data_aug_elem",    {0,     0,    0,    0,     10,   2,   0,    0,    0,    5}},
};
#define NPHASE (int)(sizeof(PHASES)/sizeof(PHASES[0]))
/* phase aggregate: tokens per 1% util (1000 us busy) */
static inline double phase_k(const gpu_t *g, const phase_t *p, int unit) {
  double tok = 0, us = 0;
  for (int i = 0; i < NKERN; i++) { tok += p->cnt[i] * kern_tokens(&KERNS[i], unit); us += p->cnt[i] * kern_us(g, &KERNS[i]); }
  return tok / (us / 1000.0);
}

#endif /* VGPU_TEST_KMIX_MODEL_H */
