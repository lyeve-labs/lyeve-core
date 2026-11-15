package db

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// OpEqual is the equality of a path condition. A field of the listed type
// compares by a filter, which names no operator, so only a path carries it.
const OpEqual = "eq"

// pathBounds are the comparisons the bounds of a path condition make, by the
// names a filter key gives them.
var pathBounds = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// maxPathRelations caps the relations a path crosses, counting a relation
// the path ends on.
const maxPathRelations = 2

// PathGate answers what the caller may read of a content type a list filter
// path reaches: whether it may read the type at all, the columns its mask
// hides there, and whether a draft of the type counts as an entry it could be
// served. An error is a rule store that cannot answer.
type PathGate func(ctx context.Context, schemaName string) (hides func(column string) bool, drafts bool, ok bool, err error)

// IsPath reports whether a list filter key is a path through relations,
// such as author.name.
func IsPath(key string) bool {
	return strings.Contains(key, ".")
}

// pathStep is one relation a path crosses: the field on the type it leaves,
// the type it reaches and that type's table, and how a row there is found
// from the row it leaves. A belongs_to reads key on the row it leaves, a
// has_one or has_many reads key on the target, which keyText marks when it
// is not a UUID column, and a many_to_many reads the pivot's from and to
// columns. drafts is whether a draft of the target counts.
type pathStep struct {
	field    domain.SchemaField
	target   *domain.Schema
	table    string
	key      string
	keyText  bool
	pivot    string
	from, to string
	drafts   bool
}

// pathCondition is a condition on a field of the last type a path reaches,
// with cond's Column the field's column and its values typed by its type.
type pathCondition struct {
	key   string
	steps []pathStep
	field domain.SchemaField
	cond  ListCondition
}

// pathConditions moves each condition whose key is a path out of conds and
// answers it resolved, in key and operator order, so one set of conditions
// is one statement text. A path is refused as an unknown key, with nothing
// that tells one refusal from another, when gate is nil, when it crosses
// more than two relations, when a step is not a relation or the last field
// is not one the type it reaches declares, and when it reaches a type the
// caller may not read or a field or key its mask hides there. An answer that
// differed would say what the caller may not read.
func (s *ContentStore) pathConditions(ctx context.Context, schemaName string, sc *domain.Schema, conds []ListCondition, gate PathGate) ([]ListCondition, []pathCondition, error) {
	var (
		own   []ListCondition
		paths []pathCondition
	)
	for _, c := range conds {
		if !IsPath(c.Column) {
			own = append(own, c)
			continue
		}
		p, err := s.pathCondition(ctx, schemaName, sc, c, gate)
		if err != nil {
			return nil, nil, err
		}
		paths = append(paths, p)
	}
	slices.SortFunc(paths, func(a, b pathCondition) int {
		return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.cond.Op, b.cond.Op))
	})
	return own, paths, nil
}

func (s *ContentStore) pathCondition(ctx context.Context, schemaName string, sc *domain.Schema, c ListCondition, gate PathGate) (pathCondition, error) {
	unknown := fmt.Errorf("%w: unknown filter key %q", domain.ErrBadRequest, c.Column)
	names := strings.Split(c.Column, ".")
	if gate == nil || len(names) > maxPathRelations+1 || slices.Contains(names, "") {
		return pathCondition{}, unknown
	}
	hides, _, ok, err := gate(ctx, schemaName)
	if err != nil || !ok {
		return pathCondition{}, cmp.Or(err, unknown)
	}
	p := pathCondition{key: c.Column}
	sourceName, source := schemaName, sc
	for _, name := range names[:len(names)-1] {
		step, targetHides, ok, err := s.pathStep(ctx, sourceName, source, name, orNone(hides), gate)
		if err != nil || !ok {
			return pathCondition{}, cmp.Or(err, unknown)
		}
		p.steps = append(p.steps, step)
		sourceName, source, hides = step.field.RelationTo, step.target, targetHides
	}
	f, ok := pathField(source, names[len(names)-1])
	relations := len(p.steps)
	if f.FieldType == "relation" {
		relations++
	}
	if !ok || relations > maxPathRelations || orNone(hides)(f.Name) || orNone(hides)(fieldColumn(f)) {
		return pathCondition{}, unknown
	}
	p.field = f
	if p.cond, err = typedPathCondition(f, c); err != nil {
		return pathCondition{}, err
	}
	return p, nil
}

// pathStep resolves the relation name on source, the type the path has
// reached, whose mask hides what hides names. ok is false for a step the
// path may not take: no such relation, a relation or key the mask hides, a
// target the caller may not read or that no longer exists, a has_one or
// has_many with no key inverseKey can read, and a many_to_many from a type
// to itself, whose pivot cannot tell its two sides apart.
func (s *ContentStore) pathStep(ctx context.Context, sourceName string, source *domain.Schema, name string, hides func(string) bool, gate PathGate) (pathStep, func(string) bool, bool, error) {
	f, ok := relationNamed(source, name)
	if !ok || hides(f.Name) {
		return pathStep{}, nil, false, nil
	}
	target, table, err := s.table(ctx, f.RelationTo)
	switch {
	case errors.Is(err, domain.ErrNotFound), err == nil && target == nil:
		return pathStep{}, nil, false, nil
	case err != nil:
		return pathStep{}, nil, false, fmt.Errorf("content list path: %w", err)
	}
	targetHides, drafts, ok, err := gate(ctx, f.RelationTo)
	if err != nil || !ok {
		return pathStep{}, nil, false, err
	}
	targetHides = orNone(targetHides)
	step := pathStep{field: f, target: target, table: table, drafts: drafts}
	switch f.RelationType {
	case domain.RelBelongsTo, "":
		step.key = fieldColumn(f)
		if hides(step.key) {
			return pathStep{}, nil, false, nil
		}
	case domain.RelHasOne, domain.RelHasMany:
		key, ok := inverseKey(sourceName, f, target)
		if !ok || targetHides(key) {
			return pathStep{}, nil, false, nil
		}
		kf, _ := filterField(target, key)
		step.key, step.keyText = key, kf.FieldType != "relation" && kf.FieldType != "uid"
	case domain.RelManyToMany:
		step.from, step.to = pivotColumn(sourceName), pivotColumn(f.RelationTo)
		if step.from == step.to {
			return pathStep{}, nil, false, nil
		}
		pivot, err := s.pivotTable(ctx, sourceName, f)
		if err != nil {
			return pathStep{}, nil, false, fmt.Errorf("content list path: %w", err)
		}
		if err := s.ensureTable(ctx, pivot); err != nil {
			return pathStep{}, nil, false, fmt.Errorf("content list path: ensure pivot tenant column: %w", err)
		}
		step.pivot = s.qualify(pivot)
	default:
		return pathStep{}, nil, false, nil
	}
	return step, targetHides, true, nil
}

// orNone is hides, or a mask that hides nothing when there is none.
func orNone(hides func(string) bool) func(string) bool {
	if hides == nil {
		return func(string) bool { return false }
	}
	return hides
}

// relationNamed answers the relation field of sc called name.
func relationNamed(sc *domain.Schema, name string) (domain.SchemaField, bool) {
	for _, f := range sc.Fields {
		if !f.System && f.Name == name && f.FieldType == "relation" && f.RelationTo != "" {
			return f, true
		}
	}
	return domain.SchemaField{}, false
}

// pathField answers the field of sc a path ends on, by its name: id, the
// two timestamps, and each declared field with a column on sc, a belongs_to
// compared by its key. A to-many relation has no column, as a filter on the
// listed type finds.
func pathField(sc *domain.Schema, name string) (domain.SchemaField, bool) {
	switch name {
	case "id", "created_at", "updated_at":
		return conditionField(sc, name)
	}
	for _, f := range sc.Fields {
		if !f.System && f.Name == name && !isVirtualRelationField(f) {
			return f, true
		}
	}
	return domain.SchemaField{}, false
}

// typedPathCondition answers c on the field f with its values typed, as the
// condition on a field of the listed type is typed. An equality takes the
// fields and values ne takes, because ne on a path is the absence of that
// equality, and a bound takes the fields and values a range does.
func typedPathCondition(f domain.SchemaField, c ListCondition) (ListCondition, error) {
	col := fieldColumn(f)
	c.Column = col
	_, bound := pathBounds[c.Op]
	if c.Op != OpEqual && !bound {
		return typedCondition(f, c)
	}
	if len(c.Values) != 1 {
		return ListCondition{}, fmt.Errorf("%w: filter %q takes one value", domain.ErrBadRequest, col)
	}
	var (
		v   any
		err error
	)
	switch {
	case bound && !orderedType(f.FieldType):
		return ListCondition{}, fmt.Errorf("%w: cannot sort or bound by %q", domain.ErrBadRequest, col)
	case bound:
		v, err = rangeValue(f, c.Values[0])
	case !ListOperatorAllowed(f.FieldType, OpNotEqual):
		return ListCondition{}, fmt.Errorf("%w: filter %q does not take %s", domain.ErrBadRequest, col, c.Op)
	default:
		v, err = conditionValue(f, OpNotEqual, c.Values[0])
	}
	if err != nil {
		return ListCondition{}, err
	}
	return ListCondition{Column: col, Op: c.Op, Values: []any{v}}, nil
}

// orderedType reports whether a field of fieldType takes a bound.
func orderedType(fieldType string) bool {
	switch fieldType {
	case "number", "integer", "decimal", "date", "datetime":
		return true
	}
	return false
}

// pathClause answers the predicate for p with its first placeholder $n, and
// the values it binds. The tenant is $1, as in the list. A positive operator
// holds when the entry has a related entry, through every step, for which it
// holds. ne, ncontains and nin hold when it has none for which eq, contains
// or in holds, so an entry with no related entry matches them, as an entry
// with no value matches them on its own field. empty=false holds when a
// related entry's field has a value, and empty=true is its negation.
func pathClause(engine string, p pathCondition, n int) (string, []any, error) {
	d := dialect.Must(engine)
	col := pathAlias(len(p.steps)) + "." + d.QuoteIdentifier(p.cond.Column)
	op, negate := p.cond.Op, false
	switch op {
	case OpNotEqual:
		op, negate = OpEqual, true
	case OpNotContains:
		op, negate = OpContains, true
	case OpNotIn:
		op, negate = OpIn, true
	case OpEmpty:
		negate = p.cond.Values[0] == true
	}
	var (
		pred string
		args = p.cond.Values
	)
	switch op {
	case OpEmpty:
		pred, args = "NOT "+blankExpr(engine, p.field, col), nil
	case OpEqual:
		pred = col + " = " + fieldParam(engine, p.field, n)
	case OpContains:
		pred = sqldialect.ILike(d.Name(), textExpr(engine, p.field, col), fmt.Sprintf("$%d", n)) + sqldialect.ILikeEscapeClause(d.Name())
		args = []any{sqldialect.ILikeValue(d.Name(), p.cond.Values[0].(string))}
	case OpIn:
		params := make([]string, len(args))
		for k := range args {
			params[k] = fieldParam(engine, p.field, n+k)
		}
		pred = col + " IN (" + strings.Join(params, ", ") + ")"
	default:
		cmpOp, ok := pathBounds[op]
		if !ok {
			return "", nil, fmt.Errorf("content list: no predicate for operator %q", op)
		}
		pred = col + " " + cmpOp + " " + fieldParam(engine, p.field, n)
	}
	for k := len(p.steps) - 1; k >= 0; k-- {
		pred = stepExists(engine, p.steps[k], k, pred)
	}
	if negate {
		return "NOT " + pred, args, nil
	}
	return pred, args, nil
}

// pathAlias names the row a path has reached after k steps. The listed row
// is r.
func pathAlias(k int) string {
	if k == 0 {
		return "r"
	}
	return fmt.Sprintf("rp%d", k)
}

// stepExists answers the predicate that holds when the row step k leaves has
// a related row through st, one this caller could be served, for which
// inner holds: in the tenant, out of the trash, and published unless a
// draft counts. A has_one serves the newest of its rows, by created_at and
// then id, and only that one counts, so a draft newest row hides the
// published one beneath it, as a populated has_one does.
func stepExists(engine string, st pathStep, k int, inner string) string {
	d := dialect.Must(engine)
	parent, a := pathAlias(k), pathAlias(k+1)
	live := func(alias string) string {
		w := " AND " + alias + ".tenant_id = $1"
		if st.target.WithSoftDelete {
			w += " AND " + alias + ".deleted_at IS NULL"
		}
		return w
	}
	served := live(a) + draftWhere(st.target, a, st.drafts)
	switch st.field.RelationType {
	case domain.RelHasOne, domain.RelHasMany:
		where := keyMatch(engine, st, a, parent) + served
		if st.field.RelationType == domain.RelHasOne {
			nb := fmt.Sprintf("rn%d", k+1)
			where += " AND NOT EXISTS (SELECT 1 FROM " + st.table + " " + nb + " WHERE " + keyMatch(engine, st, nb, parent) + live(nb) +
				" AND (" + nb + ".created_at > " + a + ".created_at OR (" + nb + ".created_at = " + a + ".created_at AND " + nb + ".id > " + a + ".id)))"
		}
		return "EXISTS (SELECT 1 FROM " + st.table + " " + a + " WHERE " + where + " AND " + inner + ")"
	case domain.RelManyToMany:
		v := fmt.Sprintf("rv%d", k+1)
		return "EXISTS (SELECT 1 FROM " + st.pivot + " " + v + " JOIN " + st.table + " " + a + " ON " + a + ".id = " + v + "." + d.QuoteIdentifier(st.to) +
			" WHERE " + v + "." + d.QuoteIdentifier(st.from) + " = " + parent + ".id AND " + v + ".tenant_id = $1" + served + " AND " + inner + ")"
	}
	return "EXISTS (SELECT 1 FROM " + st.table + " " + a + " WHERE " + a + ".id = " + parent + "." + d.QuoteIdentifier(st.key) + served + " AND " + inner + ")"
}

// keyMatch answers the predicate that the has_one or has_many key of st on
// the row alias holds the id of the row parent. A key held as text, the
// column named for the source type that inverseKey reads last, compares as
// text on PostgreSQL, which has no comparison of text with a UUID, and as a
// UUID on SQL Server, which writes one in capitals.
func keyMatch(engine string, st pathStep, alias, parent string) string {
	col := alias + "." + dialect.Must(engine).QuoteIdentifier(st.key)
	if st.keyText {
		switch engine {
		case "postgres":
			return col + " = CAST(" + parent + ".id AS TEXT)"
		case "mssql":
			return "TRY_CONVERT(UNIQUEIDENTIFIER, " + col + ") = " + parent + ".id"
		}
	}
	return col + " = " + parent + ".id"
}
