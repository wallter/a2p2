//go:build bench

package node

import "time"

// BenchOnScan tells f of each look a waiting or plain Inbox of s takes:
// the rooms walked and when the look began. Only the benchmark build
// (internal/bench, tag bench) has it; set it before s serves anything.
func BenchOnScan(s *Surface, f func(rooms int, began time.Time)) { s.scanned = f }
