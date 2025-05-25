<a href="https://pkg.go.dev/github.com/romshark/plugger">
    <img src="https://godoc.org/github.com/romshark/plugger?status.svg" alt="GoDoc">
</a>
<a href="https://goreportcard.com/report/github.com/romshark/plugger">
    <img src="https://goreportcard.com/badge/github.com/romshark/plugger" alt="GoReportCard">
</a>
<a href='https://coveralls.io/github/romshark/plugger?branch=main'>
    <img src='https://coveralls.io/repos/github/romshark/plugger/badge.svg?branch=main&service=github' alt='Coverage Status' />
</a>

# plugger (EXPERIMENTAL)

This is an experimental Go package for creating async JSON via OS pipe based plugins.

Features:
- Implements asynchronous request-response topology (multiplex)
- Supports cancelable requests (if the plugin supports it).
- Uses standard OS pipes (stdout/stderr/stdin), no networking involved.
- Executes local Go packages (requires the go toolchain to be installed).
- Executes remote Go modules like `github.com/someone/plugin@latest`
  (requires the go toolchain to be installed).
- Executes arbitrary executable files (shell scripts, binaries, etc.)
  that implement its [JSON protocol](#envelope-json-schema)
  (see [bash example](https://github.com/romshark/plugger/blob/main/testdata/test_executable.sh)).
- No external dependencies 🙌.

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
	plugger.Handle(p, "hello", // Define handler for method "hello".
		func(ctx context.Context, req shared.Request) (shared.Response, error) {
			// Logs must be written to stderr
			// since stdout is reserved for host-plugin communication!
			fmt.Fprintf(os.Stderr, "PLUG: received request: %#v\n", req)
			if req.Question == "u okay?" {
				time.Sleep(time.Second) // Simulate processing...
				if err := ctx.Err(); err != nil {
					return shared.Response{}, err // Request was canceled by host.
				}
				return shared.Response{Answer: "yeah, I'm fine!"}, nil
			}
			return shared.Response{Answer: "this is fine"}, nil
		})
	// Initialization logic goes here before Run.
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

## Envelope JSON Schema

Plugger supports any executable that implements the following
JSON schema over stdin/stdout:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://example.com/plugger/envelope.schema.json",
  "title": "Plugger RPC Envelope",
  "description": "Message wrapper exchanged between host and plugin.",
  "oneOf": [
    {
      "$ref": "#/$defs/request"
    },
    {
      "$ref": "#/$defs/response"
    },
    {
      "$ref": "#/$defs/cancel"
    }
  ],
  "$defs": {
    "id": {
      "type": "string",
      "description": "Unique request identifier (hexadecimal number).",
      "pattern": "^[0-9a-fA-F]+$"
    },
    "anyJson": {
      "description": "Arbitrary JSON payload. Returning extra fields the host does not expect is allowed",
      "type": [
        "object",
        "array",
        "string",
        "number",
        "boolean",
        "null"
      ]
    },
    "request": {
      "type": "object",
      "required": [
        "id",
        "method"
      ],
      "properties": {
        "id": {
          "$ref": "#/$defs/id"
        },
        "method": {
          "type": "string",
          "minLength": 1
        },
        "data": {
          "$ref": "#/$defs/anyJson"
        },
        "err": false,
        "cancel": false
      },
      "additionalProperties": false
    },
    "response": {
      "type": "object",
      "required": [
        "id"
      ],
      "properties": {
        "id": {
          "$ref": "#/$defs/id"
        },
        "err": {
          "type": "string"
        },
        "data": {
          "$ref": "#/$defs/anyJson"
        },
        "method": false,
        "cancel": false
      },
      "additionalProperties": false,
      "allOf": [
        {
          "if": {
            "required": [
              "err"
            ]
          },
          "then": {
            "not": {
              "required": [
                "data"
              ]
            }
          }
        }
      ]
    },
    "cancel": {
      "type": "object",
      "required": [
        "cancel"
      ],
      "properties": {
        "cancel": {
          "$ref": "#/$defs/id"
        },
        "id": false,
        "method": false,
        "err": false,
        "data": false
      },
      "additionalProperties": false,
      "description": "Cancellation message; asks the plugin to abort processing of the request whose id equals `cancel`."
    }
  }
}
```