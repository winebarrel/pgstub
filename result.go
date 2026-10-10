package pgstub

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// Result is the answer to a statement: rows, a command tag or an error.
type Result struct {
	columns []string
	types   []uint32
	rows    [][]*string
	tag     string
	err     *pgError
}

type pgError struct {
	Code    string `yaml:"code"`
	Message string `yaml:"message"`
}

// Rows returns rows. The type of each column comes from the Go type of its
// first non-nil value. A column with only nil values is text. nil is NULL.
func Rows(columns []string, rows ...[]any) Result {
	m := pgtype.NewMap()
	r := Result{columns: columns, types: make([]uint32, len(columns))}

	for i := range columns {
		r.types[i] = pgtype.TextOID

		for _, row := range rows {
			if i < len(row) && row[i] != nil {
				if t, ok := m.TypeForValue(row[i]); ok {
					r.types[i] = t.OID
				}

				break
			}
		}
	}

	for _, row := range rows {
		values := make([]*string, len(columns))

		for i := range columns {
			if i < len(row) {
				values[i] = valueToText(m, r.types[i], row[i])
			}
		}

		r.rows = append(r.rows, values)
	}

	return r
}

// Complete returns a command tag such as "CREATE INDEX" or "INSERT 0 1".
func Complete(tag string) Result {
	return Result{tag: tag}
}

// Error returns an error with a SQLSTATE code and a message.
func Error(code, message string) Result {
	return Result{err: &pgError{Code: code, Message: message}}
}

func (r Result) hasRows() bool {
	return len(r.columns) > 0
}

func (r Result) commandTag() string {
	if r.tag == "" && r.hasRows() {
		return fmt.Sprintf("SELECT %d", len(r.rows))
	}

	return r.tag
}

func (r Result) redact(rep *strings.Replacer) Result {
	if rep == nil {
		return r
	}

	out := r
	out.rows = make([][]*string, len(r.rows))

	for i, row := range r.rows {
		out.rows[i] = redactArgs(rep, row)
	}

	if r.err != nil {
		out.err = &pgError{Code: r.err.Code, Message: rep.Replace(r.err.Message)}
	}

	return out
}

// redactArgs applies rep to each non-NULL value.
func redactArgs(rep *strings.Replacer, values []*string) []*string {
	if rep == nil || values == nil {
		return values
	}

	out := make([]*string, len(values))

	for i, v := range values {
		if v != nil {
			s := rep.Replace(*v)
			out[i] = &s
		}
	}

	return out
}

// valueToText encodes a Go value in the text format of oid. If that fails,
// it uses fmt.
func valueToText(m *pgtype.Map, oid uint32, v any) *string {
	if v == nil {
		return nil
	}

	if b, err := m.Encode(oid, pgtype.TextFormatCode, v, []byte{}); err == nil {
		if b == nil {
			return nil
		}

		s := string(b)
		return &s
	}

	s := fmt.Sprint(v)
	return &s
}
