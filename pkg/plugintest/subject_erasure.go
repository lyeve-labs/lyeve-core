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
// runs under, for a test that puts tenant claims on it.
func RunSubjectErasuresCtx(ctx context.Context, t T, host core.Host, p core.Plugin, identifier string) int64 {
	t.Helper()
	declarer, ok := p.(compliance.SubjectEraserDeclarer)
	if !ok {
		t.Fatalf("%s does not declare its subject erasures", p.Name())
		return 0
	}
	var total int64
	for i, se := range declarer.SubjectErasures() {
		e := compliance.NewDeclaredEraser(p.Name(), host, se)
		if e == nil {
			t.Fatalf("%s declares subject erasure %d with no tables or no function", p.Name(), i)
			return total
		}
		n, err := e.EraseSubject(ctx, identifier)
		if err != nil {
			t.Fatalf("subject erasure %d of %s: %v", i, p.Name(), err)
			return total
		}
		total += n
	}
	return total
}
