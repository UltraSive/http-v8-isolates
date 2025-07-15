package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"rogchap.com/v8go"
)

type JSRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type JSResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func resolvePromise(val *v8go.Value, ctx *v8go.Context) (*v8go.Value, error) {
	if !val.IsPromise() {
		return val, nil
	}
	for {
		switch p, _ := val.AsPromise(); p.State() {
		case v8go.Fulfilled:
			return p.Result(), nil
		case v8go.Rejected:
			return nil, errors.New(p.Result().DetailString())
		case v8go.Pending:
			ctx.PerformMicrotaskCheckpoint() // run VM to make progress on the promise
		default:
			return nil, fmt.Errorf("illegal v8.Promise state")
		}
	}
}

func main() {
	http.ListenAndServe(":8089", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read request body
		bodyBytes, _ := io.ReadAll(r.Body)
		defer r.Body.Close()

		// Convert headers to map
		headers := make(map[string]string)
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}

		// Build JSRequest object
		jsReq := JSRequest{
			Method:  r.Method,
			URL:     r.URL.String(),
			Headers: headers,
			Body:    string(bodyBytes),
		}

		jsReqJSON, err := json.Marshal(jsReq)
		if err != nil {
			http.Error(w, "Failed to marshal request: "+err.Error(), 500)
			return
		}

		// JS script defining the global async fetch function
		script := `
		function fetch(request) {
  return new Promise((resolve, reject) => {
    resolve(JSON.stringify({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        message: "Hello from v8go fetch!",
        method: request.method,
        url: request.url,
        body: request.body
      })
    }));
  });
}
		`

		iso := v8go.NewIsolate()
		ctx := v8go.NewContext(iso)

		// Inject `request` object in JS global scope
		if _, err := ctx.RunScript(fmt.Sprintf("var request = %s;", jsReqJSON), "inject.js"); err != nil {
			http.Error(w, "Failed to inject request: "+err.Error(), 500)
			return
		}

		// Run user JS script
		if _, err := ctx.RunScript(script, "worker.js"); err != nil {
			http.Error(w, "Failed to run worker script: "+err.Error(), 500)
			return
		}

		// Call fetch(request)
		val, err := ctx.RunScript(`fetch(request)`, "callfetch.js")
		if err != nil {
			http.Error(w, "Failed to call fetch: "+err.Error(), 500)
			return
		}

		// Resolve the promise and handle the result
		resolvedVal, err := resolvePromise(val, ctx)
		if err != nil {
			http.Error(w, "Error resolving promise: "+err.Error(), 500)
			return
		}

		// Result should be an object { status, headers, body }
		resultJSON := resolvedVal.String()

		// Parse JSON string in Go
		var jsRes JSResponse
		if err := json.Unmarshal([]byte(resultJSON), &jsRes); err != nil {
			http.Error(w, "Invalid JSON response from JS: "+err.Error(), 500)
			return
		}

		// Set response headers
		for k, v := range jsRes.Headers {
			w.Header().Set(k, v)
		}

		// Write status and body
		w.WriteHeader(jsRes.Status)
		w.Write([]byte(jsRes.Body))
	}))
}
