package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "loopback listen address for deterministic mock LLM")
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("invalid -addr %q: %v", *addr, err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
		log.Fatalf("sentinel-mock-llm must bind only to a loopback address, got %q", host)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("sentinel-mock-llm listening on http://%s/v1/chat/completions", *addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("sentinel-mock-llm error: %v", err)
		}
	case <-sigCtx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-errCh
	}
}

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions)
	return mux
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	triageJSON := `{"classification":"dependency_failure","recommended_severity":"high","confidence":0.92,"evidence":["HTTP 504 gateway timeouts on primary payment route","Service payment-api impacted"],"suspected_root_cause":"Primary payment provider route is experiencing elevated upstream timeouts","recommended_steps":["Verify backup payment route health","Request human approval for route failover"],"proposed_action":"route_failover","requires_human_approval":true}`

	resp := map[string]any{
		"id":      "mock-chatcmpl-1",
		"object":  "chat.completion",
		"created": time.Now().UTC().Unix(),
		"model":   "mock-triage-model",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": triageJSON,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     140,
			"completion_tokens": 95,
			"total_tokens":      235,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
