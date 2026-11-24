//go:build race

package node

// raceDetector scales the differential schedules down under -race, where
// SQLite (transpiled to Go) runs about twenty times slower.
const raceDetector = true
