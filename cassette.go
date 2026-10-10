package pgstub

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.yaml.in/yaml/v3"
)

// cassette is the record file. It holds the same things as expectations
// written by hand, in a form a person can read and fix.
type cassette struct {
	RecordedAt    time.Time      `yaml:"recorded_at,omitempty"`
	ServerVersion string         `yaml:"server_version,omitempty"`
	Startup       *startupRecord `yaml:"startup,omitempty"`
	Exchanges     []*exchange    `yaml:"exchanges"`
}

type startupRecord struct {
	Reject *rejectRecord `yaml:"reject"`
}

type rejectRecord struct {
	Params  []string `yaml:"params,flow"`
	Code    string   `yaml:"code"`
	Message string   `yaml:"message"`
}

// exchange is a statement and what the server answered. A simple query
// holding several statements has one result per statement under results;
// otherwise the result is inline.
type exchange struct {
	Query        string    `yaml:"query"`
	Args         []*string `yaml:"args,omitempty,flow"`
	ParamTypes   []string  `yaml:"param_types,omitempty,flow"`
	resultRecord `yaml:",inline"`
	Results      []resultRecord `yaml:"results,omitempty"`
}

type resultRecord struct {
	Columns  []string    `yaml:"columns,omitempty,flow"`
	Types    []string    `yaml:"types,omitempty,flow"`
	Rows     []rowRecord `yaml:"rows,omitempty"`
	Complete string      `yaml:"complete,omitempty"`
	Error    *pgError    `yaml:"error,omitempty,flow"`
}

// rowRecord is written on one line.
type rowRecord []*string

func (r rowRecord) MarshalYAML() (any, error) {
	var n yaml.Node

	if err := n.Encode([]*string(r)); err != nil {
		return nil, err
	}

	n.Style = yaml.FlowStyle
	return &n, nil
}

func loadCassette(file string) (*cassette, error) {
	b, err := os.ReadFile(file)

	if err != nil {
		return nil, err
	}

	var c cassette

	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}

	m := pgtype.NewMap()

	// Check the types now, so a typo fails when the file is loaded rather
	// than when a statement first needs it.
	for i, x := range c.Exchanges {
		if _, err := typeOIDs(m, x.ParamTypes); err != nil {
			return nil, fmt.Errorf("%s: exchange %d: %w", file, i, err)
		}

		for _, r := range x.results() {
			if _, err := r.toResult(m); err != nil {
				return nil, fmt.Errorf("%s: exchange %d: %w", file, i, err)
			}
		}
	}

	return &c, nil
}

func (c *cassette) save(file string) error {
	c.RecordedAt = time.Now().UTC().Truncate(time.Second)

	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)

	if err := enc.Encode(c); err != nil {
		return err
	}

	return os.WriteFile(file, b.Bytes(), 0o644)
}

func (x *exchange) results() []resultRecord {
	if len(x.Results) > 0 {
		return x.Results
	}

	return []resultRecord{x.resultRecord}
}

func newExchange(m *pgtype.Map, sql string, args []*string, paramOIDs []uint32, results []Result) *exchange {
	x := &exchange{Query: sql, Args: args}

	for _, oid := range paramOIDs {
		x.ParamTypes = append(x.ParamTypes, typeName(m, oid))
	}

	if len(results) == 1 {
		x.resultRecord = toRecord(m, results[0])
	} else {
		for _, r := range results {
			x.Results = append(x.Results, toRecord(m, r))
		}
	}

	return x
}

func toRecord(m *pgtype.Map, r Result) resultRecord {
	rec := resultRecord{Columns: r.columns, Error: r.err}
	allText := true

	for _, oid := range r.types {
		rec.Types = append(rec.Types, typeName(m, oid))
		allText = allText && oid == pgtype.TextOID
	}

	if allText {
		rec.Types = nil
	}

	for _, row := range r.rows {
		rec.Rows = append(rec.Rows, row)
	}

	// The tag of rows is SELECT and the count unless the server said
	// otherwise, as it does for INSERT ... RETURNING.
	if !r.hasRows() || r.tag != fmt.Sprintf("SELECT %d", len(r.rows)) {
		rec.Complete = r.tag
	}

	return rec
}

func (rec resultRecord) toResult(m *pgtype.Map) (Result, error) {
	r := Result{columns: rec.Columns, tag: rec.Complete, err: rec.Error}

	types, err := typeOIDs(m, rec.Types)

	if err != nil {
		return Result{}, err
	}

	switch {
	case len(types) == 0:
		for range rec.Columns {
			r.types = append(r.types, pgtype.TextOID)
		}
	case len(types) == len(rec.Columns):
		r.types = types
	default:
		return Result{}, fmt.Errorf("%d columns but %d types", len(rec.Columns), len(types))
	}

	for _, row := range rec.Rows {
		if len(row) != len(rec.Columns) {
			return Result{}, fmt.Errorf("%d columns but a row of %d values", len(rec.Columns), len(row))
		}

		r.rows = append(r.rows, row)
	}

	return r, nil
}

// typeName returns the name of a type, or its OID for a type pgx does not
// know.
func typeName(m *pgtype.Map, oid uint32) string {
	if t, ok := m.TypeForOID(oid); ok {
		return t.Name
	}

	return strconv.FormatUint(uint64(oid), 10)
}

func typeOIDs(m *pgtype.Map, names []string) ([]uint32, error) {
	oids := make([]uint32, len(names))

	for i, name := range names {
		if t, ok := m.TypeForName(name); ok {
			oids[i] = t.OID
			continue
		}

		oid, err := strconv.ParseUint(name, 10, 32)

		if err != nil {
			return nil, fmt.Errorf("unknown type %q", name)
		}

		oids[i] = uint32(oid)
	}

	return oids, nil
}
