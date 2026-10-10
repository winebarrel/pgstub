package pgstub

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// call is a statement from a client.
type call struct {
	sql  string
	norm string
	// args is the parameters in text form, for matching and recording.
	args []*string
	// params, paramOIDs and paramFormats are the parameters as the client
	// sent them, for forwarding to the server.
	params       [][]byte
	paramOIDs    []uint32
	paramFormats []int16
	simple       bool
}

// description is the answer to Describe for a statement.
type description struct {
	paramOIDs []uint32
	columns   []string
	types     []uint32
	err       *pgError
}

func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

var paramRef = regexp.MustCompile(`\$(\d+)`)

// paramCount returns the highest $n in sql. A $n in a string literal is
// counted too. The only effect is an extra parameter in the description.
func paramCount(sql string) int {
	n := 0

	for _, m := range paramRef.FindAllStringSubmatch(sql, -1) {
		if i, err := strconv.Atoi(m[1]); err == nil && i > n {
			n = i
		}
	}

	return n
}

// resolve answers a statement from an expectation if one matches, and
// otherwise as the mode says.
func (s *Stub) resolve(ctx context.Context, ss *session, c *call) []Result {
	s.mu.Lock()

	for _, e := range s.expects {
		if !e.used && e.matches(c.norm, c.args) {
			e.used = true
			r := e.result
			s.mu.Unlock()
			return []Result{r}
		}
	}

	switch {
	case s.cfg.mode == modeReplay:
		norm, args := s.redact(c.norm), redactArgs(s.redactor, c.args)

		for i, x := range s.cassette.Exchanges {
			if !s.used[i] && normalizeSQL(x.Query) == norm && argsEqual(x.Args, args) {
				s.used[i] = true
				s.mu.Unlock()
				return ss.recordedResults(x)
			}
		}
	case s.cfg.mode == modeRecord || s.cfg.mode == modePassthrough && !s.cfg.strict:
		s.mu.Unlock()
		results := ss.exec(ctx, c)

		if s.cfg.mode == modeRecord {
			redacted := make([]Result, len(results))

			for i, r := range results {
				redacted[i] = r.redact(s.redactor)
			}

			x := newExchange(ss.types, s.redact(c.sql), redactArgs(s.redactor, c.args), c.paramOIDs, redacted)
			s.mu.Lock()
			s.cassette.Exchanges = append(s.cassette.Exchanges, x)
			s.mu.Unlock()
		}

		return results
	}

	s.mu.Unlock()
	return []Result{s.unexpected(c)}
}

// describe answers Describe for a statement. It uses the first unused
// expectation or exchange for the statement, but does not mark it as used,
// because the statement runs after this. The exception is an exchange with
// an error: the client stops at the error and never runs the statement.
func (s *Stub) describe(ctx context.Context, ss *session, st *statement) *description {
	norm := normalizeSQL(st.sql)
	s.mu.Lock()

	if e := s.findExpect(norm); e != nil {
		params := make([]uint32, max(paramCount(st.sql), len(st.paramOIDs)))
		copy(params, st.paramOIDs)
		d := &description{paramOIDs: params, columns: e.result.columns, types: e.result.types}
		s.mu.Unlock()
		return d
	}

	switch {
	case s.cfg.mode == modeReplay:
		if i := s.findExchange(s.redact(norm)); i >= 0 {
			x := s.cassette.Exchanges[i]
			r := ss.recordedResults(x)[0]

			if r.err != nil {
				s.used[i] = true
			}

			params, _ := typeOIDs(ss.types, x.ParamTypes)
			s.mu.Unlock()
			return &description{paramOIDs: params, columns: r.columns, types: r.types, err: r.err}
		}
	case s.cfg.mode == modeRecord || s.cfg.mode == modePassthrough && !s.cfg.strict:
		s.mu.Unlock()
		d := ss.prepare(ctx, st)

		if s.cfg.mode == modeRecord && d.err != nil {
			x := newExchange(ss.types, s.redact(st.sql), nil, nil, []Result{Result{err: d.err}.redact(s.redactor)})
			s.mu.Lock()
			s.cassette.Exchanges = append(s.cassette.Exchanges, x)
			s.mu.Unlock()
		}

		return d
	}

	s.mu.Unlock()
	return &description{err: s.unexpected(&call{sql: st.sql}).err}
}

// findExpect returns the first unused expectation for a statement.
func (s *Stub) findExpect(norm string) *Expectation {
	for _, e := range s.expects {
		if !e.used && e.sql == norm {
			return e
		}
	}

	return nil
}

// findExchange returns the index of the first unused exchange for a
// statement, or -1.
func (s *Stub) findExchange(norm string) int {
	for i, x := range s.cassette.Exchanges {
		if !s.used[i] && normalizeSQL(x.Query) == norm {
			return i
		}
	}

	return -1
}

func (s *Stub) unexpected(c *call) Result {
	if len(c.args) > 0 {
		s.fail("unexpected statement %q with args %v", c.sql, textArgs(c.args))
	} else {
		s.fail("unexpected statement %q", c.sql)
	}

	return Error("XX000", "pgstub: unexpected statement: "+c.sql)
}

// startupReject returns the error to reject a startup with, if any.
func (s *Stub) startupReject(params map[string]string) *pgError {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.startups {
		if hasAllParams(e.params, params) {
			e.used = true
			err := e.err
			return &err
		}
	}

	if s.cfg.mode == modeReplay && s.cassette.Startup != nil && s.cassette.Startup.Reject != nil {
		r := s.cassette.Startup.Reject

		if hasAllParams(r.Params, params) {
			return &pgError{Code: r.Code, Message: r.Message}
		}
	}

	return nil
}

func (s *Stub) recordReject(params []string, err *pgError) {
	if s.cfg.mode != modeRecord {
		return
	}

	err = Result{err: err}.redact(s.redactor).err

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cassette.Startup = &startupRecord{Reject: &rejectRecord{Params: params, Code: err.Code, Message: err.Message}}
}

func (s *Stub) recordServerVersion(v string) {
	if s.cfg.mode != modeRecord {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cassette.ServerVersion = v
}

func (s *Stub) serverVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.mode == modeReplay && s.cassette.ServerVersion != "" {
		return s.cassette.ServerVersion
	}

	return defaultServerVersion
}

func (s *Stub) redact(v string) string {
	if s.redactor == nil {
		return v
	}

	return s.redactor.Replace(v)
}

func textArgs(args []*string) []any {
	out := make([]any, len(args))

	for i, a := range args {
		if a != nil {
			out[i] = *a
		}
	}

	return out
}
