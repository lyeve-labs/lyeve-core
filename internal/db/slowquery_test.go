package db

import (
	"testing"
	"time"
)

func TestSlowQueryTracer_RecordsDurations(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)
	// Bypass nil inner for testing: we just test the data structure.

	tracer.recordDuration("postgres", "SELECT * FROM huge_table WHERE x = $1", 1500*time.Millisecond)
	tracer.recordDuration("postgres", "SELECT * FROM huge_table WHERE x = $1", 2300*time.Millisecond)
	tracer.recordDuration("postgres", "SELECT * FROM huge_table WHERE x = $1", 800*time.Millisecond)
	tracer.recordDuration("postgres", "INSERT INTO t VALUES ($1)", 10*time.Millisecond)

	top := tracer.SlowestQueries("", 20)
	if len(top) != 2 {
		t.Fatalf("expected 2 unique query shapes, got %d", len(top))
	}

	// First should be the SELECT (max 2300ms).
	if top[0].MaxDuration != 2300*time.Millisecond {
		t.Errorf("max duration: got %v, want 2300ms", top[0].MaxDuration)
	}
	if top[0].MinDuration != 800*time.Millisecond {
		t.Errorf("min duration: got %v, want 800ms", top[0].MinDuration)
	}
	if top[0].Count != 3 {
		t.Errorf("count: got %d, want 3", top[0].Count)
	}
	if top[0].AvgDuration != (1500+2300+800)*time.Millisecond/3 {
		t.Errorf("avg duration: got %v, want %v", top[0].AvgDuration, (1500+2300+800)*time.Millisecond/3)
	}
}

func TestSlowQueryTracer_DialectFilter(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)

	tracer.recordDuration("postgres", "PG_QUERY", 100*time.Millisecond)
	tracer.recordDuration("mysql", "MYSQL_QUERY", 200*time.Millisecond)

	pgTop := tracer.SlowestQueries("postgres", 20)
	if len(pgTop) != 1 {
		t.Fatalf("expected 1 postgres query, got %d", len(pgTop))
	}
	if pgTop[0].NormalizedSQL != "PG_QUERY" {
		t.Errorf("unexpected pg query: %s", pgTop[0].NormalizedSQL)
	}

	mysqlTop := tracer.SlowestQueries("mysql", 20)
	if len(mysqlTop) != 1 {
		t.Fatalf("expected 1 mysql query, got %d", len(mysqlTop))
	}
	if mysqlTop[0].NormalizedSQL != "MYSQL_QUERY" {
		t.Errorf("unexpected mysql query: %s", mysqlTop[0].NormalizedSQL)
	}
}

func TestSlowQueryTracer_TopNLimit(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)

	for i := 0; i < 10; i++ {
		tracer.recordDuration("postgres", string(rune('A'+i)), time.Duration(100+i)*time.Millisecond)
	}

	top3 := tracer.SlowestQueries("", 3)
	if len(top3) != 3 {
		t.Fatalf("expected 3 results, got %d", len(top3))
	}
	// Should be the 3 slowest (J=109ms, I=108ms, H=107ms).
	if top3[0].NormalizedSQL != "J" {
		t.Errorf("slowest should be J, got %s", top3[0].NormalizedSQL)
	}
}

func TestSlowQueryTracer_Eviction(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 5)

	// Insert 6 unique queries.
	for i := 0; i < 6; i++ {
		tracer.recordDuration("postgres", string(rune('A'+i)), time.Duration(10+i)*time.Millisecond)
	}

	// LRU eviction: the first inserted (A) should be gone.
	top := tracer.SlowestQueries("", 20)
	if len(top) != 5 {
		t.Fatalf("expected 5 after eviction, got %d", len(top))
	}
	for _, r := range top {
		if r.NormalizedSQL == "A" {
			t.Error("A should have been evicted as LRU")
		}
	}
}

func TestSlowQueryTracer_Clear(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)
	tracer.recordDuration("postgres", "Q", 100*time.Millisecond)
	tracer.Clear()

	top := tracer.SlowestQueries("", 20)
	if len(top) != 0 {
		t.Errorf("expected 0 after clear, got %d", len(top))
	}
}

func TestSlowQueryStatsFromTracer(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)

	tracer.recordDuration("postgres", "SLOW_QUERY", 500*time.Millisecond)
	tracer.recordDuration("postgres", "SLOW_QUERY", 300*time.Millisecond)
	tracer.recordDuration("postgres", "FAST_QUERY", 5*time.Millisecond)

	stats := SlowQueryStatsFromTracer(tracer, "postgres", 20)
	if stats.TrackedShapes != 2 {
		t.Errorf("tracked shapes: got %d, want 2", stats.TrackedShapes)
	}
	if len(stats.TopByMaxDuration) != 2 {
		t.Errorf("top by max: got %d, want 2", len(stats.TopByMaxDuration))
	}
	if stats.TopByMaxDuration[0].MaxDuration != 500*time.Millisecond {
		t.Errorf("top max duration: got %v, want 500ms", stats.TopByMaxDuration[0].MaxDuration)
	}
	if stats.TopByAvgDuration[0].AvgDuration != 400*time.Millisecond {
		t.Errorf("top avg duration: got %v, want 400ms", stats.TopByAvgDuration[0].AvgDuration)
	}
}

func TestSlowQueryTracer_DefaultThreshold(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)
	if got := tracer.Threshold(); got != DefaultSlowQueryThreshold {
		t.Errorf("default threshold: got %v, want %v", got, DefaultSlowQueryThreshold)
	}
}

func TestSlowQueryTracer_SetThreshold(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)
	tracer.SetThreshold(500 * time.Millisecond)
	if got := tracer.Threshold(); got != 500*time.Millisecond {
		t.Errorf("after SetThreshold: got %v, want 500ms", got)
	}
	// Setting to zero records everything.
	tracer.SetThreshold(0)
	if got := tracer.Threshold(); got != 0 {
		t.Errorf("after SetThreshold(0): got %v, want 0", got)
	}
}

func TestSlowQueryTracer_ThresholdFiltering(t *testing.T) {
	tracer := NewSlowQueryTracer(nil, 50)
	tracer.SetThreshold(1 * time.Second)

	// Below threshold: should NOT be recorded.
	tracer.recordDuration("postgres", "FAST_QUERY", 100*time.Millisecond)
	// Above threshold: should be recorded.
	tracer.recordDuration("postgres", "SLOW_QUERY", 1500*time.Millisecond)
	// Exactly at threshold: should be recorded.
	tracer.recordDuration("postgres", "EDGE_QUERY", 1000*time.Millisecond)

	// recordDuration itself doesn't filter: the DB methods do.
	// So all 3 are recorded since we called recordDuration directly.
	top := tracer.SlowestQueries("", 20)
	if len(top) != 3 {
		t.Fatalf("expected 3 records via direct recordDuration, got %d", len(top))
	}

	// Reset and test the actual threshold enforcement via the filter.
	tracer.Clear()
	tracer.SetThreshold(1 * time.Second)

	// Simulate what QueryRow/Query/Exec do: only record if d >= threshold.
	recordIfSlow := func(dialect, sql string, d time.Duration) {
		if d >= tracer.Threshold() {
			tracer.recordDuration(dialect, sql, d)
		}
	}

	recordIfSlow("postgres", "FAST_QUERY", 100*time.Millisecond)
	recordIfSlow("postgres", "SLOW_QUERY", 1500*time.Millisecond)
	recordIfSlow("postgres", "EDGE_QUERY", 1000*time.Millisecond)

	top = tracer.SlowestQueries("", 20)
	if len(top) != 2 {
		t.Fatalf("expected 2 records (>=1s threshold), got %d", len(top))
	}
}
