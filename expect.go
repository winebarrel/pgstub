package pgstub

import (
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
)

// Expectation is a statement the stub expects and its answer.
type Expectation struct {
	stub    *Stub
	sql     string
	args    []*string
	hasArgs bool
	result  Result
	used    bool
}

// Expect adds an expected statement. Expectations are checked before
// anything else, in the order they were added. Each one is used once.
func (s *Stub) Expect(sql string) *Expectation {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := &Expectation{stub: s, sql: normalizeSQL(sql)}
	s.expects = append(s.expects, e)
	return e
}

// WithArgs makes the expectation match only these parameters. Without
// WithArgs, any parameters match.
func (e *Expectation) WithArgs(args ...any) *Expectation {
	m := pgtype.NewMap()
	texts := make([]*string, len(args))

	for i, a := range args {
		texts[i] = valueToText(m, 0, a)
	}

	e.stub.mu.Lock()
	defer e.stub.mu.Unlock()

	e.args = texts
	e.hasArgs = true
	return e
}

// Return sets the answer. Without Return, the answer is an empty command
// tag.
func (e *Expectation) Return(r Result) *Expectation {
	e.stub.mu.Lock()
	defer e.stub.mu.Unlock()

	e.result = r
	return e
}

func (e *Expectation) matches(sql string, args []*string) bool {
	return e.sql == sql && (!e.hasArgs || argsEqual(e.args, args))
}

// StartupExpectation is a set of startup parameters that the stub rejects.
type StartupExpectation struct {
	stub   *Stub
	params []string
	err    pgError
	used   bool
}

// ExpectStartup rejects connections that set all of params at startup, as
// DSQL does for default_transaction_read_only. Use Return to set the error.
func (s *Stub) ExpectStartup(params ...string) *StartupExpectation {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := &StartupExpectation{stub: s, params: params, err: pgError{Code: "08P01", Message: "startup rejected by pgstub"}}
	s.startups = append(s.startups, e)
	return e
}

// Return sets the error for the rejected connection. Only a Result made
// with Error has an effect.
func (e *StartupExpectation) Return(r Result) *StartupExpectation {
	e.stub.mu.Lock()
	defer e.stub.mu.Unlock()

	if r.err != nil {
		e.err = *r.err
	}

	return e
}

func hasAllParams(want []string, got map[string]string) bool {
	for _, p := range want {
		if _, ok := got[p]; !ok {
			return false
		}
	}

	return true
}

func argsEqual(a, b []*string) bool {
	return slices.EqualFunc(a, b, func(x, y *string) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}

		return *x == *y
	})
}
