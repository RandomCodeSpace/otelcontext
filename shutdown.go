package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type shutdownStep struct {
	name string
	run  func(context.Context) error
}

type shutdownStepResult struct {
	Name        string    `json:"name"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Error       string    `json:"error,omitempty"`
}

type shutdownReport struct {
	StartedAt   time.Time            `json:"started_at"`
	CompletedAt time.Time            `json:"completed_at"`
	Steps       []shutdownStepResult `json:"steps"`
}

const gracefulShutdownTimeout = 30 * time.Second

func gracefulStopAdmission(ctx context.Context, graceful, force func()) error {
	done := make(chan struct{})
	go func() {
		graceful()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		force()
		<-done
		return ctx.Err()
	}
}

// executeShutdown runs the owning shutdown barriers in order. Each owner is
// bounded by ctx even when its legacy Stop method does not accept a context.
// An owner error is retained while later safe stops continue; a spent
// deadline stops the sequence because the process can no longer prove that a
// writer is quiescent.
func executeShutdown(ctx context.Context, steps []shutdownStep) (shutdownReport, error) {
	report := shutdownReport{
		StartedAt: time.Now().UTC(),
		Steps:     make([]shutdownStepResult, 0, len(steps)),
	}
	var failures []error

	for _, step := range steps {
		result := shutdownStepResult{Name: step.name, StartedAt: time.Now().UTC()}
		err := runShutdownStep(ctx, step)
		result.CompletedAt = time.Now().UTC()
		if err != nil {
			result.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", step.name, err))
		}
		report.Steps = append(report.Steps, result)
		if ctx.Err() != nil {
			break
		}
	}

	report.CompletedAt = time.Now().UTC()
	return report, errors.Join(failures...)
}

func runShutdownStep(ctx context.Context, step shutdownStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if step.run == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("panic: %v", recovered)
			}
		}()
		done <- step.run(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
