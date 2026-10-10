package pgstub

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strs(values ...string) []*string {
	out := make([]*string, len(values))

	for i, v := range values {
		if v != "<null>" {
			out[i] = &v
		}
	}

	return out
}

func rows(values ...[]string) []rowRecord {
	out := make([]rowRecord, len(values))

	for i, v := range values {
		out[i] = strs(v...)
	}

	return out
}

func TestNormalizeOIDs(t *testing.T) {
	c := &cassette{Exchanges: []*exchange{
		{
			Query: "SELECT c.oid, c.relname, c.reltoastrelid FROM pg_class c",
			resultRecord: resultRecord{
				Columns: []string{"oid", "relname", "reltoastrelid"},
				Types:   []string{"oid", "name", "oid"},
				Rows: rows(
					[]string{"26594", "users", "0"},
					[]string{"1259", "pg_class", "<null>"},
					[]string{"26600", "users_pkey", "26594"},
				),
			},
		},
		{
			Query:      "SELECT attrelid, attname FROM pg_attribute WHERE attrelid = ANY($1::oid[])",
			Args:       strs("{26594,26600,NULL}"),
			ParamTypes: []string{"_oid"},
			resultRecord: resultRecord{
				Columns: []string{"attrelid", "attname"},
				Types:   []string{"oid", "name"},
				Rows:    rows([]string{"26600", "id"}),
			},
		},
		{
			Query:      "SELECT $1::oid, $2::int4",
			Args:       strs("30000", "26594", "{}", "[0:1]={26594,26600}"),
			ParamTypes: []string{"oid", "int4", "_oid", "_oid"},
			resultRecord: resultRecord{
				Columns: []string{"oids", "n"},
				Types:   []string{"_oid", "int4"},
				Rows:    rows([]string{"{30000,1259}", "26594"}),
			},
		},
	}}

	c.normalize(&config{normalizeOIDs: true})

	// An OID of 16384 or more is numbered in the order it first appears. A
	// catalog OID, 0, NULL and a value of another type stay as they were.
	assert.Equal(t, rows(
		[]string{"16384", "users", "0"},
		[]string{"1259", "pg_class", "<null>"},
		[]string{"16385", "users_pkey", "16384"},
	), c.Exchanges[0].Rows)
	assert.Equal(t, strs("{16384,16385,NULL}"), c.Exchanges[1].Args)
	assert.Equal(t, rows([]string{"16385", "id"}), c.Exchanges[1].Rows)
	// An empty array, and one with its bounds written out, stay as they were.
	assert.Equal(t, strs("16386", "26594", "{}", "[0:1]={26594,26600}"), c.Exchanges[2].Args)
	assert.Equal(t, rows([]string{"{16386,1259}", "26594"}), c.Exchanges[2].Rows)
}

func TestNormalizeColumn(t *testing.T) {
	c := &cassette{Exchanges: []*exchange{
		{
			Query: "CREATE INDEX ASYNC i ON t (c)",
			resultRecord: resultRecord{
				Columns: []string{"job_id"},
				Rows:    rows([]string{"wqhu6ewifze5xitg3umt24h5ua"}),
			},
		},
		{
			Query: "CALL sys.wait_for_job('wqhu6ewifze5xitg3umt24h5ua')",
			resultRecord: resultRecord{
				Columns: []string{"succeeded"},
				Types:   []string{"bool"},
				Rows:    rows([]string{"t"}),
			},
		},
		{
			Query: "SELECT status FROM sys.jobs WHERE job_id = $1",
			Args:  strs("wqhu6ewifze5xitg3umt24h5ua"),
			Results: []resultRecord{
				{Columns: []string{"job_id", "status"}, Rows: rows([]string{"ckh4wd5aqbchdl5moc4wsy2ys4", "completed"}, []string{"<null>", "submitted"})},
				{Error: &pgError{Code: "XX000", Message: "job wqhu6ewifze5xitg3umt24h5ua failed"}},
			},
		},
	}}

	c.normalize(&config{normalizeColumns: map[string]string{"job_id": "job-"}})

	// Each value of the column is numbered in the order it first appears,
	// and replaced wherever else it appears too.
	assert.Equal(t, rows([]string{"job-1"}), c.Exchanges[0].Rows)
	assert.Equal(t, "CALL sys.wait_for_job('job-1')", c.Exchanges[1].Query)
	assert.Equal(t, strs("job-1"), c.Exchanges[2].Args)
	assert.Equal(t, rows([]string{"job-2", "completed"}, []string{"<null>", "submitted"}), c.Exchanges[2].Results[0].Rows)
	assert.Equal(t, "job job-1 failed", c.Exchanges[2].Results[1].Error.Message)
}

func TestNormalizeNothing(t *testing.T) {
	c := &cassette{Exchanges: []*exchange{{
		Query:        "SELECT oid FROM pg_class",
		resultRecord: resultRecord{Columns: []string{"oid"}, Types: []string{"oid"}, Rows: rows([]string{"26594"})},
	}}}

	c.normalize(&config{})
	assert.Equal(t, rows([]string{"26594"}), c.Exchanges[0].Rows)

	// A column that no result has changes nothing.
	c.normalize(&config{normalizeColumns: map[string]string{"job_id": "job-"}})
	assert.Equal(t, rows([]string{"26594"}), c.Exchanges[0].Rows)
}

// A recording whose content is unchanged keeps the file as it was, so that
// recording again leaves no diff.
func TestSaveKeepsUnchangedFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.yaml")
	c := &cassette{Exchanges: []*exchange{{Query: "SELECT 1", resultRecord: resultRecord{Complete: "SELECT 1"}}}}
	require.NoError(t, c.save(file))
	before, err := os.ReadFile(file)
	require.NoError(t, err)

	c.RecordedAt = time.Time{}
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, c.save(file))
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))

	c.Exchanges[0].Query = "SELECT 2"
	require.NoError(t, c.save(file))
	changed, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(changed), "SELECT 2")
}
