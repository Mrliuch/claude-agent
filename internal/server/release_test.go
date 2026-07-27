package server

import "testing"

func boolPtr(value bool) *bool { return &value }

func TestReleaseBuildArgsAllowOnlyWhitelistedOptions(t *testing.T) {
	args, err := releaseBuildArgs(releaseRunRequest{
		Image:       "registry.example.com/team/api:latest",
		BuildTarget: "runtime",
		BuildArgs:   []string{"VERSION=1.2.3", "FEATURE_FLAG=true"},
		Pull:        boolPtr(false),
		NoCache:     true,
	}, "Dockerfile")
	if err != nil {
		t.Fatalf("releaseBuildArgs error: %v", err)
	}
	want := []string{"build", "--no-cache", "--target", "runtime", "--build-arg", "VERSION=1.2.3", "--build-arg", "FEATURE_FLAG=true", "-f", "Dockerfile", "-t", "registry.example.com/team/api:latest", "."}
	if len(args) != len(want) {
		t.Fatalf("unexpected args: %#v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, args[i], want[i])
		}
	}
}

func TestReleaseBuildArgsRejectInvalidBuildArg(t *testing.T) {
	_, err := releaseBuildArgs(releaseRunRequest{Image: "registry.example.com/team/api:latest", BuildArgs: []string{"--network=host"}}, "Dockerfile")
	if err == nil {
		t.Fatal("expected invalid build argument to be rejected")
	}
}
