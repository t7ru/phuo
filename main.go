package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/willabides/kongplete"

	"github.com/t7ru/phuo/internal/cli"
	"github.com/t7ru/phuo/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var c cli.CLI
	k, err := kong.New(&c,
		kong.Name("phuo"),
		kong.Description("Dead simple MediaWiki extension and skin manager."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{"version": version.String()},
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	k.Exit = func(code int) { os.Exit(code) }
	kongplete.Complete(k)

	ctxK, err := k.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctxK.BindTo(ctx, (*context.Context)(nil))
	if err := ctxK.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return 130
		}
		if ee, ok := errors.AsType[*cli.ExitError](err); ok {
			fmt.Fprintln(os.Stderr, ee.Error())
			return ee.Code
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
