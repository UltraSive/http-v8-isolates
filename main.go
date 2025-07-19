package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
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

func fetchScriptFromStorage(domain string) (string, error) {
	// Split domain at the last dot
	domainParts := strings.Split(domain, ".")
	if len(domainParts) < 2 {
		return "", fmt.Errorf("invalid domain format")
	}

	// Assuming the part before ".packetware.run" is the wildcard
	wildcard := domainParts[0] // Get the second-to-last part
	bucketPath := fmt.Sprintf("https://s3.us-central-1.wasabisys.com/isolates/%s/index.js", wildcard)

	// Create a custom HTTP client with InsecureSkipVerify
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // Bypass certificate verification
			},
		},
		Timeout: 10 * time.Second, // Optional: Set a timeout for the HTTP request
	}

	resp, err := client.Get(bucketPath)
	if err != nil {
		return "", fmt.Errorf("failed to fetch script: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get script, status: %s", resp.Status)
	}

	scriptBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read script body: %w", err)
	}

	return string(scriptBytes), nil
}

func main() {
	// Create a new router
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	// Health check endpoint
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	// Main handler
	r.Post("/execute", executeHandler)

	// Get the port from the environment variable, default to 8089
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Fallback if PORT is not set
	}

	// Start the HTTP server
	log.Printf("Starting server on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func executeHandler(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	jsReq := JSRequest{
		Method:  r.Method,
		URL:     r.URL.String(),
		Headers: headers,
		Body:    string(bodyBytes),
	}

	jsReqJSON, err := json.Marshal(jsReq)
	if err != nil {
		http.Error(w, "Failed to marshal request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	domain := r.Host
	script, err := fetchScriptFromStorage(domain)
	if err != nil {
		http.Error(w, "Failed to fetch script: "+err.Error(), http.StatusInternalServerError)
		return
	}

	iso := v8go.NewIsolate()
	ctx := v8go.NewContext(iso)
	if _, err := ctx.RunScript(fmt.Sprintf("var request = %s;", jsReqJSON), "inject.js"); err != nil {
		http.Error(w, "Failed to inject request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := ctx.RunScript(script, "worker.js"); err != nil {
		http.Error(w, "Failed to run worker script: "+err.Error(), http.StatusInternalServerError)
		return
	}

	val, err := ctx.RunScript(`fetch(request)`, "callfetch.js")
	if err != nil {
		http.Error(w, "Failed to call fetch: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resolvedVal, err := resolvePromise(val, ctx)
	if err != nil {
		http.Error(w, "Error resolving promise: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resultJSON := resolvedVal.String()
	var jsRes JSResponse
	if err := json.Unmarshal([]byte(resultJSON), &jsRes); err != nil {
		http.Error(w, "Invalid JSON response from JS: "+err.Error(), http.StatusInternalServerError)
		return
	}

	for k, v := range jsRes.Headers {
		w.Header().Set(k, v)
	}

	w.WriteHeader(jsRes.Status)
	w.Write([]byte(jsRes.Body))
}
