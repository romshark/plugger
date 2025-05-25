package plugger

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
)

// envelope defines the JSON based wire format.
type envelope struct {
	ID     string          `json:"id"`               // Unique per request
	Method string          `json:"method,omitempty"` // Request side only
	Error  string          `json:"err,omitempty"`    // Set on error responses
	Data   json.RawMessage `json:"data,omitempty"`   // Payload
}

type Host struct {
	idCounter atomic.Uint64
	running   atomic.Bool
	wgRun     sync.WaitGroup
	enc       *json.Encoder
	dec       *json.Decoder
	cmd       *exec.Cmd
	closer    io.Closer // plugin stdin
	mu        sync.Mutex
	pending   map[string]chan envelope
}

// NewHost creates an empty host. Call RunPlugin afterwards.
func NewHost() *Host {
	h := &Host{pending: map[string]chan envelope{}}
	h.wgRun.Add(1)
	return h
}

var ErrAlreadyRunning = errors.New("plugin already running")

// RunPlugin spawns / go-runs / executes a plugin binary or Go module.
func (h *Host) RunPlugin(
	ctx context.Context, plugin string, pluginStderr io.Writer,
) error {
	if h.running.Load() {
		return ErrAlreadyRunning
	}
	cmd, err := spawn(plugin)
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("getting stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("getting stdout pipe: %w", err)
	}
	if pluginStderr == nil {
		pluginStderr = os.Stderr
	}
	cmd.Stderr = pluginStderr

	if err := cmd.Start(); err != nil {
		return err
	}

	h.enc = json.NewEncoder(stdin)
	h.dec = json.NewDecoder(bufio.NewReader(stdout))
	h.cmd = cmd
	h.closer = stdin
	h.running.Store(true)
	h.wgRun.Done()
	return h.run(ctx)
}

var ErrClosed = errors.New("closed")

type ErrorResponse string

func (e ErrorResponse) Error() string { return string(e) }

// Call sends a typed request and waits for the typed response.
func Call[Req any, Resp any](ctx context.Context, h *Host,
	method string, req Req) (Resp, error) {

	// Wait for the plugin to start.
	h.wgRun.Wait()

	var zero Resp
	if !h.running.Load() {
		return zero, ErrClosed
	}

	id := fmt.Sprintf("%x", h.idCounter.Add(1))
	raw, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("marshaling request: %w", err)
	}

	wait := make(chan envelope, 1)
	h.mu.Lock()
	h.pending[id] = wait
	h.mu.Unlock()

	if err := h.enc.Encode(envelope{ID: id, Method: method, Data: raw}); err != nil {
		return zero, err
	}

	select {
	case ev, ok := <-wait:
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		if !ok {
			return zero, ErrClosed
		}
		if ev.Error != "" {
			return zero, ErrorResponse(ev.Error)
		}
		if err := json.Unmarshal(ev.Data, &zero); err != nil {
			return zero, fmt.Errorf("unmarshaling response data: %w", err)
		}
		return zero, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Close closes stdin (signals EOF) and waits for plugin exit.
func (h *Host) Close() error {
	wasRunning := h.running.Swap(false)
	if !wasRunning {
		return nil
	}
	if h.closer != nil {
		_ = h.closer.Close()
	}
	if h.cmd != nil {
		return h.cmd.Wait()
	}
	return nil
}

func (h *Host) run(ctx context.Context) error {
	for {
		var ev envelope
		if err := h.dec.Decode(&ev); err != nil {
			// broadcast EOF to waiters
			h.mu.Lock()
			for _, ch := range h.pending {
				close(ch)
			}
			h.mu.Unlock()
			return err
		}
		h.mu.Lock()
		ch := h.pending[ev.ID]
		h.mu.Unlock()
		if ch != nil {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

type Plugin struct {
	enc          *json.Encoder
	dec          *json.Decoder
	endpoints    map[string]func(context.Context, json.RawMessage) (any, error)
	wgDispatcher sync.WaitGroup
}

// NewPlugin binds to the process’ own stdin/stdout.
func NewPlugin() *Plugin {
	return &Plugin{
		enc:       json.NewEncoder(os.Stdout),
		dec:       json.NewDecoder(bufio.NewReader(os.Stdin)),
		endpoints: map[string]func(context.Context, json.RawMessage) (any, error){},
	}
}

// Handle registers an RPC endpoint.
func Handle[Req any, Resp any](
	p *Plugin,
	name string,
	fn func(context.Context, Req) (Resp, error),
) {

	p.endpoints[name] = func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req Req
		if err := json.Unmarshal(raw, &req); err != nil {
			var zero Resp
			return zero, err
		}
		return fn(ctx, req)
	}
}

// Run blocks handling requests until stdin closes or ctx is done.
// Return value is suitable for os.Exit().
func (p *Plugin) Run(ctx context.Context) int {
	for {
		if ctx.Err() != nil {
			return 0
		}
		var ev envelope
		if err := p.dec.Decode(&ev); err != nil {
			// stdin closed – clean exit
			return 0
		}
		go p.dispatch(ctx, ev)
	}
}

func (p *Plugin) dispatch(ctx context.Context, ev envelope) {
	p.wgDispatcher.Add(1)
	defer p.wgDispatcher.Done()

	fn := p.endpoints[ev.Method]

	var out envelope
	out.ID = ev.ID

	if fn == nil {
		out.Error = "unknown method: " + ev.Method
		_ = p.enc.Encode(out)
		return
	}
	data, err := fn(ctx, ev.Data)
	if err != nil {
		out.Error = err.Error()
	} else if data != nil {
		out.Data, _ = json.Marshal(data)
	}
	_ = p.enc.Encode(out)
}

var reModule = regexp.MustCompile(`^[\w.\-]+(\.[\w.\-]+)+/[\w.\-/]+(@[\w.\-]+)?$`)

func spawn(plugin string) (*exec.Cmd, error) {
	switch {
	case reModule.MatchString(plugin):
		if err := requireGo(); err != nil {
			return nil, err
		}
		return exec.Command("go", "run", plugin), nil
	case isGoFile(plugin):
		if err := requireGo(); err != nil {
			return nil, err
		}
		cmd := exec.Command("go", "run", plugin)
		return cmd, nil
	case isLocalGoPackage(plugin):
		if err := requireGo(); err != nil {
			return nil, err
		}
		cmd := exec.Command("go", "run", ".")
		cmd.Dir = plugin
		return cmd, nil
	case isExecutable(plugin):
		return exec.Command(plugin), nil
	default:
		return nil, errors.New("invalid plugin path")
	}
}

func isGoFile(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	if err != nil {
		return false
	}
	return !info.IsDir() && filepath.Ext(abs) == ".go"
}

func isLocalGoPackage(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return false
	}
	cmd := exec.Command("go", "list", "-m")
	cmd.Dir = abs
	err = cmd.Run()
	return err == nil
}

func isExecutable(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		ext := filepath.Ext(abs)
		return ext == ".exe" || ext == ".bat" || ext == ".cmd"
	}
	return info.Mode().Perm()&0o111 != 0
}

func requireGo() error {
	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("go toolchain not in PATH")
	}
	return nil
}
