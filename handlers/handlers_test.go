package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"emam3/groq-chat-api.git/types"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"

	"golang.org/x/time/rate"
)

func init() {
	// Get the absolute path to the .env file
	envPath := filepath.Join(".", ".env")

	// Load .env file
	if err := godotenv.Load(envPath); err != nil {
		fmt.Printf("Warning: Error loading .env file at %s: %v\n", envPath, err)
	} else {
		fmt.Printf("Successfully loaded .env file from %s\n", envPath)
	}
}

func TestHealthCheck(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status code %d, but got %d", http.StatusOK, w.Code)
	}

	var result map[string]string
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["health"] != "ok" {
		t.Errorf("expected health status 'ok', got '%s'", result["health"])
	}
}

// Create WS connection
func createTestWSConnection(t *testing.T, sessionID string) *websocket.Conn {
	server := httptest.NewServer(http.HandlerFunc(WSChatHandler))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/chat"

	// Create WebSocket connection
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{sessionID},
	})
	if err != nil {
		t.Fatalf("failed to connect to WebSocket: %v", err)
	}
	return ws
}

func TestWSChatHandler(t *testing.T) {
	// Reset global state before starting
	messages = sync.Map{}
	breaker = &circuitBreaker{}
	requestQueue = make(chan queuedRequest, 1000)
	queueWorkerStarted = false
	sessionLimiters = sync.Map{}
	responseCache = sync.Map{}

	// Test missing session ID separately since it's an HTTP-level error
	t.Run("Missing session ID", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(WSChatHandler))
		defer server.Close()

		// Test HTTP response before WebSocket upgrade
		resp, err := http.Get(server.URL + "/ws/chat")
		if err != nil {
			t.Fatalf("failed to make HTTP request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected status code %d, got %d", http.StatusBadRequest, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if !strings.Contains(string(body), "missing session ID") {
			t.Errorf("expected error about missing session ID, got '%s'", string(body))
		}
	})

	testCases := []struct {
		name          string
		request       ChatRequest
		sessionID     string
		setupMock     func()
		expectedType  string
		checkResponse func(*testing.T, *WSMessage)
	}{
		{
			name: "empty messages",
			request: ChatRequest{
				Messages: []types.Message{},
				Stream:   false,
			},
			sessionID:    "2",
			expectedType: "error",
			checkResponse: func(t *testing.T, msg *WSMessage) {
				if msg.Type != "error" {
					t.Errorf("expected an error, got '%s'", msg.Type)
				}
				if !strings.Contains(msg.Error, "Messages array cannot be empty") {
					t.Errorf("expected error about empty messages, got '%s'", msg.Error)
				}
			},
		},
		{
			name: "rate limit exceeded",
			request: ChatRequest{
				Messages: []types.Message{
					{
						Role:    "user",
						Content: "Hello",
					},
				},
				Stream: false,
			},
			sessionID: "5",
			setupMock: func() {
				sessionLimiters.Store("5", rate.NewLimiter(0, 0))
			},
			expectedType: "error",
			checkResponse: func(t *testing.T, msg *WSMessage) {
				if msg.Type != "error" {
					t.Errorf("expected message type 'error', got '%s'", msg.Type)
				}
				if !strings.Contains(msg.Error, "Rate limit exceeded for session") {
					t.Errorf("expected rate limit error, got '%s'", msg.Error)
				}
			},
		},
		{
			name: "circuit breaker open",
			request: ChatRequest{
				Messages: []types.Message{
					{
						Role:    "user",
						Content: "Hello",
					},
				},
				Stream: false,
			},
			sessionID: "7",
			setupMock: func() {
				// Set circuit breaker to open state
				breaker.state = 1 // Open
				breaker.lastFailure = time.Now()
			},
			expectedType: "error",
			checkResponse: func(t *testing.T, msg *WSMessage) {
				if msg.Type != "error" {
					t.Errorf("expected message type 'error', got '%s'", msg.Type)
				}
				if !strings.Contains(msg.Error, "circuit breaker open") {
					t.Errorf("expected circuit breaker error, got '%s'", msg.Error)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setupMock != nil {
				tc.setupMock()
			}

			// start WS connection
			ws := createTestWSConnection(t, tc.sessionID)
			defer ws.Close()

			// send a message
			if err := ws.WriteJSON(tc.request); err != nil {
				t.Fatalf("failed to send message: %v", err)
			}

			// read the response
			var response WSMessage
			if err := ws.ReadJSON(&response); err != nil {
				t.Fatalf("failed to read response: %v", err)
			}

			// Check response
			if response.Type != tc.expectedType {
				t.Errorf("expected message type %s, got %s", tc.expectedType, response.Type)
			}

			if tc.checkResponse != nil {
				tc.checkResponse(t, &response)
			}
		})
	}
}

func TestWSConnectionLifecycle(t *testing.T) {
	// Test WebSocket connection lifecycle
	server := httptest.NewServer(http.HandlerFunc(WSChatHandler))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/chat"

	// Test connection with invalid session ID
	_, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{""},
	})
	if err == nil {
		t.Error("expected error when connecting with empty session ID")
	}

	// Test normal connection and disconnection
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{"testSessionRandomNourID"},
	})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer ws.Close()

	// test sending invalid JSON
	if err := ws.WriteMessage(websocket.TextMessage, []byte("invalid json")); err != nil {
		t.Fatalf("failed to send invalid JSON: %v", err)
	}

	// connection should be closed by the server after invalid JSON, if not then the test is failed
	_, message, err := ws.ReadMessage()
	if err == nil {
		// the message should be an error one
		var response WSMessage
		if err := json.Unmarshal(message, &response); err == nil {
			if response.Type != "error" { // if not, then it ahs failed
				t.Errorf("expected error response for invalid JSON, got %s", response.Type)
			}
		}
	} else if !websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
		// If we got an error, it should be a close error
		t.Errorf("expected close error after invalid JSON, got %v", err)
	}

	// create a new connection for the close test
	ws, _, err = websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{"close"},
	})
	if err != nil {
		t.Fatalf("failed to connect for close test: %v", err)
	}
	defer ws.Close()

	// test normal close
	if err := ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
		t.Fatalf("failed to send close message: %v", err)
	}

	// verify client is removed from wsClients
	time.Sleep(100 * time.Millisecond) // Give time for cleanup
	if _, exists := wsClients.Load("close"); exists {
		t.Error("Client should be removed from wsClients after close")
	}
}

func TestCircuitBreaker(t *testing.T) {
	cb := &circuitBreaker{}

	t.Run("initial state is closed", func(t *testing.T) {
		if !cb.allowRequest() { // closed = must allow the request
			t.Error("circuit breaker should be closed initially")
		}
	})

	t.Run("record failure and open circuit", func(t *testing.T) {
		for i := 0; i < circuitBreakerThreshold; i++ {
			cb.recordFailure()
		}
		if cb.allowRequest() { // open = no request is allowed, if allowed? then the test fails
			t.Error("circuit breaker should be open after threshold failures")
		}
	})

	t.Run("circuit breaker timeout", func(t *testing.T) {
		cb.lastFailure = time.Now().Add(-circuitBreakerTimeout - time.Second)
		if !cb.allowRequest() {
			t.Error("circuit breaker should be half-open after timeout")
		}
	})

	t.Run("record success and close circuit", func(t *testing.T) {
		cb.recordSuccess()
		if !cb.allowRequest() {
			t.Error("circuit breaker should be closed after success")
		}
	})
}

func TestCache(t *testing.T) {
	// Reset cache
	responseCache = sync.Map{}

	req := GroqRequest{
		Model: "test-model",
		Messages: []types.Message{
			{
				Role:    "user",
				Content: "test message",
			},
		},
	}

	resp := &GroqResponse{
		Choices: []struct {
			Message types.Message `json:"message"`
		}{
			{
				Message: types.Message{
					Role:    "assistant",
					Content: "test response",
				},
			},
		},
	}

	t.Run("cache miss", func(t *testing.T) {
		_, ok := getFromCache(req)
		if ok {
			t.Error("expected cache miss, got hit")
		}
	})

	t.Run("Cache hit", func(t *testing.T) {
		cacheResponse(req, resp)
		got, ok := getFromCache(req)
		if !ok {
			t.Error("expected cache hit, got miss")
		} else if got != resp {
			t.Error("Cached response doesn't match original")
		}
	})
}

func TestSessionLimiter(t *testing.T) {
	// Reset session limiters
	sessionLimiters = sync.Map{}

	sessionID := "test-session"
	limiter := getSessionLimiter(sessionID)

	t.Run("New session limiter", func(t *testing.T) {
		if limiter == nil {
			t.Error("expected non-nil limiter")
		}
	})

	t.Run("Existing session limiter", func(t *testing.T) {
		limiter2 := getSessionLimiter(sessionID)
		if limiter2 != limiter {
			t.Error("expected same limiter instance for same session")
		}
	})
}

func TestQueueWorker_WS(t *testing.T) {
	// Reset queue state
	requestQueue = make(chan queuedRequest, 1000)
	queueWorkerStarted = false
	breaker = &circuitBreaker{}

	// Create test WebSocket connection
	ws := createTestWSConnection(t, "test-session")
	defer ws.Close()

	// Ensure queue worker is running
	ensureQueueWorkerStarted()
	if !queueWorkerStarted {
		t.Error("Queue worker should be started")
	}

	// Send a test message
	testReq := ChatRequest{
		Messages: []types.Message{
			{
				Role:    "user",
				Content: "test message",
			},
		},
		Stream: false,
	}

	if err := ws.WriteJSON(testReq); err != nil {
		t.Fatalf("failed to send test message: %v", err)
	}

	// Wait for response
	var response WSMessage
	if err := ws.ReadJSON(&response); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if response.Type != "chat" && response.Type != "error" {
		t.Errorf("Unexpected response type: %s", response.Type)
	}
}

func TestCallGroqAPI_ErrorBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("marshal failure", func(t *testing.T) {
		// Create a GroqRequest with a type that can't be marshaled
		badReq := GroqRequest{
			Model:    string("Nour model, best AI model you can ever have 98"), // invalid UTF-8
			Messages: nil,
		}
		_, err := callGroqAPI(ctx, badReq)
		if err == nil {
			t.Error("expected marshal error, got nil")
		}
	})

	t.Run("Missing API key", func(t *testing.T) {
		// Temporarily clear the API key
		oldKey := os.Getenv("GROQ_API_KEY")
		os.Setenv("GROQ_API_KEY", "")
		defer os.Setenv("GROQ_API_KEY", oldKey)
		// Patch constants.GroqEndpoint to a dummy value if needed
		badReq := GroqRequest{Model: "", Messages: nil}
		_, err := callGroqAPI(ctx, badReq)
		if err == nil || !strings.Contains(err.Error(), "GROQ_API_KEY not set") {
			t.Error("expected missing API key error")
		}
	})

	// You can add more subtests for non-200, 429, invalid JSON, and context timeout if you mock httpClient
}

func TestCacheExpiration(t *testing.T) {
	responseCache = sync.Map{}
	req := GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "expire"}}}
	resp := &GroqResponse{}
	key, err := generateCacheKey(req)
	if err != nil {
		t.Fatalf("failed to generate cache key: %v", err)
	}
	responseCache.Store(key, cacheEntry{response: resp, timestamp: time.Now().Add(-cacheTTL - time.Second)})
	_, ok := getFromCache(req)
	if ok {
		t.Error("expected expired cache entry to be deleted")
	}
}

func TestCircuitBreakerHalfOpen(t *testing.T) {
	cb := &circuitBreaker{state: 1, lastFailure: time.Now().Add(-circuitBreakerTimeout - time.Second)}
	if !cb.allowRequest() {
		t.Error("expected half-open to allow request after timeout")
	}
	cb.recordSuccess()
	if !cb.allowRequest() {
		t.Error("expected closed after success")
	}
}

func TestProcessRequestQueue_ErrorPath(t *testing.T) {
	breaker = &circuitBreaker{state: 1, lastFailure: time.Now()} // open
	requestQueue = make(chan queuedRequest, 1)
	respChan := make(chan *GroqResponse, 1)
	errChan := make(chan error, 1)
	requestQueue <- queuedRequest{
		req:  GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "fail"}}},
		resp: respChan,
		err:  errChan,
	}
	go processRequestQueue()
	select {
	case err := <-errChan:
		if err == nil || !strings.Contains(err.Error(), "circuit breaker open") {
			t.Error("expected circuit breaker open error")
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for error from queue worker")
	}
}

func TestBackoff(t *testing.T) {
	for i := 0; i < 5; i++ {
		d := backoff(i)
		if d <= 0 {
			t.Errorf("expected positive backoff duration, got %v", d)
		}
	}
}

// Mock httpClient for callGroqAPI tests
var (
	originalHTTPClient = httpClient
)

func TestCallGroqAPI_MockBranches(t *testing.T) {
	defer func() {
		httpClient = originalHTTPClient
		os.Setenv("GROQ_API_KEY", os.Getenv("GROQ_API_KEY"))
	}()
	ctx := context.Background()

	// Set up mock API key for testing
	os.Setenv("GROQ_API_KEY", "test-api-key")

	t.Run("Invalid JSON from API", func(t *testing.T) {
		httpClient = &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewBufferString("invalid json")),
				}, nil
			}),
		}
		_, err := callGroqAPI(ctx, GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hi"}}})
		if err == nil {
			t.Error("expected error for invalid JSON")
		} else if !strings.Contains(err.Error(), "failed to parse") {
			t.Errorf("expected parse error, got: %v", err)
		}
	})

	t.Run("empty response from Groq", func(t *testing.T) {
		httpClient = &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewBufferString(`{"choices":[]}`)),
				}, nil
			}),
		}
		_, err := callGroqAPI(ctx, GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hi"}}})
		if err == nil {
			t.Error("expected error for empty response")
		} else if !strings.Contains(err.Error(), "empty response") {
			t.Errorf("expected empty response error, got: %v", err)
		}
	})

	t.Run("429 triggers retry", func(t *testing.T) {
		count := 0
		httpClient = &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				count++
				if count < 2 {
					return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(bytes.NewBufferString(""))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))}, nil
			}),
		}
		_, err := callGroqAPI(ctx, GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hi"}}})
		if err != nil {
			t.Errorf("expected success after retry, got %v", err)
		}
	})

	t.Run("context timeout", func(t *testing.T) {
		httpClient = &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				time.Sleep(50 * time.Millisecond)
				return nil, context.DeadlineExceeded
			}),
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		_, err := callGroqAPI(timeoutCtx, GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hi"}}})
		if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Error("expected context deadline exceeded error")
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProcessRequestQueue_CacheHit(t *testing.T) {
	requestQueue = make(chan queuedRequest, 1)
	breaker = &circuitBreaker{}
	responseCache = sync.Map{}
	groqReq := GroqRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "cache"}}}
	groqResp := &GroqResponse{Choices: []struct {
		Message types.Message `json:"message"`
	}{{Message: types.Message{Role: "assistant", Content: "cached!"}}}}
	cacheResponse(groqReq, groqResp)
	respChan := make(chan *GroqResponse, 1)
	errChan := make(chan error, 1)
	requestQueue <- queuedRequest{req: groqReq, resp: respChan, err: errChan}
	go processRequestQueue()
	select {
	case resp := <-respChan:
		if resp.Choices[0].Message.Content != "cached!" {
			t.Errorf("expected cached response, got %v", resp)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for cache hit in queue worker")
	}
}

func TestWSClientReadPump(t *testing.T) {
	// Save original client and restore after test
	originalClient := httpClient
	defer func() { httpClient = originalClient }()

	// Mock HTTP client to return immediately
	httpClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"choices":[{"message":{"role":"assistant","content":"test response"}}]}`)),
			}, nil
		}),
	}

	server := httptest.NewServer(http.HandlerFunc(WSChatHandler))
	defer server.Close()

	// Reset global state
	requestQueue = make(chan queuedRequest, 1000) // Ensure queue is large enough
	breaker = &circuitBreaker{}
	messages = sync.Map{}
	sessionLimiters = sync.Map{} // Reset session limiters

	// Create a rate limiter that allows no requests
	limiter := rate.NewLimiter(0, 0)
	sessionLimiters.Store("read-pump-test", limiter)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/chat"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{"read-pump-test"},
	})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer ws.Close()

	// Test rate limiting
	if err := ws.WriteJSON(ChatRequest{
		Messages: []types.Message{{Role: "user", Content: "test"}},
		Stream:   false,
	}); err != nil {
		t.Fatalf("failed to send message: %v", err)
	}

	// Read rate limit error response
	var response WSMessage
	if err := ws.ReadJSON(&response); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if response.Type != "error" || !strings.Contains(response.Error, "Rate limit exceeded") {
		t.Errorf("expected rate limit error, got %v", response)
	}

	// Test unexpected close
	ws.Close()
	time.Sleep(100 * time.Millisecond) // Give time for cleanup
	if _, exists := wsClients.Load("read-pump-test"); exists {
		t.Error("Client should be removed from wsClients after close")
	}
}

func TestProcessMessage_AdditionalScenarios(t *testing.T) {
	// Save original client and restore after test
	originalClient := httpClient
	defer func() { httpClient = originalClient }()

	// Mock HTTP client to return immediately
	httpClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"choices":[{"message":{"role":"assistant","content":"test response"}}]}`)),
			}, nil
		}),
	}

	server := httptest.NewServer(http.HandlerFunc(WSChatHandler))
	defer server.Close()

	// Reset global state
	messages = sync.Map{}
	requestQueue = make(chan queuedRequest, 1) // Small queue to test full scenario
	breaker = &circuitBreaker{}
	sessionLimiters = sync.Map{} // Reset session limiters

	// Test queue full scenario
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/chat"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{"process-message-test"},
	})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer ws.Close()

	// Fill the queue
	if err := ws.WriteJSON(ChatRequest{
		Messages: []types.Message{{Role: "user", Content: "test1"}},
		Stream:   false,
	}); err != nil {
		t.Fatalf("failed to send message: %v", err)
	}

	// Try to send another message to trigger queue full
	if err := ws.WriteJSON(ChatRequest{
		Messages: []types.Message{{Role: "user", Content: "test2"}},
		Stream:   false,
	}); err != nil {
		t.Fatalf("failed to send message: %v", err)
	}

	// Read queue full error
	var response WSMessage
	if err := ws.ReadJSON(&response); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if response.Type != "error" || !strings.Contains(response.Error, "request queue full") {
		t.Errorf("expected queue full error, got %v", response)
	}

	// Test message history management with a new connection
	sessionID := "history-test"
	ws2, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Session-ID": []string{sessionID},
	})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer ws2.Close()

	// Send messages and receive responses
	for i := 0; i < 25; i++ {
		if err := ws2.WriteJSON(ChatRequest{
			Messages: []types.Message{{Role: "user", Content: fmt.Sprintf("history%d", i)}},
			Stream:   false,
		}); err != nil {
			t.Fatalf("failed to send message: %v", err)
		}

		// Wait for response
		var resp WSMessage
		if err := ws2.ReadJSON(&resp); err != nil {
			t.Fatalf("failed to read response: %v", err)
		}

		time.Sleep(10 * time.Millisecond) // Small delay to prevent rate limiting
	}
}
