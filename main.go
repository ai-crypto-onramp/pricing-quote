package main

import (
	"encoding/json"
	"net/http"
)

func main() {
	run(":8080")
}

// newMux builds the HTTP routing mux for the service.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	return mux
}

// run starts the HTTP server on addr and blocks until the server exits.
func run(addr string) error {
	return http.ListenAndServe(addr, newMux())
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
