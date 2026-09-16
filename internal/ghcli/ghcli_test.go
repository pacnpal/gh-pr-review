package ghcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("GHCLI_TEST_HELPER") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

func TestGraphQLContextCancelsGhProcess(t *testing.T) {
	dir := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	gh := filepath.Join(dir, name)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	contents, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(gh, contents, 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("GHCLI_TEST_HELPER", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = (&Client{}).GraphQLContext(ctx, "query { viewer { login } }", nil, &struct{}{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}
