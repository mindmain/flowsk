// Command flowsk is the single static (CGO_ENABLED=0) entrypoint for the Flowsk
// CLI client (auto-creating its local SQLite database on first run), ConnectRPC
// network daemon (`flowsk serve`), and Model Context Protocol server
// (`flowsk mcp serve`).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mindmain/flowsk/internal/app"
	"github.com/mindmain/flowsk/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt := app.NewRuntime()
	defer func() {
		_ = rt.Close()
	}()

	rootCmd := cli.NewRootCommand(rt.CLIDependencies())
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "flowsk: %v\n", err)
		return 1
	}
	return 0
}
