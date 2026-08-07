package app

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

func TestNamedCLIForwardsChildFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		kind profileKind
	}{
		{name: "claude", args: []string{"--model", "claude-opus", "--verbose"}, kind: profileClaude},
		{name: "opencode", args: []string{"--model", "openai/gpt-5.6-sol"}, kind: profileOpenCode},
		{name: "codex", args: []string{"--model", "gpt-5.6-sol"}, kind: profileCodex},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got profile
			command, exitCode := newRootCommand(nil, io.Discard, io.Discard, func(p profile) (int, error) {
				got = p
				return 23, nil
			}, func(int) error { return nil })
			command.SetArgs(normalizeCLIArgs(append([]string{tt.name}, tt.args...)))
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if got.kind != tt.kind || got.command != tt.name || !reflect.DeepEqual(got.args, tt.args) {
				t.Fatalf("child = kind %v %q %#v", got.kind, got.command, got.args)
			}
			if *exitCode != 23 {
				t.Fatalf("exit code = %d", *exitCode)
			}
		})
	}
}

func TestExecutesProgramDirectly(t *testing.T) {
	var got profile
	command, _ := newRootCommand(nil, io.Discard, io.Discard, func(p profile) (int, error) {
		got = p
		return 0, nil
	}, func(int) error { return nil })
	command.SetArgs(normalizeCLIArgs([]string{"other-cli", "--child-flag", "value"}))
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.command != "other-cli" || !reflect.DeepEqual(got.args, []string{"--child-flag", "value"}) {
		t.Fatalf("child = %q %#v", got.command, got.args)
	}
}

func TestDirectExecutableEscapeAndCaseInsensitiveProfile(t *testing.T) {
	var got profile
	command, _ := newRootCommand(nil, io.Discard, io.Discard, func(p profile) (int, error) {
		got = p
		return 0, nil
	}, func(int) error { return nil })
	command.SetArgs(normalizeCLIArgs([]string{"--", "CODEX.EXE", "--flag"}))
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.kind != profileCodex || got.command != "CODEX.EXE" || !reflect.DeepEqual(got.args, []string{"--flag"}) {
		t.Fatalf("child = kind %v %q %#v", got.kind, got.command, got.args)
	}
}

func TestServeParsesPortFlags(t *testing.T) {
	for _, args := range [][]string{{"serve", "-p", "4312"}, {"serve", "--port", "4313"}} {
		var got int
		command, _ := newRootCommand(nil, io.Discard, io.Discard, func(profile) (int, error) {
			t.Fatal("execute called")
			return 0, nil
		}, func(port int) error {
			got = port
			return nil
		})
		command.SetArgs(normalizeCLIArgs(args))
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		want := 4312
		if args[1] == "--port" {
			want = 4313
		}
		if got != want {
			t.Fatalf("serve port = %d, want %d", got, want)
		}
	}
}

func TestServeRejectsInvalidPort(t *testing.T) {
	var stderr bytes.Buffer
	command, _ := newRootCommand(nil, io.Discard, &stderr, func(profile) (int, error) {
		t.Fatal("execute called")
		return 0, nil
	}, func(int) error {
		t.Fatal("serve called")
		return nil
	})
	command.SetArgs([]string{"serve", "--port", "0"})
	if err := command.Execute(); err == nil {
		t.Fatal("expected invalid port error")
	}
}
