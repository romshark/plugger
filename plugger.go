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
	Cancel string          `json:"cancel"`           // Request ID to cancel
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

// RunPlugin executes a plugin executable or Go file/package/module.
func (h *Host) RunPlugin(
	ctx context.Context, plugin string, pluginStderr io.WriteCloser,
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
	defer func() {
		_ = pluginStderr.Close() // Signal no more logs.
	}()

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
var ErrMalformedResponse = errors.New("malformed response")

type ErrorResponse string

func (e ErrorResponse) Error() string { return string(e) }

// Call sends a typed request and waits for the typed response.
// Returns ErrMalformedResponse if plugin returns a malformed JSON response.
// Returns ErrClosed if the plugin is closed.
func Call[Req any, Resp any](
	ctx context.Context, h *Host, method string, req Req,
) (Resp, error) {
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
			return zero, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
		}
		return zero, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		// Send cancelation message.
		if err := h.enc.Encode(envelope{Cancel: id}); err != nil {
			return zero, err
		}
		return zero, ctx.Err()
	}
}

// Close closes stdin (signals EOF) and waits for plugin exit.
// No-op if already closed.
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
	running      atomic.Bool
	wgDispatcher sync.WaitGroup
	lockCancel   sync.Mutex                    // protects cancel
	cancel       map[string]context.CancelFunc // id → cancel func
}

// NewPlugin binds to the process’ own stdin/stdout.
func NewPlugin() *Plugin {
	return &Plugin{
		enc:       json.NewEncoder(os.Stdout),
		dec:       json.NewDecoder(bufio.NewReader(os.Stdin)),
		endpoints: map[string]func(context.Context, json.RawMessage) (any, error){},
		cancel:    make(map[string]context.CancelFunc),
	}
}

// Handle registers an RPC endpoint overwriting any existing endpoint.
// Must be used before Run is invoked!
//
// WARNING: Logs must be written to os.Stderr because os.Stdout is reserved
// for host-plugin communication!
func Handle[Req any, Resp any](
	p *Plugin,
	name string,
	fn func(context.Context, Req) (Resp, error),
) {
	if p.running.Load() {
		panic("add handlers before invoking Run")
	}
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
func (p *Plugin) Run(ctx context.Context) (osReturnCode int) {
	if wasRunning := p.running.Swap(true); wasRunning {
		panic("plugin is already running")
	}
	for {
		if ctx.Err() != nil {
			// Run canceled.
			return 0
		}
		var e envelope
		if err := p.dec.Decode(&e); err != nil {
			// stdin closed – clean exit
			return 0
		}

		switch {
		case e.Cancel != "":
			// Cancelation message received.
			p.lockCancel.Lock()
			if cancelFn, ok := p.cancel[e.Cancel]; ok {
				cancelFn() // Abort the worker goroutine.
				delete(p.cancel, e.Cancel)
			}
			p.lockCancel.Unlock()
			continue // No reply for cancel.
		case e.ID == "":
			panic(`protocol violation: both "id" and "cancel" empty`)
		}

		ctxCancelable, cancelFn := context.WithCancel(ctx)

		p.lockCancel.Lock()
		p.cancel[e.ID] = cancelFn
		p.lockCancel.Unlock()

		p.wgDispatcher.Add(1)
		go p.dispatch(ctxCancelable, e)
	}
}

func (p *Plugin) dispatch(ctx context.Context, ev envelope) {
	// Register cancelation function.
	ctx, cancelFn := context.WithCancel(ctx)

	p.lockCancel.Lock()
	p.cancel[ev.ID] = cancelFn
	p.lockCancel.Unlock()

	defer func() {
		// Clean up cancelation function and release dispatcher slot.
		p.lockCancel.Lock()
		delete(p.cancel, ev.ID)
		p.lockCancel.Unlock()
		cancelFn()
		p.wgDispatcher.Done()
	}()

	fn := p.endpoints[ev.Method]

	out := envelope{ID: ev.ID}

	if fn == nil {
		out.Error = "unknown method: " + ev.Method
		if err := p.enc.Encode(out); err != nil {
			panic(fmt.Errorf("encoding unknown method response: %w", err))
		}
		return
	}
	data, err := fn(ctx, ev.Data)
	if err != nil {
		out.Error = err.Error()
	} else if data != nil {
		out.Data, _ = json.Marshal(data)
	}
	if err := p.enc.Encode(out); err != nil {
		panic(fmt.Errorf("encoding response: %w", err))
	}
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
