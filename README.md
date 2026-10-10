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

	// Run the program under test with s.ConnString().

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
- Values that change on every run, such as timestamps and generated IDs, cannot match a recording if the program sends them back as parameters.

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

## Limitations

- COPY, LISTEN/NOTIFY and replication are not supported.
- The row limit of the extended protocol Execute message is ignored. All rows are sent at once.
- Clients connect to the stub without TLS. The connection to the server follows its connection string.
- The stub is for tests. It is not for load testing or production use.
