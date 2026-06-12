package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/alecthomas/kong"

	"github.com/abtreece/fanctl/internal/config"
)

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// CLI is the root (daemon) command surface. The doctor/probe/install
// subcommands are dispatched from os.Args before kong parses, mirroring
// runnerctl, so the daemon's flag surface stays clean.
type CLI struct {
	Config   string           `name:"config" help:"Path to config file" default:"${config_path}" type:"path"`
	DryRun   bool             `name:"dry-run" help:"Compute fan levels but do not write to the BMC"`
	Once     bool             `name:"once" help:"Run a single control iteration and exit"`
	LogLevel string           `name:"log-level" help:"Log level" enum:"debug,info,warn,error" default:"info"`
	Version  kong.VersionFlag `name:"version" short:"V" help:"Print version and exit"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "doctor":
			os.Exit(runDoctor(os.Args[2:]))
		case "probe":
			os.Exit(runProbe(os.Args[2:]))
		case "install":
			os.Exit(runInstall(os.Args[2:]))
		case "run":
			// `fanctl run [flags]` is an explicit alias for the bare daemon.
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}

	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("fanctl"),
		kong.Description("Temperature-based BMC fan controller for Dell PowerEdge (IPMI).\n\nSubcommands: doctor (preflight checks), probe (confirm manual fan control works), install (systemd unit + config)."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{
			"version":     fmt.Sprintf("%s (%s, %s)", version, commit, date),
			"config_path": config.DefaultPath,
		},
	)

	log := newLogger(cli.LogLevel)

	cfg := config.Default(cli.Config)
	if err := config.LoadFile(cli.Config, cfg); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Error("failed to load config", "path", cli.Config, "err", err)
			os.Exit(1)
		}
		log.Warn("config not found, using built-in defaults", "path", cli.Config)
	}
	if err := config.Validate(cfg); err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	ctx.FatalIfErrorf(runDaemon(log, cfg, cli.DryRun, cli.Once))
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
