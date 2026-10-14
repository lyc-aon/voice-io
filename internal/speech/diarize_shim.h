// Cancellable speaker diarization through sherpa-onnx's C API.
//
// The Go binding exposes only the blocking Process(); this shim resolves the
// callback variant from the already-loaded libsherpa-onnx-c-api at runtime.
#ifndef VOICE_IO_DIARIZE_SHIM_H
#define VOICE_IO_DIARIZE_SHIM_H

#include <stdint.h>

typedef struct vio_segment {
  float start;
  float end;
  int32_t speaker;
  float confidence;
} vio_segment;

// Returns 0 on success; *out receives a copy owned by the caller (free with
// vio_free_segments). Non-zero means the C API could not be resolved/run.
int vio_diarize(const void *sd, const float *samples, int32_t n, uintptr_t handle,
                vio_segment **out, int32_t *count);

void vio_free_segments(vio_segment *segments);

#endif
