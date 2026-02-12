package wirepod_vosk

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	"gopkg.in/hraban/opus.v2"
)

var Name string = "xiaozhi"

var preconnectOnce sync.Once

func startPreconnectLoop() {
	preconnectOnce.Do(func() {
		go func() {
			deviceID := xiaozhi.GetDeviceIDFromConfig()
			clientID := xiaozhi.GetClientIDFromConfig()
			if deviceID == "" || clientID == "" {
				logger.Println("Xiaozhi STT: Preconnect skipped (missing Device-Id or Client-Id in config)")
				return
			}

			baseURL, _, _ := xiaozhi.GetKnowledgeGraphConfig()
			if baseURL == "" {
				baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
			}

			headers := http.Header{}
			headers.Add("Protocol-Version", "1")
			headers.Add("Device-Id", deviceID)
			headers.Add("Client-Id", clientID)

			backoff := 1 * time.Second
			for {
				// If a valid connection already exists, do nothing (keep it warm).
				if c, sid, exists := xiaozhi.CheckConnectionExists(deviceID); exists && c != nil && c.RemoteAddr() != nil && xiaozhi.IsReaderRunning(deviceID) {
					_ = sid
					// Check frequently so we recover quickly after "connection reset by peer".
					time.Sleep(2 * time.Second)
					continue
				}

				logger.Println(fmt.Sprintf("Xiaozhi STT: 🔌 Preconnecting websocket to %s (device=%s)...", baseURL, deviceID))
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				conn, _, _, err := dialWebsocketWithTimeout(ctx, baseURL, headers)
				cancel()
				if err != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Preconnect failed: %v (retry in %v)", err, backoff))
					time.Sleep(backoff)
					if backoff < 30*time.Second {
						backoff *= 2
					}
					continue
				}

				// Hello handshake (no listen start yet; we just keep the session alive)
				helloEvent := map[string]interface{}{
					"type":      "hello",
					"version":   1,
					"transport": "websocket",
					"features": map[string]interface{}{
						"mcp": true,
						"aec": true,
					},
					"language": "vi",
					"audio_params": map[string]interface{}{
						"format":         "opus",
						"sample_rate":    16000,
						"channels":       1,
						"frame_duration": 60,
					},
				}
				if err := conn.WriteJSON(helloEvent); err != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Preconnect hello write failed: %v", err))
					conn.Close()
					time.Sleep(backoff)
					continue
				}
				var helloResp map[string]interface{}
				if err := conn.ReadJSON(&helloResp); err != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Preconnect hello read failed: %v", err))
					conn.Close()
					time.Sleep(backoff)
					continue
				}

				sessionID := ""
				if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
					sessionID = sid
				}
				if err := xiaozhi.StoreConnection(deviceID, conn, sessionID); err != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Preconnect store connection failed: %v", err))
					conn.Close()
					time.Sleep(backoff)
					continue
				}

				logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Preconnected websocket ready (sessionID=%s)", sessionID))
				backoff = 1 * time.Second
				time.Sleep(2 * time.Second)
			}
		}()
	})
}

func dialWebsocketWithTimeout(parent context.Context, url string, headers http.Header) (*websocket.Conn, *http.Response, time.Duration, error) {
	// Use a bounded timeout specifically for the websocket handshake.
	// Without this, DialContext can appear to "hang" for a long time (DNS/TCP/TLS/handshake).
	//
	// Also retry a few times: upstream can be flaky and intermittent TCP/TLS stalls happen.
	retries := 3
	perAttemptTimeout := 12 * time.Second
	handshakeTimeout := 10 * time.Second
	backoff := 500 * time.Millisecond

	var lastErr error
	var lastResp *http.Response
	var totalStart = time.Now()

	for attempt := 1; attempt <= retries; attempt++ {
		// Respect parent cancellation
		select {
		case <-parent.Done():
			return nil, nil, time.Since(totalStart), fmt.Errorf("dial canceled: %w", parent.Err())
		default:
		}

		dialCtx, cancel := context.WithTimeout(parent, perAttemptTimeout)
		d := *websocket.DefaultDialer
		d.HandshakeTimeout = handshakeTimeout
		d.NetDialContext = (&net.Dialer{
			Timeout:   handshakeTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext

		start := time.Now()
		conn, resp, err := d.DialContext(dialCtx, url, headers)
		dur := time.Since(start)
		cancel()

		if err == nil {
			return conn, resp, time.Since(totalStart), nil
		}

		lastErr = err
		lastResp = resp

		// If we got an HTTP response, log status/body (helps when server rejects handshake).
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			logger.Println(fmt.Sprintf("Xiaozhi STT: WebSocket handshake failed (attempt=%d/%d, status=%s, body=%q, dur=%v)", attempt, retries, resp.Status, string(b), dur))
		} else {
			logger.Println(fmt.Sprintf("Xiaozhi STT: WebSocket dial failed (attempt=%d/%d, dur=%v): %v", attempt, retries, dur, err))
		}

		// Retry only on timeouts / transient network errors.
		ne, ok := err.(net.Error)
		shouldRetry := ok && ne.Timeout()
		if dialCtx.Err() == context.DeadlineExceeded {
			shouldRetry = true
		}
		if attempt < retries && shouldRetry {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		break
	}

	return nil, lastResp, time.Since(totalStart), lastErr
}

// STTHandler implements MessageHandler interface for STT
// This handler processes STT-related messages from the single reader goroutine
type STTHandler struct {
	transcriptChan     chan string
	errChan            chan error
	errorOccurred      chan struct{}
	active             bool
	transcriptReceived bool
	mu                 sync.RWMutex
}

// HandleMessage processes messages from the WebSocket connection
func (h *STTHandler) HandleMessage(messageType int, message []byte) error {
	h.mu.RLock()
	active := h.active
	h.mu.RUnlock()

	if !active {
		return nil // Handler is not active, ignore message
	}

	if messageType == websocket.TextMessage {
		var event map[string]interface{}
		if err := json.Unmarshal(message, &event); err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ERROR - Failed to unmarshal message: %v", err))
			return err
		}

		eventType, ok := event["type"].(string)
		if !ok {
			return nil
		}

		switch eventType {
		case "stt":
			if text, ok := event["text"].(string); ok {
				if text != "" {
					// Non-empty transcript
					logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ✅ STT transcript: '%s'", text))
					// Use recover to handle panic if channel is closed
					func() {
						defer func() {
							if r := recover(); r != nil {
								// Channel is closed - this can happen if STT request completed but ConnectionManager
								// still receives messages from the server
								logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  transcriptChan is closed, dropping transcript (recovered from panic: %v)", r))
							}
						}()
						select {
						case h.transcriptChan <- text:
							logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ✅ Transcript sent to channel: '%s'", text))
							h.mu.Lock()
							h.transcriptReceived = true
							h.mu.Unlock()
						default:
							logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  transcriptChan is full, dropping transcript"))
						}
					}()
				} else {
					// Empty transcript from server.
					//
					// IMPORTANT: Do NOT immediately forward "" to transcriptChan.
					// Xiaozhi upstream can emit an early `stt` event with empty text (interim/ack) and then
					// later send the real transcript. If we forward "", the STT request will end early and
					// the caller will think "no STT" (exactly what you observed).
					//
					// Instead, log and keep waiting for a non-empty transcript (or a real timeout/error).
					logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  Received empty transcript from server (stt event with empty text). Ignoring and waiting for non-empty transcript. Raw event: %s", string(message)))
				}
			} else {
				// Server sent "stt" event but no "text" field
				logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  Received 'stt' event but no 'text' field in event: %v", event))
			}
		case "error":
			// Use recover to handle panic if channels are closed
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Println(fmt.Sprintf("Xiaozhi STT Handler: ⚠️  error channels are closed, dropping error (recovered from panic: %v)", r))
					}
				}()
				select {
				case h.errorOccurred <- struct{}{}:
				default:
				}
				errorMsg := "unknown error"
				if msg, ok := event["error"].(string); ok {
					errorMsg = msg
				} else if msg, ok := event["message"].(string); ok {
					errorMsg = msg
				}
				select {
				case h.errChan <- fmt.Errorf("xiaozhi error: %s", errorMsg):
				default:
				}
			}()
		}
	}
	return nil
}

// IsActive returns whether the handler is currently active
func (h *STTHandler) IsActive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.active && !h.transcriptReceived
}

// SetActive sets the handler as active or inactive
func (h *STTHandler) SetActive(active bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = active
	// IMPORTANT: Always clear transcriptReceived when (re)activating, so the handler
	// can receive messages on the next STT request.
	if active {
		h.transcriptReceived = false
	}
}

// XiaozhiSTT handles STT via xiaozhi WebSocket service
// This follows the xiaozhi protocol as defined in go-xiaozhi-main
func Init() error {
	// Check if xiaozhi is configured in Knowledge Graph
	if vars.APIConfig.Knowledge.Provider != "xiaozhi" {
		logger.Println("Xiaozhi STT: Knowledge Graph provider is not set to xiaozhi")
		return fmt.Errorf("xiaozhi not configured as knowledge provider")
	}
	logger.Println("Xiaozhi STT initialized!")
	// Keep a warm websocket session ready so opening the mic doesn't block on Dial/Hello.
	startPreconnectLoop()
	return nil
}

func STT(sreq sr.SpeechRequest) (string, error) {
	// Helper function to safely log messages (with recover to prevent logger panics)
	safeLog := func(format string, args ...interface{}) {
		defer func() {
			if r := recover(); r != nil {
				// Log to stderr if logger panics
				fmt.Fprintf(os.Stderr, "[Xiaozhi STT] [logger panic recovered: %v] ", r)
				fmt.Fprintf(os.Stderr, format+"\n", args...)
			}
		}()
		logger.Println(fmt.Sprintf(format, args...))
	}

	safeLog("(Bot %s, Xiaozhi) Processing...", sreq.Device)

	// Get xiaozhi config
	baseURL, _, _ := xiaozhi.GetKnowledgeGraphConfig()
	if baseURL == "" {
		baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
	}

	// Connect to xiaozhi WebSocket (using xiaozhi protocol)
	// Increased timeout to 90 seconds to allow longer speech input
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second) // Increased to 120s for long speech
	defer cancel()

	// Lấy Device-Id và Client-Id từ config
	deviceID := xiaozhi.GetDeviceIDFromConfig()
	// Client-Id: luôn gửi (giống ESP32 - line 109: websocket_->SetHeader("Client-Id", Board::GetInstance().GetUuid().c_str()))
	// ESP32 luôn gửi Client-Id, không optional
	clientID := xiaozhi.GetClientIDFromConfig()

	headers := http.Header{}

	// Gửi các headers giống ESP32 (theo xiaozhi-esp32-main/main/protocols/websocket_protocol.cc)
	// Protocol-Version: version của protocol (mặc định 1)
	headers.Add("Protocol-Version", "1")

	if deviceID != "" {
		headers.Add("Device-Id", deviceID)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Using Device-Id from config: %s", deviceID))
	} else {
		logger.Println("Xiaozhi STT: WARNING - No Device-Id configured. Server may reject the connection.")
	}
	if clientID == "" {
		// Nếu chưa có Client-Id, generate mới (GetClientIDFromConfig() sẽ tự động generate nếu Knowledge.Provider == "xiaozhi")
		// Nhưng nếu Knowledge.Provider != "xiaozhi", cần generate thủ công
		clientID = xiaozhi.GenerateClientID()
		logger.Println(fmt.Sprintf("Xiaozhi STT: Generated new Client-Id: %s", clientID))
	}
	headers.Add("Client-Id", clientID)
	logger.Println(fmt.Sprintf("Xiaozhi STT: Using Client-Id: %s (giống ESP32 - bắt buộc)", clientID))

	// Authorization: chỉ gửi nếu có token (hiện tại chưa có token trong config)
	// Nếu device đã activate, server có thể yêu cầu token trong header

	// Bước 0: Kiểm tra xem có connection cũ có thể reuse không
	// REUSE FIRST (go-xiaozhi-main style): if websocket is still alive, always reuse it for STT.
	// If it's not alive, create a new one.
	var conn *websocket.Conn
	var sessionID string
	var connReused bool

	if deviceID != "" {
		storedConn, storedSessionID, exists := xiaozhi.CheckConnectionExists(deviceID)
		connectionValid := exists && storedConn != nil && storedConn.RemoteAddr() != nil && xiaozhi.IsReaderRunning(deviceID)

		if connectionValid {
			// go-xiaozhi-main behavior: if connection is alive, reuse it directly (no ping test).
			conn = storedConn
			sessionID = storedSessionID
			connReused = true
			logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ REUSING existing connection for device %s (sessionID: %s) - websocket still alive", deviceID, sessionID))
		} else {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ℹ️  No valid existing connection for device %s (exists=%v, remoteAddr=%v, readerRunning=%v) - will create new connection",
				deviceID, exists, storedConn != nil && storedConn.RemoteAddr() != nil, xiaozhi.IsReaderRunning(deviceID)))
		}
	}

	// Nếu không có connection để reuse, tạo connection mới
	if !connReused {
		// Check context before creating new connection
		select {
		case <-ctx.Done():
			safeLog("Xiaozhi STT: ⚠️  Context canceled before creating new connection: %v", ctx.Err())
			return "", fmt.Errorf("context canceled before creating new connection: %w", ctx.Err())
		default:
		}

		// Log tất cả headers được gửi để debug
		safeLog("Xiaozhi STT: Connecting to %s with headers:", baseURL)
		for key, values := range headers {
			for _, value := range values {
				safeLog("  %s: %s", key, value)
			}
		}

		var err error
		var resp *http.Response
		var dur time.Duration
		safeLog("Xiaozhi STT: Dialing websocket (timeout=12s, handshakeTimeout=10s)...")
		conn, resp, dur, err = dialWebsocketWithTimeout(ctx, baseURL, headers)
		if err != nil {
			// Check if error is due to context cancellation
			if ctx.Err() != nil {
				safeLog("Xiaozhi STT: ⚠️  Context canceled during connection: %v", ctx.Err())
				return "", fmt.Errorf("context canceled during connection: %w", ctx.Err())
			}
			if resp != nil {
				safeLog("Xiaozhi STT: Failed to connect after %v (handshake status=%s): %v", dur, resp.Status, err)
			} else {
				safeLog("Xiaozhi STT: Failed to connect after %v: %v", dur, err)
			}
			return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
		}
		safeLog("Xiaozhi STT: ✅ New WebSocket connection created")

		// Set PongHandler to automatically respond to server pings
		// This helps keep connection alive - server may send ping and expect pong
		conn.SetPongHandler(func(appData string) error {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Received pong from server for device %s", deviceID))
			return nil
		})

		// Set PingHandler to automatically respond to server pings (if server sends ping)
		conn.SetPingHandler(func(appData string) error {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Received ping from server for device %s, sending pong", deviceID))
			// Respond with pong
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
			conn.SetWriteDeadline(time.Time{})
			return err
		})

		// KHÔNG đóng connection ngay - LLM sẽ dùng lại connection này (giống botkct.py)
		// Connection sẽ được đóng sau khi LLM xong hoặc sau timeout
		// defer conn.Close() // REMOVED - để LLM có thể dùng lại connection
	}
	// Step 1: Send hello event (chỉ nếu tạo connection mới, không cần nếu reuse)
	if !connReused {
		// Python client gửi: type, version, transport, audio_params, features, language
		// NOTE: Vector robot sends Opus audio at 16kHz (PROCESSED_SAMPLE_RATE = 16000)
		// We must send the ACTUAL sample rate of the audio in hello event (16kHz)
		// Server will create Opus decoder with this sample rate and then resample PCM to 24kHz internally
		// If we send 24kHz but audio is 16kHz, Opus decoder will fail!
		helloEvent := map[string]interface{}{
			"type":      "hello",
			"version":   1,
			"transport": "websocket", // ESP32/Python luôn gửi transport: "websocket"
			"features": map[string]interface{}{
				"mcp": true,
				"aec": true,
			},
			"language": "vi", // Vietnamese language (theo Python client)
			"audio_params": map[string]interface{}{
				"format":         "opus",
				"sample_rate":    16000, // Vector robot sends Opus at 16kHz - MUST match actual audio!
				"channels":       1,
				"frame_duration": 60, // Python client dùng 60ms, không phải 20ms
			},
		}
		// Log chi tiết hello event (giống botkct.py để debug)
		helloEventJSON, _ := json.Marshal(helloEvent)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Sending hello event to %s with Device-Id: %s, Client-Id: %s", baseURL, deviceID, clientID))
		logger.Println(fmt.Sprintf("Xiaozhi STT: Hello event JSON: %s", string(helloEventJSON)))
		if err := conn.WriteJSON(helloEvent); err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send hello: %v", err))
			return "", fmt.Errorf("failed to send hello: %w", err)
		}
		logger.Println("Xiaozhi STT: Hello event sent successfully")

		// Step 2: Read hello response
		var helloResp map[string]interface{}
		if err := conn.ReadJSON(&helloResp); err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to read hello response: %v", err))
			return "", fmt.Errorf("failed to read hello response: %w", err)
		}

		// Log chi tiết hello response (giống botkct.py để debug)
		helloRespJSON, _ := json.MarshalIndent(helloResp, "", "  ")
		logger.Println(fmt.Sprintf("Xiaozhi STT: ========== HELLO RESPONSE FROM SERVER =========="))
		logger.Println(fmt.Sprintf("Xiaozhi STT: Hello response JSON:\n%s", string(helloRespJSON)))
		logger.Println(fmt.Sprintf("Xiaozhi STT: Hello response fields:"))
		for key, value := range helloResp {
			logger.Println(fmt.Sprintf("  %s: %v (type: %T)", key, value, value))
		}
		logger.Println(fmt.Sprintf("Xiaozhi STT: ================================================"))

		// Extract session_id from hello response (theo Python client)
		if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
			sessionID = sid
			logger.Println(fmt.Sprintf("Xiaozhi STT: Extracted session_id from hello response: %s", sessionID))
		} else {
			logger.Println("Xiaozhi STT: ⚠️  WARNING - Hello response missing session_id field")
		}
	} else {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Reusing connection, skipping hello event (sessionID: %s)", sessionID))
	}

	// If connection became invalid after reuse check, create new one
	// This handles the case where ConnectionManager closed the connection between reuse check and usage
	if connReused && (conn == nil || conn.RemoteAddr() == nil) {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection became invalid, creating new connection"))
		if deviceID != "" {
			xiaozhi.CloseConnection(deviceID)
		}
		connReused = false
		conn = nil
		sessionID = ""
	}

	// Step 3: Send listen start event
	// botkct.py: message = {"session_id": self.session_id, "type": "listen", "state": "start", "mode": "auto"}
	// go-xiaozhi-main: message = {"type": "listen", "mode": "manual", "state": "start"} (KHÔNG có session_id)
	// Prefer including session_id when available (botkct.py does this and some servers require it on reused sessions).
	listenStart := map[string]interface{}{
		"type":  "listen",
		"state": "start",
		"mode":  "auto", // go-xiaozhi-main dùng "manual", nhưng botkct.py dùng "auto" - giữ "auto" vì phù hợp với Vector robot
	}
	if sessionID != "" {
		listenStart["session_id"] = sessionID
	}
	// Verify connection is still valid before sending listen start
	// ConnectionManager might have closed it between reuse check and now
	if conn == nil || conn.RemoteAddr() == nil {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection became invalid before sending listen start, creating new connection"))
		if connReused && deviceID != "" {
			xiaozhi.CloseConnection(deviceID)
		}
		connReused = false
		conn = nil
		sessionID = ""
		// Create new connection inline (similar to retry logic below)
		logger.Println(fmt.Sprintf("Xiaozhi STT: Creating new connection after reused connection became invalid"))
		var err2 error
		conn, _, _, err2 = dialWebsocketWithTimeout(ctx, baseURL, headers)
		if err2 != nil {
			logger.Println("Xiaozhi STT: Failed to create new connection:", err2)
			return "", fmt.Errorf("failed to create new connection after reused connection became invalid: %w", err2)
		}
		logger.Println("Xiaozhi STT: ✅ New WebSocket connection created after reused connection became invalid")
		// Send hello event for new connection
		helloEvent := map[string]interface{}{
			"type":      "hello",
			"version":   1,
			"transport": "websocket",
			"features": map[string]interface{}{
				"audio": map[string]interface{}{
					"codecs":      []string{"opus"},
					"sample_rate": 16000,
					"channels":    1,
				},
			},
			"language": "vi",
		}
		if err2 := conn.WriteJSON(helloEvent); err2 != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send hello event on new connection: %v", err2))
			conn.Close()
			return "", fmt.Errorf("failed to send hello event on new connection: %w", err2)
		}
		logger.Println("Xiaozhi STT: Hello event sent successfully on new connection")
		// Read hello response
		var helloResp map[string]interface{}
		if err2 := conn.ReadJSON(&helloResp); err2 != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to read hello response on new connection: %v", err2))
			conn.Close()
			return "", fmt.Errorf("failed to read hello response on new connection: %w", err2)
		}
		// Extract session_id from hello response
		if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
			sessionID = sid
			logger.Println(fmt.Sprintf("Xiaozhi STT: Extracted session_id from hello response on new connection: %s", sessionID))
		}
	}

	// Log chi tiết listen start event (giống botkct.py để debug)
	listenStartJSON, _ := json.Marshal(listenStart)
	logger.Println(fmt.Sprintf("Xiaozhi STT: Sending listen start event: %s", string(listenStartJSON)))
	// IMPORTANT: If connection will be stored (deviceID != ""), use direct write for listenStart
	// because connection is not stored yet. After connection is stored, ALWAYS use xiaozhi.WriteJSON/WriteMessage
	var err error
	if connReused && deviceID != "" {
		// Connection already stored, use helper with writeMu
		err = xiaozhi.WriteJSON(deviceID, listenStart)
	} else {
		// New connection, not stored yet - use direct write (no concurrent writes yet)
		err = conn.WriteJSON(listenStart)
	}
	if err != nil {
		logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen start: %v", err))
		// If reusing connection and it fails, connection is invalid - remove it and create new one
		if connReused && deviceID != "" {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reused connection is invalid for device %s, removing from manager and creating new connection", deviceID))
			xiaozhi.CloseConnection(deviceID) // Close and remove invalid connection
			// Retry with new connection
			connReused = false
			// Create new connection
			logger.Println(fmt.Sprintf("Xiaozhi STT: Creating new connection after reused connection failed"))
			var err2 error
			conn, _, _, err2 = dialWebsocketWithTimeout(ctx, baseURL, headers)
			if err2 != nil {
				logger.Println("Xiaozhi STT: Failed to create new connection:", err2)
				return "", fmt.Errorf("failed to create new connection after reused connection failed: %w", err2)
			}
			logger.Println("Xiaozhi STT: ✅ New WebSocket connection created after reused connection failed")
			// Send hello event for new connection
			helloEvent := map[string]interface{}{
				"type":      "hello",
				"version":   1,
				"transport": "websocket",
				"features": map[string]interface{}{
					"mcp": true,
					"aec": true,
				},
				"language": "vi",
				"audio_params": map[string]interface{}{
					"format":         "opus",
					"sample_rate":    16000,
					"channels":       1,
					"frame_duration": 60,
				},
			}
			if err2 := conn.WriteJSON(helloEvent); err2 != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send hello: %v", err2))
				return "", fmt.Errorf("failed to send hello: %w", err2)
			}
			var helloResp map[string]interface{}
			if err2 := conn.ReadJSON(&helloResp); err2 != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to read hello response: %v", err2))
				return "", fmt.Errorf("failed to read hello response: %w", err2)
			}
			if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
				sessionID = sid
				logger.Println(fmt.Sprintf("Xiaozhi STT: Extracted session_id from hello response: %s", sessionID))
			}
			// Retry sending listen start
			if err2 := conn.WriteJSON(listenStart); err2 != nil {
				logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send listen start on new connection: %v", err2))
				return "", fmt.Errorf("failed to send listen start: %w", err2)
			}
		} else {
			return "", fmt.Errorf("failed to send listen start: %w", err)
		}
	}
	logger.Println("Xiaozhi STT: Listen start event sent successfully, ready to receive audio")

	// Verify connection is still valid after sending listen start
	if conn.RemoteAddr() == nil {
		logger.Println("Xiaozhi STT: ⚠️  Connection is invalid after sending listen start, cannot proceed")
		if deviceID != "" && !connReused {
			conn.Close()
		} else if deviceID != "" {
			xiaozhi.CloseConnection(deviceID)
		}
		return "", fmt.Errorf("connection is invalid after sending listen start")
	}

	// Step 4: Setup STT handler and channels for async communication
	done := make(chan struct{})
	var doneOnce sync.Once
	signalDone := func() {
		doneOnce.Do(func() {
			close(done)
		})
	}
	transcriptChan := make(chan string, 10) // Increased buffer to 10 to handle long speech transcripts
	errChan := make(chan error, 1)
	errorOccurred := make(chan struct{}, 1)
	connectionFailed := make(chan bool, 1)

	// Create STT handler instance
	sttHandler := &STTHandler{
		transcriptChan:     transcriptChan,
		errChan:            errChan,
		errorOccurred:      errorOccurred,
		active:             true,
		transcriptReceived: false,
	}

	// Store connection and start reader goroutine (if new connection)
	// If reusing connection, reader goroutine is already running
	if !connReused && deviceID != "" {
		// Store connection with retry logic (in case of race condition with LLM release)
		maxRetries := 3
		retryInterval := 200 * time.Millisecond
		var storeErr error
		for retry := 0; retry < maxRetries; retry++ {
			storeErr = xiaozhi.StoreConnection(deviceID, conn, sessionID)
			if storeErr == nil {
				// Successfully stored
				break
			}
			// Check if error is "in use" - this means LLM hasn't fully released yet
			if strings.Contains(storeErr.Error(), "in use by LLM") {
				if retry < maxRetries-1 {
					safeLog("Xiaozhi STT: ⚠️  Connection still in use (retry %d/%d), waiting %v before retry...", retry+1, maxRetries, retryInterval)
					// Wait a bit before retrying
					select {
					case <-ctx.Done():
						conn.Close()
						return "", fmt.Errorf("context canceled while retrying to store connection: %w", ctx.Err())
					case <-time.After(retryInterval):
						// Continue to retry
					}
				} else {
					// Last retry failed
					safeLog("Xiaozhi STT: ⚠️  Failed to store connection after %d retries: %v", maxRetries, storeErr)
					conn.Close()
					return "", fmt.Errorf("failed to store connection after %d retries: %w", maxRetries, storeErr)
				}
			} else {
				// Different error - don't retry
				safeLog("Xiaozhi STT: ⚠️  Failed to store connection (non-retryable error): %v", storeErr)
				conn.Close()
				return "", fmt.Errorf("failed to store connection: %w", storeErr)
			}
		}
		if storeErr != nil {
			// All retries failed
			safeLog("Xiaozhi STT: ⚠️  Failed to store connection after all retries: %v", storeErr)
			conn.Close()
			return "", fmt.Errorf("failed to store connection after all retries: %w", storeErr)
		}
		safeLog("Xiaozhi STT: Stored NEW connection for device %s (sessionID: %s) - reader goroutine started", deviceID, sessionID)
		// IMPORTANT: After connection is stored, ping goroutine is running
		// All subsequent writes MUST use xiaozhi.WriteMessage/WriteJSON to avoid concurrent write errors
		connReused = true // Mark as "reused" so all writes use writeMu
	}

	// Register STT handler with connection manager
	// Ensure handler is active when registering (in case it was deactivated from previous request)
	if deviceID != "" {
		// IMPORTANT: Prevent upstream "auto TTS" (sent immediately after listen stop) from being played
		// for locally-handled intents.
		//
		// We keep the handler pointer but deactivate it at the start of each STT turn; KG flow will
		// explicitly register/activate a fresh LLM handler when it needs TTS playback.
		if h := xiaozhi.GetLLMHandler(deviceID); h != nil {
			h.SetActive(false)
		}

		sttHandler.SetActive(true) // Ensure handler is active when registering
		xiaozhi.SetSTTHandler(deviceID, sttHandler)
		logger.Println(fmt.Sprintf("Xiaozhi STT: STT handler registered for device %s (active: true)", deviceID))
	}

	// Step 5: Audio sending goroutine (no separate reader goroutine - using connection manager's reader)
	// Similar to Vosk STT - accumulate audio and only send after user finishes speaking
	// Note: Python client gửi audio streaming liên tục, nhưng Go tích lũy và gửi sau end-of-speech
	// để phù hợp với Vector robot behavior (giống Vosk STT)
	go func() {
		defer func() {
			// Close channels when done
			defer func() {
				if r := recover(); r != nil {
					logger.Println(fmt.Sprintf("Xiaozhi STT: Recovered from panic while closing channels: %v", r))
				}
			}()
			close(transcriptChan)
			close(errChan)
		}()

		listenStopSent := false // Flag để đảm bảo chỉ gửi listen stop event một lần
		sendListenStop := func(reason string) {
			if listenStopSent {
				return
			}
			listenStop := map[string]interface{}{
				"type":  "listen",
				"state": "stop",
				"mode":  "auto",
			}
			if sessionID != "" {
				listenStop["session_id"] = sessionID
			}
			listenStopJSON, _ := json.Marshal(listenStop)
			logger.Println(fmt.Sprintf("Xiaozhi STT: Sending listen stop (%s): %s", reason, string(listenStopJSON)))
			// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
			if deviceID != "" {
				_ = xiaozhi.WriteJSON(deviceID, listenStop)
			} else if conn != nil {
				_ = conn.WriteJSON(listenStop)
			}
			listenStopSent = true
		}
		defer sendListenStop("defer")

		// Initialize VAD detection
		sreq.DetectEndOfSpeech()

		reconnectAttempts := 0
		reconnectAndRestartListen := func() error {
			if deviceID == "" {
				return fmt.Errorf("no deviceID; cannot reconnect via manager")
			}
			reconnectAttempts++
			logger.Println(fmt.Sprintf("Xiaozhi STT: 🔁 Reconnecting websocket and restarting listen (attempt %d)...", reconnectAttempts))

			// Ensure stale entry is removed.
			xiaozhi.CloseConnection(deviceID)

			newConn, _, dur, err := dialWebsocketWithTimeout(ctx, baseURL, headers)
			if err != nil {
				return fmt.Errorf("reconnect dial failed after %v: %w", dur, err)
			}

			// Hello handshake (same as initial connect)
			helloEvent := map[string]interface{}{
				"type":      "hello",
				"version":   1,
				"transport": "websocket",
				"features": map[string]interface{}{
					"mcp": true,
					"aec": true,
				},
				"language": "vi",
				"audio_params": map[string]interface{}{
					"format":         "opus",
					"sample_rate":    16000,
					"channels":       1,
					"frame_duration": 60,
				},
			}
			if err := newConn.WriteJSON(helloEvent); err != nil {
				newConn.Close()
				return fmt.Errorf("reconnect hello write failed: %w", err)
			}
			var helloResp map[string]interface{}
			if err := newConn.ReadJSON(&helloResp); err != nil {
				newConn.Close()
				return fmt.Errorf("reconnect hello read failed: %w", err)
			}
			if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
				sessionID = sid
				logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Reconnect got new session_id: %s", sessionID))
			} else {
				logger.Println("Xiaozhi STT: ⚠️  Reconnect hello response missing session_id")
			}

			// Send listen start (include session_id when available)
			listenStart := map[string]interface{}{
				"type":  "listen",
				"state": "start",
				"mode":  "auto",
			}
			if sessionID != "" {
				listenStart["session_id"] = sessionID
			}
			if err := newConn.WriteJSON(listenStart); err != nil {
				newConn.Close()
				return fmt.Errorf("reconnect listen start failed: %w", err)
			}

			// Store + re-register handler so routing/writes work.
			if err := xiaozhi.StoreConnection(deviceID, newConn, sessionID); err != nil {
				newConn.Close()
				return fmt.Errorf("reconnect store connection failed: %w", err)
			}
			sttHandler.SetActive(true)
			xiaozhi.SetSTTHandler(deviceID, sttHandler)

			// Best-effort: update local conn handle for RemoteAddr checks.
			conn = newConn
			connReused = true

			logger.Println("Xiaozhi STT: ✅ Reconnect complete; continuing to stream audio")
			return nil
		}

		chunkCount := 0 // Đếm số chunks đã gửi để log

		// KHÔNG gửi FirstReq (OpusHead/OpusTags) vì:
		// 1. go-xiaozhi-main KHÔNG gửi OpusHead/OpusTags - chỉ gửi OPUS audio frames
		// 2. Server đã biết format từ hello event (audio_params)
		// 3. FirstReq (3840 bytes) có thể chứa OpusHead/OpusTags mà server không mong đợi
		// if len(sreq.FirstReq) > 0 {
		// 	logger.Println(fmt.Sprintf("Xiaozhi STT: Skipping FirstReq (%d bytes) - server doesn't expect OpusHead/OpusTags", len(sreq.FirstReq)))
		// }

		// Tạo OPUS encoder để re-encode PCM → OPUS frames (16kHz, mono, VoIP)
		// Vector robot gửi OGG packets, nhưng server mong đợi raw OPUS frames
		opusEncoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
		if err != nil {
			logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to create OPUS encoder: %v", err))
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v", r))
					}
				}()
				select {
				case errChan <- fmt.Errorf("failed to create OPUS encoder: %w", err):
				default:
				}
			}()
			return
		}
		logger.Println("Xiaozhi STT: OPUS encoder created (16kHz, mono, VoIP) for OGG → OPUS conversion")

		// Frame size: 60ms @ 16kHz = 960 samples
		frameSize := 960
		pcmBuffer := []int16{} // Buffer để tích lũy PCM samples

		// Thêm delay nhỏ sau listen start để server sẵn sàng nhận audio (giống go-xiaozhi-main có delay 50ms)
		time.Sleep(50 * time.Millisecond)
		logger.Println("Xiaozhi STT: Ready to send audio chunks (after 50ms delay)")

	continueLoop:
		for {
			select {
			case <-done:
				return
			case <-errorOccurred:
				// Server đã trả về error, dừng gửi audio chunks
				logger.Println("Xiaozhi STT: Error occurred, stopping audio chunk sending")
				return
			default:
				chunk, err := sreq.GetNextStreamChunkOpus()
				// Update last audio chunk time when we receive audio from robot
				// This indicates robot is listening and sending audio
				if err == nil && len(chunk) > 0 && deviceID != "" {
					xiaozhi.UpdateLastAudioChunkTime(deviceID)
				}
				if err != nil {
					if err == io.EOF {
						logger.Println(fmt.Sprintf("Xiaozhi STT: End of audio stream (EOF) detected after %d chunks", chunkCount))
						sendListenStop("EOF")
						signalDone()
						return
					}
					// Check if error is "context canceled" or "DeadlineExceeded" - this is not a real error, just user cancel or timeout
					// Don't send to errChan, just return empty transcript (connection will be kept for reuse)
					errStr := err.Error()
					if strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "DeadlineExceeded") || strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "Canceled") {
						// CRITICAL: Nếu chưa có audio chunks nào (chunkCount == 0), có thể là timeout quá sớm
						// (ví dụ: user vừa mở mic, chưa kịp nói). Trong trường hợp này, đợi thêm một chút
						// để user có cơ hội nói trước khi return empty transcript
						if chunkCount == 0 {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Context canceled/timeout with 0 chunks - likely timeout too early after mic opened. Waiting 2s more for user to speak..."))
							// Đợi thêm 2 giây để user có cơ hội nói
							time.Sleep(2 * time.Second)
							// Thử lấy thêm một chunk nữa
							retryChunk, retryErr := sreq.GetNextStreamChunkOpus()
							if retryErr == nil && len(retryChunk) > 0 {
								// Có audio! Xử lý chunk này ngay
								logger.Println(fmt.Sprintf("Xiaozhi STT: ✅ Got audio chunk after retry, processing..."))
								chunk = retryChunk // Sử dụng chunk từ retry
								chunkCount++
								if deviceID != "" {
									xiaozhi.UpdateLastAudioChunkTime(deviceID)
								}
								// Tiếp tục xử lý chunk này (không return, tiếp tục vòng lặp)
								// Chunk sẽ được xử lý ở phần code phía dưới (encode và gửi)
							} else {
								// Vẫn không có audio sau retry
								// KHÔNG gửi "" vào transcriptChan - để vòng wait tự timeout hoặc đợi transcript thật từ server
								logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Still no audio after retry (context canceled/timeout) - stopping audio sending, will wait for server or timeout"))
								// Signal done to stop audio sending loop
								signalDone()
								return
							}
						} else {
							// Context canceled với chunks đã gửi - KHÔNG gửi "" vào transcriptChan
							// Để vòng wait tự timeout hoặc đợi transcript thật từ server
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Context canceled while getting audio chunk (user cancel or timeout) after %d chunks - stopping audio sending, will wait for server or timeout", chunkCount))
							// Signal done to stop audio sending loop
							signalDone()
							return
						}
					}
					// Try to send error, but don't panic if channel is closed
					logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to get audio chunk: %v (type: %T)", err, err))
					func() {
						defer func() {
							if r := recover(); r != nil {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v, Original error: %v", r, err))
							}
						}()
						select {
						case errChan <- err:
							logger.Println("Xiaozhi STT: Error sent to errChan successfully")
						default:
							// Channel might be closed or full, just log
							logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - errChan is full or closed, cannot send error: %v", err))
						}
					}()
					return
				}

				// Check for end-of-speech detection
				speechIsDone, doProcess := sreq.DetectEndOfSpeech()

				// Gửi audio chunk ngay lập tức nếu doProcess (giống botkct.py - streaming liên tục)
				// botkct.py gửi mỗi OPUS frame ngay khi encode xong, không tích lũy
				// LƯU Ý: Vector robot gửi OGG packets (có thể chứa nhiều OPUS frames)
				// Server mong đợi raw OPUS frames, không phải OGG packets
				// Giải pháp: Decode OGG → PCM → Re-encode thành OPUS frames
				if doProcess {
					// Kiểm tra error trước khi gửi
					select {
					case <-errorOccurred:
						logger.Println("Xiaozhi STT: Error occurred before sending audio chunk, stopping")
						return
					default:
					}

					chunkCount++

					// Kiểm tra xem có phải OGG format không (OGG bắt đầu với "OggS")
					isOGG := len(chunk) >= 4 && chunk[0] == 0x4f && chunk[1] == 0x67 && chunk[2] == 0x67 && chunk[3] == 0x53

					if isOGG {
						// Decode OGG → PCM
						// LƯU Ý: OpusStream có thể bị corrupt nếu OGG packets bị fragment hoặc incomplete
						// Khi reuse connection, cần đảm bảo OpusStream state được reset đúng cách
						decodedPCM := sreq.OpusDecode(chunk)
						if len(decodedPCM) == 0 {
							// Skip empty chunks (có thể do lỗi decode hoặc corrupt stream)
							// Chunk #1 thường là OGG header/metadata, không chứa audio - đây là bình thường
							// Chỉ log warning nếu không phải chunk đầu tiên hoặc nếu có nhiều empty chunks liên tiếp
							if chunkCount == 1 {
								// Chunk #1 thường là OGG header - không cần log warning
								logger.Println(fmt.Sprintf("Xiaozhi STT: ℹ️  Skipping chunk #1 (OGG header/metadata, %d bytes) - this is normal", len(chunk)))
							} else if chunkCount%50 == 0 {
								// Log mỗi 50 chunks để debug nếu có nhiều empty chunks
								logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Skipping empty decoded chunk (chunk #%d, %d bytes) - may be corrupt OGG packet", chunkCount, len(chunk)))
							}
							continue
						}

						// Validate decoded PCM data
						if len(decodedPCM)%2 != 0 {
							// Invalid PCM data (must be even number of bytes for int16 samples)
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Invalid PCM data length (%d bytes, not even) - skipping chunk #%d", len(decodedPCM), chunkCount))
							continue
						}

						// Convert PCM bytes → int16 samples
						samples := make([]int16, len(decodedPCM)/2)
						for i := 0; i < len(decodedPCM)/2; i++ {
							samples[i] = int16(binary.LittleEndian.Uint16(decodedPCM[i*2:]))
						}

						// Thêm samples vào buffer
						pcmBuffer = append(pcmBuffer, samples...)

						// Encode PCM → OPUS frames (60ms = 960 samples @ 16kHz)
						// Gửi từng OPUS frame riêng biệt (giống botkct.py)
						for len(pcmBuffer) >= frameSize {
							frameSamples := pcmBuffer[:frameSize]
							pcmBuffer = pcmBuffer[frameSize:]

							// Encode frame thành OPUS
							opusFrame := make([]byte, 1275) // Max OPUS frame size
							n, err := opusEncoder.Encode(frameSamples, opusFrame)
							if err != nil {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to encode OPUS frame: %v", err))
								continue
							}

							if n > 0 {
								// Check connection validity before sending
								if conn.RemoteAddr() == nil {
									logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection is invalid (RemoteAddr is nil) before sending OPUS frame"))
									if deviceID != "" && reconnectAttempts < 2 {
										if rerr := reconnectAndRestartListen(); rerr == nil {
											// continue to send below (via WriteMessage)
										} else {
											logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reconnect failed: %v", rerr))
										}
									}
									// If still invalid, stop.
									if conn == nil || conn.RemoteAddr() == nil {
										select {
										case connectionFailed <- true:
										default:
										}
										select {
										case errorOccurred <- struct{}{}:
										default:
										}
										return
									}
								}
								// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
								// to avoid concurrent write errors (ping goroutine is running)
								var err error
								if deviceID != "" {
									// Connection is stored, use helper with writeMu
									err = xiaozhi.WriteMessage(deviceID, websocket.BinaryMessage, opusFrame[:n])
								} else {
									// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
									err = conn.WriteMessage(websocket.BinaryMessage, opusFrame[:n])
								}
								if err != nil {
									// Check if connection was closed gracefully (don't log as error)
									if strings.Contains(err.Error(), "close sent") || strings.Contains(err.Error(), "use of closed network connection") {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection closed while sending OPUS frame, stopping audio sending"))
									} else {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send OPUS frame (%d bytes): %v", n, err))
									}
									// Attempt reconnect + retry once for common "session alive but conn dropped" failures.
									if deviceID != "" && reconnectAttempts < 2 &&
										(strings.Contains(err.Error(), "connection not found") || strings.Contains(err.Error(), "connection is closed") || strings.Contains(err.Error(), "close 1005")) {
										if rerr := reconnectAndRestartListen(); rerr == nil {
											if rerr2 := xiaozhi.WriteMessage(deviceID, websocket.BinaryMessage, opusFrame[:n]); rerr2 == nil {
												continue
											} else {
												logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Retry send after reconnect failed: %v", rerr2))
											}
										} else {
											logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reconnect failed: %v", rerr))
										}
									}

									// Mark connection as failed
									select {
									case connectionFailed <- true:
									default:
									}
									select {
									case errorOccurred <- struct{}{}:
									default:
									}
									return
								}

								// Log mỗi 10 frames để tránh spam logs
								if chunkCount%10 == 0 || chunkCount == 1 {
									logger.Println(fmt.Sprintf("Xiaozhi STT: Sent OPUS frame %d (from OGG chunk %d): %d bytes", chunkCount, chunkCount, n))
								}

								// Thêm delay nhỏ giữa các frames (giống botkct.py có sleep 0.01s)
								time.Sleep(10 * time.Millisecond)
							}
						}
					} else {
						// Không phải OGG format, gửi trực tiếp (có thể đã là raw OPUS)
						if chunkCount == 1 {
							logger.Println(fmt.Sprintf("Xiaozhi STT: First audio chunk: %d bytes (not OGG format, sending directly)", len(chunk)))
						}

						// Check connection validity before sending
						if conn.RemoteAddr() == nil {
							logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection is invalid (RemoteAddr is nil), stopping audio sending"))
							select {
							case connectionFailed <- true:
							default:
							}
							select {
							case errorOccurred <- struct{}{}:
							default:
							}
							return
						}
						// IMPORTANT: After connection is stored, ALWAYS use helper function with writeMu
						// to avoid concurrent write errors (ping goroutine is running)
						var err error
						if deviceID != "" {
							// Connection is stored, use helper with writeMu
							err = xiaozhi.WriteMessage(deviceID, websocket.BinaryMessage, chunk)
						} else {
							// No deviceID, connection not stored - use direct write (shouldn't happen in normal flow)
							err = conn.WriteMessage(websocket.BinaryMessage, chunk)
						}
						if err != nil {
							// Check if connection was closed gracefully (don't log as error)
							if strings.Contains(err.Error(), "close sent") || strings.Contains(err.Error(), "use of closed network connection") {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Connection closed while sending audio chunk, stopping audio sending"))
							} else {
								logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send audio chunk (%d bytes): %v", len(chunk), err))
							}
							// Attempt reconnect + retry once for common "session alive but conn dropped" failures.
							if deviceID != "" && reconnectAttempts < 2 &&
								(strings.Contains(err.Error(), "connection not found") || strings.Contains(err.Error(), "connection is closed") || strings.Contains(err.Error(), "close 1005")) {
								if rerr := reconnectAndRestartListen(); rerr == nil {
									if rerr2 := xiaozhi.WriteMessage(deviceID, websocket.BinaryMessage, chunk); rerr2 == nil {
										continue
									} else {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Retry send (raw chunk) after reconnect failed: %v", rerr2))
									}
								} else {
									logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  Reconnect failed: %v", rerr))
								}
							}

							// Mark connection as failed
							select {
							case connectionFailed <- true:
							default:
							}
							// Signal error occurred
							select {
							case errorOccurred <- struct{}{}:
							default:
							}
							// Try to send error, but don't panic if channel is closed
							func() {
								defer func() {
									if r := recover(); r != nil {
										logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - Failed to send error to errChan (recovered from panic): %v, Original error: %v", r, err))
									}
								}()
								select {
								case errChan <- fmt.Errorf("failed to send audio: %w", err):
									logger.Println("Xiaozhi STT: Error sent to errChan successfully")
								default:
									logger.Println(fmt.Sprintf("Xiaozhi STT: ERROR - errChan is full or closed, cannot send error: %v", err))
								}
							}()
							return
						}

						// Log mỗi 10 chunks để tránh spam logs
						if chunkCount%10 == 0 || chunkCount == 1 {
							logger.Println(fmt.Sprintf("Xiaozhi STT: Sent audio chunk %d (streaming continuously like botkct.py): %d bytes", chunkCount, len(chunk)))
						}
						// Thêm delay nhỏ giữa các chunks (giống botkct.py có sleep 0.01s trong audio_streaming_loop)
						time.Sleep(10 * time.Millisecond)
					}
				}

				// Nếu speech is done, gửi listen stop và dừng (chỉ gửi một lần)
				if speechIsDone && !listenStopSent {
					// Kiểm tra xem có error từ server không trước khi gửi audio
					select {
					case <-errorOccurred:
						logger.Println("Xiaozhi STT: Error occurred before sending audio, aborting")
						return
					default:
					}

					// CRITICAL: Kiểm tra xem có audio input thực sự từ user không
					// Nếu chunkCount quá nhỏ (< 10), có thể là robot tự phát hiện "end of speech" quá sớm
					// (ví dụ: ngay sau khi TTS dừng, robot chưa kịp nghe user nói)
					// Trong trường hợp này, KHÔNG gửi listen stop event, tiếp tục đợi audio từ user
					if chunkCount < 10 {
						logger.Println(fmt.Sprintf("Xiaozhi STT: ⚠️  End of speech detected too early (only %d chunks) - likely false positive after TTS stop. Ignoring and continuing to wait for user audio...", chunkCount))
						// Không gửi listen stop event, tiếp tục vòng lặp để đợi audio từ user
						// Note: DetectEndOfSpeech() sẽ tự reset sau một khoảng thời gian
						// Sử dụng goto để quay lại đầu vòng lặp (không thể dùng continue trong select)
						goto continueLoop
					}

					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected after %d chunks (user likely finished speaking). Audio was already streamed continuously (like botkct.py). Sending listen stop event...", chunkCount))

					sendListenStop("end_of_speech")

					// Wait longer for server to process long speech (increased from 2s to 5s)
					// This gives server more time to process long audio and send transcript
					// Server may need more time for long/complex speech recognition
					time.Sleep(5 * time.Second)
					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected, waited 5s for server to process. Will continue waiting for transcript (up to 180s total timeout)"))

					// KHÔNG đóng connection ở đây - LLM reader sẽ tiếp tục đọc từ connection này
					// Chỉ dừng gửi audio chunks, nhưng tiếp tục đọc messages để LLM reader có thể xử lý
					logger.Println(fmt.Sprintf("Xiaozhi STT: End of speech detected, stopping audio chunk sending. Connection will be managed by LLM reader."))

					// Chỉ dừng gửi audio chunks, không return - STT reader sẽ tiếp tục đọc messages
					// LLM reader sẽ đọc và xử lý LLM/TTS events từ connection này
					// Stop audio sending immediately after listen stop to avoid sending frames after stop
					// (this can cause upstream to return empty transcript).
					signalDone()
					return
					// KHÔNG return ở đây - để STT reader tiếp tục đọc messages cho LLM reader
					// Connection sẽ được đóng bởi LLM reader khi xong
				}
			}
		}
	}()

	// Step 7: Wait for transcript or error
	// Increased timeout to 180s to handle long speech processing
	// This gives server ample time to process long/complex speech and return transcript
	safeLog("Xiaozhi STT: Waiting for transcript or error (timeout: 180s)")
	timeout := time.NewTimer(180 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case transcript := <-transcriptChan:
			// CRITICAL: Ignore empty transcripts and keep waiting.
			// Upstream can emit empty stt events or our internal cancel paths can push "".
			// Returning early here causes "no STT" on subsequent attempts.
			if transcript == "" {
				safeLog("Xiaozhi STT: ⚠️  Empty transcript received (ignored). Continuing to wait for non-empty transcript for device %s (sessionID: %s)", sreq.Device, sessionID)
				continue
			}

			safeLog("Xiaozhi STT: SUCCESS - Received transcript for device %s: %s", sreq.Device, transcript)

			// Check if connection failed before storing
			failed := false
			select {
			case failed = <-connectionFailed:
			default:
			}

			// Connection already stored in manager (if new connection) or already exists (if reused)
			if deviceID != "" {
				// Check if connection is actually closed or failed
				if failed || conn.RemoteAddr() == nil {
					safeLog("Xiaozhi STT: ⚠️  Connection failed or invalid, closing")
					// Close and remove invalid connection
					if connReused {
						xiaozhi.CloseConnection(deviceID)
					} else {
						conn.Close()
					}
				} else {
					// Non-empty transcript - deactivate STT handler, LLM will take over
					sttHandler.SetActive(false)
					safeLog("Xiaozhi STT: STT handler deactivated for device %s (sessionID: %s) - connection kept for LLM", deviceID, sessionID)
				}
			} else {
				// Nếu không có deviceID, đóng connection ngay
				safeLog("Xiaozhi STT: No deviceID, closing connection immediately")
				conn.Close()
			}
			return transcript, nil

		case err := <-errChan:
			// Check if error is "DeadlineExceeded" or "context canceled" - treat as empty transcript, don't close connection
			errStr := ""
			if err != nil {
				errStr = err.Error()
			}
			if strings.Contains(errStr, "DeadlineExceeded") || strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "Canceled") {
				safeLog("Xiaozhi STT: ⚠️  DeadlineExceeded/context canceled for device %s: %v - returning empty transcript, keeping connection", sreq.Device, err)
				// Don't close connection - let it be reused for next request
				return "", nil // Return empty transcript, not error
			}
			safeLog("Xiaozhi STT: ERROR - Received error from errChan for device %s: %v (type: %T)", sreq.Device, err, err)
			// Đóng connection nếu có lỗi thực sự
			if deviceID != "" {
				xiaozhi.CloseConnection(deviceID) // Đóng connection khi có lỗi
			} else {
				conn.Close()
			}
			return "", err

		case <-ctx.Done():
			// Context canceled - this is not a real error, just user cancel or timeout
			// Don't close connection, just return empty transcript (connection will be kept for reuse)
			safeLog("Xiaozhi STT: ⚠️  Context canceled for device %s: %v - returning empty transcript, keeping connection", sreq.Device, ctx.Err())
			// Don't close connection - let it be reused for next request
			return "", nil // Return empty transcript, not error

		case <-timeout.C:
			safeLog("Xiaozhi STT: ERROR - Timeout waiting for transcript for device %s (180s)", sreq.Device)
			// Đóng connection nếu timeout
			if deviceID != "" {
				xiaozhi.CloseConnection(deviceID) // Đóng connection khi timeout
			} else {
				conn.Close()
			}
			return "", fmt.Errorf("timeout waiting for transcript")
		}
	}
}
