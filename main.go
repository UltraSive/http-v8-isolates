package main

// #define _GNU_SOURCE
// #include <sys/resource.h>
// #include <sys/time.h>
import "C"

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
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

// Shared cache of script isolates
type ScriptWorker struct {
	iso    *v8go.Isolate
	queue  chan func()
	mu     sync.Mutex
	active int // number of active concurrent users of this worker
}

var (
	scriptWorker = make(map[string]*ScriptWorker)
	scriptMu     sync.Mutex
	evictionTTL  = 5 * time.Minute
)

func getThreadCPUTime() time.Duration {
	var usage C.struct_rusage
	C.getrusage(C.RUSAGE_THREAD, &usage)

	user := time.Duration(usage.ru_utime.tv_sec)*time.Second +
		time.Duration(usage.ru_utime.tv_usec)*time.Microsecond
	sys := time.Duration(usage.ru_stime.tv_sec)*time.Second +
		time.Duration(usage.ru_stime.tv_usec)*time.Microsecond

	return user + sys
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

func fetchScriptFromStorage(function string) (string, error) {
	bucketName := os.Getenv("BUCKET_NAME")           // Fetch bucket name from environment variables
	storageEndpoint := os.Getenv("STORAGE_ENDPOINT") // Fetch storage endpoint from environment variables

	// Construct the bucket path using environment variables
	bucketPath := fmt.Sprintf("%s/%s/%s/index.js", storageEndpoint, bucketName, function)

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
	r.Get("/runtime", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	// Catch-all for any other routes
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		executeHandler(w, r)
	})

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

func getOrCreateWorker(function string) (*ScriptWorker, error) {
	scriptMu.Lock()
	defer scriptMu.Unlock()

	if worker, exists := scriptWorker[function]; exists {
		log.Println("Using existing worker.")
		worker.mu.Lock()
		worker.active++ // increment the active users
		worker.mu.Unlock()
		return worker, nil
	} else {
		log.Println("Creating new worker.")
	}

	iso := v8go.NewIsolate()
	queue := make(chan func(), 100)

	worker := &ScriptWorker{
		iso:    iso,
		queue:  queue,
		active: 1, // first user
	}
	scriptWorker[function] = worker

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		for job := range queue {
			job()
		}
	}()

	return worker, nil
}

// Call this when request finishes using the worker
func releaseWorker(function string) {
	scriptMu.Lock()
	worker, exists := scriptWorker[function]
	if !exists {
		scriptMu.Unlock()
		return
	}
	worker.mu.Lock()
	worker.active--
	activeCount := worker.active
	worker.mu.Unlock()
	if activeCount <= 0 {
		// No more users - cleanup
		delete(scriptWorker, function)
		close(worker.queue)
		worker.iso.Dispose()
	}
	scriptMu.Unlock()
}

func executeHandler(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	headers := map[string]string{}
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
	// Split domain at the last dot
	domainParts := strings.Split(domain, ".")
	if len(domainParts) < 2 {
		http.Error(w, "invalid domain format", http.StatusInternalServerError)
		return
	}

	// Assuming the part before ".function.tld" is the wildcard
	function := domainParts[0] // Get the first part

	script, err := fetchScriptFromStorage(function)
	if err != nil {
		http.Error(w, "Failed to fetch script: "+err.Error(), http.StatusInternalServerError)
		return
	}

	worker, err := getOrCreateWorker(function)
	if err != nil {
		http.Error(w, "Failed to get worker: "+err.Error(), http.StatusInternalServerError)
		return
	}

	responseCh := make(chan JSResponse)
	errorCh := make(chan error)

	worker.queue <- func() {
		defer releaseWorker(function) // Decrement active count when done

		ctx := v8go.NewContext(worker.iso)

		startCPU := getThreadCPUTime()
		cpuLimit := 10 * time.Millisecond
		done := make(chan struct{})

		go func() {
			ticker := time.NewTicker(1 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					if used := getThreadCPUTime() - startCPU; used > cpuLimit {
						log.Println("CPU time limit exceeded:", used)
						ctx.Close()
						return
					}
				}
			}
		}()

		fetchFunc := v8go.NewFunctionTemplate(worker.iso, func(info *v8go.FunctionCallbackInfo) *v8go.Value {
			if len(info.Args()) < 1 {
				msg, _ := v8go.NewValue(worker.iso, "fetch requires 1 argument")
				worker.iso.ThrowException(msg)
				return nil
			}

			url := info.Args()[0].String()
			resp, err := http.Get(url)
			if err != nil {
				msg, _ := v8go.NewValue(worker.iso, err.Error())
				worker.iso.ThrowException(msg)
				return nil
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			result, _ := v8go.NewValue(worker.iso, string(body))
			return result
		})
		ctx.Global().Set("fetch", fetchFunc.GetFunction(ctx))

		if _, err := ctx.RunScript(fmt.Sprintf("var request = %s;", jsReqJSON), "inject.js"); err != nil {
			close(done)
			errorCh <- err
			return
		}

		if _, err := ctx.RunScript(script, "worker.js"); err != nil {
			close(done)
			errorCh <- err
			return
		}

		val, err := ctx.RunScript(`handler(request)`, "handle.js")
		if err != nil {
			close(done)
			errorCh <- err
			return
		}

		resolvedVal, err := resolvePromise(val, ctx)
		if err != nil {
			close(done)
			errorCh <- err
			return
		}

		close(done)
		var jsRes JSResponse
		if err := json.Unmarshal([]byte(resolvedVal.String()), &jsRes); err != nil {
			errorCh <- err
			return
		}

		responseCh <- jsRes

		cpuUsed := getThreadCPUTime() - startCPU
		log.Printf("CPU time used: %s", cpuUsed)

		ctx.Close()
	}

	select {
	case res := <-responseCh:
		for k, v := range res.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(res.Status)
		w.Write([]byte(res.Body))
	case err := <-errorCh:
		http.Error(w, "Script error: "+err.Error(), http.StatusInternalServerError)
	case <-time.After(2 * time.Second):
		http.Error(w, "Script timed out", http.StatusGatewayTimeout)
	}
}
