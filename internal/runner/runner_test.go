package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestRunPreservesStreamsArgumentsEnvironmentAndExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), Options{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestRunnerHelper", "--", "--child-flag", "value"},
		Env:     append(os.Environ(), "PXPIPE_RUNNER_HELPER=1"),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 23 {
		t.Fatalf("exit code = %d", code)
	}
	if stdout.String() != "--child-flag,value\n" || stderr.String() != "helper stderr\n" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunnerHelper(t *testing.T) {
	if os.Getenv("PXPIPE_RUNNER_HELPER") != "1" {
		return
	}
	fmt.Println(strings.Join(os.Args[len(os.Args)-2:], ","))
	fmt.Fprintln(os.Stderr, "helper stderr")
	os.Exit(23)
}

func TestEnvironment(t *testing.T) {
	got := Environment(
		[]string{"KEEP=yes", "REPLACE=old", "REMOVE=yes", "lower=keep"},
		map[string]string{"REPLACE": "new", "ADD": "yes"},
		[]string{"REMOVE"},
	)
	want := map[string]string{"KEEP": "yes", "REPLACE": "new", "ADD": "yes", "lower": "keep"}
	actual := make(map[string]string)
	for _, item := range got {
		key, value, _ := strings.Cut(item, "=")
		actual[key] = value
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("environment = %#v", actual)
	}
}
