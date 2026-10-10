package pgstub_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/winebarrel/pgstub"
)

// rawConn talks to a stub message by message, for what pgx never sends.
func rawConn(t *testing.T, s *pgstub.Stub) *pgproto3.Frontend {
	t.Helper()

	conn, err := net.Dial("tcp", s.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	fe := pgproto3.NewFrontend(conn, conn)
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{"user": "u"}})
	require.NoError(t, fe.Flush())
	receiveUntilReady(t, fe)
	return fe
}

// receiveUntilReady returns the messages up to ReadyForQuery, as their types
// with the code of an error.
func receiveUntilReady(t *testing.T, fe *pgproto3.Frontend) []string {
	t.Helper()

	var got []string

	for {
		msg, err := fe.Receive()
		require.NoError(t, err)

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			return got
		case *pgproto3.ErrorResponse:
			got = append(got, "Error "+m.Code)
		case *pgproto3.AuthenticationOk, *pgproto3.ParameterStatus, *pgproto3.BackendKeyData:
		default:
			got = append(got, typeName(msg))
		}
	}
}

func typeName(msg pgproto3.BackendMessage) string {
	switch msg.(type) {
	case *pgproto3.ParseComplete:
		return "ParseComplete"
	case *pgproto3.BindComplete:
		return "BindComplete"
	case *pgproto3.CloseComplete:
		return "CloseComplete"
	case *pgproto3.ParameterDescription:
		return "ParameterDescription"
	case *pgproto3.RowDescription:
		return "RowDescription"
	case *pgproto3.NoData:
		return "NoData"
	case *pgproto3.DataRow:
		return "DataRow"
	case *pgproto3.CommandComplete:
		return "CommandComplete"
	}

	return "?"
}

func TestProtocol(t *testing.T) {
	s := pgstub.Start(t)
	s.Expect(`SELECT $1`).Return(pgstub.Rows([]string{"v"}, []any{"x"}))
	s.Expect(`SELECT 2`).Return(pgstub.Rows([]string{"v"}, []any{int64(2)}))

	for name, tc := range map[string]struct {
		send []pgproto3.FrontendMessage
		want []string
	}{
		"unknown statement": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.Bind{PreparedStatement: "nope"},
				&pgproto3.Execute{},
				&pgproto3.Sync{},
			},
			want: []string{"Error 26000"},
		},
		"describe unknown": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.Describe{ObjectType: 'S', Name: "nope"},
				&pgproto3.Sync{},
				&pgproto3.Describe{ObjectType: 'P', Name: "nope"},
				&pgproto3.Sync{},
				&pgproto3.Execute{Portal: "nope"},
				&pgproto3.Sync{},
			},
			want: []string{"Error 26000", "Error 34000", "Error 34000"},
		},
		"close": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.Parse{Name: "s", Query: "SELECT $1"},
				&pgproto3.Close{ObjectType: 'S', Name: "s"},
				&pgproto3.Close{ObjectType: 'P', Name: ""},
				&pgproto3.Flush{},
				&pgproto3.Bind{PreparedStatement: "s"},
				&pgproto3.Sync{},
			},
			want: []string{"ParseComplete", "CloseComplete", "CloseComplete", "Error 26000"},
		},
		"binary parameter of unknown type": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.Parse{Query: "SELECT $1"},
				&pgproto3.Bind{ParameterFormatCodes: []int16{1}, Parameters: [][]byte{{1}}},
				&pgproto3.Execute{},
				&pgproto3.Sync{},
			},
			want: []string{"ParseComplete", "BindComplete", "Error 22P03"},
		},
		"binary result": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.Parse{Query: "SELECT 2", ParameterOIDs: []uint32{0}},
				&pgproto3.Describe{ObjectType: 'S'},
				&pgproto3.Bind{ResultFormatCodes: []int16{1}},
				&pgproto3.Describe{ObjectType: 'P'},
				&pgproto3.Execute{},
				&pgproto3.Sync{},
			},
			want: []string{"ParseComplete", "ParameterDescription", "RowDescription", "BindComplete", "RowDescription", "DataRow", "CommandComplete"},
		},
		"no data": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.Parse{Query: "SELECT $1"},
				&pgproto3.Describe{ObjectType: 'S'},
				&pgproto3.Sync{},
			},
			want: []string{"ParseComplete", "ParameterDescription", "RowDescription"},
		},
		"unsupported": {
			send: []pgproto3.FrontendMessage{
				&pgproto3.CopyDone{},
			},
			want: []string{"Error 0A000"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fe := rawConn(t, s)

			for _, m := range tc.send {
				fe.Send(m)
			}

			require.NoError(t, fe.Flush())

			var got []string

			for range countSyncs(tc.send) {
				got = append(got, receiveUntilReady(t, fe)...)
			}

			assert.Equal(t, tc.want, got)
		})
	}
}

func countSyncs(msgs []pgproto3.FrontendMessage) int {
	n := 0

	for _, m := range msgs {
		switch m.(type) {
		case *pgproto3.Sync, *pgproto3.CopyDone:
			n++
		}
	}

	return n
}

func TestProtocolBinaryResultErrors(t *testing.T) {
	// A file fixed by hand can hold a value that is not of its type, and a
	// type pgx has no binary format for.
	file := filepath.Join(t.TempDir(), "x.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`exchanges:
  - query: SELECT bad
    columns: [n]
    types: [int4]
    rows: [[abc]]
  - query: SELECT unknown
    columns: [indkey]
    types: ["22"]
    rows: [[1 2]]
`), 0o644))

	s := pgstub.Start(t, pgstub.Replay(file))

	for _, sql := range []string{"SELECT bad", "SELECT unknown"} {
		t.Run(sql, func(t *testing.T) {
			fe := rawConn(t, s)
			fe.Send(&pgproto3.Parse{Query: sql})
			fe.Send(&pgproto3.Bind{ResultFormatCodes: []int16{1}})
			fe.Send(&pgproto3.Execute{})
			fe.Send(&pgproto3.Sync{})
			require.NoError(t, fe.Flush())

			assert.Equal(t, []string{"ParseComplete", "BindComplete", "Error 22P03"}, receiveUntilReady(t, fe))
		})
	}
}

func TestProtocolDescribePortalError(t *testing.T) {
	rec := &recorder{TB: t}
	s := pgstub.Start(rec)
	fe := rawConn(t, s)

	fe.Send(&pgproto3.Parse{Query: "SELECT 1"})
	fe.Send(&pgproto3.Bind{})
	fe.Send(&pgproto3.Describe{ObjectType: 'P'})
	fe.Send(&pgproto3.Execute{})
	fe.Send(&pgproto3.Sync{})
	require.NoError(t, fe.Flush())

	assert.Equal(t, []string{"ParseComplete", "BindComplete", "Error XX000"}, receiveUntilReady(t, fe))
	assert.Len(t, rec.errors, 1)
}
