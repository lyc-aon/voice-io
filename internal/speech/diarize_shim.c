#define _GNU_SOURCE
#include "diarize_shim.h"

#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

typedef int32_t (*progress_fn)(int32_t done, int32_t total, void *arg);
typedef const void *(*process_fn)(const void *sd, const float *samples, int32_t n,
                                  progress_fn callback, void *arg);
typedef int32_t (*num_segments_fn)(const void *result);
typedef const vio_segment *(*sort_fn)(const void *result);
typedef void (*destroy_segment_fn)(const vio_segment *segments);
typedef void (*destroy_result_fn)(const void *result);

// Implemented in Go (diarize.go).
extern int32_t vioDiarizeProgress(int32_t done, int32_t total, void *arg);

static int32_t progress(int32_t done, int32_t total, void *arg) {
  return vioDiarizeProgress(done, total, arg);
}

int vio_diarize(const void *sd, const float *samples, int32_t n, uintptr_t handle,
                vio_segment **out, int32_t *count) {
  *out = NULL;
  *count = 0;
  process_fn process =
      (process_fn)dlsym(RTLD_DEFAULT, "SherpaOnnxOfflineSpeakerDiarizationProcessWithCallback");
  num_segments_fn num_segments =
      (num_segments_fn)dlsym(RTLD_DEFAULT, "SherpaOnnxOfflineSpeakerDiarizationResultGetNumSegments");
  sort_fn sort_by_start =
      (sort_fn)dlsym(RTLD_DEFAULT, "SherpaOnnxOfflineSpeakerDiarizationResultSortByStartTime");
  destroy_segment_fn destroy_segments =
      (destroy_segment_fn)dlsym(RTLD_DEFAULT, "SherpaOnnxOfflineSpeakerDiarizationDestroySegment");
  destroy_result_fn destroy_result =
      (destroy_result_fn)dlsym(RTLD_DEFAULT, "SherpaOnnxOfflineSpeakerDiarizationDestroyResult");
  if (!process || !num_segments || !sort_by_start || !destroy_segments || !destroy_result) {
    return 1;
  }
  const void *result = process(sd, samples, n, progress, (void *)handle);
  if (result == NULL) {
    return 0;  // cancelled or no speech
  }
  int32_t total = num_segments(result);
  if (total > 0) {
    const vio_segment *sorted = sort_by_start(result);
    if (sorted != NULL) {
      vio_segment *copy = (vio_segment *)malloc(sizeof(vio_segment) * (size_t)total);
      if (copy != NULL) {
        memcpy(copy, sorted, sizeof(vio_segment) * (size_t)total);
        *out = copy;
        *count = total;
      }
      destroy_segments(sorted);
    }
  }
  destroy_result(result);
  return 0;
}

void vio_free_segments(vio_segment *segments) { free(segments); }
