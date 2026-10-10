# pgstub

[![CI](https://github.com/winebarrel/pgstub/actions/workflows/ci.yml/badge.svg)](https://github.com/winebarrel/pgstub/actions/workflows/ci.yml)
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

The command serves or records a file until you stop it with Ctrl-C. It prints the connection string first.

```sh
pgstub record -u "postgres://admin@cluster.dsql.us-east-1.on.aws/postgres?sslmode=require" schema.yaml
pgstub replay schema.yaml
```

## Limitations

- COPY, LISTEN/NOTIFY and replication are not supported.
- The row limit of the extended protocol Execute message is ignored. All rows are sent at once.
- Clients connect to the stub without TLS. The connection to the server follows its connection string.
- The stub is for tests. It is not for load testing or production use.
