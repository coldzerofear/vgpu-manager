/*
Copyright 2026 coldzerofear

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

/*
 * Asks the driver how many bytes one 4x4 block of each BC format costs.
 *
 * WHY THIS IS NOT PART OF array_size_probe.c
 *
 * cuArray3DCreate refuses BC6H_UF16 / BC6H_SF16 on every shape and every
 * NumChannels, so the CUDA probe cannot measure them -- yet
 * include/array_size.h has to size them anyway, because falling through to
 * VGPU_ARRAY_UNKNOWN_FORMAT_BYTES charges 8 bytes per channel for a format
 * that really costs 1 byte per texel, i.e. 8x to 32x too much, which is the
 * false-OOM direction.
 *
 * OpenGL exposes the same formats through ARB_texture_compression_bptc and
 * reports the exact compressed footprint via GL_TEXTURE_COMPRESSED_IMAGE_SIZE:
 * blocks x bytes-per-block, with none of the allocator granularity that makes
 * the CUDA figure a rounded-up number. Same GPU, same driver, so it is a
 * measurement of the thing we need and not a guess from the format name.
 *
 * SELF-CALIBRATING
 *
 * The six BC formats CUDA *does* accept are measured here too, and the tool
 * exits non-zero unless every one of them comes back at the bytes-per-block
 * the CUDA sweep recorded. That is the whole argument for believing the BC6H
 * row: if the method reproduces six known answers on this driver, the seventh
 * is a reading rather than an inference. A disagreement means the method (or
 * the driver) changed and the BC6H entry in array_size.h needs re-deriving.
 *
 * HOW TO BUILD AND RUN
 *
 *   gcc -O2 -o bc_block_probe library/tools/bc_block_probe.c -ldl
 *   ./bc_block_probe
 *
 * Needs no headers and no link-time GL: libEGL.so.1 and libGL.so.1 are opened
 * at runtime and the handful of enumerators used are declared below, with
 * their values from the GL registry. Needs a working EGL implementation but no
 * X server -- the context comes from EGL_EXT_platform_device. A software
 * rasteriser will do, and running it against one is a useful cross-check:
 * Mesa's llvmpipe reports the same table as the NVIDIA driver, which is what
 * "bytes per block is a property of the format" is supposed to mean.
 */

#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>

typedef void *EGLDisplay, *EGLConfig, *EGLContext, *EGLDeviceEXT, *EGLSurface;
typedef unsigned int EGLBoolean, EGLenum;
typedef int32_t EGLint;
typedef unsigned int GLenum, GLuint;
typedef int GLint, GLsizei;

/* EGL 1.5 / EGL_EXT_platform_device. */
#define EGL_PLATFORM_DEVICE_EXT   0x313F
#define EGL_RENDERABLE_TYPE       0x3040
#define EGL_OPENGL_BIT            0x0008
#define EGL_SURFACE_TYPE          0x3033
#define EGL_PBUFFER_BIT           0x0001
#define EGL_NONE                  0x3038
#define EGL_OPENGL_API            0x30A2
#define EGL_CONTEXT_MAJOR_VERSION 0x3098

/* GL 1.1 / 3.0 / EXT_texture_compression_s3tc / ARB_texture_compression_bptc. */
#define GL_NO_ERROR                      0
#define GL_TEXTURE_2D                    0x0DE1
#define GL_RENDERER                      0x1F01
#define GL_VERSION                       0x1F02
#define GL_TEXTURE_COMPRESSED_IMAGE_SIZE 0x86A0
#define GL_TEXTURE_COMPRESSED            0x86A1

/* 256x256 texels is 64x64 = 4096 blocks of 4x4, so the per-block figure comes
 * out of an exact division and a wrong block geometry would not divide. */
#define PROBE_EXTENT 256
#define PROBE_BLOCKS ((PROBE_EXTENT / 4) * (PROBE_EXTENT / 4))

struct bc_format {
  const char *name;
  GLenum gl_format;
  /* Bytes per block the CUDA sweep measured, or 0 for the formats CUDA
   * refuses. A non-zero value here is a calibration the run must reproduce. */
  int cuda_bytes_per_block;
};

static const struct bc_format BC_FORMATS[] = {
    {"BC1 (S3TC DXT1)", 0x83F0, 8},
    {"BC2 (S3TC DXT3)", 0x83F2, 16},
    {"BC3 (S3TC DXT5)", 0x83F3, 16},
    {"BC4 (RGTC1 RED)", 0x8DBB, 8},
    {"BC5 (RGTC2 RG)", 0x8DBD, 16},
    {"BC6H_UF16 (BPTC unsigned float)", 0x8E8F, 0},
    {"BC6H_SF16 (BPTC signed float)", 0x8E8E, 0},
    {"BC7 (BPTC UNORM)", 0x8E8C, 16},
};

/* Everything the tool calls, resolved at runtime. */
static struct {
  EGLBoolean (*QueryDevicesEXT)(EGLint, EGLDeviceEXT *, EGLint *);
  EGLDisplay (*GetPlatformDisplayEXT)(EGLenum, void *, const EGLint *);
  EGLBoolean (*Initialize)(EGLDisplay, EGLint *, EGLint *);
  EGLBoolean (*ChooseConfig)(EGLDisplay, const EGLint *, EGLConfig *, EGLint, EGLint *);
  EGLBoolean (*BindAPI)(EGLenum);
  EGLContext (*CreateContext)(EGLDisplay, EGLConfig, EGLContext, const EGLint *);
  EGLBoolean (*MakeCurrent)(EGLDisplay, EGLSurface, EGLSurface, EGLContext);
  void *(*GetProcAddress)(const char *);
} egl;

static struct {
  void (*GenTextures)(GLsizei, GLuint *);
  void (*BindTexture)(GLenum, GLuint);
  void (*DeleteTextures)(GLsizei, const GLuint *);
  void (*GetTexLevelParameteriv)(GLenum, GLint, GLenum, GLint *);
  GLenum (*GetError)(void);
  const unsigned char *(*GetString)(GLenum);
  void (*TexStorage2D)(GLenum, GLsizei, GLenum, GLsizei, GLsizei);
} gl;

/* A headless OpenGL context on the first EGL device that accepts one. */
static int make_context(void) {
  void *libegl = dlopen("libEGL.so.1", RTLD_NOW);
  void *libgl = dlopen("libGL.so.1", RTLD_NOW);
  EGLDeviceEXT devices[8];
  EGLint ndevices = 0, major = 0, minor = 0, nconfigs = 0;
  EGLDisplay display = NULL;
  EGLConfig config;
  EGLContext context;
  const EGLint config_attrs[] = {EGL_SURFACE_TYPE,     EGL_PBUFFER_BIT,
                                 EGL_RENDERABLE_TYPE,  EGL_OPENGL_BIT,
                                 EGL_NONE};
  const EGLint context_attrs[] = {EGL_CONTEXT_MAJOR_VERSION, 4, EGL_NONE};

  if (libegl == NULL || libgl == NULL) {
    fprintf(stderr, "dlopen libEGL/libGL: %s\n", dlerror());
    return 0;
  }
  *(void **)&egl.GetProcAddress = dlsym(libegl, "eglGetProcAddress");
  *(void **)&egl.Initialize = dlsym(libegl, "eglInitialize");
  *(void **)&egl.ChooseConfig = dlsym(libegl, "eglChooseConfig");
  *(void **)&egl.BindAPI = dlsym(libegl, "eglBindAPI");
  *(void **)&egl.CreateContext = dlsym(libegl, "eglCreateContext");
  *(void **)&egl.MakeCurrent = dlsym(libegl, "eglMakeCurrent");
  if (egl.GetProcAddress == NULL) {
    fprintf(stderr, "no eglGetProcAddress\n");
    return 0;
  }
  /* Extension entry points come from eglGetProcAddress, not dlsym: the GLVND
   * dispatch library does not export them. */
  *(void **)&egl.QueryDevicesEXT = egl.GetProcAddress("eglQueryDevicesEXT");
  *(void **)&egl.GetPlatformDisplayEXT = egl.GetProcAddress("eglGetPlatformDisplayEXT");
  if (egl.QueryDevicesEXT == NULL || egl.GetPlatformDisplayEXT == NULL) {
    fprintf(stderr, "no EGL_EXT_platform_device; this tool needs a headless"
                    " EGL device\n");
    return 0;
  }
  if (!egl.QueryDevicesEXT(8, devices, &ndevices) || ndevices == 0) {
    fprintf(stderr, "eglQueryDevicesEXT found no device\n");
    return 0;
  }
  for (EGLint i = 0; i < ndevices; i++) {
    display = egl.GetPlatformDisplayEXT(EGL_PLATFORM_DEVICE_EXT, devices[i], NULL);
    if (display != NULL && egl.Initialize(display, &major, &minor)) {
      break;
    }
    display = NULL;
  }
  if (display == NULL) {
    fprintf(stderr, "eglInitialize failed on all %d devices\n", ndevices);
    return 0;
  }
  if (!egl.ChooseConfig(display, config_attrs, &config, 1, &nconfigs) || nconfigs == 0) {
    fprintf(stderr, "eglChooseConfig found no OpenGL config\n");
    return 0;
  }
  egl.BindAPI(EGL_OPENGL_API);
  context = egl.CreateContext(display, config, NULL, context_attrs);
  if (context == NULL) {
    fprintf(stderr, "eglCreateContext failed\n");
    return 0;
  }
  /* Surfaceless: nothing is rendered, the textures are only ever asked their
   * size. */
  if (!egl.MakeCurrent(display, NULL, NULL, context)) {
    fprintf(stderr, "eglMakeCurrent failed\n");
    return 0;
  }

  *(void **)&gl.GenTextures = dlsym(libgl, "glGenTextures");
  *(void **)&gl.BindTexture = dlsym(libgl, "glBindTexture");
  *(void **)&gl.DeleteTextures = dlsym(libgl, "glDeleteTextures");
  *(void **)&gl.GetTexLevelParameteriv = dlsym(libgl, "glGetTexLevelParameteriv");
  *(void **)&gl.GetError = dlsym(libgl, "glGetError");
  *(void **)&gl.GetString = dlsym(libgl, "glGetString");
  *(void **)&gl.TexStorage2D = egl.GetProcAddress("glTexStorage2D"); /* GL 4.2 */
  if (gl.GenTextures == NULL || gl.GetTexLevelParameteriv == NULL ||
      gl.TexStorage2D == NULL) {
    fprintf(stderr, "missing a required GL entry point\n");
    return 0;
  }
  return 1;
}

/* Compressed image size of one PROBE_EXTENT square level, or 0 if the driver
 * will not allocate the format. */
static GLint measure(GLenum format) {
  GLuint texture = 0;
  GLint compressed = 0, size = 0;

  gl.GenTextures(1, &texture);
  gl.BindTexture(GL_TEXTURE_2D, texture);
  while (gl.GetError() != GL_NO_ERROR) {
    /* Drain errors left by anything before us, so the check below is ours. */
  }
  gl.TexStorage2D(GL_TEXTURE_2D, 1, format, PROBE_EXTENT, PROBE_EXTENT);
  if (gl.GetError() != GL_NO_ERROR) {
    gl.DeleteTextures(1, &texture);
    return 0;
  }
  gl.GetTexLevelParameteriv(GL_TEXTURE_2D, 0, GL_TEXTURE_COMPRESSED, &compressed);
  gl.GetTexLevelParameteriv(GL_TEXTURE_2D, 0, GL_TEXTURE_COMPRESSED_IMAGE_SIZE, &size);
  gl.DeleteTextures(1, &texture);
  /* A format the driver silently stored uncompressed would give a meaningless
   * per-block figure, so treat it as no measurement at all. */
  return compressed ? size : 0;
}

int main(void) {
  int calibration_failures = 0, measured = 0;

  if (!make_context()) {
    return 1;
  }
  printf("GL_RENDERER: %s\n", gl.GetString(GL_RENDERER));
  printf("GL_VERSION : %s\n\n", gl.GetString(GL_VERSION));
  printf("%dx%d texels = %d blocks of 4x4\n\n", PROBE_EXTENT, PROBE_EXTENT,
         PROBE_BLOCKS);
  printf("%-34s %-12s %-12s %s\n", "format", "image_size", "bytes/block",
         "vs CUDA sweep");

  for (size_t i = 0; i < sizeof(BC_FORMATS) / sizeof(BC_FORMATS[0]); i++) {
    const struct bc_format *f = &BC_FORMATS[i];
    GLint size = measure(f->gl_format);
    const char *verdict;

    if (size == 0) {
      printf("%-34s %-12s %-12s %s\n", f->name, "-", "-", "driver refused it");
      if (f->cuda_bytes_per_block != 0) {
        calibration_failures++;
      }
      continue;
    }
    measured++;
    if (f->cuda_bytes_per_block == 0) {
      verdict = "(CUDA refuses this format)";
    } else if (size == f->cuda_bytes_per_block * PROBE_BLOCKS) {
      verdict = "matches";
    } else {
      verdict = "DIFFERS -- re-derive array_size.h";
      calibration_failures++;
    }
    printf("%-34s %-12d %-12d %s\n", f->name, size, size / PROBE_BLOCKS, verdict);
  }

  if (measured == 0) {
    printf("\nnothing was measured\n");
    return 1;
  }
  if (calibration_failures != 0) {
    printf("\n%d calibration format(s) disagree with the CUDA sweep: the BC6H\n"
           "row in include/array_size.h rests on this method reproducing them,\n"
           "so it has to be re-derived before this run can be used.\n",
           calibration_failures);
    return 1;
  }
  printf("\nevery format CUDA also accepts came back at the bytes-per-block the\n"
         "CUDA sweep recorded, so the BC6H rows above are a measurement.\n");
  return 0;
}
