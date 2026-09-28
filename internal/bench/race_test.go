//go:build race

package bench

// raceEnabled reports whether the race detector is compiled in. A throughput
// figure measured under it describes the instrumentation, not the product.
const raceEnabled = true
