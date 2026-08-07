package app

import (
	"fmt"
	"io"
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
	switch strings.ToLower(filepath.Base(command)) {
	case "claude", "claude.exe":
		return claudeProfile(command, args)
	case "opencode", "opencode.exe":
		return openCodeProfile(command, args)
	case "codex", "codex.exe":
		return codexProfile(command, args)
	default:
		return genericProfile(command, args)
	}
}
