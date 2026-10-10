// Package pgstub is a PostgreSQL wire protocol stub for tests.
//
// A test starts a stub, says which statements to expect and what to return,
// and hands the stub's connection string to the program under test. The stub
// can also stand in front of a real server: it passes what it was not told
// about through to the server, or records the server's answers to a file
// that a later run replays without the server.
//
// The stub does not parse SQL. A statement is matched by its text with the
// whitespace normalized, so syntax a stock PostgreSQL rejects is handled the
// same as any other.
package pgstub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type mode int

const (
	modeStub mode = iota
	modePassthrough
	modeRecord
	modeReplay
)

type config struct {
	mode     mode
	upstream string
	file     string
	strict   bool
	redact   []string
	password func(context.Context) (string, error)
}

// Option configures a stub.
type Option func(*config)

// Passthrough sends statements without an expectation to the server at
// connStr.
func Passthrough(connStr string) Option {
	return func(c *config) {
		c.mode = modePassthrough
		c.upstream = connStr
	}
}

// Record sends statements without an expectation to the server at connStr
// and writes what the server answered to file when the stub is closed.
func Record(connStr, file string) Option {
	return func(c *config) {
		c.mode = modeRecord
		c.upstream = connStr
		c.file = file
	}
}

// Replay answers statements without an expectation from file. A statement
// the file does not hold either fails the test.
func Replay(file string) Option {
	return func(c *config) {
		c.mode = modeReplay
		c.file = file
	}
}

// Strict fails the test on a statement without an expectation even in
// passthrough mode.
func Strict() Option {
	return func(c *config) {
		c.strict = true
	}
}

// Redact replaces old with new in what is recorded, so that an endpoint or
// an account ID does not end up in the file. In replay mode, the same
// replacement is made in what a client sends before it is matched against
// the file, so a test can pass the same options to both.
func Redact(old, new string) Option {
	return func(c *config) {
		c.redact = append(c.redact, old, new)
	}
}

// UpstreamPassword sets how the password for the server is made. It is
// called for each connection, which suits a token that expires such as a
// DSQL IAM authentication token.
func UpstreamPassword(f func(context.Context) (string, error)) Option {
	return func(c *config) {
		c.password = f
	}
}

// Stub is a server speaking the PostgreSQL wire protocol.
type Stub struct {
	cfg      config
	upstream *pgconn.Config
	// redactor is made from the Redact options, and nil without them.
	redactor *strings.Replacer
	report   func(format string, args ...any)

	mu       sync.Mutex
	ln       net.Listener
	expects  []*Expectation
	startups []*StartupExpectation
	cassette *cassette
	used     []bool
	conns    map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
	// ctx is canceled by Close, to stop what sessions wait for on the
	// server.
	ctx    context.Context
	cancel context.CancelFunc
}

// New makes a stub. It does not listen until Serve is called.
func New(opts ...Option) (*Stub, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Stub{
		conns:  map[net.Conn]struct{}{},
		ctx:    ctx,
		cancel: cancel,
		report: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...) //nolint:errcheck
		},
	}

	for _, opt := range opts {
		opt(&s.cfg)
	}

	if len(s.cfg.redact) > 0 {
		s.redactor = strings.NewReplacer(s.cfg.redact...)
	}

	switch s.cfg.mode {
	case modePassthrough, modeRecord:
		upstream, err := pgconn.ParseConfig(s.cfg.upstream)

		if err != nil {
			return nil, fmt.Errorf("parse upstream connection string: %w", err)
		}

		s.upstream = upstream
		s.cassette = &cassette{}
	case modeReplay:
		c, err := loadCassette(s.cfg.file)

		if err != nil {
			return nil, err
		}

		s.cassette = c
		s.used = make([]bool, len(c.Exchanges))
	}

	return s, nil
}

// Start makes a stub listening on a local port for the length of the test.
// A statement the stub cannot answer fails t. When the test ends, the stub
// is closed and, in record mode, the file is written.
func Start(t testing.TB, opts ...Option) *Stub {
	t.Helper()

	s, err := New(opts...)

	if err != nil {
		t.Fatal(err)
	}

	s.report = t.Errorf
	ln, err := net.Listen("tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	s.ln = ln
	go s.serve() //nolint:errcheck

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	return s
}

// Serve accepts connections on ln until the stub is closed.
func (s *Stub) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	return s.serve()
}

func (s *Stub) serve() error {
	for {
		conn, err := s.ln.Accept()

		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()

			if closed {
				return nil
			}

			return err
		}

		s.mu.Lock()

		if s.closed {
			s.mu.Unlock()
			conn.Close() //nolint:errcheck
			return nil
		}

		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go func() {
			defer s.wg.Done()
			newSession(s, conn).run(s.ctx)

			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// Addr returns the address the stub listens on.
func (s *Stub) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln.Addr()
}

// ConnString returns a connection string for the stub. The user and the
// database are not checked, so any will do.
func (s *Stub) ConnString() string {
	return fmt.Sprintf("postgres://pgstub@%s/pgstub?sslmode=disable", s.Addr())
}

// Close stops the stub and closes the open connections. In record mode it
// then writes the file.
func (s *Stub) Close() error {
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()
		return nil
	}

	s.closed = true
	var errs []error

	if s.ln != nil {
		errs = append(errs, s.ln.Close())
	}

	for conn := range s.conns {
		conn.Close() //nolint:errcheck
	}

	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()

	if s.cfg.mode == modeRecord {
		errs = append(errs, s.cassette.save(s.cfg.file))
	}

	return errors.Join(errs...)
}

// AssertExpectations reports to t each expectation that no statement used,
// and returns whether all were used.
func (s *Stub) AssertExpectations(t testing.TB) bool {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	ok := true

	for _, e := range s.startups {
		if !e.used {
			t.Errorf("pgstub: expected a startup with %v, but none came", e.params)
			ok = false
		}
	}

	for _, e := range s.expects {
		if !e.used {
			t.Errorf("pgstub: expected %q, but it did not come", e.sql)
			ok = false
		}
	}

	return ok
}

func (s *Stub) fail(format string, args ...any) {
	s.report("pgstub: "+format, args...)
}
