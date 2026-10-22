package plugintest

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RunSubjectErasures runs the erasures p declares through
// compliance.SubjectEraserDeclarer for identifier on host, behind the same
// table check the engine puts them behind, and returns the rows they report.
//
// p should be a fresh instance that has never started: the engine takes its
// erasures from one, so an erasure that reads anything Start builds fails
// here first. The global eraser registry is not touched.
func RunSubjectErasures(t T, host core.Host, p core.Plugin, identifier string) int64 {
	t.Helper()
	return RunSubjectErasuresCtx(context.Background(), t, host, p, identifier)
}

// RunSubjectErasuresCtx is RunSubjectErasures with the context the erasure
// runs under, for a test that puts tenant claims on it. The subject's
// addresses are resolved first, as the engine resolves them, from the
// accounts in sys_users and the addresses p declares.
func RunSubjectErasuresCtx(ctx context.Context, t T, host core.Host, p core.Plugin, identifier string) int64 {
	t.Helper()
	declarer, ok := p.(compliance.SubjectEraserDeclarer)
	if !ok {
		t.Fatalf("%s does not declare its subject erasures", p.Name())
		return 0
	}
	erasers := []compliance.SubjectEraser{SubjectAccounts(host)}
	for i, se := range declarer.SubjectErasures() {
		e := compliance.NewDeclaredEraser(p.Name(), host, se)
		if e == nil {
			t.Fatalf("%s declares subject erasure %d with no tables or no function", p.Name(), i)
			return 0
		}
		erasers = append(erasers, e)
	}
	ctx, err := compliance.ResolveSubjectAddresses(ctx, identifier, erasers)
	if err != nil {
		t.Fatalf("subject addresses for %s: %v", p.Name(), err)
		return 0
	}
	var total int64
	for i, e := range erasers[1:] {
		n, err := e.EraseSubject(ctx, identifier)
		if err != nil {
			t.Fatalf("subject erasure %d of %s: %v", i, p.Name(), err)
			return total
		}
		total += n
	}
	return total
}

// SubjectAccounts returns an eraser that resolves a subject's accounts and
// their addresses in sys_users as the engine's account eraser does, and
// erases nothing. Registered beside a plugin's erasers, it lets a test run
// the engine's fan-out, which reads the subject's addresses through it.
func SubjectAccounts(host core.Host) compliance.SubjectEraser {
	return subjectAccounts{host: host}
}

type subjectAccounts struct{ host core.Host }

func (subjectAccounts) EraseSubject(context.Context, string) (int64, error) { return 0, nil }

func (subjectAccounts) EraserName() string { return "accounts" }

func (a subjectAccounts) SubjectAccounts(ctx context.Context, identifier string) ([]compliance.SubjectAccount, error) {
	return compliance.SubjectAccounts(ctx, a.host, identifier)
}
