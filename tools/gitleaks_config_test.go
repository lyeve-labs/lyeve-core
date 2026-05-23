package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitleaksFindings scans dir as plain files with this repository's
// configuration and returns the rule id of every finding.
func gitleaksFindings(t *testing.T, dir string) []string {
	t.Helper()
	bin, err := exec.LookPath("gitleaks")
	if err != nil {
		t.Skip("gitleaks is not on PATH, so the secret scan configuration cannot be exercised here")
	}
	config, err := filepath.Abs(filepath.Join("..", ".gitleaks.toml"))
	if err != nil {
		t.Fatalf("resolve the gitleaks config: %v", err)
	}
	report := filepath.Join(t.TempDir(), "report.json")
	cmd := exec.Command(bin, "detect", "--no-git", "--no-banner",
		"--source", dir, "--config", config,
		"--report-format", "json", "--report-path", report, "--exit-code", "0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitleaks failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read the gitleaks report: %v", err)
	}
	var findings []struct {
		RuleID string `json:"RuleID"`
	}
	if err := json.Unmarshal(raw, &findings); err != nil {
		t.Fatalf("parse the gitleaks report: %v\n%s", err, raw)
	}
	ids := make([]string, 0, len(findings))
	for _, f := range findings {
		ids = append(ids, f.RuleID)
	}
	return ids
}

// A license key pasted into the tree is a finding. A stopword naming the
// product would drop it, because a stopword discards every finding whose
// secret contains the word, and the key's prefix is the product's name.
func TestGitleaksConfig_CatchesALicenseKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := "lyeve_key_" + "9f3c0a7d51e24b86c0d9a1f37e5b2c48d6a0f1e93b7c25d84e0a6f19c3b5d72e"
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("license_key: "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := gitleaksFindings(t, dir)
	for _, id := range ids {
		if id == "lyeve-license-key" {
			return
		}
	}
	t.Fatalf("a license key scanned clean, findings: %v", ids)
}
