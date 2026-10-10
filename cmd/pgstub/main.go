package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/winebarrel/pgstub"
)

// version is stamped in by GoReleaser at release time.
var version string

var cli struct {
	Version kong.VersionFlag

	Replay pgstub.ReplayCmd `cmd:"" help:"Answer from a record file."`
	Record pgstub.RecordCmd `cmd:"" help:"Record what a server answers to a file."`
}

func main() {
	kctx := kong.Parse(&cli,
		kong.Name("pgstub"),
		kong.Description("PostgreSQL wire protocol stub for tests."),
		kong.Vars{"version": resolveVersion(version)},
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := kctx.Run(&pgstub.Context{
		Context:   ctx,
		Output:    os.Stdout,
		ErrOutput: os.Stderr,
	})

	kctx.FatalIfErrorf(err)
}
