package main

import (
	"fmt"
	"runtime"

	so "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"gitlab.com/integral-inference/voice-io/internal/version"
)

func cmdVersion([]string) error {
	fmt.Printf("voice-io %s (%s/%s, %s)\n", version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("sherpa-onnx %s, onnxruntime %s\n", so.GetVersion(), so.GetOnnxruntimeVersion())
	return nil
}
