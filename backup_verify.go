package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
)

const restoreReadyTimeout = 60 * time.Second

type boundedProcessLog struct {
	mu        sync.Mutex
	data      []byte
	remaining int
}

func (b *boundedProcessLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(data)
	if b.remaining <= 0 {
		return original, nil
	}
	if len(data) > b.remaining {
		data = data[:b.remaining]
	}
	b.data = append(b.data, data...)
	b.remaining -= len(data)
	return original, nil
}

func (b *boundedProcessLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

func verifyRestoredCandidate(ctx context.Context, cfg *config.Config) (float64, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve restore candidate executable: %w", err)
	}
	started := time.Now()
	log := &boundedProcessLog{remaining: 256 << 10}
	command := exec.CommandContext(ctx, executable) // #nosec G204 -- exact running binary, no operator command string.
	command.Env = restoreCandidateEnvironment(os.Environ())
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start restored candidate: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	readyClient, readyURL, err := restoredReadinessClient(cfg)
	if err != nil {
		stopRestoreCandidate(command, done)
		return 0, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, restoreReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, requestErr := http.NewRequestWithContext(readyCtx, http.MethodGet, readyURL, nil)
		if requestErr != nil {
			stopRestoreCandidate(command, done)
			return 0, requestErr
		}
		response, requestErr := readyClient.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				readySeconds := time.Since(started).Seconds()
				if err := command.Process.Signal(os.Interrupt); err != nil {
					stopRestoreCandidate(command, done)
					return 0, fmt.Errorf("stop restored candidate after readiness: %w", err)
				}
				select {
				case waitErr := <-done:
					if waitErr != nil {
						return 0, fmt.Errorf("restored candidate exited after readiness: %w\n%s", waitErr, log.String())
					}
					return readySeconds, nil
				case <-time.After(40 * time.Second):
					_ = command.Process.Kill()
					<-done
					return 0, errors.New("restored candidate did not complete graceful shutdown within 40 seconds")
				}
			}
		}
		select {
		case waitErr := <-done:
			return 0, fmt.Errorf("restored candidate exited before readiness: %w\n%s", waitErr, log.String())
		case <-readyCtx.Done():
			stopRestoreCandidate(command, done)
			return 0, fmt.Errorf("restored candidate readiness timeout: %w\n%s", readyCtx.Err(), log.String())
		case <-ticker.C:
		}
	}
}

func restoreCandidateEnvironment(current []string) []string {
	overrides := map[string]string{
		"AGGREGATE_ALLOW_REBUILD": "false",
		"DB_AUTOMIGRATE":          "false",
	}
	result := make([]string, 0, len(current)+len(overrides))
	for _, item := range current {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, item)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func restoredReadinessClient(cfg *config.Config) (*http.Client, string, error) {
	transport := &http.Transport{Proxy: nil}
	scheme := "http"
	port, err := strconv.Atoi(cfg.HTTPPort)
	if err != nil || port < 1 || port > 65535 {
		return nil, "", fmt.Errorf("invalid HTTP_PORT %q for readiness proof", cfg.HTTPPort)
	}
	if cfg.TLSEnabled() {
		scheme = "https"
		certPath := cfg.TLSCertFile
		if certPath == "" {
			certPath = filepath.Join(cfg.TLSCacheDir, "cert.pem")
		}
		certificatePEM, err := os.ReadFile(certPath) // #nosec G304 -- resolved configured certificate used by the child.
		if err != nil {
			return nil, "", fmt.Errorf("read restored candidate certificate: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(certificatePEM) {
			return nil, "", errors.New("restored candidate certificate file contains no certificate")
		}
		serverName := "127.0.0.1"
		if block, _ := pem.Decode(certificatePEM); block != nil {
			if certificate, parseErr := x509.ParseCertificate(block.Bytes); parseErr == nil && len(certificate.IPAddresses) == 0 && len(certificate.DNSNames) > 0 {
				serverName = certificate.DNSNames[0]
			}
		}
		transport.TLSClientConfig = &tls.Config{ // #nosec G402 -- verification is enabled against the configured certificate.
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: serverName,
		}
	}
	return &http.Client{Timeout: time.Second, Transport: transport}, scheme + "://127.0.0.1:" + strconv.Itoa(port) + "/ready", nil
}

func stopRestoreCandidate(command *exec.Cmd, done <-chan error) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Signal(os.Interrupt)
	select {
	case <-done:
		return
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		<-done
	}
}
