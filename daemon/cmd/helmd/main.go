// helmd is Helm's per-machine daemon ("the hand").
//
//	helmd run                     start the daemon (foreground; use systemd/launchd for background)
//	helmd caps                    print detected capabilities as JSON
//	helmd hook pre-tool-use --run <id> [--sock <path>]
//	                              Claude Code PreToolUse hook (reads stdin, writes decision to stdout)
//	helmd version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/helm/helm/daemon/internal/conn"
	"github.com/helm/helm/daemon/internal/executor"
	"github.com/helm/helm/daemon/internal/hook"
	"github.com/helm/helm/daemon/internal/registry"
	"github.com/helm/helm/daemon/internal/supervisor"
)

var version = "0.1.0-dev"

func helmHome() string {
	if h := os.Getenv("HELM_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".helm")
}

func defaultSock() string { return filepath.Join(helmHome(), "run", "helmd.sock") }

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "caps":
		caps := registry.Detect(context.Background(), version, nil, 4)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(caps)
	case "hook":
		cmdHook(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: helmd run | caps | hook pre-tool-use --run <id> | version")
	os.Exit(2)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	server := fs.String("server", os.Getenv("HELM_SERVER"), "control plane URL")
	token := fs.String("token", os.Getenv("HELM_TOKEN"), "machine token (TODO(M0-D1): read from ~/.helm/credentials)")
	maxRuns := fs.Int("max-runs", 4, "max concurrent runs")
	_ = fs.Parse(args)
	if *server == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "helmd run: --server and --token are required")
		os.Exit(2)
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := conn.New(conn.Options{ServerURL: *server, Token: *token, Log: log}, &conn.MemSpool{})
	sup := supervisor.New(client, supervisor.Options{
		Executors: []executor.Executor{executor.NewCodex(), executor.NewClaude()},
		MaxRuns:   *maxRuns,
		WorkRoot:  filepath.Join(helmHome(), "work"),
		HookSock:  defaultSock(),
		Log:       log,
	})

	hs, err := hook.Listen(defaultSock(), sup)
	if err != nil {
		log.Error("policy socket", "err", err)
		os.Exit(1)
	}
	defer hs.Close()

	if err := client.PutCapabilities(ctx, registry.Detect(ctx, version, nil, *maxRuns)); err != nil {
		log.Warn("capabilities not reported yet; will retry on reconnect", "err", err)
	}
	log.Info("helmd started", "version", version, "server", *server)
	client.Run(ctx, sup)
}

func cmdHook(args []string) {
	if len(args) < 1 || args[0] != "pre-tool-use" {
		usage()
	}
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	runID := fs.String("run", "", "run id")
	sock := fs.String("sock", defaultSock(), "helmd policy socket")
	_ = fs.Parse(args[1:])
	hook.RunPreToolUse(os.Stdin, os.Stdout, *sock, *runID)
}
