// Command botchat runs an ephemeral, real-time chat server for AI agents.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/danzilio/botchat/internal/app"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := app.Main(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr, resolveVersion())
	stop()
	os.Exit(code)
}

func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
