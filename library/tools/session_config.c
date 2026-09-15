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

/*
 * vgpu-session-config -- write a session quota region by hand.
 *
 * In production the GPU-node agent materializes <base>/<session>/config/
 * vgpu.config before the pod's first CUDA call (design §6.3). This tool does
 * the same thing from a shell so the library can be exercised, and the
 * fail-closed paths tested, without any Kubernetes plumbing.
 *
 * It shares include/hook.h with the library, so the region it writes is the
 * struct the library reads -- a layout change breaks the build here rather
 * than producing a file the library silently rejects.
 *
 *   vgpu-session-config --session s1 \
 *       --device GPU-xxxx,mem=8192,core=50 \
 *       --device GPU-yyyy
 */

#include <getopt.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "include/hook.h"
#include "include/session.h"


static resource_data_t config;

static void usage(const char *argv0) {
  fprintf(stderr,
      "usage: %s --session <id> [options]\n"
      "  --session <id>       session id (the pod's LUPINE_SESSION)\n"
      "  --base <dir>         session base dir (default %s,\n"
      "                       or $%s)\n"
      "  --device <spec>      repeatable, max %d. spec is\n"
      "                       <uuid>[,mem=<MiB>][,core=<pct>][,soft=<pct>]\n"
      "                       mem omitted  -> memory limit off for that device\n"
      "                       core omitted -> core limit off for that device\n"
      "                       soft > core  -> soft limit (burstable) instead of hard\n"
      "  --mode <n>           compatibility_mode (default %d = SESSION)\n"
      "  --pod-uid/--pod-name/--namespace/--container <s>   identity, optional\n"
      "  --print              dump what was written\n",
      argv0, SESSION_BASE_DEFAULT, SESSION_BASE_ENV, MAX_DEVICE_COUNT,
      SESSION_COMPATIBILITY_MODE);
}

/* Parse "<uuid>[,k=v]..." into devices[index]. Mirrors the field semantics of
 * init_g_vgpu_config_by_env() in loader.c: total_memory is the cap the library
 * enforces, real_memory the physical size it reports, and a soft limit above
 * the hard one turns hard_limit off. */
static int parse_device(char *spec, int index) {
  device_t *d = &config.devices[index];
  char *save = NULL;
  char *uuid = strtok_r(spec, ",", &save);
  if (uuid == NULL || *uuid == '\0') {
    fprintf(stderr, "device %d: empty uuid\n", index);
    return -1;
  }
  if (snprintf(d->uuid, UUID_BUFFER_SIZE, "%s", uuid) >= UUID_BUFFER_SIZE) {
    fprintf(stderr, "device %d: uuid too long: %s\n", index, uuid);
    return -1;
  }
  d->activate = 1;

  long mem = 0, core = 0, soft = 0;
  for (char *kv = strtok_r(NULL, ",", &save); kv != NULL; kv = strtok_r(NULL, ",", &save)) {
    char *eq = strchr(kv, '=');
    if (eq == NULL) {
      fprintf(stderr, "device %d: expected key=value, got \"%s\"\n", index, kv);
      return -1;
    }
    *eq = '\0';
    long v = strtol(eq + 1, NULL, 10);
    if (strcmp(kv, "mem") == 0) {
      mem = v;
    } else if (strcmp(kv, "core") == 0) {
      core = v;
    } else if (strcmp(kv, "soft") == 0) {
      soft = v;
    } else {
      fprintf(stderr, "device %d: unknown key \"%s\"\n", index, kv);
      return -1;
    }
  }

  if (mem > 0) {
    d->memory_limit = 1;
    d->total_memory = (uint64_t)mem * 1024 * 1024;
    d->real_memory = d->total_memory; /* phase 1 disables oversold: ratio == 1 */
  }
  if (core > 0) {
    d->core_limit = 1;
    d->hard_limit = 1;
    d->hard_core = (int32_t)core;
    if (soft > core) {
      d->hard_limit = 0;
      d->soft_core = (int32_t)soft;
    }
  }
  return 0;
}

static void print_config(const char *path) {
  printf("wrote %s\n  compatibility_mode=%d\n", path, config.compatibility_mode);
  for (int i = 0; i < MAX_DEVICE_COUNT; i++) {
    const device_t *d = &config.devices[i];
    if (!d->activate) {
      continue;
    }
    printf("  [%d] %s mem=%s", i, d->uuid, d->memory_limit ? "" : "off");
    if (d->memory_limit) {
      printf("%luMiB", (unsigned long)(d->total_memory / (1024 * 1024)));
    }
    printf(" core=%s", d->core_limit ? "" : "off");
    if (d->core_limit) {
      printf("%d%%%s", d->hard_core, d->hard_limit ? " hard" : "");
      if (!d->hard_limit) {
        printf(" soft=%d%%", d->soft_core);
      }
    }
    printf("\n");
  }
}

int main(int argc, char **argv) {
  static const struct option opts[] = {
      {"session",   required_argument, NULL, 's'},
      {"base",      required_argument, NULL, 'b'},
      {"device",    required_argument, NULL, 'd'},
      {"mode",      required_argument, NULL, 'm'},
      {"pod-uid",   required_argument, NULL, 'u'},
      {"pod-name",  required_argument, NULL, 'n'},
      {"namespace", required_argument, NULL, 'N'},
      {"container", required_argument, NULL, 'c'},
      {"print",     no_argument,       NULL, 'p'},
      {"help",      no_argument,       NULL, 'h'},
      {NULL, 0, NULL, 0},
  };

  const char *session = NULL;
  int devices = 0, do_print = 0, opt;

  config.compatibility_mode = SESSION_COMPATIBILITY_MODE;

  while ((opt = getopt_long(argc, argv, "", opts, NULL)) != -1) {
    switch (opt) {
    case 's': session = optarg; break;
    case 'b': setenv(SESSION_BASE_ENV, optarg, 1); break;
    case 'd':
      if (devices >= MAX_DEVICE_COUNT) {
        fprintf(stderr, "at most %d devices\n", MAX_DEVICE_COUNT);
        return 1;
      }
      if (parse_device(optarg, devices++) != 0) {
        return 1;
      }
      break;
    case 'm': config.compatibility_mode = (int32_t)strtol(optarg, NULL, 10); break;
    case 'u': snprintf(config.pod_uid, sizeof(config.pod_uid), "%s", optarg); break;
    case 'n': snprintf(config.pod_name, sizeof(config.pod_name), "%s", optarg); break;
    case 'N': snprintf(config.pod_namespace, sizeof(config.pod_namespace), "%s", optarg); break;
    case 'c': snprintf(config.container_name, sizeof(config.container_name), "%s", optarg); break;
    case 'p': do_print = 1; break;
    case 'h': usage(argv[0]); return 0;
    default: usage(argv[0]); return 1;
    }
  }

  if (session == NULL || *session == '\0' || devices == 0) {
    usage(argv[0]);
    return 1;
  }

  char root[PATH_MAX];
  if (snprintf(root, sizeof(root), "%s/%s", session_base(), session) >= (int)sizeof(root)) {
    fprintf(stderr, "session path too long\n");
    return 1;
  }
  if (session_make_dirs(root) != 0) {
    perror("failed to create session directories");
    return 1;
  }

  /* Publish the root so write_file_to_config_path() -- the same writer the
   * library uses, header stamping and locking included -- targets this
   * session instead of the node-global path. */
  if (setenv(SESSION_PATH_ENV, root, 1) != 0) {
    perror("setenv");
    return 1;
  }
  session_paths_reset();

  if (write_file_to_config_path(&config) != 0) {
    fprintf(stderr, "failed to write %s\n", session_path(SESSION_CONFIG));
    return 1;
  }
  if (do_print) {
    print_config(session_path(SESSION_CONFIG));
  }
  return 0;
}
