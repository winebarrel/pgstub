package pgstub_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/winebarrel/pgstub"
)

// runCmd starts a command and returns the connection string it prints and
// a function that stops it and returns its error.
func runCmd(t *testing.T, cmd interface{ Run(*pgstub.Context) error }) (string, *strings.Builder, func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	r, w := io.Pipe()
	errOut := &strings.Builder{}
	done := make(chan error, 1)

	go func() {
		done <- cmd.Run(&pgstub.Context{Context: ctx, Output: w, ErrOutput: errOut})
		w.Close()
	}()

	line, err := bufio.NewReader(r).ReadString('\n')
	require.NoError(t, err)
	go io.Copy(io.Discard, r)

	return strings.TrimSpace(line), errOut, func() error {
		cancel()
		return <-done
	}
}

func TestReplayCmd(t *testing.T) {
	connStr, errOut, stop := runCmd(t, &pgstub.ReplayCmd{Listen: "127.0.0.1:0", File: "testdata/replay.yaml"})
	conn := connect(t, connStr)

	tag, err := conn.Exec(context.Background(), `CALL sys.wait_for_job('abc123')`)
	require.NoError(t, err)
	assert.Equal(t, "CALL", tag.String())

	_, err = conn.Exec(context.Background(), `SELECT 42`)
	assert.Error(t, err)

	conn.Close(context.Background())
	require.NoError(t, stop())
	assert.Equal(t, "pgstub: unexpected statement \"SELECT 42\"\n", errOut.String())
}

func TestRecordCmd(t *testing.T) {
	upstreamConnStr, _ := upstream(t)
	file := filepath.Join(t.TempDir(), "record.yaml")

	connStr, _, stop := runCmd(t, &pgstub.RecordCmd{Listen: "127.0.0.1:0", Upstream: upstreamConnStr, Redact: []string{"secret=XXX"}, File: file})
	conn := connect(t, connStr)

	var s string
	require.NoError(t, conn.QueryRow(context.Background(), `SELECT 'secret'`).Scan(&s))
	assert.Equal(t, "secret", s)

	conn.Close(context.Background())
	require.NoError(t, stop())

	b, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(b), "query: SELECT 'XXX'")
}

func TestRecordCmdNormalize(t *testing.T) {
	upstreamConnStr, _ := upstream(t)
	file := filepath.Join(t.TempDir(), "record.yaml")

	connStr, _, stop := runCmd(t, &pgstub.RecordCmd{
		Listen:          "127.0.0.1:0",
		Upstream:        upstreamConnStr,
		NormalizeOIDs:   true,
		NormalizeColumn: []string{"job_id=job-"},
		File:            file,
	})
	conn := connect(t, connStr)
	ctx := context.Background()

	var oid uint32
	require.NoError(t, conn.QueryRow(ctx, `SELECT 100000::oid`).Scan(&oid))
	var job string
	require.NoError(t, conn.QueryRow(ctx, `SELECT md5('x') AS job_id`).Scan(&job))
	require.NoError(t, conn.QueryRow(ctx, `SELECT '`+job+`'`).Scan(&job))

	conn.Close(ctx)
	require.NoError(t, stop())

	b, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(b), `- ["16384"]`)
	assert.Contains(t, string(b), "query: SELECT 'job-1'")
}

func TestCmdErrors(t *testing.T) {
	c := &pgstub.Context{Context: context.Background(), Output: io.Discard, ErrOutput: io.Discard}

	err := (&pgstub.ReplayCmd{Listen: "127.0.0.1:0", Redact: []string{"nosep"}, File: "testdata/replay.yaml"}).Run(c)
	assert.ErrorContains(t, err, "want OLD=NEW")

	err = (&pgstub.RecordCmd{Listen: "127.0.0.1:0", Redact: []string{"=x"}, File: "x.yaml"}).Run(c)
	assert.ErrorContains(t, err, "want OLD=NEW")

	err = (&pgstub.RecordCmd{Listen: "127.0.0.1:0", NormalizeColumn: []string{"job_id"}, File: "x.yaml"}).Run(c)
	assert.ErrorContains(t, err, "want COLUMN=PREFIX")

	err = (&pgstub.ReplayCmd{Listen: "127.0.0.1:0", File: "testdata/nosuchfile.yaml"}).Run(c)
	assert.Error(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	err = (&pgstub.ReplayCmd{Listen: ln.Addr().String(), File: "testdata/replay.yaml"}).Run(c)
	assert.Error(t, err)
}
