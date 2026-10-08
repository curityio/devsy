package agent

import (
	"errors"
	"os"
	"testing"
)

func TestFinishBuild_ImageEnvWinsOverCredentialsCleanup(t *testing.T) {
	t.Setenv("PATH", "/builder/bin")

	var calls []string
	cleanup := func() {
		calls = append(calls, "cleanup")
		_ = os.Setenv("PATH", "/builder/bin")
	}
	applyImageEnv := func() error {
		calls = append(calls, "apply")
		return os.Setenv("PATH", "/image/bin")
	}

	if err := finishBuild(cleanup, nil, applyImageEnv); err != nil {
		t.Fatalf("finishBuild: %v", err)
	}
	if got := os.Getenv("PATH"); got != "/image/bin" {
		t.Errorf("PATH = %q, want the image's %q", got, "/image/bin")
	}
	if len(calls) != 2 || calls[0] != "cleanup" || calls[1] != "apply" {
		t.Errorf("calls = %v, want [cleanup apply]", calls)
	}
}

func TestFinishBuild_FailedBuildCleansUpAndSkipsImageEnv(t *testing.T) {
	buildErr := errors.New("build failed")
	cleanups := 0
	applied := false

	err := finishBuild(
		func() { cleanups++ },
		buildErr,
		func() error { applied = true; return nil },
	)

	if !errors.Is(err, buildErr) {
		t.Errorf("err = %v, want %v", err, buildErr)
	}
	if cleanups != 1 {
		t.Errorf("cleanup ran %d times, want 1", cleanups)
	}
	if applied {
		t.Error("image env applied after a failed build")
	}
}

func TestFinishBuild_NoCleanup(t *testing.T) {
	applied := false

	if err := finishBuild(nil, nil, func() error { applied = true; return nil }); err != nil {
		t.Fatalf("finishBuild: %v", err)
	}
	if !applied {
		t.Error("image env not applied")
	}
}
