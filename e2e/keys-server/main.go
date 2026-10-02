// Command keys-server is the fixture GitHub used by the console e2e.
// GET /e2euser.keys returns one public key; every other path is 404.
package main

import (
	"log"
	"net/http"
)

const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHomerExampleKeyMaterial1234567890 homer\n"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/e2euser.keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(key))
	})
	log.Fatal(http.ListenAndServe(":8080", mux))
}
