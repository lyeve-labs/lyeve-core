package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestEnvSource(t *testing.T) {
	ctx := context.Background()

	t.Run("resolves a set variable", func(t *testing.T) {
		t.Setenv("API_KEY_PEPPER", "s3cr3t-pepper")
		got, err := EnvSource{}.Secret(ctx, APIKeyPepper)
		if err != nil {
			t.Fatalf("Secret() error = %v, want nil", err)
		}
		if string(got) != "s3cr3t-pepper" {
			t.Fatalf("Secret() = %q, want %q", got, "s3cr3t-pepper")
		}
	})

	t.Run("unset variable is ErrNotFound", func(t *testing.T) {
		t.Setenv("API_KEY_PEPPER", "")
		_, err := EnvSource{}.Secret(ctx, APIKeyPepper)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Secret() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("unmapped name is ErrNotFound", func(t *testing.T) {
		_, err := EnvSource{}.Secret(ctx, "no_such_secret")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Secret() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("custom mapping overrides default", func(t *testing.T) {
		t.Setenv("MY_PEPPER", "custom")
		src := EnvSource{Mapping: map[string]string{APIKeyPepper: "MY_PEPPER"}}
		got, err := src.Secret(ctx, APIKeyPepper)
		if err != nil || string(got) != "custom" {
			t.Fatalf("Secret() = %q, %v; want %q, nil", got, err, "custom")
		}
	})
}

func TestStaticSource(t *testing.T) {
	ctx := context.Background()
	src := StaticSource{APIKeyPepper: []byte("pep")}

	got, err := src.Secret(ctx, APIKeyPepper)
	if err != nil || string(got) != "pep" {
		t.Fatalf("Secret() = %q, %v; want %q, nil", got, err, "pep")
	}

	// Returned bytes must be a copy: mutating them must not corrupt the source.
	got[0] = 'X'
	again, _ := src.Secret(ctx, APIKeyPepper)
	if string(again) != "pep" {
		t.Fatalf("source mutated through returned slice: got %q", again)
	}

	if _, err := src.Secret(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key error = %v, want ErrNotFound", err)
	}
}

// errSource is a Source that always fails with a non-ErrNotFound error,
// modeling a real backend (KMS/Vault) outage.
type errSource struct{ err error }

func (e errSource) Secret(context.Context, string) ([]byte, error) { return nil, e.err }

func TestChain(t *testing.T) {
	ctx := context.Background()

	t.Run("returns first hit and skips nil members", func(t *testing.T) {
		chain := Chain{nil, StaticSource{}, StaticSource{APIKeyPepper: []byte("from-static")}}
		got, err := chain.Secret(ctx, APIKeyPepper)
		if err != nil || string(got) != "from-static" {
			t.Fatalf("Secret() = %q, %v; want %q, nil", got, err, "from-static")
		}
	})

	t.Run("ErrNotFound falls through to next source", func(t *testing.T) {
		t.Setenv("API_KEY_PEPPER", "from-env")
		chain := Chain{StaticSource{}, EnvSource{}}
		got, err := chain.Secret(ctx, APIKeyPepper)
		if err != nil || string(got) != "from-env" {
			t.Fatalf("Secret() = %q, %v; want %q, nil", got, err, "from-env")
		}
	})

	t.Run("missing everywhere is ErrNotFound", func(t *testing.T) {
		_, err := Chain{StaticSource{}, StaticSource{}}.Secret(ctx, APIKeyPepper)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Secret() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("real backend error is not masked by a later source", func(t *testing.T) {
		boom := errors.New("vault unreachable")
		// A working static source sits AFTER the failing one. The chain must
		// still surface the real error rather than silently using the weaker
		// source: failing closed on custody errors is the safe behavior.
		chain := Chain{errSource{boom}, StaticSource{APIKeyPepper: []byte("ignored")}}
		_, err := chain.Secret(ctx, APIKeyPepper)
		if !errors.Is(err, boom) {
			t.Fatalf("Secret() error = %v, want %v", err, boom)
		}
	})
}
