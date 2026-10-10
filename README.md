# pgstub

[![CI](https://github.com/winebarrel/pgstub/actions/workflows/ci.yml/badge.svg)](https://github.com/winebarrel/pgstub/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/winebarrel/pgstub.svg)](https://pkg.go.dev/github.com/winebarrel/pgstub)
[![codecov](https://codecov.io/gh/winebarrel/pgstub/graph/badge.svg?token=40xNWfPaX8)](https://codecov.io/gh/winebarrel/pgstub)
[![AI Generated](https://img.shields.io/badge/AI%20Generated-Claude-orange?logo=anthropic)](https://claude.ai/claude-code)

pgstub is a PostgreSQL wire protocol stub for tests.

In a test, you start a stub and tell it which statements to expect and what to return. Then you give its connection string to the program under test.

The stub can also forward statements to a real server and record the answers to a file. Later test runs replay the file without the server. Use this to test against a server that you cannot run in CI, such as Aurora DSQL.

The stub does not parse SQL and knows nothing about SQL dialects. It returns only what the test set or what was recorded.

## Installation

```sh
go get github.com/winebarrel/pgstub
```

To use the stub outside Go, install the command:

```sh
go install github.com/winebarrel/pgstub/cmd/pgstub@latest
```

## Usage

The example uses database/sql with the pgx driver (`github.com/jackc/pgx/v5/stdlib`). Any PostgreSQL client works.

```go
func TestApply(t *testing.T) {
	// The stub stops when the test ends. No need to close it.
	s := pgstub.Start(t)

	s.Expect(`CREATE INDEX ASYNC idx ON t (c)`).
		Return(pgstub.Rows([]string{"job_id"}, []any{"j1"}))
	s.Expect(`CALL sys.wait_for_job('j1')`).
		Return(pgstub.Complete("CALL"))
	s.Expect(`ALTER TABLE t DROP COLUMN c`).
		Return(pgstub.Error("0A000", "unsupported ALTER TABLE DROP COLUMN statement"))

	// The program under test connects to the stub as to any PostgreSQL.
	db, err := sql.Open("pgx", s.ConnString())
	require.NoError(t, err)
	defer db.Close()

	var jobID string
	err = db.QueryRow(`CREATE INDEX ASYNC idx ON t (c)`).Scan(&jobID)
	require.NoError(t, err)
	assert.Equal(t, "j1", jobID)

	_, err = db.Exec(`CALL sys.wait_for_job('j1')`)
	require.NoError(t, err)

	_, err = db.Exec(`ALTER TABLE t DROP COLUMN c`)
	assert.ErrorContains(t, err, "SQLSTATE 0A000")

	s.AssertExpectations(t)
}
```

`Return` takes one of these:

- `Rows(columns, rows...)`: rows. The column types come from the Go values.
- `Complete(tag)`: a command tag such as `CREATE INDEX`.
- `Error(code, message)`: an error with a SQLSTATE code.

Add `WithArgs(args...)` after `Expect` to match the parameters too. `ExpectStartup(params...)` rejects connections that set those parameters at startup.

### Modes

Expectations are always checked first. The mode decides what happens to a statement that matches no expectation.

| Option | The statement |
| --- | --- |
| (none) | fails the test |
| `Passthrough(connStr)` | is sent to the server |
| `Record(connStr, file)` | is sent to the server, and the answer is saved to the file when the test ends |
| `Replay(file)` | is answered from the file. If the file has no answer, the test fails. |

Other options:

- `Strict()`: in passthrough mode, fail the test instead of sending the statement to the server.
- `Redact(old, new)`: replace a value, such as an account ID, in the recorded file. Pass the same option to `Replay`.
- `UpstreamPassword(f)`: get the server password from a function for each connection. Use it for tokens that expire.
- `NormalizeOIDs()`: in record mode, replace the OIDs of the objects the test creates with numbers that are the same on every recording. See below.
- `NormalizeColumn(column, prefix)`: in record mode, replace each value of a result column, such as a job ID, with `prefix` and a number, wherever it appears in the file. See below.

### Record and replay

Record once against a real server, commit the file, and replay it in CI. A common way is to record when a connection string is set, and replay otherwise:

```go
func TestApplyDSQL(t *testing.T) {
	file := "testdata/apply_dsql.yaml"
	opts := []pgstub.Option{pgstub.Replay(file)}

	if dsn := os.Getenv("DSQL_DSN"); dsn != "" {
		opts = []pgstub.Option{pgstub.Record(dsn, file)}
	}

	opts = append(opts, pgstub.Redact("123456789012", "ACCOUNT_ID"))
	s := pgstub.Start(t, opts...)

	// Run the program under test with s.ConnString().
}
```

To record, run the test with the connection string. The password can come from `PGPASSWORD`:

```sh
export DSQL_DSN="postgres://admin@abc123.dsql.us-east-1.on.aws/postgres?sslmode=require"
export PGPASSWORD="$(aws dsql generate-db-connect-admin-auth-token --hostname abc123.dsql.us-east-1.on.aws --region us-east-1)"
go test -run TestApplyDSQL .
```

Without `DSQL_DSN`, the same test replays the file and needs no server.

Things to know:

- The file is written when the test ends. Statements answered by expectations are not recorded.
- The server must be in the same state each time you record. Clean up what the test creates, before and after.
- If the program sends a different statement or different parameters, replay fails. Record again. The diff of the file shows what changed on the server side too.
- Values that change on every run, such as timestamps and generated IDs, cannot match a recording if the program sends them back as parameters. `NormalizeOIDs` and `NormalizeColumn` handle two kinds of them. See below.
- If a new recording has the same answers as the file, the file is not written, so `recorded_at` does not change.

### Values that change on every recording

The server gives a new object a new OID, and some servers make up an ID for each job. These values make a new recording differ from the old one even when nothing else changed. Two options replace them when the file is written:

- `NormalizeOIDs()` replaces each OID of 16384 or more in values of type `oid` and `oid[]`, in parameters and in result columns. The first OID that appears becomes 16384, the next new one 16385, and so on. OIDs below 16384 belong to the system catalogs and do not change, so they are kept. OIDs written into the statement text are not replaced.
- `NormalizeColumn(column, prefix)` replaces each value of the result column `column` with `prefix` and a number: `prefix1` for the first value, `prefix2` for the next new one, and so on. The same value is replaced wherever else it appears in the file: in statements, parameters, rows and error messages.

A program that sends such a value back gets the replaced value from the file in replay mode, so its statements still match. For example, with Aurora DSQL:

```go
opts = append(opts, pgstub.NormalizeOIDs(), pgstub.NormalizeColumn("job_id", "job-"))
```

`CREATE INDEX ASYNC` returns `job-1`, and the program's `CALL sys.wait_for_job('job-1')` matches the recorded statement.

### Matching

Two statements match when their text is the same after whitespace is normalized, and their parameters are the same.

Each expectation and each recorded answer is used once. If the same statement comes several times, the answers come back in the recorded order. The order of different statements does not matter.

### Record file

The record file is YAML. You can read it and edit it by hand.

```yaml
recorded_at: 2026-07-23T00:00:00Z
server_version: "16.9"
startup_rejects:
  - params: [default_transaction_read_only]
    error: {code: 0A000, message: setting configuration parameter "default_transaction_read_only" not supported}
exchanges:
  - query: SELECT relname FROM pg_class WHERE relnamespace = $1::regnamespace
    args: [public]
    param_types: [text]
    columns: [relname]
    types: [name]
    rows:
      - [users]
  - query: CREATE INDEX ASYNC idx_users_name ON public.users (name)
    columns: [job_id]
    rows:
      - [abc123]
  - query: CALL sys.wait_for_job('abc123')
    complete: CALL
  - query: ALTER TABLE public.users DROP COLUMN email
    error: {code: 0A000, message: unsupported ALTER TABLE DROP COLUMN statement}
```

- Values are in the PostgreSQL text format.
- `types` is omitted when all columns are text.
- `complete` is omitted when it is `SELECT` and the row count.
- If one query has several statements, it has a `results` list instead.
- `startup_rejects` lists the startup parameters that the server rejected. A connection that sets all the parameters of an entry is rejected. Errors that are not about the parameters, such as authentication errors, are not recorded.

### Command

The command does the same for tests that are not written in Go. It serves or records a file until you stop it with Ctrl-C, and prints the connection string first. It listens on `127.0.0.1:5432` unless `-l` says otherwise.

To record, start the command, run your tests against it, then stop it to write the file:

```sh
pgstub record -u "postgres://admin@abc123.dsql.us-east-1.on.aws/postgres?sslmode=require" recording.yaml
# In another terminal:
psql "postgres://pgstub@127.0.0.1:5432/pgstub?sslmode=disable" -f apply.sql
# Then press Ctrl-C in the first terminal.
```

To replay, start the command in the background and run the same tests:

```sh
pgstub replay recording.yaml &
psql "postgres://pgstub@127.0.0.1:5432/pgstub?sslmode=disable" -f apply.sql
kill %1
```

A statement that is not in the file gets an error, and the command prints it to stderr.

`pgstub record` takes `--normalize-oids` and `--normalize-column COLUMN=PREFIX`, which do what `NormalizeOIDs` and `NormalizeColumn` do.

## Limitations

- COPY, LISTEN/NOTIFY and replication are not supported.
- The row limit of the extended protocol Execute message is ignored. All rows are sent at once.
- Clients connect to the stub without TLS. The connection to the server follows its connection string.
- The stub is for tests. It is not for load testing or production use.
