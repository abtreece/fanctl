// Package gpu reads NVIDIA GPU temperatures via nvidia-smi. It exists so the
// fan curve can account for passively-cooled datacenter GPUs (e.g. a Tesla T4),
// whose temperature is not exposed through the BMC's IPMI sensors and which rely
// entirely on chassis airflow.
package gpu

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes a command and returns its combined output. Injected for tests.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the default Runner: it shells out to the real binary.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Reader queries GPU temperatures through nvidia-smi.
type Reader struct {
	bin string
	run Runner
}

// New returns a Reader. bin defaults to "nvidia-smi"; run defaults to ExecRunner.
func New(bin string, run Runner) *Reader {
	if bin == "" {
		bin = "nvidia-smi"
	}
	if run == nil {
		run = ExecRunner
	}
	return &Reader{bin: bin, run: run}
}

// Temp is a single GPU's temperature reading.
type Temp struct {
	Index   int
	Celsius int
}

// Temperatures returns the temperature of each GPU. It returns an error if the
// command fails or yields no parseable reading, so callers can fail safe.
func (r *Reader) Temperatures(ctx context.Context) ([]Temp, error) {
	out, err := r.run(ctx, r.bin, "--query-gpu=temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	var temps []Temp
	for i, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		v, err := strconv.Atoi(ln)
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: unparseable temperature %q", ln)
		}
		temps = append(temps, Temp{Index: i, Celsius: v})
	}
	if len(temps) == 0 {
		return nil, fmt.Errorf("nvidia-smi: no GPU temperatures reported")
	}
	return temps, nil
}

// Max returns the hottest GPU temperature and whether any were present.
func Max(temps []Temp) (int, bool) {
	best, ok := 0, false
	for _, t := range temps {
		if !ok || t.Celsius > best {
			best, ok = t.Celsius, true
		}
	}
	return best, ok
}
