package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

//go:embed embed/fanctl.service
var unitFile string

//go:embed embed/config.example.yaml
var exampleConfig string

const (
	unitPath       = "/etc/systemd/system/fanctl.service"
	installCfgDir  = "/etc/fanctl"
	installCfgPath = "/etc/fanctl/config.yaml"
)

// runInstall writes the systemd unit and an initial config, then (unless
// --no-enable) reloads systemd and enables the service. Requires root.
func runInstall(args []string) int {
	fs := flag.NewFlagSet("fanctl install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	noEnable := fs.Bool("no-enable", false, "Write files but do not enable/start the service")
	force := fs.Bool("force", false, "Overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "install: must run as root (writes /etc/systemd/system and /etc/fanctl)")
		return 1
	}

	if err := os.MkdirAll(installCfgDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install: create %s: %v\n", installCfgDir, err)
		return 1
	}

	// Never clobber an existing operator config unless forced.
	if _, err := os.Stat(installCfgPath); errors.Is(err, os.ErrNotExist) || *force {
		if err := os.WriteFile(installCfgPath, []byte(exampleConfig), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "install: write %s: %v\n", installCfgPath, err)
			return 1
		}
		fmt.Printf("wrote %s\n", installCfgPath)
	} else {
		fmt.Printf("kept existing %s (use --force to overwrite)\n", installCfgPath)
	}
	// Always refresh the example alongside it.
	_ = os.WriteFile(filepath.Join(installCfgDir, "config.example.yaml"), []byte(exampleConfig), 0o644)

	if err := os.WriteFile(unitPath, []byte(unitFile), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "install: write %s: %v\n", unitPath, err)
		return 1
	}
	fmt.Printf("wrote %s\n", unitPath)

	if *noEnable {
		fmt.Println("skipping enable (--no-enable); run `systemctl enable --now fanctl` when ready")
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "fanctl.service"},
	} {
		if out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "install: %v: %v\n%s", c, err, out)
			return 1
		}
	}
	fmt.Println("enabled and started fanctl.service")
	fmt.Println("\nnext: review /etc/fanctl/config.yaml, then `fanctl doctor` and `fanctl probe`")
	return 0
}
