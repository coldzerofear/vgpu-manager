/*
 * Shared-ledger reader for the GPU accounting tests (test_alloc_async_accounting.c,
 * test_runtime_capture_accounting.cu). Header-only, one translation unit each.
 */
#ifndef VGPU_TEST_LEDGER_H
#define VGPU_TEST_LEDGER_H

#include <fcntl.h>
#include <stdio.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

/* ------------------------------------------------------------------ ledger --
 *
 * Mirror of the shared virtual-memory ledger (include/hook.h: process_used_t /
 * device_vmem_used_t / device_vmemory_t). Reading it is what makes these cases
 * able to see a charge instead of guessing at one from an allocation result.
 * Keep in step with hook.h; a layout change here shows up as a charge that
 * never appears, and the cases say so rather than passing quietly. */
#define LEDGER_PATH        "/tmp/.vmem_node/vmem_node.config"
#define LEDGER_MAX_PIDS    1024
#define LEDGER_MAX_DEVICES 16
/* Must track hook.h. The region carries a 16-byte frozen header padded to one
 * cache line, which shifts devices[] -- and every field read below -- down by
 * CACHELINE_SIZE. The file size is a separate permanent constant, deliberately
 * decoupled from sizeof(): the library never resizes the file, so a mapping
 * held across a container restart can never extend past EOF. */
#define LEDGER_HEADER_SIZE 128                 /* CACHELINE_SIZE            */
#define LEDGER_FILE_SIZE   (320 * 1024)        /* VMEM_NODE_FILE_SIZE       */
#define LEDGER_MAGIC       0x564D4E44U         /* VMEM_NODE_MAGIC, "VMND"   */
#define LEDGER_VERSION     1U                  /* VMEM_NODE_LAYOUT_VERSION  */

typedef struct { int pid; size_t used; } ledger_proc_t;
typedef struct {
  ledger_proc_t processes[LEDGER_MAX_PIDS];
  unsigned int  processes_size;
  unsigned char lock_byte;
} ledger_dev_t;
typedef struct {
  unsigned int  magic;
  unsigned int  layout_version;
  unsigned int  region_size;
  unsigned int  device_count;
  unsigned char _pad[LEDGER_HEADER_SIZE - 16];
  ledger_dev_t  devices[LEDGER_MAX_DEVICES];
} ledger_t;

static const volatile ledger_t *g_ledger;
/* Set when the ledger EXISTS but this test cannot interpret it. Kept separate
 * from "g_ledger == NULL" on purpose: the two states look identical to a
 * reader of g_ledger but mean opposite things. Absent is a legitimate
 * configuration (VMEMORY_NODE_ENABLED=0) and skipping is right. Present but
 * unreadable means this mirror has drifted from hook.h -- a real breakage that
 * must fail, or an ABI change silently converts every assertion below into a
 * skip and the suite still looks green. That is exactly what happened when the
 * frozen header was added. */
static int g_ledger_broken;

static void ledger_open(void) {
  int fd = open(LEDGER_PATH, O_RDONLY);
  if (fd < 0) return;
  struct stat sb;
  if (fstat(fd, &sb) != 0) { close(fd); return; }
  /* Against the file's own permanent size, not sizeof(ledger_t): the two are
   * decoupled by design (see LEDGER_FILE_SIZE). A mismatch means the mirror
   * above has drifted from hook.h, so every field read here would land at the
   * wrong offset -- report it as broken rather than mapping it and asserting
   * on garbage. */
  if ((size_t)sb.st_size != LEDGER_FILE_SIZE) {
    printf("  [warn] " LEDGER_PATH " is %lld bytes, expected %d --\n"
           "         the ledger mirror in this test no longer matches hook.h\n",
           (long long)sb.st_size, LEDGER_FILE_SIZE);
    g_ledger_broken = 1;
    close(fd);
    return;
  }
  void *p = mmap(NULL, sizeof(ledger_t), PROT_READ, MAP_SHARED, fd, 0);
  close(fd);
  if (p == MAP_FAILED) return;

  /* The header catches what a size check cannot: a layout that changed without
   * changing the file size. It is also how a region caught mid-rebuild is
   * recognised -- the library publishes magic last, so an incomplete rebuild
   * reads as "not this layout" rather than as valid data. */
  const volatile ledger_t *l = (const volatile ledger_t *)p;
  if (l->magic != LEDGER_MAGIC || l->layout_version != LEDGER_VERSION ||
      l->region_size != (unsigned int)sizeof(ledger_t) ||
      l->device_count != (unsigned int)LEDGER_MAX_DEVICES) {
    printf("  [warn] " LEDGER_PATH " header is magic=%#x ver=%u size=%u count=%u,\n"
           "         expected magic=%#x ver=%u size=%zu count=%d --\n"
           "         the ledger mirror in this test no longer matches hook.h\n",
           l->magic, l->layout_version, l->region_size, l->device_count,
           LEDGER_MAGIC, LEDGER_VERSION, sizeof(ledger_t), LEDGER_MAX_DEVICES);
    g_ledger_broken = 1;
    return;
  }
  g_ledger = l;
}

/* Bytes currently charged to this process, summed over devices, or -1 if the
 * ledger is not available. Read without the record lock: every field consulted
 * is a naturally aligned scalar, and a charge that is momentarily stale only
 * makes the assertions below more conservative, never less. */
static long long ledger_used_self(void) {
  if (g_ledger == NULL) return -1;
  int self = getpid();
  long long total = 0;
  for (int d = 0; d < LEDGER_MAX_DEVICES; d++) {
    unsigned int n = g_ledger->devices[d].processes_size;
    if (n > LEDGER_MAX_PIDS) continue;   /* torn/garbage read, skip this device */
    for (unsigned int i = 0; i < n; i++) {
      if (g_ledger->devices[d].processes[i].pid == self) {
        total += (long long)g_ledger->devices[d].processes[i].used;
      }
    }
  }
  return total;
}

#endif /* VGPU_TEST_LEDGER_H */
