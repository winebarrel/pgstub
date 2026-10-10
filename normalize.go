package pgstub

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// firstNormalOID is the lowest OID the server gives an object that a client
// creates. Lower OIDs belong to the system catalogs and are the same on every
// server of a version.
const firstNormalOID = 16384

// NormalizeOIDs replaces each OID of firstNormalOID or more in the recorded
// file with a number that is the same on every recording: firstNormalOID for
// the first OID that appears, then the next number for each new one. It
// covers values of type oid and oid[] in arguments and in result columns,
// not OIDs written into the statement text.
//
// The server gives a new object a new OID, so without this a recording made
// again differs wherever an OID appears. A client that sends back an OID it
// read gets the replaced one when the file is replayed, so its statements
// still match.
func NormalizeOIDs() Option {
	return func(c *config) {
		c.normalizeOIDs = true
	}
}

// NormalizeColumn replaces each value of the result column named column in
// the recorded file with prefix and a number: 1 for the first value that
// appears, then the next number for each new one. The same value is replaced
// wherever else it appears, in statements, arguments, rows and error
// messages.
//
// Use it for a value the server makes up on each run, such as a job ID, that
// the client sends back. Pass a prefix that cannot be mistaken for other
// text in the file.
func NormalizeColumn(column, prefix string) Option {
	return func(c *config) {
		if c.normalizeColumns == nil {
			c.normalizeColumns = map[string]string{}
		}

		c.normalizeColumns[column] = prefix
	}
}

// normalize applies the Normalize options to the cassette.
func (c *cassette) normalize(cfg *config) {
	if cfg.normalizeOIDs {
		c.normalizeOIDs()
	}

	if len(cfg.normalizeColumns) > 0 {
		c.normalizeColumns(cfg.normalizeColumns)
	}
}

// normalizeOIDs numbers the OIDs in the order the client met them: the
// arguments of an exchange come before its results.
func (c *cassette) normalizeOIDs() {
	oids := map[string]string{}

	mapOID := func(v string) string {
		n, err := strconv.ParseUint(v, 10, 32)

		if err != nil || n < firstNormalOID {
			return v
		}

		if _, ok := oids[v]; !ok {
			oids[v] = strconv.Itoa(firstNormalOID + len(oids))
		}

		return oids[v]
	}

	mapValue := func(typ string, v *string) *string {
		if v == nil {
			return nil
		}

		var out string

		switch typ {
		case "oid":
			out = mapOID(*v)
		case "_oid":
			out = mapArray(*v, mapOID)
		default:
			return v
		}

		return &out
	}

	for _, x := range c.Exchanges {
		for i, typ := range x.ParamTypes {
			if i < len(x.Args) {
				x.Args[i] = mapValue(typ, x.Args[i])
			}
		}

		for _, r := range x.resultRecords() {
			for _, row := range r.Rows {
				for i, typ := range r.Types {
					row[i] = mapValue(typ, row[i])
				}
			}
		}
	}
}

// mapArray applies f to each element of an array in text form, such as
// {1,2,NULL}. An oid[] has no quoted or nested elements.
func mapArray(v string, f func(string) string) string {
	inner, ok := strings.CutPrefix(v, "{")

	if !ok {
		return v
	}

	inner, ok = strings.CutSuffix(inner, "}")

	if !ok || inner == "" {
		return v
	}

	elems := strings.Split(inner, ",")

	for i, e := range elems {
		if e != "NULL" {
			elems[i] = f(e)
		}
	}

	return "{" + strings.Join(elems, ",") + "}"
}

// normalizeColumns numbers the values of the named columns, then replaces
// each value wherever it appears in the cassette.
func (c *cassette) normalizeColumns(prefixes map[string]string) {
	var pairs []string
	seen := map[string]bool{}
	counts := map[string]int{}

	for _, x := range c.Exchanges {
		for _, r := range x.resultRecords() {
			for i, col := range r.Columns {
				prefix, ok := prefixes[col]

				if !ok {
					continue
				}

				for _, row := range r.Rows {
					if row[i] == nil || seen[*row[i]] {
						continue
					}

					seen[*row[i]] = true
					counts[col]++
					pairs = append(pairs, *row[i], fmt.Sprintf("%s%d", prefix, counts[col]))
				}
			}
		}
	}

	if len(pairs) == 0 {
		return
	}

	rep := strings.NewReplacer(pairs...)

	for _, x := range c.Exchanges {
		x.Query = rep.Replace(x.Query)
		x.Args = redactArgs(rep, x.Args)

		for _, r := range x.resultRecords() {
			for i, row := range r.Rows {
				r.Rows[i] = rowRecord(redactArgs(rep, row))
			}

			if r.Error != nil {
				r.Error.Message = rep.Replace(r.Error.Message)
			}
		}
	}
}

// resultRecords returns pointers to the results of an exchange, so that they
// can be changed in place.
func (x *exchange) resultRecords() []*resultRecord {
	if len(x.Results) == 0 {
		return []*resultRecord{&x.resultRecord}
	}

	out := make([]*resultRecord, len(x.Results))

	for i := range x.Results {
		out[i] = &x.Results[i]
	}

	return out
}

// sameContent reports whether two encoded cassettes differ only in
// recorded_at.
func sameContent(a, b []byte) bool {
	strip := func(b []byte) []string {
		return slices.DeleteFunc(strings.Split(string(b), "\n"), func(line string) bool {
			return strings.HasPrefix(line, "recorded_at:")
		})
	}

	return slices.Equal(strip(a), strip(b))
}
