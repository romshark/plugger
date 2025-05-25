package plugger_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/romshark/plugger"
)

func writeFile(t *testing.T, name, body string) {
	err := os.WriteFile(name, []byte(strings.TrimSpace(body)+"\n"), 0o777)
	if err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) string {
	c, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(data []byte) (int, error) {
	w.t.Log(string(data))
	return len(data), nil
}

func TestCallLocalGoPackage(t *testing.T) {
	// Absolute path to the plugger source directory (this package).
	_, thisFile, _, _ := runtime.Caller(0)
	pluggerDir := filepath.Dir(thisFile)

	// Create a tiny plugin module in temp dir.
	modDir := filepath.Join(t.TempDir(), "t1_module")
	t.Logf("mod-dir: %s", modDir)
	if err := os.MkdirAll(modDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(modDir); err != nil {
			t.Errorf("cleaning up mod dir: %v", err)
		}
	})

	// go.mod with replace lets the plugin import local "plugger"
	writeFile(t, filepath.Join(modDir, "go.mod"), fmt.Sprintf(`
		module exampleplugin
		go 1.24

		require github.com/romshark/plugger v0.0.0
		replace github.com/romshark/plugger => %s
	`, pluggerDir))

	// plugin main.go
	writeFile(t, filepath.Join(modDir, "main.go"),
		readFile(t, "testdata/t1_plugin_main.go.txt"))

	// Launch host and plugin.
	ctx := t.Context()
	h := plugger.NewHost()
	go func() {
		err := h.RunPlugin(ctx, modDir, testLogWriter{t: t})
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("RunPlugin error: %v", err)
		}
	}()

	testPlugin(t, h)

	// Cleanup.
	if err := h.Close(); err != nil {
		t.Fatalf("closing host: %v", err)
	}
}

func TestCallLocalGoFile(t *testing.T) {
	pkgDir := filepath.Join(t.TempDir(), "t1_package")
	if err := os.MkdirAll(pkgDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(pkgDir); err != nil {
			t.Errorf("cleaning up mod dir: %v", err)
		}
	})

	// plugin main.go
	mainFile := filepath.Join(pkgDir, "main.go")
	t.Logf("main-file: %s", mainFile)
	writeFile(t, mainFile,
		readFile(t, "testdata/t1_plugin_main.go.txt"))

	// Launch host and plugin.
	ctx := t.Context()
	h := plugger.NewHost()
	go func() {
		err := h.RunPlugin(ctx, mainFile, testLogWriter{t: t})
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("RunPlugin error: %v", err)
		}
	}()

	testPlugin(t, h)

	// Cleanup.
	if err := h.Close(); err != nil {
		t.Fatalf("closing host: %v", err)
	}
}

func TestCallBashScriptExecutable(t *testing.T) {
	// Launch host and plugin.
	ctx := t.Context()
	h := plugger.NewHost()
	go func() {
		err := h.RunPlugin(ctx, "testdata/test_executable.sh", testLogWriter{t: t})
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("RunPlugin error: %v", err)
		}
	}()

	testPlugin(t, h)

	// Cleanup.
	if err := h.Close(); err != nil {
		t.Fatalf("closing host: %v", err)
	}
}

func testPlugin(t *testing.T, h *plugger.Host) {
	// Happy path.
	type AddReq struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	type AddResp struct {
		Sum int `json:"sum"`
	}

	got, err := plugger.Call[AddReq, AddResp](
		t.Context(), h, "add", AddReq{A: 2, B: 3},
	)
	if err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	if got.Sum != 5 {
		t.Fatalf("unexpected result: %d", got.Sum)
	}

	// Unknown method.
	_, err = plugger.Call[AddReq, AddResp](
		t.Context(), h, "does_not_exist", AddReq{A: 1, B: 1},
	)
	if err == nil {
		t.Fatalf("expected error for unknown method")
	}
	if msg := err.Error(); msg != "unknown method: does_not_exist" {
		t.Fatalf("unexpected error message: %q", msg)
	}

	// Malformed payload (string where an int is expected).
	type MalformedReq struct {
		A string `json:"a"`
		B int    `json:"b"`
	}

	_, err = plugger.Call[MalformedReq, AddResp](
		t.Context(), h, "add", MalformedReq{A: "2", B: 3},
	)
	if err == nil {
		t.Fatalf("expected error from bad payload")
	}
}

func TestMain(m *testing.M) {
	// Ensure `go test` can resolve the local module path in editable mode
	// when running from an IDE without 'go list -m'.
	// Ensure the 'go' tool is present for the test run.
	if _, err := exec.LookPath("go"); err != nil {
		panic("go toolchain required for plugger tests")
	}
	os.Exit(m.Run())
}
