# plugger (EXPERIMENTAL)

This is an experimental Go package for creating async JSON via OS pipe based plugins.
Create a host, run a Go package (will use run `go run` internally) or a Go module
(like `github.com/someone/plugin@latest`) or an arbitrary executable file and use `Call`
to query it via stdin/stdout.

## Example

**cmd/host/main.go**

```sh
$ go run ./cmd/host -p "./cmd/plugin"
PLUG: received request: shared.Request{Question:"u okay?"}
PLUG: received request: shared.Request{Question:"how is it?"}
2025/05/23 19:44:03 ERR: 3: unknown method: wrongmethod
2025/05/23 19:44:03 RESP: 2: shared.Response{Answer:"this is fine"}
2025/05/23 19:44:04 RESP: 1: shared.Response{Answer:"yeah, I'm fine!"}
2025/05/23 19:44:04 DONE
```

If the plugin is hosted on GitHub you can run it as:

```sh
go run ./cmd/host -p github.com/your/plugin@latest
```

```go
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"sync"

	"pluginexample/shared"

	"github.com/romshark/plugger"
)

func main() {
	fPlugin := flag.String(
		"p", "plugin",
		"path to executable file, a local Go package or a remote Go module",
	)
	flag.Parse()
	if *fPlugin == "" {
		log.Print("please provide a plugin with -p")
		os.Exit(1)
	}
	h := plugger.NewHost()
	ctx := context.Background()
	go func() { // Run the plugin in the background.
		if err := h.RunPlugin(ctx, *fPlugin, os.Stderr); err != nil {
			if !errors.Is(err, io.EOF) {
				log.Fatal(err)
			}
		}
	}()

	// Send three async requests
	// The third request is intentionally targeting an inexistent endpoint.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // This request will take 1s to process.
		defer wg.Done()
		request(ctx, h, "1", "hello", "u okay?")
	}()
	go func() { // This request will respond immediately
		defer wg.Done()
		request(ctx, h, "2", "hello", "how is it?")
	}()
	go func() { // This request is intentionally targeting an inexistent endpoint.
		defer wg.Done()
		request(ctx, h, "3", "wrongmethod", "yo")
	}()
	wg.Wait()

	if err := h.Close(); err != nil { // Close stdin pipe shutting the plugin down.
		log.Print("ERR: closing plugin: ", err)
	}
	log.Println("DONE")
}

func request(ctx context.Context, h *plugger.Host, reqPrefix, method, question string) {
	resp, err := plugger.Call[shared.Request, shared.Response](
		ctx, h, method, shared.Request{Question: question},
	)
	if err != nil {
		log.Printf("ERR: %s: %v", reqPrefix, err)
	} else {
		log.Printf("RESP: %s: %#v", reqPrefix, resp)
	}
}
```

**cmd/plugin/main.go**

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"pluginexample/shared"

	"github.com/romshark/plugger"
)

func main() {
	p := plugger.NewPlugin()
	plugger.Handle(p, "hello",
		func(ctx context.Context, req shared.Request) (shared.Response, error) {
			fmt.Fprintf(os.Stderr, "PLUG: received request: %#v\n", req)
			if req.Question == "u okay?" {
				time.Sleep(1 * time.Second)
				return shared.Response{Answer: "yeah, I'm fine!"}, nil
			}
			return shared.Response{Answer: "this is fine"}, nil
		})
	os.Exit(p.Run(context.Background()))
}
```

**shared/shared.go**

```go
package shared

type Request struct {
	Question string `json:"question"`
}

type Response struct {
	Answer string `json:"answer"`
}
```