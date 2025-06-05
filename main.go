package main

import (
	"emam3/groq-chat-api.git/handlers"
	"log"
	"net/http"
	"runtime"
	"time"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	mux := http.NewServeMux()

	// Register routes
	mux.HandleFunc("/health", handlers.HealthCheck)
	mux.HandleFunc("/ws/chat", handlers.WSChatHandler)

	// Configure server
	srv := &http.Server{
		Addr:         ":8000",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	log.Println("Server is running on http://localhost:8000")
	log.Fatal(srv.ListenAndServe())
}
