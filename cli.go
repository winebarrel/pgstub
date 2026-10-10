package pgstub

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
)

// Context carries what commands need from the process they run in.
type Context struct {
	// Context is canceled to stop the stub.
	Context   context.Context
	Output    io.Writer
	ErrOutput io.Writer
}

// ReplayCmd serves answers from a record file.
type ReplayCmd struct {
	Listen string   `short:"l" default:"127.0.0.1:5432" env:"PGSTUB_LISTEN" help:"Address to listen on."`
	Redact []string `placeholder:"OLD=NEW" help:"Replace OLD with NEW in client statements before matching. Use the same values as for record."`
	File   string   `arg:"" type:"existingfile" help:"Record file to answer from."`
}

func (cmd *ReplayCmd) Run(c *Context) error {
	opts, err := redactOptions(cmd.Redact)

	if err != nil {
		return err
	}

	return serve(c, cmd.Listen, append(opts, Replay(cmd.File))...)
}

// RecordCmd records a server's answers to a file.
type RecordCmd struct {
	Listen   string   `short:"l" default:"127.0.0.1:5432" env:"PGSTUB_LISTEN" help:"Address to listen on."`
	Upstream string   `short:"u" required:"" env:"PGSTUB_UPSTREAM" help:"Connection string of the server. PGPASSWORD can hold the password."`
	Redact   []string `placeholder:"OLD=NEW" help:"Replace OLD with NEW in the recorded file."`
	File     string   `arg:"" type:"path" help:"Record file to write on exit."`
}

func (cmd *RecordCmd) Run(c *Context) error {
	opts, err := redactOptions(cmd.Redact)

	if err != nil {
		return err
	}

	return serve(c, cmd.Listen, append(opts, Record(cmd.Upstream, cmd.File))...)
}

func redactOptions(pairs []string) ([]Option, error) {
	var opts []Option

	for _, p := range pairs {
		old, new, ok := strings.Cut(p, "=")

		if !ok || old == "" {
			return nil, fmt.Errorf("--redact %q: want OLD=NEW", p)
		}

		opts = append(opts, Redact(old, new))
	}

	return opts, nil
}

// serve runs a stub until c.Context is canceled. It first prints the
// connection string, so that a script can wait for it.
func serve(c *Context, addr string, opts ...Option) error {
	s, err := New(opts...)

	if err != nil {
		return err
	}

	s.report = func(format string, args ...any) {
		fmt.Fprintf(c.ErrOutput, format+"\n", args...) //nolint:errcheck
	}

	ln, err := net.Listen("tcp", addr)

	if err != nil {
		return err
	}

	s.ln = ln
	served := make(chan error, 1)

	go func() {
		served <- s.serve()
	}()

	fmt.Fprintln(c.Output, s.ConnString()) //nolint:errcheck

	select {
	case <-c.Context.Done():
		return s.Close()
	case err := <-served:
		s.Close() //nolint:errcheck
		return err
	}
}
