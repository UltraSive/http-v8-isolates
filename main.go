package main

import (
	"encoding/json"
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

func main() {
	http.ListenAndServe(":8089", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read request body
		bodyBytes, _ := io.ReadAll(r.Body)
		defer r.Body.Close()

		// Convert Go headers to simple map[string]string (take first value)
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

		// JS script: user defines global async fetch(request)
		// The function must return a plain object with { status, headers, body }
		script := `
		function fetch(request) {
  return JSON.stringify({
    status: 200,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      message: "Hello from v8go fetch!",
      method: request.method,
      url: request.url,
      body: request.body
    })
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

		// Call fetch(request) and await the Promise result
		val, err := ctx.RunScript(`fetch(request)`, "callfetch.js")
		if err != nil {
			http.Error(w, "Failed to call fetch: "+err.Error(), 500)
			return
		}

		/*
			// val is a Promise, so we await it synchronously
			// (v8go offers Await function)
			promise, err := val.AsPromise()
			if err != nil {
				http.Error(w, "Value is not a Promise: "+err.Error(), 500)
				return
			}

			result, err := promise.Await()
			if err != nil {
				http.Error(w, "Failed awaiting promise: "+err.Error(), 500)
				return
			}*/

		// Result should be an object { status, headers, body }
		// Serialize to JSON string for parsing
		resultJSON := val.String() //result.String()

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
