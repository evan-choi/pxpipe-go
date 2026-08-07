package app

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

func TestClaudeForwardsChildFlags(t *testing.T) {
	var got profile
	command, exitCode := newRootCommand(nil, io.Discard, io.Discard, func(p profile) (int, error) {
		got = p
		return 23, nil
	})
	command.SetArgs([]string{"claude", "--model", "claude-opus", "--verbose"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.command != "claude" || !reflect.DeepEqual(got.args, []string{"--model", "claude-opus", "--verbose"}) {
		t.Fatalf("child = %q %#v", got.command, got.args)
	}
	if *exitCode != 23 {
		t.Fatalf("exit code = %d", *exitCode)
	}
}

func TestRunExecutesNamedProgramDirectly(t *testing.T) {
	var got profile
	command, _ := newRootCommand(nil, io.Discard, io.Discard, func(p profile) (int, error) {
		got = p
		return 0, nil
	})
	command.SetArgs([]string{"run", "other-cli", "--child-flag", "value"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.command != "other-cli" || !reflect.DeepEqual(got.args, []string{"--child-flag", "value"}) {
		t.Fatalf("child = %q %#v", got.command, got.args)
	}
}

func TestRunRequiresExecutable(t *testing.T) {
	var stderr bytes.Buffer
	command, _ := newRootCommand(nil, io.Discard, &stderr, func(profile) (int, error) {
		t.Fatal("execute called")
		return 0, nil
	})
	command.SetArgs([]string{"run"})
	if err := command.Execute(); err == nil {
		t.Fatal("expected argument validation error")
	}
}
