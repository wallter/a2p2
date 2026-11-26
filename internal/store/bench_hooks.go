//go:build bench

package store

import "time"

// BenchOnTx tells f of every transaction of every store once it ends:
// whether it could write and when it began. Only the benchmark build
// (internal/bench, tag bench) has it; set it before any store is used.
func BenchOnTx(f func(write bool, began time.Time)) { txHook = f }
