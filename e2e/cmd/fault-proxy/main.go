//go:build e2e

// fault-proxy is a transparent, harness-only TCP fault injector. It never
// interprets TLS, credentials or payloads and has no production entry point.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
)

type route struct {
	mu          sync.Mutex
	mode        string
	changed     chan struct{}
	connections map[net.Conn]bool
	target      string
}

func (r *route) configure(mode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = mode
	close(r.changed)
	r.changed = make(chan struct{})
	if mode == "disconnect" {
		for c := range r.connections {
			_ = c.Close()
		}
	}
}

func (r *route) allowed(done <-chan struct{}) bool {
	for {
		r.mu.Lock()
		mode, changed := r.mode, r.changed
		r.mu.Unlock()
		if mode == "" {
			return true
		}
		if mode == "disconnect" {
			return false
		}
		select {
		case <-changed:
		case <-done:
			return false
		}
	}
}

func (r *route) handle(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		return
	}
	defer server.Close()
	r.mu.Lock()
	r.connections[client], r.connections[server] = true, true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.connections, client); delete(r.connections, server); r.mu.Unlock() }()
	done := make(chan struct{})
	var once sync.Once
	copyStream := func(dst, src net.Conn) {
		defer once.Do(func() { close(done); client.Close(); server.Close() })
		buffer := make([]byte, 16*1024)
		for r.allowed(done) {
			n, err := src.Read(buffer)
			if n > 0 {
				if !r.allowed(done) {
					return
				}
				if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go copyStream(server, client)
	copyStream(client, server)
}

func main() {
	routes := map[string]*route{}
	for name, address := range map[string]string{"relay-a": ":9441", "relay-b": ":9442", "relay-c": ":9443"} {
		r := &route{target: name + ":9444", changed: make(chan struct{}), connections: map[net.Conn]bool{}}
		routes[name] = r
		listener, err := net.Listen("tcp", address)
		if err != nil {
			log.Fatal("proxy listener failed")
		}
		go func() {
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				go r.handle(c)
			}
		}()
	}
	http.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", 405)
			return
		}
		var input struct {
			Target string `json:"target"`
			Mode   string `json:"mode"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || routes[input.Target] == nil || (input.Mode != "" && input.Mode != "stall" && input.Mode != "disconnect") {
			http.Error(w, "invalid", 400)
			return
		}
		routes[input.Target].configure(input.Mode)
		w.WriteHeader(204)
	})
	log.Fatal(http.ListenAndServe(":9440", nil))
}
