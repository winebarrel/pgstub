package pgstub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
)

// defaultServerVersion is reported when there is no server and the record
// file has no version.
const defaultServerVersion = "17.0"

// reportedParams are the parameters sent to a client at startup, with the
// values used when there is no server.
var reportedParams = [][2]string{
	{"server_version", ""},
	{"server_encoding", "UTF8"},
	{"client_encoding", "UTF8"},
	{"DateStyle", "ISO, MDY"},
	{"IntervalStyle", "postgres"},
	{"TimeZone", "UTC"},
	{"integer_datetimes", "on"},
	{"standard_conforming_strings", "on"},
}

type statement struct {
	sql       string
	paramOIDs []uint32
	desc      *description
}

type portal struct {
	stmt          *statement
	params        [][]byte
	paramFormats  []int16
	resultFormats []int16
}

// session serves one client connection.
type session struct {
	stub    *Stub
	conn    net.Conn
	be      *pgproto3.Backend
	up      *pgconn.PgConn
	types   *pgtype.Map
	stmts   map[string]*statement
	portals map[string]*portal
	tx      byte
	// skipping is set after an error in the extended protocol. Messages are
	// ignored until the next Sync.
	skipping bool
}

func newSession(s *Stub, conn net.Conn) *session {
	return &session{
		stub:    s,
		conn:    conn,
		be:      pgproto3.NewBackend(conn, conn),
		types:   pgtype.NewMap(),
		stmts:   map[string]*statement{},
		portals: map[string]*portal{},
		tx:      'I',
	}
}

func (ss *session) run(ctx context.Context) {
	defer ss.conn.Close() //nolint:errcheck

	if !ss.startup(ctx) {
		return
	}

	if ss.up != nil {
		defer ss.up.Close(context.Background()) //nolint:errcheck
	}

	for {
		msg, err := ss.be.Receive()

		if err != nil {
			return
		}

		if ss.skipping {
			switch msg.(type) {
			case *pgproto3.Sync, *pgproto3.Terminate:
			default:
				continue
			}
		}

		switch m := msg.(type) {
		case *pgproto3.Query:
			ss.query(ctx, m.String)
		case *pgproto3.Parse:
			ss.stmts[m.Name] = &statement{sql: m.Query, paramOIDs: m.ParameterOIDs}
			ss.be.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			ss.bind(m)
		case *pgproto3.Describe:
			ss.describe(ctx, m)
		case *pgproto3.Execute:
			ss.execute(ctx, m)
		case *pgproto3.Close:
			if m.ObjectType == 'S' {
				delete(ss.stmts, m.Name)
			} else {
				delete(ss.portals, m.Name)
			}

			ss.be.Send(&pgproto3.CloseComplete{})
		case *pgproto3.Sync:
			ss.skipping = false
			ss.readyForQuery()
		case *pgproto3.Flush:
			ss.flush()
		case *pgproto3.Terminate:
			return
		default:
			ss.sendError(&pgError{Code: "0A000", Message: fmt.Sprintf("pgstub: unsupported message %T", msg)})
			ss.readyForQuery()
		}
	}
}

func (ss *session) startup(ctx context.Context) bool {
	var params map[string]string

	for params == nil {
		msg, err := ss.be.ReceiveStartupMessage()

		if err != nil {
			return false
		}

		switch m := msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			// The client and the stub talk in plain text.
			if _, err := ss.conn.Write([]byte("N")); err != nil {
				return false
			}
		case *pgproto3.StartupMessage:
			params = m.Parameters
		default:
			return false
		}
	}

	if e := ss.stub.startupReject(params); e != nil {
		ss.fatal(e)
		return false
	}

	status := map[string]string{"server_version": ss.stub.serverVersion()}

	if ss.stub.upstream != nil {
		up, err := ss.connect(ctx, params)

		if err != nil {
			if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
				e := &pgError{Code: pgErr.Code, Message: pgErr.Message}
				ss.stub.recordReject(runtimeParamNames(params), e)
				ss.fatal(e)
			} else {
				ss.stub.fail("connect to upstream: %v", err)
				ss.fatal(&pgError{Code: "08006", Message: "pgstub: connect to upstream: " + err.Error()})
			}

			return false
		}

		ss.up = up

		for _, p := range reportedParams {
			if v := up.ParameterStatus(p[0]); v != "" {
				status[p[0]] = v
			}
		}

		ss.stub.recordServerVersion(status["server_version"])
	}

	ss.be.Send(&pgproto3.AuthenticationOk{})

	for _, p := range reportedParams {
		v, ok := status[p[0]]

		if !ok {
			v = p[1]
		}

		ss.be.Send(&pgproto3.ParameterStatus{Name: p[0], Value: v})
	}

	ss.be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 0}})
	ss.readyForQuery()
	return true
}

// connect connects to the server with the client's runtime parameters.
func (ss *session) connect(ctx context.Context, params map[string]string) (*pgconn.PgConn, error) {
	cfg := ss.stub.upstream.Copy()

	for _, k := range runtimeParamNames(params) {
		cfg.RuntimeParams[k] = params[k]
	}

	if f := ss.stub.cfg.password; f != nil {
		password, err := f(ctx)

		if err != nil {
			return nil, err
		}

		cfg.Password = password
	}

	return pgconn.ConnectConfig(ctx, cfg)
}

// runtimeParamNames returns the names of the startup parameters except the
// user and the database. Those two are for the stub, not the server.
func runtimeParamNames(params map[string]string) []string {
	var names []string

	for k := range params {
		if k != "user" && k != "database" {
			names = append(names, k)
		}
	}

	slices.Sort(names)
	return names
}

func (ss *session) query(ctx context.Context, sql string) {
	if isEmptyQuery(sql) {
		ss.be.Send(&pgproto3.EmptyQueryResponse{})
		ss.readyForQuery()
		return
	}

	results := ss.stub.resolve(ctx, ss, &call{sql: sql, norm: normalizeSQL(sql), simple: true})

	for _, r := range results {
		if r.err != nil {
			ss.sendError(r.err)
			break
		}

		if r.hasRows() {
			ss.be.Send(rowDescription(r.columns, r.types, nil))

			for _, row := range r.rows {
				values := make([][]byte, len(row))

				for i, v := range row {
					if v != nil {
						values[i] = []byte(*v)
					}
				}

				ss.be.Send(&pgproto3.DataRow{Values: values})
			}
		}

		ss.complete(r.commandTag(), sql)
	}

	ss.readyForQuery()
}

func (ss *session) bind(m *pgproto3.Bind) {
	st, ok := ss.stmts[m.PreparedStatement]

	if !ok {
		ss.extendedError(&pgError{Code: "26000", Message: fmt.Sprintf("prepared statement %q does not exist", m.PreparedStatement)})
		return
	}

	ss.portals[m.DestinationPortal] = &portal{
		stmt:          st,
		params:        m.Parameters,
		paramFormats:  m.ParameterFormatCodes,
		resultFormats: m.ResultFormatCodes,
	}

	ss.be.Send(&pgproto3.BindComplete{})
}

func (ss *session) describe(ctx context.Context, m *pgproto3.Describe) {
	if m.ObjectType == 'S' {
		st, ok := ss.stmts[m.Name]

		if !ok {
			ss.extendedError(&pgError{Code: "26000", Message: fmt.Sprintf("prepared statement %q does not exist", m.Name)})
			return
		}

		d := ss.statementDesc(ctx, st)

		if d.err != nil {
			ss.extendedError(d.err)
			return
		}

		ss.be.Send(&pgproto3.ParameterDescription{ParameterOIDs: d.paramOIDs})
		ss.sendRowDescription(d, nil)
		return
	}

	p, ok := ss.portals[m.Name]

	if !ok {
		ss.extendedError(&pgError{Code: "34000", Message: fmt.Sprintf("portal %q does not exist", m.Name)})
		return
	}

	d := ss.statementDesc(ctx, p.stmt)

	if d.err != nil {
		ss.extendedError(d.err)
		return
	}

	ss.sendRowDescription(d, p.resultFormats)
}

// statementDesc returns the description of st. It is looked up once and
// then cached.
func (ss *session) statementDesc(ctx context.Context, st *statement) *description {
	if st.desc != nil {
		return st.desc
	}

	d := ss.stub.describe(ctx, ss, st)

	if d.err == nil {
		st.desc = d
	}

	return d
}

func (ss *session) sendRowDescription(d *description, formats []int16) {
	if len(d.columns) == 0 {
		ss.be.Send(&pgproto3.NoData{})
		return
	}

	ss.be.Send(rowDescription(d.columns, d.types, formats))
}

func (ss *session) execute(ctx context.Context, m *pgproto3.Execute) {
	p, ok := ss.portals[m.Portal]

	if !ok {
		ss.extendedError(&pgError{Code: "34000", Message: fmt.Sprintf("portal %q does not exist", m.Portal)})
		return
	}

	oids := p.stmt.paramOIDs

	if p.stmt.desc != nil {
		oids = p.stmt.desc.paramOIDs
	}

	args, err := ss.textArgs(p.params, p.paramFormats, oids)

	if err != nil {
		ss.extendedError(&pgError{Code: "22P03", Message: "pgstub: " + err.Error()})
		return
	}

	c := &call{
		sql:          p.stmt.sql,
		norm:         normalizeSQL(p.stmt.sql),
		args:         args,
		params:       p.params,
		paramOIDs:    oids,
		paramFormats: p.paramFormats,
	}

	r := ss.stub.resolve(ctx, ss, c)[0]

	if r.err != nil {
		ss.extendedError(r.err)
		return
	}

	for _, row := range r.rows {
		values := make([][]byte, len(row))

		for i, v := range row {
			if v == nil {
				continue
			}

			b, err := textToFormat(ss.types, r.types[i], formatFor(p.resultFormats, i), *v)

			if err != nil {
				ss.extendedError(&pgError{Code: "22P03", Message: fmt.Sprintf("pgstub: column %q: %v", r.columns[i], err)})
				return
			}

			values[i] = b
		}

		ss.be.Send(&pgproto3.DataRow{Values: values})
	}

	ss.complete(r.commandTag(), c.sql)
}

// exec runs a statement on the server.
func (ss *session) exec(ctx context.Context, c *call) []Result {
	if !c.simple {
		return []Result{ss.upstreamResult(readResult(ss.up.ExecParams(ctx, c.sql, c.params, c.paramOIDs, c.paramFormats, nil)))}
	}

	mrr := ss.up.Exec(ctx, c.sql)
	var results []Result
	failed := false

	for mrr.NextResult() {
		r := ss.upstreamResult(readResult(mrr.ResultReader()))
		failed = failed || r.err != nil
		results = append(results, r)
	}

	if err := mrr.Close(); err != nil && !failed {
		results = append(results, ss.upstreamResult(&pgconn.Result{Err: err}))
	}

	return results
}

// prepare asks the server to describe a statement.
func (ss *session) prepare(ctx context.Context, st *statement) *description {
	sd, err := ss.up.Prepare(ctx, "", st.sql, st.paramOIDs)

	if err != nil {
		return &description{err: ss.upstreamResult(&pgconn.Result{Err: err}).err}
	}

	d := &description{paramOIDs: sd.ParamOIDs}

	for _, f := range sd.Fields {
		d.columns = append(d.columns, f.Name)
		d.types = append(d.types, f.DataTypeOID)
	}

	return d
}

// readResult is like ResultReader.Read, but it also keeps the columns when
// there are no rows.
func readResult(rr *pgconn.ResultReader) *pgconn.Result {
	r := &pgconn.Result{}

	for rr.NextRow() {
		row := make([][]byte, len(rr.Values()))

		for i, v := range rr.Values() {
			if v != nil {
				row[i] = slices.Clone(v)
			}
		}

		r.Rows = append(r.Rows, row)
	}

	r.FieldDescriptions = slices.Clone(rr.FieldDescriptions())
	r.CommandTag, r.Err = rr.Close()
	return r
}

func (ss *session) upstreamResult(pr *pgconn.Result) Result {
	if pr.Err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](pr.Err); ok {
			return Result{err: &pgError{Code: pgErr.Code, Message: pgErr.Message}}
		}

		ss.stub.fail("upstream: %v", pr.Err)
		return Error("08006", "pgstub: upstream: "+pr.Err.Error())
	}

	r := Result{tag: pr.CommandTag.String()}

	for _, f := range pr.FieldDescriptions {
		r.columns = append(r.columns, f.Name)
		r.types = append(r.types, f.DataTypeOID)
	}

	for _, row := range pr.Rows {
		values := make([]*string, len(row))

		for i, v := range row {
			if v != nil {
				s := string(v)
				values[i] = &s
			}
		}

		r.rows = append(r.rows, values)
	}

	return r
}

func (ss *session) recordedResults(x *exchange) []Result {
	var results []Result

	for _, rec := range x.results() {
		// The file was checked when it was loaded.
		r, _ := rec.toResult(ss.types)
		results = append(results, r)
	}

	return results
}

// textArgs returns the parameters in text form.
func (ss *session) textArgs(params [][]byte, formats []int16, oids []uint32) ([]*string, error) {
	args := make([]*string, len(params))

	for i, b := range params {
		if b == nil {
			continue
		}

		var oid uint32

		if i < len(oids) {
			oid = oids[i]
		}

		s, err := formatToText(ss.types, oid, formatFor(formats, i), b)

		if err != nil {
			return nil, fmt.Errorf("parameter $%d: %w", i+1, err)
		}

		args[i] = &s
	}

	return args, nil
}

func (ss *session) complete(tag, sql string) {
	ss.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
	ss.tx = txStatusAfter(ss.tx, tag, sql)
}

// isEmptyQuery reports whether sql has only whitespace and comments.
// PostgreSQL answers such a query with EmptyQueryResponse, and database/sql
// with the pgx driver pings with "-- ping".
func isEmptyQuery(sql string) bool {
	for {
		sql = strings.TrimSpace(sql)

		switch {
		case sql == "":
			return true
		case strings.HasPrefix(sql, "--"):
			_, rest, ok := strings.Cut(sql, "\n")

			if !ok {
				return true
			}

			sql = rest
		case strings.HasPrefix(sql, "/*"):
			rest, ok := skipBlockComment(sql)

			if !ok {
				return false
			}

			sql = rest
		default:
			return false
		}
	}
}

// skipBlockComment returns what follows the block comment at the start of
// sql. Block comments nest. It returns false if the comment is not closed.
func skipBlockComment(sql string) (string, bool) {
	depth := 0

	for i := 0; i+1 < len(sql); i++ {
		switch sql[i : i+2] {
		case "/*":
			depth++
			i++
		case "*/":
			depth--
			i++

			if depth == 0 {
				return sql[i+1:], true
			}
		}
	}

	return "", false
}

// stayInTx matches the statements that end with a COMMIT or ROLLBACK tag but
// leave the session in a transaction: ROLLBACK TO a savepoint, and COMMIT or
// ROLLBACK AND CHAIN.
var stayInTx = regexp.MustCompile(`(?i)^(ROLLBACK|ABORT)(\s+(WORK|TRANSACTION))?\s+TO\s|\sAND\s+CHAIN\s*;?\s*$`)

// txStatusAfter returns the transaction status after a statement. It is
// worked out the same way in every mode, so a replay sees the same status
// as the recording. It looks at the whole query text, so it can be wrong for
// a query with several statements, such as "ROLLBACK TO sp; COMMIT", or with
// a leading comment.
func txStatusAfter(status byte, tag, sql string) byte {
	switch tag {
	case "BEGIN", "START TRANSACTION":
		return 'T'
	case "COMMIT", "ROLLBACK":
		if stayInTx.MatchString(strings.TrimSpace(sql)) {
			return 'T'
		}

		return 'I'
	case "PREPARE TRANSACTION":
		return 'I'
	}

	return status
}

func (ss *session) sendError(e *pgError) {
	ss.be.Send(&pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: e.Code, Message: e.Message})

	if ss.tx == 'T' {
		ss.tx = 'E'
	}
}

func (ss *session) extendedError(e *pgError) {
	ss.sendError(e)
	ss.skipping = true
}

func (ss *session) fatal(e *pgError) {
	ss.be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: e.Code, Message: e.Message})
	ss.flush()
}

func (ss *session) readyForQuery() {
	ss.be.Send(&pgproto3.ReadyForQuery{TxStatus: ss.tx})
	ss.flush()
}

// flush sends buffered messages. If it fails, the next read fails too.
func (ss *session) flush() {
	_ = ss.be.Flush()
}

func rowDescription(columns []string, types []uint32, formats []int16) *pgproto3.RowDescription {
	fields := make([]pgproto3.FieldDescription, len(columns))

	for i, name := range columns {
		fields[i] = pgproto3.FieldDescription{
			Name:         []byte(name),
			DataTypeOID:  types[i],
			DataTypeSize: -1,
			TypeModifier: -1,
			Format:       formatFor(formats, i),
		}
	}

	return &pgproto3.RowDescription{Fields: fields}
}

// formatFor returns the format of the i-th value. formats has one code for
// all values, or one code for each.
func formatFor(formats []int16, i int) int16 {
	switch {
	case len(formats) == 1:
		return formats[0]
	case i < len(formats):
		return formats[i]
	default:
		return pgtype.TextFormatCode
	}
}

func textToFormat(m *pgtype.Map, oid uint32, format int16, text string) ([]byte, error) {
	if format == pgtype.TextFormatCode {
		return []byte(text), nil
	}

	t, ok := m.TypeForOID(oid)

	if !ok {
		return nil, fmt.Errorf("no binary format for type %d", oid)
	}

	v, err := t.Codec.DecodeValue(m, oid, pgtype.TextFormatCode, []byte(text))

	if err != nil {
		return nil, err
	}

	return m.Encode(oid, format, v, []byte{})
}

func formatToText(m *pgtype.Map, oid uint32, format int16, b []byte) (string, error) {
	if format == pgtype.TextFormatCode {
		return string(b), nil
	}

	t, ok := m.TypeForOID(oid)

	if !ok {
		return "", fmt.Errorf("binary value of unknown type %d", oid)
	}

	v, err := t.Codec.DecodeValue(m, oid, format, b)

	if err != nil {
		return "", err
	}

	text, err := m.Encode(oid, pgtype.TextFormatCode, v, []byte{})
	return string(text), err
}
