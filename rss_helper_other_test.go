//go:build !unix

package rowpack

// peakRSSBytes returns the process peak resident set size; unsupported on
// this platform, so benchmarks report 0.
func peakRSSBytes() int64 { return 0 }
