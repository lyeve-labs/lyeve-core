package sqlx

import (
	"testing"
)

func TestTenantSchemaName_ShortSlugUnchanged(t *testing.T) {
	if got := TenantSchemaName("acme"); got != "tenant_acme" {
		t.Fatalf("TenantSchemaName(acme) = %q, want tenant_acme", got)
	}
}

func TestTenantSchemaName_Boundary(t *testing.T) {
	// 56 chars + "tenant_" (7) = 63 bytes, which is the Postgres ceiling and
	// therefore the point at which the name must switch to a hash.
	short := make([]byte, 56)
	for i := range short {
		short[i] = 'a'
	}
	if got := TenantSchemaName(string(short)); got != "tenant_"+string(short) {
		t.Fatalf("TenantSchemaName(56 a's) = %q, want the verbatim prefix", got)
	}

	long := make([]byte, 57)
	for i := range long {
		long[i] = 'b'
	}
	got := TenantSchemaName(string(long))
	if got == "tenant_"+string(long) {
		t.Fatalf("TenantSchemaName(57 b's) returned the verbatim name %q, want a hash", got)
	}
	if len(got) > 63 {
		t.Fatalf("TenantSchemaName(57 b's) = %q exceeds 63 bytes", got)
	}
	if len(got) < len("tenant_") {
		t.Fatalf("TenantSchemaName(57 b's) = %q lost the prefix", got)
	}
}

func TestTenantSchemaName_DeterministicAndDistinct(t *testing.T) {
	a := TenantSchemaName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 63 a's
	b := TenantSchemaName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab") // 63, differs at end
	if a != TenantSchemaName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatal("TenantSchemaName must be deterministic")
	}
	if a == b {
		t.Fatal("distinct long slugs must hash to distinct schema names")
	}
}
