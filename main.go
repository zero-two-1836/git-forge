// Command gitforge is a standalone GitHub-compatible git forge: git smart
// HTTP (protocol v0 + v2), REST v3, GraphQL v4 and MCP, all served from one
// in-memory store. It speaks the make.fun harness lifecycle: bind an
// ephemeral port, register it with the env, serve until the env says done,
// then exit 0.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

func init() {
	// The sandbox gives the entry 2 CPUs no matter how many the host has;
	// matching the scheduler to that keeps goroutines off throttled threads.
	runtime.GOMAXPROCS(2)
	// The working set is small and short-lived; a lazier GC trades a little
	// memory for noticeably less collection CPU on the hot paths.
	debug.SetGCPercent(400)
}

func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	application := newApp(newStore(), token)
	server := &http.Server{Handler: application}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		_ = server.Serve(listener)
	}()

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
	waitDone(envURL)
	_ = server.Close()
}

// register posts our port to the env, retrying transient failures with
// backoff (250ms doubling, capped at 2s). 200 and 409 mean the env has the
// port; 400 and 405 are fatal.
func register(envURL string, port int) error {
	client := &http.Client{Timeout: 30 * time.Second}
	body, _ := json.Marshal(map[string]int{"port": port})
	backoff := 250 * time.Millisecond
	for {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			switch {
			case res.StatusCode >= 200 && res.StatusCode < 300 || res.StatusCode == 409:
				return nil
			case res.StatusCode == 400 || res.StatusCode == 405:
				return fmt.Errorf("env refused registration with status %d", res.StatusCode)
			}
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// waitDone polls the env until it reports the trial is over.
func waitDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var payload struct {
				Done bool `json:"done"`
			}
			_ = json.NewDecoder(res.Body).Decode(&payload)
			res.Body.Close()
			if payload.Done {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}
