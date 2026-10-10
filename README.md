# pgstub

[![CI](https://github.com/winebarrel/pgstub/actions/workflows/ci.yml/badge.svg)](https://github.com/winebarrel/pgstub/actions/workflows/ci.yml)
[![AI Generated](https://img.shields.io/badge/AI%20Generated-Claude-orange?logo=anthropic)](https://claude.ai/claude-code)

pgstub is a PostgreSQL wire protocol stub for tests.

A test starts a stub, says which statements to expect and what to return, and hands the stub's connection string to the program under test. The stub can also stand in front of a real server: it passes what it was not told about through to the server, or records the server's answers to a file that a later run replays without the server. This makes it possible to test against the behavior of a server you cannot run in CI, such as Aurora DSQL.

The stub does not parse SQL, and has no knowledge of any dialect. It returns only what a test wrote or what was recorded.

## Installation

```sh
go get github.com/winebarrel/pgstub
```

The command, for use outside Go:

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

	// Run the program under test against s.ConnString().

	s.AssertExpectations(t)
}
```

`Return` takes rows (`Rows`), a command tag (`Complete`) or an error with its SQLSTATE (`Error`). `WithArgs` after `Expect` matches the parameters too. `ExpectStartup` rejects a connection whose startup message sets the given parameters.

### Modes

An option to `Start` says what to do with a statement no expectation matches. Expectations are always matched first.

| Option | Behind the stub | A statement without an expectation |
| --- | --- | --- |
| none | nothing | fails the test |
| `Passthrough(connStr)` | a real server | goes to the server |
| `Record(connStr, file)` | a real server | goes to the server, and its answer is written to file when the test ends |
| `Replay(file)` | nothing | is answered from file, or fails the test |

`Strict()` makes passthrough fail the test on a statement without an expectation too. `Redact(old, new)` keeps a value such as an account ID out of the file; give the same option to replay. `UpstreamPassword(f)` makes the server password per connection, for a token that expires.

### Matching

A statement matches when its text is the same with the whitespace normalized, and its parameters are the same. An expectation or a recorded answer is used once, so the same statement coming several times gets the answers in the order they were recorded. The order between different statements does not matter.

### Record file

The file is YAML, so a recording can be read and fixed by hand.

```yaml
recorded_at: 2026-07-23T00:00:00Z
server_version: "16.9"
startup:
  reject:
    params: [default_transaction_read_only]
    code: 0A000
    message: setting configuration parameter "default_transaction_read_only" not supported
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

Values are in the text format of PostgreSQL. `types` is left out when every column is text, and `complete` when it is `SELECT` and the row count. A query holding several statements has a list of `results` instead.

### Command

The command serves a record file, or records one, until it is stopped with Ctrl-C. It prints the connection string first.

```sh
pgstub record -u "postgres://admin@cluster.dsql.us-east-1.on.aws/postgres?sslmode=require" schema.yaml
pgstub replay schema.yaml
```

## Limitations

- COPY, LISTEN/NOTIFY and replication are not supported.
- The client and the stub talk in plain text. The stub talks to the server as the connection string says.
- It is not meant for load testing or for production.
