package pgstub_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/winebarrel/pgstub"
)

// recorder stands in for t where a test checks what the stub reports.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func connect(t *testing.T, connStr string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(context.Background(), connStr)
	require.NoError(t, err)

	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func pgErrorCode(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}

	return ""
}

func TestExpect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()

	s := pgstub.Start(t)
	s.Expect(`CREATE INDEX   ASYNC idx ON t (c)`).
		Return(pgstub.Rows([]string{"job_id"}, []any{"j1"}))
	s.Expect(`CALL sys.wait_for_job('j1')`).
		Return(pgstub.Complete("CALL"))
	s.Expect(`ALTER TABLE t DROP COLUMN c`).
		Return(pgstub.Error("0A000", "unsupported ALTER TABLE DROP COLUMN statement"))

	conn := connect(t, s.ConnString())

	// With no server and no file, a default version is reported.
	assert.Equal("17.0", conn.PgConn().ParameterStatus("server_version"))

	// Whitespace does not matter.
	var jobID string
	require.NoError(conn.QueryRow(ctx, "CREATE INDEX ASYNC idx\n  ON t (c)").Scan(&jobID))
	assert.Equal("j1", jobID)

	tag, err := conn.Exec(ctx, `CALL sys.wait_for_job('j1')`)
	require.NoError(err)
	assert.Equal("CALL", tag.String())

	_, err = conn.Exec(ctx, `ALTER TABLE t DROP COLUMN c`)
	assert.Equal("0A000", pgErrorCode(err))
	assert.ErrorContains(err, "unsupported ALTER TABLE DROP COLUMN statement")

	assert.True(s.AssertExpectations(t))
}

func TestExpectRowTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()

	s := pgstub.Start(t)
	ts := time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC)
	s.Expect(`SELECT * FROM t`).
		Return(pgstub.Rows([]string{"id", "name", "ok", "at", "tags", "note"},
			[]any{int64(1), "a", true, ts, []string{"x", "y z"}, nil},
			[]any{int64(2), "b", false, ts, []string{}, "n"},
		))

	conn := connect(t, s.ConnString())
	rows, err := conn.Query(ctx, `SELECT * FROM t`)
	require.NoError(err)

	type row struct {
		ID   int64
		Name string
		OK   bool
		At   time.Time
		Tags []string
		Note *string
	}

	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	require.NoError(err)

	for i := range got {
		got[i].At = got[i].At.UTC()
	}

	n := "n"
	assert.Equal([]row{
		{ID: 1, Name: "a", OK: true, At: ts, Tags: []string{"x", "y z"}},
		{ID: 2, Name: "b", OK: false, At: ts, Tags: []string{}, Note: &n},
	}, got)
	assert.Equal("SELECT 2", rows.CommandTag().String())
}

func TestRowsUnknownType(t *testing.T) {
	type point struct{ X, Y int }

	s := pgstub.Start(t)
	s.Expect(`SELECT p`).Return(pgstub.Rows([]string{"p"}, []any{point{1, 2}}))

	// A value of a type pgx does not know is text, written with fmt.
	var p string
	require.NoError(t, connect(t, s.ConnString()).QueryRow(context.Background(), `SELECT p`).Scan(&p))
	assert.Equal(t, "{1 2}", p)
}

func TestExpectArgs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()

	s := pgstub.Start(t)
	s.Expect(`SELECT name FROM t WHERE id = $1 AND schema = ANY($2)`).
		WithArgs(1, []string{"public"}).
		Return(pgstub.Rows([]string{"name"}, []any{"one"}))
	s.Expect(`SELECT name FROM t WHERE id = $1 AND schema = ANY($2)`).
		WithArgs(2, []string{"public"}).
		Return(pgstub.Rows([]string{"name"}, []any{"two"}))

	conn := connect(t, s.ConnString())
	var name string

	// Expectations are matched by their arguments, not by their order.
	require.NoError(conn.QueryRow(ctx, `SELECT name FROM t WHERE id = $1 AND schema = ANY($2)`, 2, []string{"public"}).Scan(&name))
	assert.Equal("two", name)
	require.NoError(conn.QueryRow(ctx, `SELECT name FROM t WHERE id = $1 AND schema = ANY($2)`, 1, []string{"public"}).Scan(&name))
	assert.Equal("one", name)
}

func TestExpectAnyArgs(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

	s := pgstub.Start(t)
	s.Expect(`DELETE FROM t WHERE id = $1`).Return(pgstub.Complete("DELETE 1"))

	conn := connect(t, s.ConnString())
	tag, err := conn.Exec(ctx, `DELETE FROM t WHERE id = $1`, 42)
	require.NoError(err)
	require.Equal("DELETE 1", tag.String())
}

func TestUnexpected(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	rec := &recorder{TB: t}
	s := pgstub.Start(rec)
	s.Expect(`SELECT 1`).Return(pgstub.Rows([]string{"n"}, []any{1}))

	conn := connect(t, s.ConnString())

	_, err := conn.Exec(ctx, `SELECT 2`)
	assert.Equal("XX000", pgErrorCode(err))

	_, err = conn.Exec(ctx, `SELECT $1::int`, 3)
	assert.Equal("XX000", pgErrorCode(err))

	// An expectation is used once.
	var n int
	assert.NoError(conn.QueryRow(ctx, `SELECT 1`).Scan(&n))
	assert.Error(conn.QueryRow(ctx, `SELECT 1`).Scan(&n))

	assert.Equal([]string{
		`pgstub: unexpected statement "SELECT 2"`,
		`pgstub: unexpected statement "SELECT $1::int"`,
		`pgstub: unexpected statement "SELECT 1"`,
	}, rec.errors)
}

func TestAssertExpectations(t *testing.T) {
	assert := assert.New(t)

	s := pgstub.Start(t)
	s.Expect(`SELECT 1`)
	s.ExpectStartup("default_transaction_read_only")

	rec := &recorder{TB: t}
	assert.False(s.AssertExpectations(rec))
	assert.Equal([]string{
		"pgstub: expected a startup with [default_transaction_read_only], but none came",
		`pgstub: expected "SELECT 1", but it did not come`,
	}, rec.errors)
}

func TestExpectStartup(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	s := pgstub.Start(t)
	s.ExpectStartup("default_transaction_read_only").
		Return(pgstub.Error("0A000", `setting configuration parameter "default_transaction_read_only" not supported`))

	cfg, err := pgx.ParseConfig(s.ConnString())
	require.NoError(t, err)
	cfg.RuntimeParams["default_transaction_read_only"] = "on"

	_, err = pgx.ConnectConfig(ctx, cfg)
	assert.Equal("0A000", pgErrorCode(err))

	// Without the parameter, the connection is accepted.
	connect(t, s.ConnString())
	assert.True(s.AssertExpectations(t))
}

func TestExpectStartupDefaultError(t *testing.T) {
	s := pgstub.Start(t)
	s.ExpectStartup("default_transaction_read_only")

	cfg, err := pgx.ParseConfig(s.ConnString())
	require.NoError(t, err)
	cfg.RuntimeParams["default_transaction_read_only"] = "on"

	_, err = pgx.ConnectConfig(context.Background(), cfg)
	assert.Equal(t, "08P01", pgErrorCode(err))
}

func TestExpectNull(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	rec := &recorder{TB: t}
	s := pgstub.Start(rec)
	s.Expect(`SELECT name FROM t WHERE parent = $1`).
		WithArgs(nil).
		Return(pgstub.Rows([]string{"name"}, []any{(*string)(nil)}))
	s.Expect(`SELECT name FROM t WHERE parent = $1`).
		WithArgs("x").
		Return(pgstub.Rows([]string{"name"}, []any{"y"}))

	conn := connect(t, s.ConnString())

	// NULL matches only NULL.
	var name *string
	assert.NoError(conn.QueryRow(ctx, `SELECT name FROM t WHERE parent = $1`, nil).Scan(&name))
	assert.Nil(name)
	assert.Error(conn.QueryRow(ctx, `SELECT name FROM t WHERE parent = $1`, nil).Scan(&name))
	assert.Len(rec.errors, 1)
}

func TestClients(t *testing.T) {
	ctx := context.Background()
	s := pgstub.Start(t)
	s.Expect(`SELECT 1`).Return(pgstub.Rows([]string{"n"}, []any{1}))

	// A client that goes away before its startup message.
	c, err := net.Dial("tcp", s.Addr().String())
	require.NoError(t, err)
	c.Close()

	// A client that asks for TLS is told to go on in plain text.
	conn := connect(t, strings.Replace(s.ConnString(), "sslmode=disable", "sslmode=prefer", 1))

	// A cancel request has nothing to cancel.
	assert.NoError(t, conn.PgConn().CancelRequest(ctx))

	var n int
	assert.NoError(t, conn.QueryRow(ctx, `SELECT 1`).Scan(&n))
}

func TestTransaction(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

	s := pgstub.Start(t)
	s.Expect(`begin`).Return(pgstub.Complete("BEGIN"))
	s.Expect(`CREATE TABLE t (c int)`).Return(pgstub.Complete("CREATE TABLE"))
	s.Expect(`commit`).Return(pgstub.Complete("COMMIT"))
	s.Expect(`begin`).Return(pgstub.Complete("BEGIN"))
	s.Expect(`DROP TABLE t`).Return(pgstub.Error("42P01", `table "t" does not exist`))
	s.Expect(`rollback`).Return(pgstub.Complete("ROLLBACK"))

	conn := connect(t, s.ConnString())

	tx, err := conn.Begin(ctx)
	require.NoError(err)
	require.Equal(byte('T'), conn.PgConn().TxStatus())
	_, err = tx.Exec(ctx, `CREATE TABLE t (c int)`)
	require.NoError(err)
	require.NoError(tx.Commit(ctx))
	require.Equal(byte('I'), conn.PgConn().TxStatus())

	tx, err = conn.Begin(ctx)
	require.NoError(err)
	_, err = tx.Exec(ctx, `DROP TABLE t`)
	require.Error(err)
	require.Equal(byte('E'), conn.PgConn().TxStatus())
	require.NoError(tx.Rollback(ctx))
	require.Equal(byte('I'), conn.PgConn().TxStatus())

	require.True(s.AssertExpectations(t))
}

func TestEmptyQuery(t *testing.T) {
	s := pgstub.Start(t)
	conn := connect(t, s.ConnString())

	// Only whitespace and comments, as the pgx driver for database/sql
	// sends to ping.
	for _, sql := range []string{" ", "-- ping", "/* a /* nested */ comment */\n-- and a line", "-- line\n  "} {
		_, err := conn.Exec(context.Background(), sql)
		require.NoError(t, err, sql)
	}
}

func TestNotEmptyQuery(t *testing.T) {
	rec := &recorder{TB: t}
	s := pgstub.Start(rec)
	conn := connect(t, s.ConnString())

	// A statement after a comment, and a comment that is not closed.
	for _, sql := range []string{"-- c\nSELECT 1", "/* c */ SELECT 1", "/* not closed"} {
		_, err := conn.Exec(context.Background(), sql)
		assert.Equal(t, "XX000", pgErrorCode(err), sql)
	}

	assert.Len(t, rec.errors, 3)
}

func TestReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()

	s := pgstub.Start(t, pgstub.Replay("testdata/replay.yaml"))
	conn := connect(t, s.ConnString())

	// The version comes from the file.
	assert.Equal("16.9", conn.PgConn().ParameterStatus("server_version"))

	type index struct {
		Schema, Table, Name, Definition string
	}

	query := `SELECT n.nspname, c.relname, i.relname, pg_get_indexdef(x.indexrelid)
FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class c ON c.oid = x.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = ANY($1)`

	// The same statement answers in the order it was recorded.
	for _, want := range [][]index{
		{{"public", "users", "users_pkey", "CREATE UNIQUE INDEX users_pkey ON public.users USING btree_index (id) INCLUDE (name, email)"}},
		{},
	} {
		rows, err := conn.Query(ctx, query, []string{"public"})
		require.NoError(err)
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[index])
		require.NoError(err)
		assert.Equal(want, append([]index{}, got...))
	}

	var jobID string
	require.NoError(conn.QueryRow(ctx, `CREATE INDEX ASYNC idx_users_name ON public.users (name)`).Scan(&jobID))
	assert.Equal("abc123", jobID)

	tag, err := conn.Exec(ctx, `CALL sys.wait_for_job('abc123')`)
	require.NoError(err)
	assert.Equal("CALL", tag.String())

	_, err = conn.Exec(ctx, `ALTER TABLE public.users DROP COLUMN email`)
	assert.Equal("0A000", pgErrorCode(err))

	var n int64
	require.NoError(conn.QueryRow(ctx, `SELECT count(*) FROM public.users WHERE id > $1`, 10).Scan(&n))
	assert.Equal(int64(3), n)

	// A recorded error at describe time.
	_, err = conn.Exec(ctx, `SELECT nope FROM public.users WHERE id = $1`, 1)
	assert.Equal("42703", pgErrorCode(err))

	// Several statements in one query.
	results, err := conn.PgConn().Exec(ctx, `SET lock_timeout = '1s'; SELECT 1 AS one`).ReadAll()
	require.NoError(err)
	require.Len(results, 2)
	assert.Equal("SET", results[0].CommandTag.String())
	assert.Equal([][][]byte{{[]byte("1")}}, results[1].Rows)

	// A connection with the rejected parameters fails as recorded.
	cfg, err := pgx.ParseConfig(s.ConnString())
	require.NoError(err)
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	_, err = pgx.ConnectConfig(ctx, cfg)
	assert.Equal("0A000", pgErrorCode(err))
}

func TestReplayMiss(t *testing.T) {
	ctx := context.Background()

	rec := &recorder{TB: t}
	s := pgstub.Start(rec, pgstub.Replay("testdata/replay.yaml"))
	conn := connect(t, s.ConnString())

	// The arguments are part of the match.
	var n int64
	err := conn.QueryRow(ctx, `SELECT count(*) FROM public.users WHERE id > $1`, 11).Scan(&n)
	assert.Equal(t, "XX000", pgErrorCode(err))

	// A statement the file does not hold fails when it is described.
	_, err = conn.Exec(ctx, `DELETE FROM public.users WHERE id = $1`, 1)
	assert.Equal(t, "XX000", pgErrorCode(err))

	assert.Equal(t, []string{
		`pgstub: unexpected statement "SELECT count(*) FROM public.users WHERE id > $1" with args [11]`,
		`pgstub: unexpected statement "DELETE FROM public.users WHERE id = $1"`,
	}, rec.errors)
}

func TestReplayExpectFirst(t *testing.T) {
	s := pgstub.Start(t, pgstub.Replay("testdata/replay.yaml"))
	s.Expect(`CALL sys.wait_for_job('abc123')`).Return(pgstub.Error("XX000", "job failed"))

	conn := connect(t, s.ConnString())
	_, err := conn.Exec(context.Background(), `CALL sys.wait_for_job('abc123')`)
	assert.ErrorContains(t, err, "job failed")
}

func TestLoadErrors(t *testing.T) {
	for name, content := range map[string]string{
		"syntax":       "exchanges: [",
		"param type":   "exchanges:\n  - query: SELECT $1\n    param_types: [nosuchtype]\n",
		"column type":  "exchanges:\n  - query: SELECT 1\n    columns: [a]\n    types: [nosuchtype]\n",
		"types count":  "exchanges:\n  - query: SELECT 1\n    columns: [a, b]\n    types: [int4]\n",
		"row width":    "exchanges:\n  - query: SELECT 1\n    columns: [a]\n    rows: [[1, 2]]\n",
		"result types": "exchanges:\n  - query: SELECT 1; SELECT 2\n    results:\n      - columns: [a]\n        types: [nosuchtype]\n",
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "x.yaml")
			require.NoError(t, os.WriteFile(file, []byte(content), 0o644))

			_, err := pgstub.New(pgstub.Replay(file))
			assert.Error(t, err)
		})
	}

	_, err := pgstub.New(pgstub.Replay("testdata/nosuchfile.yaml"))
	assert.Error(t, err)

	_, err = pgstub.New(pgstub.Passthrough("://"))
	assert.Error(t, err)
}

func TestServe(t *testing.T) {
	s, err := pgstub.New()
	require.NoError(t, err)
	s.Expect(`SELECT 1`).Return(pgstub.Rows([]string{"n"}, []any{1}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()

	conn := connect(t, "postgres://u@"+ln.Addr().String()+"/d?sslmode=disable")
	var n int
	require.NoError(t, conn.QueryRow(context.Background(), `SELECT 1`).Scan(&n))
	assert.Equal(t, 1, n)

	require.NoError(t, s.Close())
	require.NoError(t, <-served)
	require.NoError(t, s.Close())
}

func TestDatabaseSQL(t *testing.T) {
	s := pgstub.Start(t)
	s.Expect(`SELECT name FROM t WHERE id = $1`).WithArgs(1).
		Return(pgstub.Rows([]string{"name"}, []any{"one"}))
	s.Expect(`SELECT name FROM t WHERE id = $1`).WithArgs(2).
		Return(pgstub.Rows([]string{"name"}, []any{"two"}))

	db, err := sql.Open("pgx", s.ConnString())
	require.NoError(t, err)
	defer db.Close()

	// The driver pings the connection before it uses it again.
	for id, want := range map[int]string{1: "one", 2: "two"} {
		var name string
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT name FROM t WHERE id = $1`, id).Scan(&name))
		assert.Equal(t, want, name)
	}

	assert.True(t, s.AssertExpectations(t))
}
