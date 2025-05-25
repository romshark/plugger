package plugger_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

type logWriter struct {
	t         *testing.T
	lock      sync.Mutex
	listeners []chan<- string
}

var _ io.WriteCloser = new(logWriter)

func newLogWriter(t *testing.T) *logWriter { return &logWriter{t: t} }

func (w *logWriter) AddReader(c chan<- string) {
	w.lock.Lock()
	w.listeners = append(w.listeners, c)
	w.lock.Unlock()
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.lock.Lock()
	m := string(b)
	for _, l := range w.listeners {
		l <- m
	}
	w.t.Log(m)
	w.lock.Unlock()
	return len(b), nil
}

func (w *logWriter) Close() error {
	w.lock.Lock()
	for _, l := range w.listeners {
		close(l)
	}
	w.lock.Unlock()
	return nil
}

func TestCallLocalGoPackage(t *testing.T) {
	h, _ := launchLocalModule(t, t.Context(), "test_happy",
		"testdata/t1_plugin_main.go.txt")
	testPlugin(t, h)
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
		err := h.RunPlugin(ctx, mainFile, newLogWriter(t))
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
		err := h.RunPlugin(ctx, "testdata/test_executable.sh", newLogWriter(t))
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

func TestCancelRequest(t *testing.T) {
	h, logWriter := launchLocalModule(t, t.Context(), "test_cancel",
		"testdata/tcancel_plugin_main.go.txt")

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel the call immediately.
	_, err := plugger.Call[AddReq, AddResp](
		ctx, h, "add", AddReq{A: 1, B: 1},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected err context.Canceled; received: %v", err)
	}

	c := make(chan string, 2)
	logWriter.AddReader(c)

	if m := <-c; m != "request received\n" {
		t.Fatalf("unexpected log: %q", m)
	}
	if m := <-c; m != "request canceled\n" {
		t.Fatalf("unexpected log: %q", m)
	}
}

func TestMalformedResponse(t *testing.T) {
	h, _ := launchLocalModule(t, t.Context(), "test_malformed_response",
		"testdata/tinvalresp_plugin_main.go.txt")

	_, err := plugger.Call[AddReq, AddResp](
		t.Context(), h, "malformed_response", AddReq{A: 1, B: 1},
	)
	const expectErrMsg = "malformed response: json: " +
		"cannot unmarshal string into Go struct field AddResp.sum of type int"
	if !errors.Is(err, plugger.ErrMalformedResponse) || err.Error() != expectErrMsg {
		t.Fatalf("unexpected error: %v", err)
	}
}

type AddReq struct {
	A int `json:"a"`
	B int `json:"b"`
}

type AddResp struct {
	Sum int `json:"sum"`
}

type MalformedReq struct {
	A string `json:"a"`
	B int    `json:"b"`
}

func testPlugin(t *testing.T, h *plugger.Host) {
	// "add" method.
	got, err := plugger.Call[AddReq, AddResp](
		t.Context(), h, "add", AddReq{A: 2, B: 3},
	)
	if err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	if got.Sum != 5 {
		t.Fatalf("unexpected result: %d", got.Sum)
	}

	// "simulated_error" method.
	_, err = plugger.Call[struct{}, struct{}](
		t.Context(), h, "simulated_error", struct{}{},
	)
	if err == nil || err.Error() != "simulated error" {
		t.Fatalf("unexpected error: %v", err)
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

func launchLocalModule(
	t *testing.T, ctx context.Context, testDirName, mainFilePath string,
) (*plugger.Host, *logWriter) {
	// Absolute path to the plugger source directory (this package).
	_, thisFile, _, _ := runtime.Caller(0)
	pluggerDir := filepath.Dir(thisFile)

	// Create a tiny plugin module in temp dir.
	modDir := filepath.Join(t.TempDir(), testDirName)
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
	mainFileContents := readFile(t, mainFilePath)
	writeFile(t, filepath.Join(modDir, "main.go"), mainFileContents)

	// Launch host and plugin.
	h := plugger.NewHost()
	logWriter := newLogWriter(t)
	go func() {
		err := h.RunPlugin(ctx, modDir, logWriter)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("RunPlugin error: %v", err)
		}
	}()

	t.Cleanup(func() {
		// Cleanup.
		if err := h.Close(); err != nil {
			t.Fatalf("closing host: %v", err)
		}
	})

	return h, logWriter
}
