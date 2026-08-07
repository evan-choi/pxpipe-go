package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
)

// Options configures a directly executed child process.
type Options struct {
	Command string
	Args    []string
	Env     []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run directly executes a child, inherits its terminal streams, forwards
// process signals, and returns the child's exit status.
func Run(ctx context.Context, options Options) (int, error) {
	command := exec.Command(options.Command, options.Args...)
	command.Env = options.Env
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	if err := command.Start(); err != nil {
		if errorsIsExecutable(err) {
			return 127, fmt.Errorf("execute %s: %w", options.Command, err)
		}
		return 1, fmt.Errorf("start %s: %w", options.Command, err)
	}

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, forwardedSignals()...)
	defer signal.Stop(signals)

	for {
		select {
		case err := <-wait:
			if err == nil {
				return 0, nil
			}
			if exitError, ok := err.(*exec.ExitError); ok {
				return childExitCode(exitError), nil
			}
			return 1, fmt.Errorf("wait for %s: %w", options.Command, err)
		case processSignal := <-signals:
			_ = command.Process.Signal(processSignal)
		case <-ctx.Done():
			_ = command.Process.Kill()
			<-wait
			return 1, ctx.Err()
		}
	}
}

// Environment applies exact-name removals and replacements to a base env.
func Environment(base []string, set map[string]string, unset []string) []string {
	removed := make(map[string]struct{}, len(set)+len(unset))
	type replacement struct{ key, value string }
	replacements := make(map[string]replacement, len(set))
	for key := range set {
		canonical := environmentKey(key)
		removed[canonical] = struct{}{}
		replacements[canonical] = replacement{key: key, value: set[key]}
	}
	for _, key := range unset {
		removed[environmentKey(key)] = struct{}{}
	}
	environment := make([]string, 0, len(base)+len(replacements))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, found := removed[environmentKey(key)]; !found {
			environment = append(environment, item)
		}
	}
	for _, replacement := range replacements {
		environment = append(environment, replacement.key+"="+replacement.value)
	}
	return environment
}

func environmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func errorsIsExecutable(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}
