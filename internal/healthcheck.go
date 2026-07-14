package pricing

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// RunHealthcheck performs a single GET /healthz against localhost:8080 and
// returns 0 on success, 1 on failure. Used by the Docker HEALTHCHECK directive
// in the distroless image (which has no shell/curl).
func RunHealthcheck() int {
	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	url := fmt.Sprintf("http://localhost:%s/healthz", port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}