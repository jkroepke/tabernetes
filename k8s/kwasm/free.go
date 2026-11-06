package main

import (
	"context"
	"fmt"
	"runtime"
	"syscall/js"
	"text/tabwriter"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// wasmLimit is the address space of wasm32. The linear memory only grows, so
// "used" in the wasm row is the high-water mark of the control plane.
const wasmLimit = 4 << 30

// free shows the memory of the control plane. Flags like -h are ignored,
// the sizes are always human readable.
func free(_ context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w := tabwriter.NewWriter(hc.Stdout, 0, 0, 3, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "\ttotal\tused\tfree\t")
	// terminal.js exposes the WebAssembly.Memory of the instance.
	if mem := js.Global().Get("kwasmMemory"); mem.Truthy() {
		used := uint64(mem.Get("buffer").Get("byteLength").Float())
		fmt.Fprintf(w, "wasm:\t%s\t%s\t%s\t\n", size(wasmLimit), size(used), size(wasmLimit-used))
	}
	fmt.Fprintf(w, "heap:\t%s\t%s\t%s\t\n", size(m.HeapSys), size(m.HeapInuse), size(m.HeapSys-m.HeapInuse))
	w.Flush()
	last := "never"
	if m.LastGC > 0 {
		last = time.Since(time.Unix(0, int64(m.LastGC))).Round(time.Second).String() + " ago"
	}
	fmt.Fprintf(hc.Stdout, "goroutines: %d, live objects: %d, gc cycles: %d, last gc: %s\n",
		runtime.NumGoroutine(), m.HeapObjects, m.NumGC, last)
	return nil
}

// size formats bytes like "free -h".
func size(b uint64) string {
	f, units := float64(b), []string{"B", "Ki", "Mi", "Gi"}
	i := 0
	for ; f >= 1024 && i < len(units)-1; i++ {
		f /= 1024
	}
	if i == 0 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}
