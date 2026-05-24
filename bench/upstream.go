package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := "9877"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("OK"))
	})

	fmt.Printf("upstream listening on :%s\n", port)
	http.ListenAndServe(":"+port, mux)
}
