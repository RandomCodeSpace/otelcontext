package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxCommandOutput = 64 << 10

type execRunner struct{}

func defaultRunner(runner CommandRunner) CommandRunner {
	if runner != nil {
		return runner
	}
	return execRunner{}
}

func (execRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	started := time.Now()
	path, err := exec.LookPath(command.Name)
	if err != nil {
		return CommandResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("required client %q is missing from PATH", command.Name)
	}
	cmd := exec.CommandContext(ctx, path, command.Args...) // #nosec G204 -- fixed adapter command with parsed config arguments.
	cmd.Env = append(os.Environ(), command.Env...)
	var stdin *os.File
	if command.StdinPath != "" {
		stdin, err = os.Open(command.StdinPath) // #nosec G304 -- verified bundle artifact.
		if err != nil {
			return CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
		}
		defer func() { _ = stdin.Close() }()
		cmd.Stdin = stdin
	}
	var stdoutFile *os.File
	var stdout bytes.Buffer
	if command.StdoutPath != "" {
		stdoutFile, err = os.OpenFile(command.StdoutPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- fresh staging path.
		if err != nil {
			return CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
		}
		defer func() { _ = stdoutFile.Close() }()
		cmd.Stdout = stdoutFile
	} else {
		cmd.Stdout = &limitedBuffer{buffer: &stdout, remaining: maxCommandOutput}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buffer: &stderr, remaining: maxCommandOutput}
	err = cmd.Run()
	if stdoutFile != nil {
		if syncErr := stdoutFile.Sync(); err == nil && syncErr != nil {
			err = syncErr
		}
		if closeErr := stdoutFile.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" && err == nil {
		output = strings.TrimSpace(stderr.String())
	}
	result := CommandResult{
		Output:   sanitizeCommandText(output, command.Redactions),
		ExitCode: exitCode(err),
		Duration: time.Since(started),
	}
	if err != nil {
		detail := sanitizeCommandText(strings.TrimSpace(stderr.String()), command.Redactions)
		if detail == "" {
			detail = sanitizeCommandText(err.Error(), command.Redactions)
		}
		if command.StdoutPath != "" {
			_ = os.Remove(command.StdoutPath)
		}
		return result, fmt.Errorf("%s failed with exit %d: %s", command.Display, result.ExitCode, detail)
	}
	return result, nil
}

type limitedBuffer struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if w.remaining <= 0 {
		return original, nil
	}
	if len(data) > w.remaining {
		data = data[:w.remaining]
	}
	_, _ = w.buffer.Write(data)
	w.remaining -= len(data)
	return original, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func sanitizeCommandText(value string, redactions []string) string {
	for _, secret := range redactions {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func runRecorded(ctx context.Context, runner CommandRunner, step string, command Command, records *[]CommandRecord) (CommandResult, error) {
	result, err := runner.Run(ctx, command)
	record := CommandRecord{
		Step:       step,
		Command:    command.Display,
		DurationMS: result.Duration.Milliseconds(),
		ExitCode:   result.ExitCode,
		Output:     result.Output,
	}
	*records = append(*records, record)
	return result, err
}

func requireVersion(ctx context.Context, runner CommandRunner, name string, args []string, display, want string, records *[]CommandRecord, redactions ...string) error {
	result, err := runRecorded(ctx, runner, "client-version", Command{
		Name:       name,
		Args:       args,
		Display:    display,
		Redactions: redactions,
	}, records)
	if err != nil {
		return err
	}
	if !strings.Contains(result.Output, want) {
		return fmt.Errorf("%s has wrong version: got %q, require output containing %q", name, result.Output, want)
	}
	return nil
}
