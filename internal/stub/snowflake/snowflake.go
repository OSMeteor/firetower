package snowFlakeByGo

import (
	"math/rand"
	"sync/atomic"
	"time"
)

// Worker is a lightweight ID generator that mimics the API of the original
// snowflake implementation used by the project.
type Worker struct {
	counter uint64
}

// NewWorker initialises a pseudo random counter so that IDs from different
// workers are unlikely to clash during tests.
func NewWorker(id int64) (*Worker, error) {
	rand.Seed(time.Now().UnixNano() + id)
	return &Worker{counter: uint64(rand.Int63())}, nil
}

// GetId returns a monotonically increasing identifier.
func (w *Worker) GetId() int64 {
	return int64(atomic.AddUint64(&w.counter, 1))
}
