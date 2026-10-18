// helmd is Helm's per-machine daemon ("the hand").
//
//	helmd register --server <url> --code <code> [--name <machine>]
//	                              exchange a one-time code for a machine token (~/.helm/credentials)
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/helm/helm/daemon/internal/conn"
	"github.com/helm/helm/daemon/internal/creds"
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
	case "register":
		cmdRegister(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "usage: helmd register --server <url> --code <code> | run | caps | hook pre-tool-use --run <id> | version")
	os.Exit(2)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	server := fs.String("server", os.Getenv("HELM_SERVER"), "control plane URL (default: from credentials)")
	maxRuns := fs.Int("max-runs", 4, "max concurrent runs")
	labels := fs.String("labels", os.Getenv("HELM_LABELS"), "comma-separated machine labels, e.g. gpu,dingtalk")
	_ = fs.Parse(args)
	// The token is read from the credentials file (or HELM_TOKEN for tests), never from argv.
	token := os.Getenv("HELM_TOKEN")
	if c, err := creds.Load(helmHome()); err == nil {
		if *server == "" {
			*server = c.Server
		}
		if token == "" {
			token = c.Token
		}
	} else if token == "" {
		fmt.Fprintln(os.Stderr, "helmd run: not registered:", err, "\nrun: helmd register --server <url> --code <code>")
		os.Exit(2)
	}
	if *server == "" {
		fmt.Fprintln(os.Stderr, "helmd run: no server URL")
		os.Exit(2)
	}
	var labelList []string
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labelList = append(labelList, l)
		}
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := conn.New(conn.Options{ServerURL: *server, Token: token, Log: log}, &conn.MemSpool{})
	sup := supervisor.New(client, supervisor.Options{
		Executors: []executor.Executor{executor.NewCodex(lookPath("codex")), executor.NewClaude(lookPath("claude"), selfPath(), filepath.Join(helmHome(), "run"))},
		MaxRuns:   *maxRuns,
		WorkRoot:  filepath.Join(helmHome(), "work"),
		HookSock:  defaultSock(),
		Log:       log,
		Load:      registry.NewLoadSampler(),
	})

	hs, err := hook.Listen(defaultSock(), sup)
	if err != nil {
		log.Error("policy socket", "err", err)
		os.Exit(1)
	}
	defer hs.Close()

	if err := client.PutCapabilities(ctx, registry.Detect(ctx, version, labelList, *maxRuns)); err != nil {
		log.Warn("capabilities not reported yet; will retry on reconnect", "err", err)
	}
	log.Info("helmd started", "version", version, "server", *server)
	client.Run(ctx, sup)
}

func lookPath(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

func selfPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "helmd"
}

func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	server := fs.String("server", "", "control plane URL, e.g. https://helm.<tailnet>.ts.net")
	code := fs.String("code", "", "one-time registration code from the board (机器 → 生成注册码)")
	host, _ := os.Hostname()
	name := fs.String("name", host, "machine name shown on the board")
	_ = fs.Parse(args)
	if *server == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "helmd register: --server and --code are required")
		os.Exit(2)
	}
	c, err := creds.Register(context.Background(), *server, *code, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := creds.Save(helmHome(), c); err != nil {
		fmt.Fprintln(os.Stderr, "save credentials:", err)
		os.Exit(1)
	}
	fmt.Printf("registered %s as %s; credentials saved to %s\n", *name, c.MachineID, creds.Path(helmHome()))
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
