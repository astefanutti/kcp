/*
Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package shard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/abiosoft/lineprefix"
	"github.com/fatih/color"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/kcp/cmd/test-server/helpers"
	kcpclientset "github.com/kcp-dev/kcp/pkg/client/clientset/versioned/cluster"
	"github.com/kcp-dev/kcp/test/e2e/framework"
)

//go:embed *.yaml
var embeddedResources embed.FS

type headWriter interface {
	io.Writer
	StopOut()
}

type Shard struct {
	name        string
	runtimeDir  string
	logFilePath string
	args        []string

	terminatedCh <-chan error
	writer       headWriter
}

func NewShard(name, runtimeDir, logFilePath string, args []string) *Shard {
	return &Shard{
		name:        name,
		runtimeDir:  runtimeDir,
		logFilePath: logFilePath,
		args:        args,
	}
}

// Start starts a kcp Shard server.
func (s *Shard) Start(ctx context.Context, quiet bool) error {
	logger := klog.FromContext(ctx).WithValues("shard", s.name)
	// setup color output
	prefix := strings.ToUpper(s.name)
	blue := color.New(color.BgBlue, color.FgHiWhite).SprintFunc()

	out := lineprefix.New(
		lineprefix.Prefix(blue(prefix)),
		lineprefix.Color(color.New(color.FgHiBlue)),
	)

	// write audit policy
	if err := os.MkdirAll(s.runtimeDir, 0755); err != nil {
		return err
	}
	bs, err := embeddedResources.ReadFile("audit-policy.yaml")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.runtimeDir, "audit-policy.yaml"), bs, 0644); err != nil {
		return err
	}

	// setup command
	commandLine := append(framework.StartKcpCommand(), framework.TestServerArgs()...)
	commandLine = append(commandLine, s.args...)
	commandLine = append(commandLine,
		"--audit-log-maxsize", "1024",
		"--audit-log-mode=batch",
		"--audit-log-batch-max-wait=1s",
		"--audit-log-batch-max-size=1000",
		"--audit-log-batch-buffer-size=10000",
		"--audit-log-batch-throttle-burst=15",
		"--audit-log-batch-throttle-enable=true",
		"--audit-log-batch-throttle-qps=10",
		"--audit-policy-file", filepath.Join(s.runtimeDir, "audit-policy.yaml"),
		"--virtual-workspaces-workspaces.authorization-cache.resync-period=1s",
	)
	fmt.Fprintf(out, "running: %v\n", strings.Join(commandLine, " "))

	cmd := exec.CommandContext(ctx, commandLine[0], commandLine[1:]...) //nolint:gosec
	if err := os.MkdirAll(filepath.Dir(s.logFilePath), 0755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(s.logFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	s.writer = helpers.NewHeadWriter(logFile, out)
	cmd.Stdout = s.writer
	cmd.Stdin = os.Stdin
	cmd.Stderr = s.writer

	if quiet {
		s.writer.StopOut()
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		if err := cmd.Process.Kill(); err != nil {
			logger.Error(err, "failed to kill process")
		}
	}()

	// Start a goroutine that will notify when the process has exited
	terminatedCh := make(chan error, 1)
	s.terminatedCh = terminatedCh
	go func() {
		terminatedCh <- cmd.Wait()
	}()

	// wait for admin.kubeconfig
	kubeconfigPath := filepath.Join(s.runtimeDir, "admin.kubeconfig")
	logger.Info("Waiting for kubeconfig", "path", kubeconfigPath)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled")
		case err := <-s.terminatedCh:
			var exitErr *exec.ExitError
			if err == nil {
				return fmt.Errorf("kcp Shard %s terminated unexpectedly with exit code 0", s.name)
			} else if errors.As(err, &exitErr) {
				return fmt.Errorf("kcp Shard %s terminated with exit code %d", s.name, exitErr.ExitCode())
			}
			return fmt.Errorf("kcp Shard %s terminated with unknown error: %w", s.name, err)
		default:
		}
		if _, err := os.Stat(kubeconfigPath); err == nil {
			break
		}
		time.Sleep(time.Millisecond * 1000)
	}
	logger.Info("Found kubeconfig", "path", kubeconfigPath)

	return nil
}

func (s *Shard) WaitForReady(ctx context.Context) (<-chan error, error) {
	// wait for readiness
	logger := klog.FromContext(ctx)
	logger.Info("Waiting for shard /readyz to succeed")
	lastSeenUnready := sets.NewString()
	for {
		time.Sleep(100 * time.Millisecond)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled")
		case err := <-s.terminatedCh:
			var exitErr *exec.ExitError
			if err == nil {
				return nil, fmt.Errorf("kcp Shard %s terminated unexpectedly with exit code 0", s.name)
			} else if errors.As(err, &exitErr) {
				return nil, fmt.Errorf("kcp Shard %s terminated with exit code %d", s.name, exitErr.ExitCode())
			}
			return nil, fmt.Errorf("kcp Shard %s terminated with unknown error: %w", s.name, err)
		default:
		}

		// intentionally load again every iteration because it can change
		kubeconfigPath := filepath.Join(s.runtimeDir, "admin.kubeconfig")
		configLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
			&clientcmd.ConfigOverrides{CurrentContext: "system:admin"},
		)
		config, err := configLoader.ClientConfig()
		if err != nil {
			continue
		}
		kcpClient, err := kcpclientset.NewForConfig(config)
		if err != nil {
			logger.Error(err, "Failed to create kcp client")
			continue
		}

		res := kcpClient.RESTClient().Get().AbsPath("/readyz").Do(ctx)
		if _, err := res.Raw(); err != nil {
			unreadyComponents := unreadyComponentsFromError(err)
			if !lastSeenUnready.Equal(unreadyComponents) {
				logger.V(3).Info("kcp shard not ready", "unreadyComponents", unreadyComponents.List())
				lastSeenUnready = unreadyComponents
			}
		}
		var rc int
		res.StatusCode(&rc)
		if rc == http.StatusOK {
			break
		}
	}
	if !logger.V(3).Enabled() {
		s.writer.StopOut()
	}

	prefix := strings.ToUpper(s.name)
	inverse := color.New(color.BgHiWhite, color.FgBlue).SprintFunc()
	successOut := lineprefix.New(
		lineprefix.Prefix(inverse(fmt.Sprintf(" %s ", prefix))),
		lineprefix.Color(color.New(color.FgHiWhite)),
	)

	fmt.Fprintf(successOut, "Shard is ready\n")
	return s.terminatedCh, nil
}

// there doesn't seem to be any simple way to get a metav1.Status from the Go client, so we get
// the content in a string-formatted error, unfortunately.
func unreadyComponentsFromError(err error) sets.String {
	innerErr := strings.TrimPrefix(strings.TrimSuffix(err.Error(), `") has prevented the request from succeeding`), `an error on the server ("`)
	unreadyComponents := sets.NewString()
	for _, line := range strings.Split(innerErr, `\n`) {
		if name := strings.TrimPrefix(strings.TrimSuffix(line, ` failed: reason withheld`), `[-]`); name != line {
			// NB: sometimes the error we get is truncated (server-side?) to something like: `\n[-]poststar") has prevented the request from succeeding`
			// In those cases, the `name` here is also truncated, but nothing we can do about that. For that reason, the list of components returned is
			// not durable and should not be parsed.
			unreadyComponents.Insert(name)
		}
	}
	return unreadyComponents
}
