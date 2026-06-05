package sandbox

import (
	"context"
	"reflect"
	"testing"
)

func TestDockerCommandsBuildExpectedArgv(t *testing.T) {
	var calls [][]string
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}}
	ctx := context.Background()
	_ = d.CreateNetwork(ctx, "q-int", true)
	_ = d.CreateVolume(ctx, "q-work")
	_ = d.Remove(ctx, "q-agent")
	want := [][]string{
		{"docker", "network", "create", "--internal", "q-int"},
		{"docker", "volume", "create", "q-work"},
		{"docker", "rm", "-f", "q-agent"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
}
