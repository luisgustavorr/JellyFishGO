package modules

import (
	"fmt"
	"runtime"
)

func LogMemUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("🩺 💾 HeapAlloc: %.2fMB | Sys: %.2fMB | NumGC: %d\n",
		float64(m.HeapAlloc)/1024/1024,
		float64(m.Sys)/1024/1024,
		m.NumGC)
}

func PrintMemStats() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("Alloc = %v MiB", m.Alloc/1024/1024)
	fmt.Printf("\tTotalAlloc = %v MiB", m.TotalAlloc/1024/1024)
	fmt.Printf("\tSys = %v MiB", m.Sys/1024/1024)
	fmt.Printf("\tNumGC = %v\n", m.NumGC)
}
