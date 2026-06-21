package engine

import (
	"testing"
)

func TestDistLock_NewDistLock(t *testing.T) {
	lock := NewDistLock("test-lock")
	if lock == nil {
		t.Fatal("expected non-nil lock")
	}
	if lock.IsHeld() {
		t.Error("new lock should not be held")
	}
}

func TestDistLock_ReleaseWhenNotHeld(t *testing.T) {
	lock := NewDistLock("test-release")
	err := lock.Release()
	if err != nil {
		t.Errorf("release when not held should not error: %v", err)
	}
}

func TestDistLockConfig_Defaults(t *testing.T) {
	lock := NewDistLock("defaults")
	if lock.name != "lyeve_defaults" {
		t.Errorf("expected lyeve_defaults, got %s", lock.name)
	}
}

func TestDefaultConfigs(t *testing.T) {
	wpCfg := DefaultWorkerPoolConfig()
	if wpCfg.Size != 100 {
		t.Errorf("expected 100 workers, got %d", wpCfg.Size)
	}

	trCfg := DefaultTrackerConfig()
	if trCfg.LeakThreshold == 0 {
		t.Error("expected non-zero leak threshold")
	}

	plCfg := DefaultParallelConfig()
	if plCfg.MaxConcurrent != 8 {
		t.Errorf("expected 8 concurrent, got %d", plCfg.MaxConcurrent)
	}

	ahCfg := DefaultAsyncHookConfig()
	if ahCfg.Timeout == 0 {
		t.Error("expected non-zero timeout")
	}
}
