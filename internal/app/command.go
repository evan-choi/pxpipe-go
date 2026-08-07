package app

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

type executeProfile func(profile) (int, error)

func newRootCommand(stdin io.Reader, stdout, stderr io.Writer, execute executeProfile) (*cobra.Command, *int) {
	exitCode := new(int)
	root := &cobra.Command{
		Use:           "pxpipe",
		Short:         "Run AI CLIs through pxpipe",
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
	root.AddCommand(&cobra.Command{
		Use:                "claude [args...]",
		Short:              "Run Claude Code through pxpipe",
		DisableFlagParsing: true,
		Run: func(_ *cobra.Command, args []string) {
			run(claudeProfile("claude", args))
		},
	})
	root.AddCommand(&cobra.Command{
		Use:                "run <executable> [args...]",
		Short:              "Run another executable through the current proxy profile",
		DisableFlagParsing: true,
		Args:               cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			run(claudeProfile(args[0], args[1:]))
		},
	})
	return root, exitCode
}
