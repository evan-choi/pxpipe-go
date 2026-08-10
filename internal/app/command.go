package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type executeProfile func(profile) (int, error)
type executeServer func(port int) error

func newRootCommand(stdin io.Reader, stdout, stderr io.Writer, execute executeProfile, serve executeServer) (*cobra.Command, *int) {
	exitCode := new(int)
	root := &cobra.Command{
		Use:           "pxpipe <executable> [args...]",
		Short:         "Run AI CLIs through pxpipe or serve the proxy directly",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true

	run := func(p profile) {
		code, err := execute(p)
		*exitCode = code
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
	}
	execCommand := &cobra.Command{
		Use:                "exec <executable> [args...]",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			run(profileForExecutable(args[0], args[1:]))
		},
	}
	root.AddCommand(execCommand)
	port := defaultServePort
	serveCommand := &cobra.Command{
		Use:   "serve",
		Short: "Run a loopback reverse proxy",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if port < 1 || port > 65535 {
				return fmt.Errorf("port must be between 1 and 65535")
			}
			return nil
		},
		Run: func(_ *cobra.Command, _ []string) {
			if err := serve(port); err != nil {
				*exitCode = 1
				fmt.Fprintln(stderr, err)
			}
		},
	}
	serveCommand.Flags().IntVarP(&port, "port", "p", defaultServePort, "loopback port")
	root.AddCommand(serveCommand)
	return root, exitCode
}

func normalizeCLIArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	if args[0] == "--" && len(args) > 1 {
		return append([]string{"exec"}, args[1:]...)
	}
	switch args[0] {
	case "serve", "help", "-h", "--help":
		return append([]string(nil), args...)
	default:
		return append([]string{"exec"}, args...)
	}
}

func profileForExecutable(command string, args []string) profile {
	name := filepath.Base(command)
	switch {
	case strings.EqualFold(name, "claude.app"):
		return claudeDesktopProfile(resolveClaudeDesktopExecutable(command), args)
	case strings.EqualFold(name, "claude"), strings.EqualFold(name, "claude.exe"):
		if isClaudeDesktopExecutable(command) {
			return claudeDesktopProfile(command, args)
		}
		return claudeProfile(command, args)
	case strings.EqualFold(name, "opencode"), strings.EqualFold(name, "opencode.exe"):
		return openCodeProfile(command, args)
	case strings.EqualFold(name, "codex"), strings.EqualFold(name, "codex.exe"):
		return codexProfile(command, args)
	default:
		return genericProfile(command, args)
	}
}

func resolveClaudeDesktopExecutable(command string) string {
	const executable = "Contents/MacOS/Claude"
	if filepath.Base(command) != command {
		return filepath.Join(command, executable)
	}
	var applications []string
	if home, err := os.UserHomeDir(); err == nil {
		applications = append(applications, filepath.Join(home, "Applications", "Claude.app"))
	}
	applications = append(applications, "/Applications/Claude.app")
	for _, application := range applications {
		path := filepath.Join(application, executable)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return filepath.Join(applications[len(applications)-1], executable)
}

func isClaudeDesktopExecutable(command string) bool {
	if !strings.EqualFold(filepath.Base(command), "Claude") {
		return false
	}
	dir := filepath.Dir(command)
	for _, name := range []string{"MacOS", "Contents", "Claude.app"} {
		if !strings.EqualFold(filepath.Base(dir), name) {
			return false
		}
		dir = filepath.Dir(dir)
	}
	return true
}
