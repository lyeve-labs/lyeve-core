package config

import "testing"

// LYEVE_OVERRIDABLE gives the environment the same opt-in the file's
// !overridable tag gives, and it is a variable rather than a control on the
// admin page on purpose. What makes an opt-in safe is that the operator
// declares it at deploy time, in the artifact they control.

func openResolver(t *testing.T, list string, file map[string]FileValue) *Resolver {
	t.Helper()
	t.Setenv(OverridableEnvKey, list)
	return NewResolver(file)
}

func TestResolve_EnvNotListedStaysPinned(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "900")
	r := openResolver(t, "", nil)

	got := r.Resolve("jwt_expiry_secs")
	if got.Value != "900" || got.From != SourceEnv {
		t.Fatalf("value %q from %v, want 900 from env", got.Value, got.From)
	}
	if got.Overridable {
		t.Fatal("an unlisted environment key must not be editable from the admin page")
	}
}

func TestResolve_ListedEnvKeyIsEditableAndKeepsItsValue(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "900")
	r := openResolver(t, "JWT_EXPIRY_SECS", nil)

	got := r.Resolve("jwt_expiry_secs")
	if got.Value != "900" || got.From != SourceEnv {
		t.Fatalf("value %q from %v, want the variable's own value until something is stored", got.Value, got.From)
	}
	if !got.Overridable {
		t.Fatal("a listed key must be editable")
	}
}

func TestResolve_StoredValueWinsOverAListedEnvKey(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "900")
	r := openResolver(t, "jwt_expiry_secs", nil)
	r.SetAdminLayer(map[string]string{"jwt_expiry_secs": "1800"})

	got := r.Resolve("jwt_expiry_secs")
	if got.Value != "1800" || got.From != SourceAdmin {
		t.Fatalf("value %q from %v, want 1800 from admin", got.Value, got.From)
	}
}

func TestResolve_StoredValueIsIgnoredWhenTheKeyIsNotListed(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "900")
	r := openResolver(t, "", nil)
	r.SetAdminLayer(map[string]string{"jwt_expiry_secs": "1800"})

	if got := r.Resolve("jwt_expiry_secs"); got.Value != "900" || got.From != SourceEnv {
		t.Fatalf("value %q from %v, want the variable to hold", got.Value, got.From)
	}
}

// The list is normalized the way every other key is, so a deployment may write
// it in either case and with either separator.
func TestResolve_ListIsCaseAndSeparatorInsensitive(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "900")
	r := openResolver(t, " jwt.expiry-secs , ", nil)

	if !r.Resolve("JWT_EXPIRY_SECS").Overridable {
		t.Fatal("the list should normalize the same way a key does")
	}
}

func TestResolve_EmptyEntriesOpenNothing(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "900")
	r := openResolver(t, ",,  ,", nil)

	if r.Resolve("jwt_expiry_secs").Overridable {
		t.Fatal("an empty entry must not open a key")
	}
	if _, ok := r.envOpen[""]; ok {
		t.Fatal("an empty entry must not reach the set")
	}
}

// The veto. A secret stays operator-only however the variable is written, so a
// typo cannot open one.
func TestResolve_OperatorOnlyKeyIgnoresTheList(t *testing.T) {
	t.Setenv("KMS_PROVIDER", "aws")
	r := openResolver(t, "KMS_PROVIDER", nil)
	r.SetAdminLayer(map[string]string{"kms_provider": "local"})

	got := r.Resolve("kms_provider")
	if got.Value != "aws" || got.From != SourceEnv {
		t.Fatalf("value %q from %v, want aws from env", got.Value, got.From)
	}
	if got.Overridable {
		t.Fatal("an operator-only key is never editable")
	}
	if !got.OperatorOnly {
		t.Fatal("the resolution should say why")
	}
}

// An operator-only key set only in the environment must report its
// environment value, since that value is the one in effect.
func TestResolve_OperatorOnlyKeyReportsItsEnvironmentValue(t *testing.T) {
	t.Setenv("KMS_REGION", "eu-west-1")
	r := openResolver(t, "", nil)

	got := r.Resolve("kms_region")
	if got.Value != "eu-west-1" || got.From != SourceEnv {
		t.Fatalf("value %q from %v, want eu-west-1 from env", got.Value, got.From)
	}
}

// The variable cannot open itself. A deployment that listed it would let the
// admin layer rewrite the opt-in.
func TestResolve_TheListCannotOpenItself(t *testing.T) {
	r := openResolver(t, OverridableEnvKey, nil)
	if r.envOverridable(OverridableEnvKey) {
		t.Fatal("LYEVE_OVERRIDABLE must never be editable from the admin page")
	}
	if r.Resolve(OverridableEnvKey).Overridable {
		t.Fatal("LYEVE_OVERRIDABLE must resolve as pinned")
	}
}

// A listed key with no variable set behaves like an unlisted one: the file answers,
// then the admin layer, then nothing.
func TestResolve_ListingAKeyThatIsNotSetChangesNothing(t *testing.T) {
	r := openResolver(t, "example_host", map[string]FileValue{
		"EXAMPLE_HOST": {Value: "mail.example.com", Origin: "lyeve.yaml:3"},
	})

	got := r.Resolve("example_host")
	if got.Value != "mail.example.com" || got.From != SourceFile {
		t.Fatalf("value %q from %v, want the file value", got.Value, got.From)
	}
	if got.Overridable {
		t.Fatal("an untagged file key stays locked whatever the environment list says")
	}
}

// An empty variable is how an operator turns a setting off, and opening the key
// must not change that: the emptiness is the value until something is stored.
func TestResolve_AnOpenedEmptyVariableStaysEmpty(t *testing.T) {
	t.Setenv("EXAMPLE_HOST", "")
	r := openResolver(t, "example_host", map[string]FileValue{
		"EXAMPLE_HOST": {Value: "mail.example.com", Origin: "lyeve.yaml:3"},
	})

	got := r.Resolve("example_host")
	if got.Value != "" || got.From != SourceEnv {
		t.Fatalf("value %q from %v, want the empty variable to hold over the file", got.Value, got.From)
	}
	if !got.Overridable {
		t.Fatal("it is still editable, which is the point of listing it")
	}
}
