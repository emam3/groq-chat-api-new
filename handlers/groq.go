package handlers

import (
	"bytes"
	"context"
	"emam3/groq-chat-api.git/constants"
	"emam3/groq-chat-api.git/types"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

// Circuit breaker state
type circuitBreaker struct {
	failures    int32
	lastFailure time.Time
	state       int32 // 0: closed, 1: open, 2: half-open
	mu          sync.RWMutex
}

var breaker = &circuitBreaker{}

// Request queue
type queuedRequest struct {
	req  GroqRequest
	resp chan *GroqResponse
	err  chan error
}

var requestQueue = make(chan queuedRequest, 1000)
var queueWorkerStarted = false
var queueWorkerMutex sync.Mutex

// Per-session rate limiters
var sessionLimiters = sync.Map{}

// Cache for similar requests
type cacheEntry struct {
	response  *GroqResponse
	timestamp time.Time
}

var responseCache = sync.Map{}

const (
	circuitBreakerThreshold = 20
	circuitBreakerTimeout   = 10 * time.Second
	cacheTTL                = 5 * time.Minute
	maxCacheSize            = 1000
)

// WebSocket connection upgrade configuration
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// webSocket client structure
type WSClient struct {
	conn      *websocket.Conn
	sessionID string
	send      chan []byte
	limiter   *rate.Limiter
}

// WebSocket message structure
type WSMessage struct {
	Type    string      `json:"type"` // "chat", "error", "status"
	Content interface{} `json:"content"`
	Error   string      `json:"error,omitempty"`
}

// Active WebSocket connections
var wsClients = sync.Map{}

func HealthCheck(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"health": "ok",
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

// structs
type ChatRequest struct {
	Messages []types.Message `json:"messages"`
	Stream   bool            `json:"stream"`
}

type GroqRequest struct {
	Model    string          `json:"model"`
	Messages []types.Message `json:"messages"`
}

type GroqResponse struct {
	Choices []struct {
		Message types.Message `json:"message"`
	} `json:"choices"`
}

// Replace global messages map with concurrent map
var messages = sync.Map{}

// Optimized HTTP client
var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout:   5 * time.Second,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		MaxConnsPerHost:       50,
		IdleConnTimeout:       30 * time.Second,
		DisableKeepAlives:     false,
		ResponseHeaderTimeout: 10 * time.Second,
	},
	Timeout: 30 * time.Second,
}

// retry configuration
const (
	maxRetries     = 3
	initialBackoff = 100 * time.Millisecond
	maxBackoff     = 1 * time.Second
)

// add exponential backoff with jitter
// ## WHY?
// if many requests failed at the same time, they will retry after the same interval
// so they will fail again.. so here we are just trying to make then try at different time

func backoff(attempt int) time.Duration {
	backoff := initialBackoff * time.Duration(1<<uint(attempt))
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	// add jitter
	jitter := time.Duration(rand.Int63n(int64(backoff / 4)))
	return backoff + jitter
}

func callGroqAPI(ctx context.Context, groqReq GroqRequest) (*GroqResponse, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ { // loop and send the requet again incase it failed
		if attempt > 0 {
			// wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}

		jsonBody, err := json.Marshal(groqReq)
		if err != nil {
			return nil, fmt.Errorf("failed to encode request: %w", err)
		}

		groqAPIKey := os.Getenv("GROQ_API_KEY")
		fmt.Println("groqAPIKey : ", groqAPIKey)
		if groqAPIKey == "" {
			return nil, fmt.Errorf("GROQ_API_KEY not set")
		}

		// create a new context with longer timeout
		// ## WHY? if we decided later to increase the original ctx timeout to 60s for ex
		// then the api timeout will be limited to 40
		apiCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()

		httpReq, err := http.NewRequestWithContext(apiCtx, "POST", constants.GroqEndpoint, bytes.NewBuffer(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+groqAPIKey)

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			lastErr = err
			if os.IsTimeout(err) {
				continue // skip the iteration and try again
			}
			return nil, fmt.Errorf("failed to contact groq API: %w", err)
		}

		// Check for rate limiting
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			lastErr = fmt.Errorf("rate limited by groq API")
			continue // skip the iteration and try again
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("groq API error: %s", string(body))
			if resp.StatusCode >= 500 { //most likely to be a server error
				continue // skip the iteration and try again
			}
			return nil, lastErr
		}

		var groqResp GroqResponse
		if err := json.NewDecoder(resp.Body).Decode(&groqResp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to parse groq response: %w", err)
		}
		resp.Body.Close()

		if len(groqResp.Choices) == 0 {
			return nil, fmt.Errorf("empty response from groq")
		}

		return &groqResp, nil
	}
	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

func (cb *circuitBreaker) allowRequest() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	if atomic.LoadInt32(&cb.state) == 1 { // خpen
		if time.Since(cb.lastFailure) > circuitBreakerTimeout {
			atomic.StoreInt32(&cb.state, 2) // اalf-open
			return true
		}
		return false
	}
	return true
}

func (cb *circuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	failures := atomic.AddInt32(&cb.failures, 1)
	cb.lastFailure = time.Now()

	if failures >= circuitBreakerThreshold {
		atomic.StoreInt32(&cb.state, 1) // Open
	}
}

func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	atomic.StoreInt32(&cb.failures, 0)
	atomic.StoreInt32(&cb.state, 0) // Closed
}

func getSessionLimiter(sessionID string) *rate.Limiter {
	limiter, _ := sessionLimiters.LoadOrStore(sessionID, rate.NewLimiter(50, 20))
	return limiter.(*rate.Limiter)
}

func getFromCache(req GroqRequest) (*GroqResponse, bool) {
	key, err := generateCacheKey(req)
	if err != nil {
		return nil, false
	}
	if value, ok := responseCache.Load(key); ok {
		entry := value.(cacheEntry)
		if time.Since(entry.timestamp) < cacheTTL {
			return entry.response, true
		}
		responseCache.Delete(key)
	}
	return nil, false
}

func cacheResponse(req GroqRequest, resp *GroqResponse) {
	key, err := generateCacheKey(req)
	if err != nil {
		return
	}
	responseCache.Store(key, cacheEntry{
		response:  resp,
		timestamp: time.Now(),
	})
}

func generateCacheKey(req GroqRequest) (string, error) {
	if len(req.Messages) == 0 {
		return "", fmt.Errorf("generateCacheKey: empty Messages slice")
	}
	return fmt.Sprintf("%s-%s", req.Model, req.Messages[len(req.Messages)-1].Content), nil
}

// start the queuing system (Queue worker)
func ensureQueueWorkerStarted() {
	queueWorkerMutex.Lock()
	defer queueWorkerMutex.Unlock()

	if !queueWorkerStarted {
		queueWorkerStarted = true
		go processRequestQueue()
	}
}

// Process requests from the queue
func processRequestQueue() {
	for req := range requestQueue {
		// Check circuit breaker
		if !breaker.allowRequest() {
			req.err <- fmt.Errorf("circuit breaker open")
			continue
		}

		// Try to get from cache first
		if cached, ok := getFromCache(req.req); ok {
			req.resp <- cached
			continue
		}

		// Make the actual API call with a new context
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		resp, err := callGroqAPI(ctx, req.req)
		cancel()

		if err != nil {
			breaker.recordFailure()
			req.err <- err
			continue
		}

		breaker.recordSuccess()
		cacheResponse(req.req, resp)
		req.resp <- resp
	}
}

// WebSocket handler (MAIN HANDLER)
func WSChatHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		http.Error(w, "missing session ID", http.StatusBadRequest)
		return
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("failed to upgrade connection: %v", err)
		return
	}

	// create new client
	client := &WSClient{
		conn:      conn,
		sessionID: sessionID,
		send:      make(chan []byte, 256),
		limiter:   getSessionLimiter(sessionID),
	}

	// Store client connection
	wsClients.Store(sessionID, client)

	// start goroutines for reading and writing
	go client.writePump()
	go client.readPump()
}

func (c *WSClient) readPump() {
	defer func() {
		c.conn.Close()
		wsClients.Delete(c.sessionID)
	}()

	for {
		var msg ChatRequest
		err := c.conn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			break
		}

		// Check rate limit
		if !c.limiter.Allow() {
			c.send <- createWSMessage("error", nil, "Rate limit exceeded for session")
			continue
		}

		// Process message
		go c.processMessage(msg)
	}
}

func (c *WSClient) writePump() {
	defer func() {
		c.conn.Close()
		wsClients.Delete(c.sessionID)
	}()

	for message := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
			log.Printf("webSocket write error: %v", err)
			return
		}
	}
}

func (c *WSClient) processMessage(msg ChatRequest) {
	// Ensure queue worker is running
	ensureQueueWorkerStarted()

	// Get history
	var history []types.Message
	if value, exists := messages.Load(c.sessionID); exists {
		history = value.([]types.Message)
		if len(history) > 20 {
			history = history[len(history)-20:]
		}
	}

	if len(msg.Messages) == 0 {
		c.send <- createWSMessage("error", nil, "Messages array cannot be empty")
		return
	}

	history = append(history, msg.Messages[0])
	messages.Store(c.sessionID, history)

	// Prepare groq request
	groqReq := GroqRequest{
		Model:    "meta-llama/llama-4-scout-17b-16e-instruct",
		Messages: history,
	}

	// Create channels for response with larger buffer
	respChan := make(chan *GroqResponse, 1)
	errChan := make(chan error, 1)

	// Queue the request with timeout
	select {
	case requestQueue <- queuedRequest{
		req:  groqReq,
		resp: respChan,
		err:  errChan,
	}:
		// if we failed queuing the request, then the channel is full
	default:
		c.send <- createWSMessage("error", nil, "Service temporarily unavailable - request queue full")
		return
	}

	// wait for response and get its value from the channel
	select {
	case resp := <-respChan:
		assistantMessage := resp.Choices[0].Message
		history = append(history, assistantMessage)
		if len(history) > 20 {
			history = history[len(history)-20:]
		}
		messages.Store(c.sessionID, history)
		c.send <- createWSMessage("chat", history, "")

	case err := <-errChan:
		var errorMsg string
		switch {
		case os.IsTimeout(err):
			errorMsg = "request timeout - groq API took too long to respond"
		case strings.Contains(err.Error(), "rate limited"):
			errorMsg = "rate limited by groq API"
		case strings.Contains(err.Error(), "circuit breaker"):
			errorMsg = "service temporarily unavailable - circuit breaker open"
		default:
			errorMsg = fmt.Sprintf("groq API error: %v", err)
		}
		c.send <- createWSMessage("error", nil, errorMsg)

	case <-time.After(45 * time.Second): // incase no response or error, wait
		c.send <- createWSMessage("error", nil, "request timeout - groq API took too long to respond")
	}
}

func createWSMessage(msgType string, content interface{}, errMsg string) []byte {
	msg := WSMessage{
		Type:    msgType,
		Content: content,
		Error:   errMsg,
	}
	jsonMsg, _ := json.Marshal(msg)
	return jsonMsg
}
