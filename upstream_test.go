package pgstub_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/winebarrel/pgstub"
)

// upstream returns the connection string of the server for the tests that
// need one, after making an empty schema for the test.
func upstream(t *testing.T) (connStr, schema string) {
	t.Helper()

	connStr = os.Getenv("PGSTUB_TEST_UPSTREAM")

	if connStr == "" {
		connStr = "postgres://postgres@127.0.0.1:15432/postgres?sslmode=disable"
	}

	schema = strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	conn := connect(t, connStr)
	ctx := context.Background()

	_, err := conn.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE; CREATE SCHEMA %[1]s", schema))
	require.NoError(t, err)

	return connStr, schema
}

func TestPassthrough(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	connStr, schema := upstream(t)

	s := pgstub.Start(t, pgstub.Passthrough(connStr))
	s.Expect(fmt.Sprintf(`CREATE INDEX ASYNC idx ON %s.t (c)`, schema)).
		Return(pgstub.Rows([]string{"job_id"}, []any{"j1"}))

	conn := connect(t, s.ConnString())

	// The client sees the server's version.
	direct := connect(t, connStr)
	assert.Equal(direct.PgConn().ParameterStatus("server_version"), conn.PgConn().ParameterStatus("server_version"))

	_, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.t (c int)`, schema))
	require.NoError(err)

	// The server does not know this syntax, but the expectation answers it.
	var jobID string
	require.NoError(conn.QueryRow(ctx, fmt.Sprintf(`CREATE INDEX ASYNC idx ON %s.t (c)`, schema)).Scan(&jobID))
	assert.Equal("j1", jobID)

	_, err = conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.t VALUES ($1), ($2)`, schema), 1, 2)
	require.NoError(err)

	var sum int64
	require.NoError(conn.QueryRow(ctx, fmt.Sprintf(`SELECT sum(c) FROM %s.t WHERE c > $1`, schema), 0).Scan(&sum))
	assert.Equal(int64(3), sum)

	// The server's own errors come through.
	_, err = conn.Exec(ctx, `SELECT * FROM nosuchtable`)
	assert.Equal("42P01", pgErrorCode(err))
	_, err = conn.Exec(ctx, `SELECT * FROM nosuchtable WHERE c = $1`, 1)
	assert.Equal("42P01", pgErrorCode(err))
}

func TestPassthroughStrict(t *testing.T) {
	connStr, _ := upstream(t)

	rec := &recorder{TB: t}
	s := pgstub.Start(rec, pgstub.Passthrough(connStr), pgstub.Strict())
	s.Expect(`SELECT 2`).Return(pgstub.Rows([]string{"n"}, []any{2}))
	conn := connect(t, s.ConnString())

	var n int
	assert.NoError(t, conn.QueryRow(context.Background(), `SELECT 2`).Scan(&n))
	assert.Equal(t, 2, n)

	_, err := conn.Exec(context.Background(), `SELECT 1`)
	assert.Equal(t, "XX000", pgErrorCode(err))
	_, err = conn.Exec(context.Background(), `SELECT $1::int`, 1)
	assert.Equal(t, "XX000", pgErrorCode(err))
	assert.Len(t, rec.errors, 2)
}

func TestPassthroughStartupReject(t *testing.T) {
	connStr, _ := upstream(t)
	s := pgstub.Start(t, pgstub.Passthrough(connStr))

	cfg, err := pgx.ParseConfig(s.ConnString())
	require.NoError(t, err)
	cfg.RuntimeParams["default_transaction_read_only"] = "bogus"

	_, err = pgx.ConnectConfig(context.Background(), cfg)
	assert.Equal(t, "22023", pgErrorCode(err))
}

func TestUpstreamLost(t *testing.T) {
	ctx := context.Background()
	connStr, _ := upstream(t)

	rec := &recorder{TB: t}
	s := pgstub.Start(rec, pgstub.Passthrough(connStr))
	conn := connect(t, s.ConnString())

	var pid int32
	require.NoError(t, conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))

	_, err := connect(t, connStr).Exec(ctx, `SELECT pg_terminate_backend($1)`, pid)
	require.NoError(t, err)

	// The server says why it closed the connection, and after that there
	// is no connection to send to.
	_, err = conn.Exec(ctx, `SELECT 1`)
	assert.Equal(t, "57P01", pgErrorCode(err))
	_, err = conn.Exec(ctx, `SELECT 1`)
	assert.Equal(t, "08006", pgErrorCode(err))
	assert.Len(t, rec.errors, 1)
}

func TestUpstreamDown(t *testing.T) {
	rec := &recorder{TB: t}
	s := pgstub.Start(rec, pgstub.Passthrough("postgres://postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1"))

	_, err := pgx.Connect(context.Background(), s.ConnString())
	assert.Equal(t, "08006", pgErrorCode(err))
	assert.Len(t, rec.errors, 1)
}

func TestUpstreamPassword(t *testing.T) {
	connStr, _ := upstream(t)
	called := 0

	s := pgstub.Start(t, pgstub.Passthrough(connStr), pgstub.UpstreamPassword(func(context.Context) (string, error) {
		called++
		return "token", nil
	}))

	connect(t, s.ConnString())
	connect(t, s.ConnString())
	assert.Equal(t, 2, called)

	s2 := pgstub.Start(&recorder{TB: t}, pgstub.Passthrough(connStr), pgstub.UpstreamPassword(func(context.Context) (string, error) {
		return "", fmt.Errorf("no token")
	}))

	_, err := pgx.Connect(context.Background(), s2.ConnString())
	assert.Equal(t, "08006", pgErrorCode(err))
}

// workload does what a program under test might, and returns what it saw.
func workload(t *testing.T, connStr, schema string) []string {
	t.Helper()

	ctx := context.Background()
	var out []string

	conn, err := pgx.Connect(ctx, connStr)
	require.NoError(t, err)
	defer conn.Close(ctx)

	add := func(v ...any) {
		out = append(out, fmt.Sprintln(v...))
	}

	tag, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.users (id int PRIMARY KEY, name text, tags text[], at timestamptz, secret text)`, schema))
	add(tag, err)

	// The same statement before and after a change.
	count := func() {
		var n int64
		require.NoError(t, conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.users`, schema)).Scan(&n))
		add(n)
	}

	count()

	tx, err := conn.Begin(ctx)
	require.NoError(t, err)

	for i := 1; i <= 3; i++ {
		tag, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.users VALUES ($1, $2, $3, $4, $5)`, schema),
			i, fmt.Sprintf("user%d", i), []string{"a", "b c"}, time.Date(2026, 7, 23, 0, 0, i, 0, time.UTC), "acct-123456")
		add(tag, err)
	}

	add(tx.Commit(ctx))
	count()

	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT id, name, tags, at, secret FROM %s.users WHERE id >= $1 ORDER BY id`, schema), 2)
	require.NoError(t, err)

	for rows.Next() {
		var (
			id     int32
			name   string
			tags   []string
			at     time.Time
			secret string
		)

		require.NoError(t, rows.Scan(&id, &name, &tags, &at, &secret))
		add(id, name, tags, at.UTC(), secret)
	}

	add(rows.Err())

	// No rows still has columns.
	rows, err = conn.Query(ctx, fmt.Sprintf(`SELECT id FROM %s.users WHERE id > $1`, schema), 100)
	require.NoError(t, err)
	add(rows.Next(), len(rows.FieldDescriptions()))
	rows.Close()

	_, err = conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.users (id) VALUES (1)`, schema))
	add(pgErrorCode(err))

	// A type pgx does not know comes in text.
	var indkey string
	require.NoError(t, conn.QueryRow(ctx, `SELECT indkey FROM pg_index WHERE indrelid = $1::regclass`, schema+".users").Scan(&indkey))
	add(indkey)

	_, err = conn.Exec(ctx, `SELECT nope FROM pg_class WHERE oid = $1`, 1)
	add(pgErrorCode(err))

	results, err := conn.PgConn().Exec(ctx, fmt.Sprintf(`SET lock_timeout = '1s'; SELECT count(*) FROM %s.users`, schema)).ReadAll()
	require.NoError(t, err)

	for _, r := range results {
		add(r.CommandTag, r.Rows)
	}

	_, err = conn.PgConn().Exec(ctx, `SELECT 1; SELECT nope; SELECT 2`).ReadAll()
	add(pgErrorCode(err))

	return out
}

func TestRecordReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	connStr, schema := upstream(t)
	file := filepath.Join(t.TempDir(), "record.yaml")

	s := pgstub.Start(t, pgstub.Record(connStr, file), pgstub.Redact("acct-123456", "acct-XXX"))
	recorded := workload(t, s.ConnString(), schema)
	require.NoError(s.Close())

	assert.Equal("CREATE TABLE <nil>\n"+
		"0\n"+
		"INSERT 0 1 <nil>\nINSERT 0 1 <nil>\nINSERT 0 1 <nil>\n"+
		"<nil>\n"+
		"3\n"+
		"2 user2 [a b c] 2026-07-23 00:00:02 +0000 UTC acct-123456\n"+
		"3 user3 [a b c] 2026-07-23 00:00:03 +0000 UTC acct-123456\n"+
		"<nil>\n"+
		"false 1\n"+
		"23505\n"+
		"1\n"+
		"42703\n"+
		"SET []\n"+
		"SELECT 1 [[[51]]]\n"+
		"42703\n", strings.Join(recorded, ""))

	b, err := os.ReadFile(file)
	require.NoError(err)
	yaml := string(b)

	assert.Contains(yaml, "server_version: ")
	assert.Contains(yaml, "param_types: [int4, text, _text, timestamptz, text]")
	assert.Contains(yaml, "types: [int4, text, _text, timestamptz, text]")
	assert.Contains(yaml, "acct-XXX")
	assert.NotContains(yaml, "acct-123456")
	assert.Contains(yaml, `types: ["22"]`)
	assert.Contains(yaml, "complete: INSERT 0 1")
	assert.NotContains(yaml, "complete: SELECT 2")

	// The schema is gone, so only the file can answer.
	_, err = connect(t, connStr).Exec(context.Background(), fmt.Sprintf("DROP SCHEMA %s CASCADE", schema))
	require.NoError(err)

	// With the same options, the client's arguments match the file. The
	// rows come back as the file holds them.
	r := pgstub.Start(t, pgstub.Replay(file), pgstub.Redact("acct-123456", "acct-XXX"))
	replayed := workload(t, r.ConnString(), schema)

	for i := range recorded {
		recorded[i] = strings.ReplaceAll(recorded[i], "acct-123456", "acct-XXX")
	}

	assert.Equal(recorded, replayed)
}

// connectWith connects with runtime parameters.
func connectWith(connStr string, params map[string]string) error {
	cfg, err := pgx.ParseConfig(connStr)

	if err != nil {
		return err
	}

	for k, v := range params {
		cfg.RuntimeParams[k] = v
	}

	conn, err := pgx.ConnectConfig(context.Background(), cfg)

	if err == nil {
		conn.Close(context.Background())
	}

	return err
}

func TestRecordStartupReject(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	connStr, _ := upstream(t)
	file := filepath.Join(t.TempDir(), "record.yaml")

	readOnly := map[string]string{"default_transaction_read_only": "bogus"}
	timeout := map[string]string{"statement_timeout": "bogus", "search_path": "public"}

	s := pgstub.Start(t, pgstub.Record(connStr, file))
	assert.Equal("22023", pgErrorCode(connectWith(s.ConnString(), readOnly)))
	assert.Equal("22023", pgErrorCode(connectWith(s.ConnString(), readOnly)))
	assert.Equal("22023", pgErrorCode(connectWith(s.ConnString(), timeout)))
	assert.NoError(connectWith(s.ConnString(), nil))
	require.NoError(s.Close())

	// Each rejected set of parameters is recorded once.
	b, err := os.ReadFile(file)
	require.NoError(err)
	assert.Contains(string(b), `startup_rejects:
  - params: [default_transaction_read_only]
    error: {code: "22023", message: parameter "default_transaction_read_only" requires a Boolean value}
  - params: [search_path, statement_timeout]
    error: {code: "22023", message: 'invalid value for parameter "statement_timeout": "bogus"'}
`)

	r := pgstub.Start(t, pgstub.Replay(file))
	assert.Equal("22023", pgErrorCode(connectWith(r.ConnString(), readOnly)))
	assert.Equal("22023", pgErrorCode(connectWith(r.ConnString(), timeout)))
	assert.NoError(connectWith(r.ConnString(), map[string]string{"search_path": "public"}))
	assert.NoError(connectWith(r.ConnString(), nil))
}

func TestRecordStartupErrorNotAboutParams(t *testing.T) {
	connStr, _ := upstream(t)
	file := filepath.Join(t.TempDir(), "record.yaml")

	// The database does not exist. That has nothing to do with the
	// parameters, so it is not recorded.
	noDB := strings.Replace(connStr, "/postgres?", "/nosuchdb?", 1)
	s := pgstub.Start(t, pgstub.Record(noDB, file))
	assert.Equal(t, "3D000", pgErrorCode(connectWith(s.ConnString(), map[string]string{"search_path": "public"})))
	require.NoError(t, s.Close())

	b, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "startup_rejects")
}

func TestRecordWithExpect(t *testing.T) {
	ctx := context.Background()
	connStr, _ := upstream(t)
	file := filepath.Join(t.TempDir(), "record.yaml")

	s := pgstub.Start(t, pgstub.Record(connStr, file))
	s.Expect(`SELECT 'expected'`).Return(pgstub.Rows([]string{"v"}, []any{"from the test"}))
	conn := connect(t, s.ConnString())

	var v string
	require.NoError(t, conn.QueryRow(ctx, `SELECT 'expected'`).Scan(&v))
	assert.Equal(t, "from the test", v)
	require.NoError(t, conn.QueryRow(ctx, `SELECT 'recorded'`).Scan(&v))
	assert.Equal(t, "recorded", v)

	conn.Close(ctx)
	require.NoError(t, s.Close())

	// Only what the server answered is in the file.
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(b), "SELECT 'recorded'")
	assert.NotContains(t, string(b), "expected")
}

// txStatuses runs nested transactions and returns the status after each
// step.
func txStatuses(t *testing.T, conn *pgx.Conn) string {
	t.Helper()

	ctx := context.Background()
	var out []byte
	step := func(err error) {
		require.NoError(t, err)
		out = append(out, conn.PgConn().TxStatus())
	}

	tx, err := conn.Begin(ctx)
	step(err)
	nested, err := tx.Begin(ctx)
	step(err)
	_, err = nested.Exec(ctx, `SELECT nope`)
	require.Error(t, err)
	step(nil)
	step(nested.Rollback(ctx))
	step(tx.Commit(ctx))

	for _, sql := range []string{"BEGIN", "COMMIT AND CHAIN", "ROLLBACK AND NO CHAIN", "BEGIN", "ROLLBACK AND CHAIN;", "ROLLBACK WORK"} {
		_, err := conn.Exec(ctx, sql)
		step(err)
	}

	return string(out)
}

func TestTxStatus(t *testing.T) {
	connStr, _ := upstream(t)

	want := txStatuses(t, connect(t, connStr))
	assert.Equal(t, "TTETITTITTI", want)

	s := pgstub.Start(t, pgstub.Passthrough(connStr))
	assert.Equal(t, want, txStatuses(t, connect(t, s.ConnString())))
}
