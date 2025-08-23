#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <string.h>

extern int extract_container_pids(char *base_path, int *pids, int *pids_size);

//static int int_compare(const void *a, const void *b) {
//  const int *pa = (const int *)a;
//  const int *pb = (const int *)b;
//  return (*pa > *pb) - (*pa < *pb);
//}

int check_container_pid_by_open_kernel(unsigned int pid, int *pids_on_container, int pids_size) {
  int ret = 0;
  if (pid == 0 || !pids_on_container || pids_size <= 0) {
    return ret;
  }
  for (int i = 0; i < pids_size; i++) {
    if (pid == pids_on_container[i]) {
      ret = 1;
      break;
    }
  }
//  if (bsearch(&pid, pids_on_container, (size_t)pids_size, sizeof(int), int_compare)) {
//    ret = 1;
//  }
  return ret;
}

int str2int(char *str) {
    int l = strlen(str);
    int res = 0;
    for (int i = 0; i < l; i++) {
        res *= 10;
        res += str[i] - '0';
    }
    return res;
}

int main(int argc, char **argv) {
    int search_pid = 0;
    switch (argc) {
    case 2:
      break;
    case 3:
      search_pid = str2int(argv[2]);
      break;
    default:
      printf("wrong arguments: %s [base_path] [search_pid]\n", argv[0]);
      return -1;
    }

    char *base_path = argv[1];
    int pids_size = 1024;
    int pids_on_container[1024];
    int ret = extract_container_pids(base_path, pids_on_container, &pids_size);
    if (ret != 0) {
      printf("extract_container_pids failed: %s\n", base_path);
      return -1;
    }
    if (search_pid > 0) {
      ret = check_container_pid_by_open_kernel(search_pid, pids_on_container, pids_size);
      if (ret) {
         printf("matched pid %d\n", search_pid);
      }
    }
    printf("pid size: %d\n", pids_size);
    for (int i = 0; i < pids_size; i++) {
        printf("%d\n", pids_on_container[i]);
    }
    return 0;
}