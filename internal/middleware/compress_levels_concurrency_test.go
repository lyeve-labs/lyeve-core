package middleware

import (
	"sync"
	"testing"
)

// The pools are reached from WriteHeader, so every serving goroutine hits them
// at once. A plain map would race here, and a racing map is not a data race
// the process survives: Go reports "concurrent map read and map write" as a
// fatal error. Run under -race, this fails against a plain map.
func TestWriterPools_ConcurrentLevelsShareOnePool(t *testing.T) {
	const goroutines = 64
	levels := []int{1, 4, 6, 9}

	for _, tc := range []struct {
		name string
		get  func(int) *sync.Pool
	}{
		{"gzip", gzipWriterPoolForLevel},
		{"brotli", brotliWriterPoolForLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make([]*sync.Pool, goroutines)
			var start, done sync.WaitGroup
			start.Add(1)
			for i := 0; i < goroutines; i++ {
				done.Add(1)
				go func(i int) {
					defer done.Done()
					start.Wait()
					seen[i] = tc.get(levels[i%len(levels)])
				}(i)
			}
			start.Done()
			done.Wait()

			// A level must resolve to one pool for every caller. Handing out a
			// second pool for a level it already had would silently discard the
			// writers already pooled under it.
			perLevel := map[int]*sync.Pool{}
			for i, got := range seen {
				if got == nil {
					t.Fatalf("goroutine %d got no pool", i)
				}
				level := levels[i%len(levels)]
				if first, ok := perLevel[level]; ok && first != got {
					t.Fatalf("level %d resolved to two different pools", level)
				}
				perLevel[level] = got
			}
		})
	}
}
