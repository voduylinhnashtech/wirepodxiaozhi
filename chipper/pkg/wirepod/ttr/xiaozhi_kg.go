package wirepod_ttr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fforchino/vector-go-sdk/pkg/vector"
	"github.com/fforchino/vector-go-sdk/pkg/vectorpb"
	"github.com/gorilla/websocket"
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	"gopkg.in/hraban/opus.v2"
)

const (
	// Vector's ExternalAudioStreamPlayback in this codebase is exercised via /api-sdk/play_sound,
	// which sends 1024-byte PCM chunks with ~60ms pacing at 8kHz mono 16-bit.
	// Using the same chunk size here improves reliability (some robots seem to ignore smaller chunks).
	vectorAudioChunkBytes = 1024
	// Prefer 16kHz for Vector ExternalAudioStreamPlayback.
	// This matches the OpenAI TTS path in this repo and tends to be more reliable than 8kHz on some builds.
	vectorAudioFrameRate = 16000
	vectorAudioVolume    = 80
	// Default chunk pacing (~25ms). One 1024-byte 16kHz mono chunk ≈ 32ms of audio;
	// slightly faster pacing helps keep the robot's buffer fed. On slow/loaded hosts
	// (e.g. TV boxes) scheduler jitter can make sleeps irregular — override with
	// XIAOZHI_VECTOR_CHUNK_PACE_MS (e.g. 32–40) to align closer to real-time and reduce stutter.
	vectorChunkPaceDefault = 25 * time.Millisecond
)

// getVectorChunkPace returns delay between ExternalAudioStream chunks sent to the robot.
// Set XIAOZHI_VECTOR_CHUNK_PACE_MS to an integer in milliseconds (10–150; empty = default 25).
var getVectorChunkPace = sync.OnceValue(func() time.Duration {
	v := strings.TrimSpace(os.Getenv("XIAOZHI_VECTOR_CHUNK_PACE_MS"))
	if v == "" {
		return vectorChunkPaceDefault
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 10 || ms > 150 {
		return vectorChunkPaceDefault
	}
	return time.Duration(ms) * time.Millisecond
})

// getKGLLMResponseTimeout is how long we wait for the first LLM text (llm or tts sentence_start)
// before failing with "timeout waiting for response" → robot says to check web logs.
// Override with XIAOZHI_KG_LLM_TIMEOUT_SEC (15–180, empty = 30). Increase on slow networks or busy servers.
var getKGLLMResponseTimeout = sync.OnceValue(func() time.Duration {
	v := strings.TrimSpace(os.Getenv("XIAOZHI_KG_LLM_TIMEOUT_SEC"))
	if v == "" {
		return 30 * time.Second
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 15 || sec > 180 {
		return 30 * time.Second
	}
	return time.Duration(sec) * time.Second
})

// After replyStarted (tts/audio), optional wait for text for logging/return value (ESP32 may stream audio first).
var getKGTextAfterAudioTimeout = sync.OnceValue(func() time.Duration {
	v := strings.TrimSpace(os.Getenv("XIAOZHI_KG_TEXT_AFTER_AUDIO_SEC"))
	if v == "" {
		return 90 * time.Second
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 5 || sec > 300 {
		return 90 * time.Second
	}
	return time.Duration(sec) * time.Second
})

func xiaozhiDebugAudio() bool {
	return os.Getenv("XIAOZHI_DEBUG_AUDIO") == "1"
}

// pcmAbsStats16LE returns simple amplitude stats for signed 16-bit little-endian PCM.
// Useful to distinguish "we sent chunks" vs "we sent (near) silence".
func pcmAbsStats16LE(pcm []byte) (peakAbs int32, avgAbs int32) {
	if len(pcm) < 2 {
		return 0, 0
	}
	var sumAbs int64
	var peak int32
	n := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		v := int32(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
		sumAbs += int64(v)
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return peak, int32(sumAbs / int64(n))
}

// xiaozhiTTSGain returns a multiplicative gain applied to 16-bit PCM before sending to Vector.
// Default is 4.0 (per user request). You can override via env XIAOZHI_TTS_GAIN (e.g. "2.5").
func xiaozhiTTSGain() float64 {
	// Explicit env override always wins (useful for quick tuning without UI changes).
	v := strings.TrimSpace(os.Getenv("XIAOZHI_TTS_GAIN"))
	if v == "" {
		// Use Knowledge Graph config when provider is xiaozhi.
		if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
			switch strings.ToLower(strings.TrimSpace(vars.APIConfig.Knowledge.XiaozhiTTSVolume)) {
			case "high":
				return 4.0
			case "medium":
				return 2.0
			case "normal", "":
				return 1.0
			default:
				// Backward/advanced: allow numeric string (e.g. "3", "4.5").
				if f, err := strconv.ParseFloat(vars.APIConfig.Knowledge.XiaozhiTTSVolume, 64); err == nil && f > 0 {
					if f > 10 {
						return 10
					}
					return f
				}
				return 1.0
			}
		}
		return 1.0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 1.0
	}
	// Keep sane bounds.
	if f > 10 {
		return 10
	}
	return f
}

// applyGainSoftLimit16LE applies gain then a soft limiter (tanh) to avoid hard clipping/distortion.
// Input/output are signed 16-bit little-endian PCM.
func applyGainSoftLimit16LE(pcm []byte, gain float64) []byte {
	if gain <= 1.0 || len(pcm) < 2 {
		return pcm
	}
	// Copy-on-write to avoid mutating shared buffers.
	out := make([]byte, len(pcm))
	// Scale to keep a bit of headroom.
	const outScale = 0.98
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		x := float64(s) / 32768.0
		y := math.Tanh(x*gain) * outScale
		iv := int32(math.Round(y * 32767.0))
		if iv > math.MaxInt16 {
			iv = math.MaxInt16
		} else if iv < math.MinInt16 {
			iv = math.MinInt16
		}
		binary.LittleEndian.PutUint16(out[i:i+2], uint16(int16(iv)))
	}
	return out
}

// AudioQueue manages audio playback serialization per robot
type AudioQueue struct {
	ESN                   string
	AudioDone             chan bool
	AudioCurrentlyPlaying bool
	HasPlayedAudio        bool // Track if this robot has ever played audio (for warm-up delay)
}

var AudioQueues []AudioQueue
var audioQueueMutex sync.Mutex

// AudioClientPool stores persistent audio clients per robot (ESN)
// This keeps audio pipeline always warm, eliminating warm-up delays
type AudioClientEntry struct {
	Client interface {
		Send(*vectorpb.ExternalAudioStreamRequest) error
	}
	Ctx       context.Context
	Cancel    context.CancelFunc
	Robot     *vector.Vector // Keep robot reference to recreate if needed
	LastUsed  time.Time      // Track when client was last used (for detecting stale clients)
	SessionID string         // Track session ID when client was created (to detect websocket reconnect)
}

var audioClientPool = make(map[string]*AudioClientEntry) // key: ESN
var audioClientPoolMutex sync.RWMutex

// LLMHandler implements MessageHandler interface for LLM
// This handler processes LLM/TTS-related messages from the single reader goroutine
type LLMHandler struct {
	textResponse            chan string
	errChan                 chan error
	ttsStopChan             chan bool
	audioStreamCompleteChan chan bool // Signal when AudioStreamComplete has been sent
	active                  bool
	audioChunkCount         int
	ttsStopped              bool      // Flag to indicate TTS has stopped
	ttsStopReceived         bool      // TTS stop event received (do NOT finalize synchronously in reader)
	audioFinalized          bool      // AudioStreamComplete already sent for this TTS turn
	lastFrameTime           time.Time // Track when last audio frame was received
	esn                     string    // Robot ESN for checking first audio playback
	websocketReconnected    bool      // Flag to indicate websocket was reconnected (for first chunk delay)
	// longPostPrepareWait is set when StreamingXiaozhiKG already slept ≥1200ms after AudioStreamPrepare.
	// In that case an extra long pause after the first 1024-byte chunk creates a gap (robot buffer drains → missing/cut start).
	longPostPrepareWait bool
	mu                  sync.RWMutex
	sendMu              sync.Mutex // Mutex to serialize vclient.Send() calls (gRPC streams are not thread-safe)
	// Audio processing (synchronous)
	vclient interface {
		Send(*vectorpb.ExternalAudioStreamRequest) error
	}
	opusDecoder       *opus.Decoder
	accumulatedBuffer []byte
	chunkCount        int
	audioQueueStarted bool
	lastSendTime      time.Time // Track when we last sent audio (for flush timer)
	flushTimer        *time.Ticker
	flushTimerStop    chan bool // Signal to stop flush timer
	// Health check for audio playback
	healthCheckTicker        *time.Ticker
	healthCheckStop          chan bool // Signal to stop health check
	lastHealthCheck          time.Time // Track last health check time
	chunksSentSinceLastCheck int       // Track chunks sent since last health check
	// replyStarted signals that the server began answering (tts start, first audio, or llm text).
	// ESP32 streams audio without requiring a text event first; wire-pod used to wait only on
	// textResponse and timed out. Merged with textResponse in StreamingXiaozhiKG select.
	replyStarted     chan struct{}
	replyStartedOnce sync.Once
	// robot is set for commands_enable: play animations from {{playAnimationWI||...}} in upstream text.
	robot *vector.Vector
	// cmdTextSeen dedupes identical llm vs tts sentence_start payloads in one TTS turn.
	cmdTextSeen map[string]struct{}
}

// signalReplyStarted fires once when TTS/audio/text pipeline has visibly started (xiaozhi-esp32 style).
func (h *LLMHandler) signalReplyStarted() {
	if h == nil || h.replyStarted == nil {
		return
	}
	h.replyStartedOnce.Do(func() {
		select {
		case h.replyStarted <- struct{}{}:
		default:
		}
	})
}

func (h *LLMHandler) maybePerformCommandsFromText(text string, skipEmojiFallback bool) {
	if h == nil || strings.TrimSpace(text) == "" {
		return
	}
	if !vars.APIConfig.Knowledge.CommandsEnable {
		return
	}
	h.mu.Lock()
	if h.cmdTextSeen == nil {
		h.cmdTextSeen = make(map[string]struct{})
	}
	if _, dup := h.cmdTextSeen[text]; dup {
		h.mu.Unlock()
		return
	}
	h.cmdTextSeen[text] = struct{}{}
	robot := h.robot
	h.mu.Unlock()
	PerformXiaozhiCommandsFromLLMText(text, robot, skipEmojiFallback)
}

func (h *LLMHandler) maybePerformOttoEmotion(emotion string) bool {
	if h == nil || strings.TrimSpace(emotion) == "" {
		return false
	}
	if !vars.APIConfig.Knowledge.CommandsEnable {
		return false
	}
	h.mu.RLock()
	robot := h.robot
	h.mu.RUnlock()
	return PerformXiaozhiOttoEmotion(emotion, robot)
}

func padToMultiple(b []byte, multiple int) []byte {
	if multiple <= 0 {
		return b
	}
	rem := len(b) % multiple
	if rem == 0 {
		return b
	}
	pad := multiple - rem
	out := make([]byte, len(b)+pad)
	copy(out, b)
	// remaining bytes are already zero
	return out
}

// HandleMessage processes messages from the WebSocket connection
func (h *LLMHandler) HandleMessage(messageType int, message []byte) error {
	h.mu.RLock()
	active := h.active
	h.mu.RUnlock()

	if !active {
		return nil // Handler is not active, ignore message
	}

	if messageType == websocket.TextMessage {
		var event map[string]interface{}
		if err := json.Unmarshal(message, &event); err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ERROR - Failed to unmarshal message: %v", err))
			return err
		}

		eventType, ok := event["type"].(string)
		if !ok {
			return nil
		}

		switch eventType {
		case "llm":
			// xiaozhi-esp32 Application: type "llm" may include "emotion" (Otto-style) for display; we map to Vector animations.
			emStr := ""
			if em, ok := event["emotion"].(string); ok {
				emStr = strings.TrimSpace(em)
			}
			ottoPlayed := false
			if emStr != "" {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM emotion (Otto-style): '%s'", emStr))
				ottoPlayed = h.maybePerformOttoEmotion(emStr)
			}
			if text, ok := event["text"].(string); ok && text != "" {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM text: '%s'", text))
				h.maybePerformCommandsFromText(text, ottoPlayed)
				select {
				case h.textResponse <- text:
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM text sent to channel: '%s'", text))
					h.signalReplyStarted()
				default:
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  textResponse channel is full, dropping text"))
				}
			} else if emStr != "" {
				// Emotion-only llm (no text yet) — unblock reply wait like tts/audio-first.
				h.signalReplyStarted()
			}
		case "tts":
			if state, ok := event["state"].(string); ok {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔊 TTS state: %s", state))
				// Log TTS state even if audio playback is not available (for debugging)
				h.mu.RLock()
				vclientAvailable := h.vclient != nil
				opusDecoderAvailable := h.opusDecoder != nil
				h.mu.RUnlock()
				if !vclientAvailable || !opusDecoderAvailable {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  TTS state received but audio playback not available (vclient: %v, opusDecoder: %v) - will log audio frames but not play", vclientAvailable, opusDecoderAvailable))
				}
				if state == "start" {
					// Reset counter when TTS starts
					h.mu.Lock()
					h.audioChunkCount = 0
					h.ttsStopReceived = false
					h.audioFinalized = false
					h.cmdTextSeen = make(map[string]struct{})
					h.mu.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS started, ready to receive Opus frames"))
					// ESP32-style: server may stream audio before any llm text — unblock KG wait.
					h.signalReplyStarted()
				} else if state == "sentence_start" {
					// TTS sentence_start contains the full text response (priority over LLM event)
					if text, ok := event["text"].(string); ok && text != "" {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS sentence_start text: '%s'", text))
						h.maybePerformCommandsFromText(text, false)
						select {
						case h.textResponse <- text:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS sentence_start text sent to channel: '%s'", text))
							h.signalReplyStarted()
						default:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  textResponse channel is full, dropping text"))
						}
					}
				} else if state == "stop" {
					// CRITICAL: Do NOT finalize synchronously here.
					// This handler is called from the single websocket reader goroutine via ConnectionManager.
					// Sleeping/sending chunks here will block reads and can cause upstream to close (1005) or
					// cut off trailing audio frames.
					h.mu.Lock()
					h.ttsStopReceived = true
					h.mu.Unlock()
					logger.Println("[Xiaozhi KG Handler] 🟡 TTS stop received; will finalize after last audio frames arrive (non-blocking)")
				}
			}
		case "error":
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
		case "goodbye":
			// Server sends goodbye event - similar to ESP32, we should only signal TTS stop, not close connection
			// Connection should remain open for reuse (like ESP32 does)
			sessionID := ""
			if sid, ok := event["session_id"].(string); ok {
				sessionID = sid
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 👋 Received goodbye event (session_id: %s) - signaling TTS stop but keeping connection for reuse", sessionID))
			// Some servers end a TTS stream with "goodbye" instead of sending a final "tts" state:"stop".
			// If we don't finalize (flush + AudioStreamComplete), Vector may buffer and never play anything.
			h.mu.RLock()
			alreadyStopped := h.ttsStopReceived
			h.mu.RUnlock()
			if !alreadyStopped {
				// Trigger the same finalize logic as a normal TTS stop.
				_ = h.HandleMessage(websocket.TextMessage, []byte(`{"type":"tts","state":"stop"}`))
			}
			// Don't close connection - let it be reused for next request (like ESP32)
		}
	} else if messageType == websocket.BinaryMessage {
		// Audio data (Opus-encoded from server)
		// Process audio synchronously: Decode Opus → PCM → Resample → Send to robot
		h.mu.Lock()
		h.audioChunkCount++
		count := h.audioChunkCount
		h.lastFrameTime = time.Now()
		vclient := h.vclient
		opusDecoder := h.opusDecoder
		accumulatedBuffer := h.accumulatedBuffer
		h.mu.Unlock()

		// ALWAYS log first few frames to debug audio reception
		if count <= 5 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔊 Received audio frame #%d (size: %d bytes) - vclient: %v, opusDecoder: %v", count, len(message), vclient != nil, opusDecoder != nil))
		}

		// Skip if vclient or opusDecoder is nil
		// IMPORTANT: Log audio frames even if playback is not available (for debugging)
		if vclient == nil || opusDecoder == nil {
			// Log first few frames and then every 50th frame if nil (to avoid spam)
			if count <= 5 || count%50 == 0 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Audio frame #%d (size: %d bytes) but audio playback not available (vclient: %v, opusDecoder: %v) - waiting for audio setup...", count, len(message), vclient != nil, opusDecoder != nil))
			}
			// Try to get vclient and opusDecoder again (may have been set after handler registration)
			h.mu.RLock()
			vclient = h.vclient
			opusDecoder = h.opusDecoder
			h.mu.RUnlock()
			// If still nil, return (but we've logged the frame for debugging)
			if vclient == nil || opusDecoder == nil {
				// Log warning for first few frames to help debug
				if count <= 5 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Skipping audio frame #%d - vclient/opusDecoder still not ready (may arrive later)", count))
				}
				return nil
			}
			// If now available, continue processing (don't skip frame)
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ vclient/opusDecoder now available, processing frame #%d", count))
		}

		// Decode OPUS → PCM
		pcmBuffer := make([]int16, 1440) // 60ms @ 24kHz max
		n, err := opusDecoder.Decode(message, pcmBuffer)
		if err != nil {
			if count <= 5 || count%50 == 0 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Opus decode error (skipping frame #%d, size: %d bytes): %v", count, len(message), err))
			}
			return nil
		}
		if n == 0 {
			if count <= 5 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Opus decode returned 0 samples (frame #%d, size: %d bytes) - skipping", count, len(message)))
			}
			return nil
		}

		// Log decode success for first few frames
		if count <= 5 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Opus decoded: frame #%d → %d samples (PCM)", count, n))
		}

		// Convert int16 → PCM bytes (little-endian)
		framePCMBytes := make([]byte, n*2)
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint16(framePCMBytes[i*2:], uint16(pcmBuffer[i]))
		}

		// Downsample 24kHz → 16kHz (stream-friendly: no per-frame padding; chunking happens on accumulated buffer)
		downsampledBytes := downsample24kTo16kLinear(framePCMBytes)
		// Boost loudness (x4 by default) with soft limiter to avoid clipping/distortion.
		downsampledBytes = applyGainSoftLimit16LE(downsampledBytes, xiaozhiTTSGain())
		if len(downsampledBytes) == 0 {
			if count <= 5 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Downsample returned 0 bytes (frame #%d, PCM size: %d bytes)", count, len(framePCMBytes)))
			}
			return nil
		}
		// First playable PCM after decode: server may send binary before tts/llm text (ESP32-style).
		h.signalReplyStarted()

		// Log downsample success for first few frames
		if count <= 5 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Downsampled: frame #%d → %d bytes total (24kHz→16kHz)", count, len(downsampledBytes)))
		}

		// Accumulate into buffer
		accumulatedBuffer = append(accumulatedBuffer, downsampledBytes...)

		// Log buffer status for first few frames
		if count <= 5 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 📦 Buffer after frame #%d: %d bytes accumulated", count, len(accumulatedBuffer)))
		}

		// Send audio chunks using the same chunk size (1024 bytes) but prefer 16kHz playback.
		// This matches the OpenAI TTS path in this repo and tends to be more reliable than 8kHz on some builds.
		//
		// Code hiện tại áp dụng logic tương tự:
		// - Chunk size ưu tiên: 1024 bytes (giống Play Audio)
		// - Delay giữa chunks: 60ms (giống Play Audio)
		// - AudioFrameRate: 16000 (prefer 16kHz; matches OpenAI TTS path)
		// - AudioVolume: 80 (avoid "send ok but silent" on some builds)
		// - Gửi AudioStreamComplete khi xong (giống Play Audio)
		//
		// Khác biệt: Play Audio chia file trước, code này accumulate buffer real-time
		// nhưng vẫn ưu tiên gửi chunk 1024 bytes khi buffer đủ lớn
		chunksSentInFrame := 0
		// CRITICAL: Lock only when needed, unlock before sending to avoid deadlock
		h.mu.Lock()
		for len(accumulatedBuffer) >= vectorAudioChunkBytes {
			chunkSize := vectorAudioChunkBytes
			chunkToSend := make([]byte, chunkSize)
			copy(chunkToSend, accumulatedBuffer[:chunkSize])
			accumulatedBuffer = accumulatedBuffer[chunkSize:]
			// UNLOCK before sending to avoid deadlock (vclient.Send might block)
			h.mu.Unlock()

			// Send to robot (same pattern as Play Audio)
			// Add retry logic like OpenAI TTS to ensure audio is always sent
			// No per-chunk success logs here — logging synchronously can jitter audio on slow hosts.
			if xiaozhiDebugAudio() && (count <= 5) {
				peakAbs, avgAbs := pcmAbsStats16LE(chunkToSend)
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔎 PCM stats (16LE): peakAbs=%d avgAbs=%d (chunkBytes=%d, frame#=%d)", peakAbs, avgAbs, len(chunkToSend), count))
			}
			var err error
			maxRetries := 3
			retryDelay := 10 * time.Millisecond
			for retry := 0; retry < maxRetries; retry++ {
				if vclient == nil {
					err = fmt.Errorf("vclient is nil")
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - vclient is nil, cannot send chunk"))
					break
				}

				// CRITICAL: Use sendMu to serialize vclient.Send() calls (gRPC streams are not thread-safe)
				// This prevents race conditions when multiple goroutines call Send() simultaneously
				h.sendMu.Lock()
				// Use defer/recover to catch any panics
				func() {
					defer func() {
						if r := recover(); r != nil {
							err = fmt.Errorf("panic in vclient.Send(): %v", r)
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  PANIC in vclient.Send(): %v", r))
						}
					}()
					err = vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
							AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
								AudioChunkSizeBytes: uint32(len(chunkToSend)),
								AudioChunkSamples:   chunkToSend,
							},
						},
					})
				}()
				h.sendMu.Unlock() // Always unlock after Send() completes (success or error)

				if err != nil {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔴 vclient.Send() returned error: %v", err))
				}

				if err == nil {
					// Success - break retry loop
					chunksSentInFrame++
					// IMPORTANT: Update chunkCount when sending chunks directly (not via flush timer)
					// Lock is already released above, so we can lock again safely
					h.mu.Lock()
					h.chunkCount++
					h.chunksSentSinceLastCheck++
					h.mu.Unlock()
					// Re-lock to update buffer for next iteration (don't break yet, continue loop)
					h.mu.Lock()
					h.accumulatedBuffer = accumulatedBuffer
					// Check if there's more data to send
					accumulatedBuffer = h.accumulatedBuffer
					h.mu.Unlock()
					// Break retry loop, continue to next chunk if buffer >= vectorAudioChunkBytes
					break
				} else {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Error sending chunk (retry %d/%d): %v", retry+1, maxRetries, err))
				}
				// If error is EOF/closed, don't retry
				if err != nil && (strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed")) {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send audio chunk (stream closed): %v", err))
					// Lock is already released, so we can lock again safely
					h.mu.Lock()
					h.vclient = nil
					esn := h.esn
					h.mu.Unlock()
					// CRITICAL: Remove invalid client from pool immediately
					// This ensures next StreamingXiaozhiKG call will create a new client
					if esn != "" {
						RemoveAudioClient(esn)
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Removed invalid audio client from pool for ESN: %s (stream closed)", esn))
					}
					return nil
				}
				// Retry with exponential backoff
				if retry < maxRetries-1 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Retry %d/%d sending audio chunk: %v", retry+1, maxRetries, err))
					time.Sleep(retryDelay)
					retryDelay *= 2 // Exponential backoff
				}
			}
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send audio chunk after %d retries: %v", maxRetries, err))
				// Don't break - continue processing other chunks
				// This ensures we don't lose all audio if one chunk fails
				if count <= 5 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Chunk send failed, but continuing (buffer size: %d)", len(accumulatedBuffer)))
				}
			}
			// Re-lock to update buffer and check for next iteration
			h.mu.Lock()
			// Update accumulatedBuffer (chunk was already removed before unlock)
			h.accumulatedBuffer = accumulatedBuffer
			chunkCount := h.chunkCount
			h.mu.Unlock()
			// Delay after chunk 1 before chunk 2: must not add a huge gap if we already waited 1200ms+
			// after AudioStreamPrepare (longPostPrepareWait) — that caused audible dropout at TTS start.
			if chunkCount == 1 {
				h.mu.RLock()
				esn := h.esn
				longPostPrepare := h.longPostPrepareWait
				h.mu.RUnlock()
				switch {
				case longPostPrepare:
					// Prepare already included a long robot warm-up; use normal pacing only.
					d := getVectorChunkPace()
					time.Sleep(d)
				case hasRobotPlayedAudio(esn):
					time.Sleep(50 * time.Millisecond)
				default:
					// Short Prepare (e.g. 50ms) + first conversation ever: brief settle only (was 300ms → gap/cut start).
					time.Sleep(100 * time.Millisecond)
				}
			} else {
				time.Sleep(getVectorChunkPace()) // Normal delay for subsequent chunks
			}
			// Re-lock for next iteration
			h.mu.Lock()
			accumulatedBuffer = h.accumulatedBuffer
		}
		// Final update of buffer
		h.accumulatedBuffer = accumulatedBuffer
		h.mu.Unlock()

		if xiaozhiDebugAudio() && (count <= 5 || count%10 == 0) {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Opus frame #%d processed (%d bytes) - sent %d chunks immediately, buffer size: %d bytes", count, len(message), chunksSentInFrame, len(accumulatedBuffer)))
			if chunksSentInFrame == 0 && len(accumulatedBuffer) >= vectorAudioChunkBytes {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  WARNING - Buffer >= %d but no chunks sent! vclient=%v", vectorAudioChunkBytes, vclient != nil))
			}
		}
	}
	return nil
}

// IsActive returns whether the handler is currently active
func (h *LLMHandler) IsActive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.active
}

// SetActive sets the handler as active or inactive
func (h *LLMHandler) SetActive(active bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = active
	if !active {
		// Reset counter when deactivating
		h.audioChunkCount = 0
	}
}

// WaitForAudio_Queue waits for current audio playback to complete before starting new one
func WaitForAudio_Queue(esn string) {
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	for i, q := range AudioQueues {
		if q.ESN == esn {
			if q.AudioCurrentlyPlaying {
				audioQueueMutex.Unlock()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Waiting for current audio to finish...", esn))
				for range AudioQueues[i].AudioDone {
					break
				}
				audioQueueMutex.Lock()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Previous audio finished, starting new audio", esn))
			}
			return
		}
	}
}

// StartAudio_Queue marks audio playback as started
func StartAudio_Queue(esn string) {
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	// Check if queue exists for this ESN
	for i, q := range AudioQueues {
		if q.ESN == esn {
			if q.AudioCurrentlyPlaying {
				audioQueueMutex.Unlock()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Waiting for previous audio to finish...", esn))
				for range AudioQueues[i].AudioDone {
					break
				}
				audioQueueMutex.Lock()
			}
			AudioQueues[i].AudioCurrentlyPlaying = true
			// HasPlayedAudio remains true if it was already set (not reset on new playback)
			logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Audio playback started", esn))
			return
		}
	}

	// Create new queue if doesn't exist
	var aq AudioQueue
	aq.AudioCurrentlyPlaying = true
	aq.AudioDone = make(chan bool, 1)
	aq.ESN = esn
	aq.HasPlayedAudio = false // First time, hasn't played audio yet
	AudioQueues = append(AudioQueues, aq)
	logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | New audio queue created, audio playback started", esn))
}

// StopAudio_Queue marks audio playback as finished
func StopAudio_Queue(esn string) {
	defer func() {
		if r := recover(); r != nil {
			// Use fmt.Fprintf to stderr to avoid logger panic
			fmt.Fprintf(os.Stderr, "[Xiaozhi Audio Queue] Device: %s | PANIC in StopAudio_Queue (recovered): %v\n", esn, r)
		}
	}()
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()

	for i, q := range AudioQueues {
		if q.ESN == esn {
			AudioQueues[i].AudioCurrentlyPlaying = false
			AudioQueues[i].HasPlayedAudio = true // Mark that this robot has played audio at least once
			select {
			case AudioQueues[i].AudioDone <- true:
			default:
			}
			// Use safe logging to prevent panic
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(os.Stderr, "[Xiaozhi Audio Queue] Device: %s | Audio playback finished (logger panic recovered: %v)\n", esn, r)
					}
				}()
				logger.Println(fmt.Sprintf("[Xiaozhi Audio Queue] Device: %s | Audio playback finished", esn))
			}()
			return
		}
	}
}

// hasRobotPlayedAudio checks if this robot has ever played audio (for warm-up delay)
func hasRobotPlayedAudio(esn string) bool {
	audioQueueMutex.Lock()
	defer audioQueueMutex.Unlock()
	for _, q := range AudioQueues {
		if q.ESN == esn {
			return q.HasPlayedAudio
		}
	}
	return false // If queue doesn't exist, it's first time
}

// RemoveAudioClient removes an audio client from the pool (e.g., when stream is closed)
func RemoveAudioClient(esn string) {
	audioClientPoolMutex.Lock()
	defer audioClientPoolMutex.Unlock()
	if poolEntry, exists := audioClientPool[esn]; exists {
		if poolEntry.Cancel != nil {
			poolEntry.Cancel() // Cancel context to close stream
		}
		delete(audioClientPool, esn)
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Removed audio client from pool (stream closed or invalid)", esn))
	}
}

// StreamingXiaozhiKG handles knowledge graph requests using xiaozhi WebSocket
// This provides real-time voice conversation with TTS audio playback on robot
// isConversationMode: if true, LLM will use {{newVoiceRequest||now}} to continue conversation
func StreamingXiaozhiKG(esn string, transcribedText string, isKG bool, isConversationMode bool) (string, error) {
	// Ensure esn is not empty to prevent panics in logger calls
	if esn == "" {
		esn = "unknown"
	}

	// NOTE: Audio client is now managed by persistent pool (audioClientPool)
	// No need for per-request audio context - audio client stays open and is reused
	// This keeps audio pipeline always warm, eliminating warm-up delays

	// Create separate context for LLM request (can be canceled when LLM response is received)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure context is canceled when function returns

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== STARTING StreamingXiaozhiKG ==========", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ESN: %s, TranscribedText: '%s', isKG: %v, isConversationMode: %v", esn, esn, transcribedText, isKG, isConversationMode))
	if strings.TrimSpace(transcribedText) == "" {
		// If STT returns an empty transcript, do NOT send an empty text query upstream.
		// This causes the server to hang (no response) and can block subsequent STT/TTS turns
		// even though the websocket session is still alive.
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Empty transcript - skipping upstream text query (will not call LLM)", esn))
		return "", fmt.Errorf("empty transcript")
	}

	// Get robot connection - try to create even if robot not in vars.BotInfo.Robots
	// This allows audio playback for any robot that makes a request
	var robot *vector.Vector
	var guid string
	var target string
	matched := false

	// Step 1: Try to find robot in vars.BotInfo.Robots
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Searching for robot in vars.BotInfo.Robots (count: %d)...", esn, len(vars.BotInfo.Robots)))
	for i, bot := range vars.BotInfo.Robots {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Checking Robot[%d]: ESN=%s, IP=%s, GUID=%s", esn, i, bot.Esn, bot.IPAddress, bot.GUID))
		if esn == bot.Esn {
			guid = bot.GUID
			if guid == "" {
				guid = vars.BotInfo.GlobalGUID
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s |   Robot GUID is empty, using GlobalGUID: %s", esn, guid))
			}
			target = bot.IPAddress + ":443"
			matched = true
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Found robot in vars.BotInfo.Robots (IP: %s, GUID: %s)", esn, bot.IPAddress, guid))
			break
		}
	}
	if !matched {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot %s not found in vars.BotInfo.Robots", esn, esn))
	}

	// Robot connection is optional - allow LLM/TTS to work even without robot
	// This is important for Android builds where robot connection might fail with 401
	var err error
	if matched {
		robot, err = vector.New(vector.WithSerialNo(esn), vector.WithToken(guid), vector.WithTarget(target))
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create robot connection: %v. Continuing without robot connection (LLM/TTS will still work).", esn, err))
			robot = nil
		} else {
			// Test connection with BatteryState
			_, err = robot.Conn.BatteryState(ctx, &vectorpb.BatteryStateRequest{})
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Robot connection test failed: %v. Continuing without robot connection (LLM/TTS will still work).", esn, err))
				robot = nil
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot connection established successfully", esn))
			}
		}
	} else {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot not found, continuing without robot connection (LLM/TTS will still work)", esn))
		robot = nil
	}

	// Lấy Device-Id từ config
	deviceID := xiaozhi.GetDeviceIDFromConfig()

	// Bước 1: Thử lấy connection từ STT (giống botkct.py - dùng cùng connection cho STT và text message)
	// Dùng CheckConnection để kiểm tra mà không mark "in use" (giống STT)
	var conn *websocket.Conn
	var sessionID string
	var connFromSTT bool

	if deviceID != "" {
		if storedConn, storedSessionID, exists := xiaozhi.CheckConnection(deviceID); exists {
			// Connection exists and can be reused - now mark it as "in use" for LLM
			conn = storedConn
			sessionID = storedSessionID
			connFromSTT = true
			// Mark connection as "in use" for LLM
			xiaozhi.GetConnection(deviceID) // This marks it as "in use"
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ REUSING connection from STT (sessionID: %s) - giống botkct.py", esn, sessionID))
		}
	}

	// Nếu không có connection từ STT, tạo connection mới
	if conn == nil {
		connFromSTT = false
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  No connection from STT, creating new connection", esn))

		// Get xiaozhi config
		baseURL, _, _ := xiaozhi.GetKnowledgeGraphConfig()
		if baseURL == "" {
			baseURL = "wss://api.tenclass.net/xiaozhi/v1/"
		}

		// Lấy Client-Id từ config
		clientID := xiaozhi.GetClientIDFromConfig()
		headers := http.Header{}
		// Protocol-Version header (giống botkct.py)
		headers.Add("Protocol-Version", "1")
		if deviceID != "" {
			headers.Add("Device-Id", deviceID)
			logger.Println("Xiaozhi KG: Using Device-Id from config:", deviceID)
		}
		if clientID != "" {
			headers.Add("Client-Id", clientID)
			logger.Println("Xiaozhi KG: Using Client-Id from config:", clientID)
		}
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | WebSocket connection headers: Protocol-Version=1, Device-Id=%s, Client-Id=%s", esn, deviceID, clientID))

		// Connect to xiaozhi WebSocket
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔌 Connecting to WebSocket: %s", esn, baseURL))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Headers: %v", esn, headers))
		var resp *http.Response
		var err error
		conn, resp, err = websocket.DefaultDialer.Dial(baseURL, headers)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ WebSocket connection failed: %v (type: %T)", esn, err, err))
			if resp != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | HTTP Response Status: %s", esn, resp.Status))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | HTTP Response Headers: %v", esn, resp.Header))
			}
			return "", fmt.Errorf("failed to connect to xiaozhi: %w", err)
		}
		if resp != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ WebSocket connected! HTTP Status: %s", esn, resp.Status))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Response Headers: %v", esn, resp.Header))
		}

		// Step 1: Send hello event (giống botkct.py line 543-557)
		helloEvent := map[string]interface{}{
			"type":    "hello",
			"version": 1,
			"features": map[string]interface{}{
				"mcp": true,
				"aec": true,
			},
			"transport": "websocket",
			"language":  "vi",
			"audio_params": map[string]interface{}{
				"format":         "opus",
				"sample_rate":    16000, // botkct.py uses 16kHz
				"channels":       1,
				"frame_duration": 60, // botkct.py uses 60
			},
		}
		if err := conn.WriteJSON(helloEvent); err != nil {
			conn.Close()
			return "", fmt.Errorf("failed to send hello: %w", err)
		}

		// Read hello response
		var helloResp map[string]interface{}
		if err := conn.ReadJSON(&helloResp); err != nil {
			conn.Close()
			return "", fmt.Errorf("failed to read hello response: %w", err)
		}
		logger.Println("Xiaozhi KG: Connected and hello received")

		// Extract session_id from hello response
		if sid, ok := helloResp["session_id"].(string); ok && sid != "" {
			sessionID = sid
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Using session_id from hello: %s", esn, sessionID))
		}

		// Store new connection in manager and start reader (like STT does)
		if deviceID != "" {
			if err := xiaozhi.StoreConnection(deviceID, conn, sessionID); err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Failed to store connection: %v", esn, err))
				conn.Close()
				return "", fmt.Errorf("failed to store connection: %w", err)
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Stored NEW connection for device %s (sessionID: %s) - reader goroutine started", esn, deviceID, sessionID))
			// Mark connection as "in use" for LLM
			xiaozhi.GetConnection(deviceID) // This marks it as "in use"
		}
	}

	// Cleanup: Giữ connection trong manager để reuse cho request tiếp theo (giống botkct.py)
	// Chỉ đóng connection nếu có lỗi hoặc connection không còn valid
	defer func() {
		// Connection is now always in manager (either from STT or newly created)
		// Don't close it here - let it be reused for next request
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Connection kept in manager for reuse (connFromSTT: %v)", esn, connFromSTT))
	}()

	// Step 2: Create LLM handler FIRST (BEFORE sending text query)
	// CRITICAL: Server may send llm/tts messages immediately after receiving text query
	// Handler must be ready to receive these messages
	textResponse := make(chan string, 5) // Increased buffer to handle both LLM and TTS sentence_start events
	errChan := make(chan error, 1)
	ttsStopChan := make(chan bool, 1) // Signal when TTS stops (for connection release timing)

	// Create LLM handler instance (vclient and opusDecoder will be set later after audio setup)
	llmHandler := &LLMHandler{
		textResponse:            textResponse,
		errChan:                 errChan,
		ttsStopChan:             ttsStopChan,
		audioStreamCompleteChan: make(chan bool, 1), // Signal when AudioStreamComplete has been sent
		active:                  false,              // Will be activated when registered
		audioChunkCount:         0,
		accumulatedBuffer:       []byte{},
		chunkCount:              0,
		audioQueueStarted:       false,
		esn:                     esn, // Store ESN for checking first audio playback
		replyStarted:            make(chan struct{}, 1),
		robot:                   robot,
	}

	// CRITICAL: Register LLM handler BEFORE Vector audio setup, WaitForAudio_Queue, and text query.
	// After STT sends listen stop, the server may stream tts/llm/binary immediately; if we only
	// called SetLLMHandler after audio Prepare + queue waits, ConnectionManager had LLMHandler=nil
	// and dropped all frames ("no LLM handler is set").
	if deviceID != "" {
		llmHandler.SetActive(true)
		xiaozhi.SetLLMHandler(deviceID, llmHandler)
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler registered EARLY (before Vector audio / queue) for deviceID: %s", esn, deviceID))
	}

	// CRITICAL: Setup audio BEFORE sending text query
	// Audio frames may arrive immediately after TTS sentence_start
	// vclient and opusDecoder must be ready to process them
	var vclient interface {
		Send(*vectorpb.ExternalAudioStreamRequest) error
	}
	var audioPrepareSent bool
	var opusDecoder *opus.Decoder

	// Setup audio playback client (only if robot connection exists)
	// CRITICAL: Reuse audio client from pool to keep pipeline always warm
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Checking robot connection status: robot == nil? %v", esn, robot == nil))
	if robot != nil {
		// Try to get existing audio client from pool
		audioClientPoolMutex.RLock()
		poolEntry, exists := audioClientPool[esn]
		audioClientPoolMutex.RUnlock()

		if exists && poolEntry != nil {
			// CRITICAL: Check if robot connection is still valid AND matches current robot before reusing client
			// If robot connection was closed/reconnected (e.g., after websocket close), audio client may be invalid
			robotValid := false
			websocketReconnected := false
			if robot != nil && robot.Conn != nil {
				// CRITICAL: Check if websocket was reconnected (SessionID changed)
				// When websocket reconnects, robot may need more time to process AudioStreamPrepare
				// Even though audio client (gRPC stream) is independent of websocket, the robot's audio pipeline
				// may need to be "reset" or "prepared" again after websocket reconnect
				if poolEntry.SessionID != "" && sessionID != "" && poolEntry.SessionID != sessionID {
					websocketReconnected = true
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  SessionID changed (old: %s, new: %s). Websocket was recreated - will use longer delay for AudioStreamPrepare.", esn, poolEntry.SessionID, sessionID))
					// Update SessionID in pool entry (for logging)
					audioClientPoolMutex.Lock()
					if entry, exists := audioClientPool[esn]; exists {
						entry.SessionID = sessionID
					}
					audioClientPoolMutex.Unlock()
				}
				// CRITICAL: Don't check poolEntry.Robot != robot because each StreamingXiaozhiKG call creates a NEW robot instance
				// Audio client (gRPC stream) is independent of robot instance - it only depends on robot IP/GUID
				// Instead, test the current robot connection to see if it's valid and can use the audio client
				// Test robot connection with a simple call (timeout 2s to avoid blocking)
				testCtx, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := robot.Conn.BatteryState(testCtx, &vectorpb.BatteryStateRequest{})
				testCancel()
				if err == nil {
					robotValid = true
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot connection is valid, can reuse audio client (robot instance may differ but connection is same)", esn))
				} else {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot connection test failed: %v. Will remove invalid client from pool and create new one.", esn, err))
					// Remove invalid client from pool
					audioClientPoolMutex.Lock()
					if poolEntry.Cancel != nil {
						poolEntry.Cancel() // Cancel context to close stream
					}
					delete(audioClientPool, esn)
					audioClientPoolMutex.Unlock()
					// Fall through to create new client
					vclient = nil
					audioPrepareSent = false
				}
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Robot connection is nil, cannot reuse audio client. Will remove from pool.", esn))
				// Remove client from pool since robot connection is invalid
				audioClientPoolMutex.Lock()
				if poolEntry.Cancel != nil {
					poolEntry.Cancel() // Cancel context to close stream
				}
				delete(audioClientPool, esn)
				audioClientPoolMutex.Unlock()
				// Fall through to create new client
				vclient = nil
				audioPrepareSent = false
			}

			if robotValid {
				// CRITICAL: Check if audio client context is still valid (not canceled)
				// If context was canceled (e.g., after websocket close), audio stream is closed
				if poolEntry.Ctx != nil {
					select {
					case <-poolEntry.Ctx.Done():
						// Context was canceled - audio stream is closed, cannot reuse
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Audio client context was canceled (stream closed). Will remove from pool and create new one.", esn))
						// Remove invalid client from pool
						audioClientPoolMutex.Lock()
						delete(audioClientPool, esn)
						audioClientPoolMutex.Unlock()
						// Fall through to create new client
						vclient = nil
						audioPrepareSent = false
						robotValid = false // Prevent reuse
					default:
						// Context is still valid - can reuse
					}
				}

				if robotValid {
					// Reuse existing audio client (pipeline already warm!)
					vclient = poolEntry.Client
					if websocketReconnected {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ REUSING persistent audio client (pipeline already warm, context valid) - BUT websocket reconnected, will use longer delay", esn))
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ REUSING persistent audio client (pipeline already warm, context valid)", esn))
					}

					// CRITICAL: Must send AudioStreamPrepare again for each TTS session
					// Vector SDK requires Prepare before each audio playback (even with same client)
					// When websocket reconnects, robot may need more time to process Prepare (similar to new client)
					// CRITICAL: Serialize prepare with any potential concurrent chunk flushes.
					llmHandler.sendMu.Lock()
					err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
						AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamPrepare{
							AudioStreamPrepare: &vectorpb.ExternalAudioStreamPrepare{
								AudioFrameRate: vectorAudioFrameRate,
								// Vector AudioVolume is 0-100; higher values may lead to silence on some builds.
								AudioVolume: vectorAudioVolume,
							},
						},
					})
					llmHandler.sendMu.Unlock()
					if err != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to send AudioStreamPrepare on reused client (stream may be closed): %v. Will remove from pool and create new one.", esn, err))
						// Remove invalid client from pool and create new one
						audioClientPoolMutex.Lock()
						if poolEntry.Cancel != nil {
							poolEntry.Cancel() // Cancel context to close stream
						}
						delete(audioClientPool, esn)
						audioClientPoolMutex.Unlock()
						// Fall through to create new client
						vclient = nil
						audioPrepareSent = false
						robotValid = false // Prevent reuse
					} else {
						audioPrepareSent = true
						if websocketReconnected {
							// CRITICAL: When websocket reconnects, robot needs more time to process AudioStreamPrepare
							// This is similar to creating a new client - robot's audio pipeline may need to be "reset"
							// Use longer delay (similar to new client after reconnect) to ensure audio plays correctly
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent on reused client (%dkHz, volume %d) - stream is valid, BUT websocket reconnected, using longer delay", esn, vectorAudioFrameRate/1000, vectorAudioVolume))
							time.Sleep(time.Millisecond * 1200) // Longer delay when websocket reconnects (similar to new client)
							llmHandler.longPostPrepareWait = true
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (1200ms - websocket reconnected, robot needs time to process)", esn))
						} else {
							// Normal reuse - pipeline is warm, minimal delay needed
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent on reused client (%dkHz, volume %d) - stream is valid, pipeline is warm", esn, vectorAudioFrameRate/1000, vectorAudioVolume))
							time.Sleep(time.Millisecond * 50) // Minimal delay - pipeline is warm, only need time for robot to process Prepare
							llmHandler.longPostPrepareWait = false
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (50ms - pipeline always warm, only robot processing time)", esn))
						}
					}
				}
			}
		}

		if vclient == nil || !audioPrepareSent {
			// Create new audio client (either pool was empty or reused client failed)
			// Create new audio client and add to pool
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Creating NEW persistent audio client for robot...", esn))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Robot connection details - Target: %s, ESN: %s, GUID: %s", esn, target, esn, guid))

			// Create persistent context (never cancel, keep stream open)
			persistentAudioCtx, persistentAudioCancel := context.WithCancel(context.Background())
			audioClient, err := robot.Conn.ExternalAudioStreamPlayback(persistentAudioCtx)
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create audio playback client: %v", esn, err))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Continuing without audio playback. TTS messages will still be received and logged.", esn))
				vclient = nil
				audioPrepareSent = false
			} else {
				vclient = audioClient
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio playback client created successfully", esn))

				// Prepare audio stream (only once, when creating new client)
				// CRITICAL: Serialize prepare with any potential concurrent chunk flushes.
				llmHandler.sendMu.Lock()
				err = vclient.Send(&vectorpb.ExternalAudioStreamRequest{
					AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamPrepare{
						AudioStreamPrepare: &vectorpb.ExternalAudioStreamPrepare{
							AudioFrameRate: vectorAudioFrameRate,
							AudioVolume:    vectorAudioVolume,
						},
					},
				})
				llmHandler.sendMu.Unlock()
				if err != nil {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to send AudioStreamPrepare: %v. Disabling audio playback.", esn, err))
					vclient = nil
					audioPrepareSent = false
				} else {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent successfully (%dkHz, volume %d)", esn, vectorAudioFrameRate/1000, vectorAudioVolume))
					audioPrepareSent = true

					// Delay needed for NEW client creation (whether first time or after reconnect)
					// Even if robot has played audio before, this is a NEW stream, so needs longer delay
					// If client is reused from pool, pipeline is already warm (handled above)
					// This is a NEW client creation, so always use longer delay for new stream
					isFirstAudio := !hasRobotPlayedAudio(esn)
					if isFirstAudio {
						time.Sleep(time.Millisecond * 1500) // Longer delay for first audio ever (robot warm-up) - increased from 1000ms to 1500ms
						llmHandler.longPostPrepareWait = true
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (1500ms for FIRST audio ever - robot warm-up)", esn))
					} else {
						// Robot has played audio before, but this is a NEW client (pool was empty or old client failed)
						// After websocket close/reconnect, robot needs time to reset audio pipeline for new stream
						// Increased from 500ms to 1200ms to ensure robot is ready for new stream
						time.Sleep(time.Millisecond * 1200) // Longer delay for new client after websocket reconnect (new stream needs time)
						llmHandler.longPostPrepareWait = true
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (1200ms for new client after reconnect - new stream needs time)", esn))
					}

					// Store in pool for reuse (keep pipeline warm)
					// NOTE: SessionID is tracked but NOT used to invalidate audio client
					// Audio client only depends on robot connection, not websocket connection
					audioClientPoolMutex.Lock()
					audioClientPool[esn] = &AudioClientEntry{
						Client:    audioClient,
						Ctx:       persistentAudioCtx,
						Cancel:    persistentAudioCancel,
						Robot:     robot,
						LastUsed:  time.Now(),
						SessionID: sessionID, // Track sessionID for logging only (not used to invalidate)
					}
					audioClientPoolMutex.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio client stored in pool (will be reused for next TTS, pipeline stays warm, sessionID: %s)", esn, sessionID))
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ℹ️  Audio pipeline will remain open even if websocket reconnects (audio client independent of websocket)", esn))
				}
			}
		}
	} else {
		vclient = nil
		audioPrepareSent = false
		opusDecoder = nil
	}

	// Create opus decoder and set in handler BEFORE sending text query
	if vclient != nil && audioPrepareSent {
		var err error
		opusDecoder, err = opus.NewDecoder(24000, 1)
		if err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to create Opus decoder: %v. Disabling audio playback.", esn, err))
			vclient = nil
		} else {
			// Set vclient and opusDecoder in handler BEFORE sending text query
			llmHandler.mu.Lock()
			llmHandler.vclient = vclient
			llmHandler.opusDecoder = opusDecoder
			llmHandler.lastSendTime = time.Now()
			llmHandler.flushTimer = time.NewTicker(50 * time.Millisecond)
			llmHandler.flushTimerStop = make(chan bool, 1)
			// Start health check ticker (check every 2 seconds)
			llmHandler.healthCheckTicker = time.NewTicker(2 * time.Second)
			llmHandler.healthCheckStop = make(chan bool, 1)
			llmHandler.lastHealthCheck = time.Now()
			llmHandler.chunksSentSinceLastCheck = 0
			llmHandler.mu.Unlock()
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio setup complete BEFORE text query - vclient and opusDecoder ready", esn))

			// Start flush timer goroutine
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] PANIC in flush timer goroutine (recovered): %v", r))
					}
					llmHandler.mu.Lock()
					if llmHandler.flushTimer != nil {
						llmHandler.flushTimer.Stop()
					}
					llmHandler.mu.Unlock()
				}()
				for {
					select {
					case <-llmHandler.flushTimer.C:
						llmHandler.mu.Lock()
						vclient := llmHandler.vclient
						accumulatedBuffer := llmHandler.accumulatedBuffer
						lastSendTime := llmHandler.lastSendTime
						lastFrameTime := llmHandler.lastFrameTime
						ttsStopped := llmHandler.ttsStopped
						ttsStopReceived := llmHandler.ttsStopReceived
						audioFinalized := llmHandler.audioFinalized
						chunkCount := llmHandler.chunkCount
						audioChunkCount := llmHandler.audioChunkCount
						llmHandler.mu.Unlock()

						// Check if we should auto-send AudioStreamComplete
						// If no new frames for a while AND we are no longer sending audio chunks to the robot,
						// send complete to unblock Vector playback and allow next turn.
						//
						// IMPORTANT: We must NOT key only off "no frames", because the websocket reader can be
						// temporarily busy sending audio to the robot (gRPC + pacing sleeps). In that case, frames
						// may be queued but not yet read; auto-completing would cut TTS mid-sentence.
						// Also check if lastFrameTime is not zero (at least one frame was received)
						timeSinceLastFrame := time.Since(lastFrameTime)
						timeSinceLastSend := time.Since(lastSendTime)

						// Auto-complete when:
						// - We previously sent at least one chunk (chunkCount > 0)
						// - We haven't received a new Opus frame for a LONG time
						// - We also haven't sent any audio chunk for a while (so we're not mid-send)
						//
						// NOTE: Using a short "no frames" window (e.g. 2s) can cut off long TTS mid-sentence
						// if upstream has jitter/pauses between audio bursts. Prefer a longer idle window here.
						if !ttsStopped &&
							vclient != nil &&
							chunkCount > 0 &&
							!lastFrameTime.IsZero() &&
							timeSinceLastFrame > 12*time.Second &&
							!lastSendTime.IsZero() &&
							timeSinceLastSend > 3*time.Second {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⏰ No new audio frames for %v and no sends for %v (chunks: %d, frames: %d), auto-sending AudioStreamComplete", timeSinceLastFrame, timeSinceLastSend, chunkCount, audioChunkCount))
							llmHandler.mu.Lock()
							llmHandler.ttsStopped = true
							llmHandler.mu.Unlock()

							// Send final buffer if any
							if len(accumulatedBuffer) > 0 {
								finalBuf := padToMultiple(accumulatedBuffer, vectorAudioChunkBytes)
								sentFinal := 0
								for len(finalBuf) >= vectorAudioChunkBytes {
									chunk := finalBuf[:vectorAudioChunkBytes]
									finalBuf = finalBuf[vectorAudioChunkBytes:]
									// CRITICAL: Use sendMu to serialize vclient.Send() calls
									llmHandler.sendMu.Lock()
									err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
										AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
											AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
												AudioChunkSizeBytes: uint32(len(chunk)),
												AudioChunkSamples:   chunk,
											},
										},
									})
									llmHandler.sendMu.Unlock()
									if err != nil {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Failed to send final buffer chunk before auto-complete: %v", err))
										break
									}
									sentFinal++
									time.Sleep(getVectorChunkPace())
								}
								llmHandler.mu.Lock()
								llmHandler.accumulatedBuffer = []byte{}
								llmHandler.mu.Unlock()
								if sentFinal > 0 {
									logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent %d final padded chunk(s) before auto-complete", sentFinal))
									time.Sleep(200 * time.Millisecond)
								}
							}

							// Send AudioStreamComplete
							// CRITICAL: Use sendMu to serialize vclient.Send() calls
							llmHandler.sendMu.Lock()
							err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
									AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
								},
							})
							llmHandler.sendMu.Unlock()
							if err == nil {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Auto-sent AudioStreamComplete (total chunks: %d)", chunkCount))
								select {
								case llmHandler.audioStreamCompleteChan <- true:
									logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ AudioStreamComplete signal sent"))
								default:
								}
								// Signal TTS stop
								select {
								case llmHandler.ttsStopChan <- true:
								default:
								}
							} else {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Failed to auto-send AudioStreamComplete: %v", err))
							}
							continue
						}

						// If server signaled TTS stop, finalize soon after the last frame arrives.
						// This avoids cutting off trailing frames while still completing promptly.
						if ttsStopReceived && !audioFinalized && vclient != nil && !lastFrameTime.IsZero() && timeSinceLastFrame > 250*time.Millisecond {
							llmHandler.mu.Lock()
							// Re-check under lock to avoid duplicate finalization.
							if llmHandler.audioFinalized {
								llmHandler.mu.Unlock()
								continue
							}
							// Mark finalized to prevent double-send.
							llmHandler.audioFinalized = true
							// Snapshot buffer for flush.
							buf := llmHandler.accumulatedBuffer
							llmHandler.accumulatedBuffer = []byte{}
							llmHandler.ttsStopped = true
							llmHandler.mu.Unlock()

							// Flush any remaining buffer (pad to 1024 boundary, paced).
							if len(buf) > 0 {
								finalBuf := padToMultiple(buf, vectorAudioChunkBytes)
								for len(finalBuf) >= vectorAudioChunkBytes {
									chunk := finalBuf[:vectorAudioChunkBytes]
									finalBuf = finalBuf[vectorAudioChunkBytes:]
									llmHandler.sendMu.Lock()
									err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
										AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
											AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
												AudioChunkSizeBytes: uint32(len(chunk)),
												AudioChunkSamples:   chunk,
											},
										},
									})
									llmHandler.sendMu.Unlock()
									if err != nil {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Failed to flush final chunk on stop: %v", err))
										break
									}
									time.Sleep(getVectorChunkPace())
								}
							}

							// Send AudioStreamComplete.
							llmHandler.sendMu.Lock()
							err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
									AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
								},
							})
							llmHandler.sendMu.Unlock()
							if err != nil {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Failed to send AudioStreamComplete on stop: %v", err))
							} else {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ AudioStreamComplete sent (stop-finalize) (total chunks: %d)", chunkCount))
								select {
								case llmHandler.audioStreamCompleteChan <- true:
								default:
								}
							}

							// Signal TTS stop (now that finalize is done).
							select {
							case llmHandler.ttsStopChan <- true:
								logger.Println("[Xiaozhi KG Handler] ✅ TTS stop signal sent (after finalize)")
							default:
							}

							// Stop helper goroutines.
							if llmHandler.healthCheckStop != nil {
								select {
								case llmHandler.healthCheckStop <- true:
								default:
								}
							}
							if llmHandler.flushTimerStop != nil {
								select {
								case llmHandler.flushTimerStop <- true:
								default:
								}
							}

							continue
						}

						// IMPORTANT: Do NOT stream chunks from the flush timer.
						// We already stream chunks in the main audio-handler path (per incoming Opus frame).
						// Having BOTH send paths causes jitter/bursts (even with sendMu serialization),
						// which can sound like dropped chunks / choppy playback on the robot.
						//
						// Flush timer responsibilities:
						// - Auto-send AudioStreamComplete when stream ends (no frames)
						// - Flush leftover buffer (<1024 bytes) if frames stop before reaching a full chunk
						if !ttsStopped && vclient != nil && len(accumulatedBuffer) > 0 && !lastFrameTime.IsZero() && timeSinceLastFrame > 500*time.Millisecond {
							// Pad leftover to 1024 boundary and send it, then keep remaining (if any) for next tick.
							finalBuf := padToMultiple(accumulatedBuffer, vectorAudioChunkBytes)
							if len(finalBuf) >= vectorAudioChunkBytes && time.Since(lastSendTime) > 50*time.Millisecond {
								chunkToSend := finalBuf[:vectorAudioChunkBytes]
								remaining := finalBuf[vectorAudioChunkBytes:]
								llmHandler.sendMu.Lock()
								err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
									AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
										AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
											AudioChunkSizeBytes: uint32(len(chunkToSend)),
											AudioChunkSamples:   chunkToSend,
										},
									},
								})
								llmHandler.sendMu.Unlock()
								if err != nil {
									if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  vclient stream closed (EOF/closed)"))
										llmHandler.mu.Lock()
										llmHandler.vclient = nil
										llmHandler.mu.Unlock()
										continue
									}
									logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Error flushing leftover audio chunk: %v", err))
								} else {
									llmHandler.mu.Lock()
									llmHandler.chunkCount++
									llmHandler.chunksSentSinceLastCheck++
									llmHandler.accumulatedBuffer = remaining
									llmHandler.lastSendTime = time.Now()
									llmHandler.mu.Unlock()
									time.Sleep(60 * time.Millisecond)
								}
							}
						}
					case <-llmHandler.flushTimerStop:
						return
					}
				}
			}()

			// Start health check goroutine to monitor audio playback
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] PANIC in health check goroutine (recovered): %v", r))
					}
					llmHandler.mu.Lock()
					if llmHandler.healthCheckTicker != nil {
						llmHandler.healthCheckTicker.Stop()
					}
					llmHandler.mu.Unlock()
				}()
				for {
					select {
					case <-llmHandler.healthCheckTicker.C:
						llmHandler.mu.Lock()
						vclient := llmHandler.vclient
						chunkCount := llmHandler.chunkCount
						chunksSentSinceLastCheck := llmHandler.chunksSentSinceLastCheck
						lastHealthCheck := llmHandler.lastHealthCheck
						esn := llmHandler.esn
						llmHandler.mu.Unlock()

						// Health check: DO NOT resend AudioStreamPrepare during active playback.
						// In practice, resending Prepare mid-stream can reset Vector's audio pipeline and cause
						// audible dropouts (chunks "missing") even when vclient.Send() returns success.
						//
						// Safer logic:
						// - Only consider a "recover" action if we've been completely stuck:
						//   no chunks sent since last check AND no new audio frames for a long time.
						// - Otherwise just reset counters.
						timeSinceLastCheck := time.Since(lastHealthCheck)
						timeSinceLastFrame := time.Since(llmHandler.lastFrameTime)

						shouldRecover := vclient != nil &&
							chunkCount > 0 &&
							chunksSentSinceLastCheck == 0 &&
							timeSinceLastCheck > 6*time.Second &&
							!llmHandler.lastFrameTime.IsZero() &&
							timeSinceLastFrame > 6*time.Second

						if shouldRecover {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🏥 Health check: stream appears stuck (no frames for %v, no chunks sent for %v, total chunks: %d). Not resending AudioStreamPrepare to avoid dropouts; will remove client so next request recreates stream.", timeSinceLastFrame, timeSinceLastCheck, chunkCount))
							RemoveAudioClient(esn)
						}

						// Reset counters for next interval
						llmHandler.mu.Lock()
						llmHandler.lastHealthCheck = time.Now()
						llmHandler.chunksSentSinceLastCheck = 0
						llmHandler.mu.Unlock()
					case <-llmHandler.healthCheckStop:
						return
					}
				}
			}()

			// Start audio queue
			WaitForAudio_Queue(esn)
			StartAudio_Queue(esn)
			llmHandler.mu.Lock()
			llmHandler.audioQueueStarted = true
			llmHandler.mu.Unlock()
		}
	}

	// Verify LLM handler still registered after audio setup (registered early above).
	if deviceID != "" {
		maxRetries := 5
		handlerVerified := false
		for i := 0; i < maxRetries; i++ {
			verifyHandler := xiaozhi.GetLLMHandler(deviceID)
			if verifyHandler != nil && verifyHandler.IsActive() {
				handlerVerified = true
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler verified BEFORE text query (audio ready: %v, retry: %d/%d)", esn, vclient != nil && audioPrepareSent, i+1, maxRetries))
				break
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Handler verification failed (retry %d/%d): handler=%v, active=%v", esn, i+1, maxRetries, verifyHandler != nil, verifyHandler != nil && verifyHandler.IsActive()))
			if i < maxRetries-1 {
				time.Sleep(20 * time.Millisecond)
				llmHandler.SetActive(true)
				xiaozhi.SetLLMHandler(deviceID, llmHandler)
			}
		}
		if !handlerVerified {
			verifyHandler := xiaozhi.GetLLMHandler(deviceID)
			if verifyHandler != nil {
				verifyHandler.SetActive(true)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler force-activated as last resort", esn))
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ ERROR - LLM handler is nil before text query for deviceID: %s", esn, deviceID))
				return "", fmt.Errorf("LLM handler is nil - connection may not exist in manager for deviceID: %s", deviceID)
			}
		}
		time.Sleep(50 * time.Millisecond)
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Ready to send text query", esn))
	} else {
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - deviceID is empty, cannot register LLM handler! This will cause llm/tts messages to be ignored!", esn))
	}

	// Step 3: Send text query (giống botkct.py line 789 - gửi trực tiếp sau khi nhận STT, KHÔNG gửi listen start)
	// Nếu dùng connection từ STT, sessionID đã có sẵn
	// Nếu tạo connection mới, sessionID đã được extract từ hello response ở trên
	// botkct.py (line 634-638) sử dụng format: {"session_id": "...", "type": "text", "text": "..."}
	// botkct.py KHÔNG gửi listen start trước text message, nó gửi text message trực tiếp trên cùng connection
	textForUpstream := AppendXiaozhiUserTextCommandHint(transcribedText)
	textMessage := map[string]interface{}{
		"type": "text",
		"text": textForUpstream,
	}
	// Extract session_id from hello response if available (giống botkct.py)
	if sessionID != "" {
		textMessage["session_id"] = sessionID
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Using session_id from hello for text query: %s", esn, sessionID))
	}
	// KHÔNG thêm device_id hay client_id vào message body (theo botkct.py)

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== SENDING TEXT QUERY ==========", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Sending text query: %s", esn, textForUpstream))
	textMessageJSON, _ := json.Marshal(textMessage)
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text message JSON: %s", esn, string(textMessageJSON)))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Connection status: conn == nil? %v, connFromSTT? %v, sessionID: %s", esn, conn == nil, connFromSTT, sessionID))

	// Use WriteJSON helper if connection is in manager (to serialize writes)
	if deviceID != "" {
		if err := xiaozhi.WriteJSON(deviceID, textMessage); err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ ERROR - Failed to send text query: %v", esn, err))
			return "", fmt.Errorf("failed to send text query: %w", err)
		}
	} else {
		if err := conn.WriteJSON(textMessage); err != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ ERROR - Failed to send text query: %v", esn, err))
			return "", fmt.Errorf("failed to send text query: %w", err)
		}
	}

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Text query sent successfully to server", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========================================", esn))

	// No separate reader goroutine - using connection manager's reader
	// LLM handler will receive messages from connection manager's reader goroutine
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Using connection manager's reader goroutine (no separate reader)", esn))

	// Text query was already sent above (after hello response)
	// Now we wait for LLM response from the server
	// Messages will be handled by connection manager's reader goroutine and routed to LLM handler

	// Send text query (we need to convert to audio first)
	// For now, we'll send a simple text message
	// In production, you'd use TTS to convert text to Opus audio first
	logger.Println("Xiaozhi KG: Sending query: " + textForUpstream)

	// Note: xiaozhi expects audio input, so we need to handle text differently
	// For now, we'll just wait for response

	// Calculate shouldContinueConversation BEFORE starting audio processing goroutine
	// This allows us to trigger DoNewRequest immediately after AudioStreamComplete
	// NOTE: We'll update this when we receive the text response (to check for {{newVoiceRequest||now}})
	shouldContinueConversation := false
	if vars.APIConfig.Knowledge.SaveChat {
		shouldContinueConversation = true
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ SaveChat enabled - continuous conversation will be activated after audio", esn))
	} else if isConversationMode {
		shouldContinueConversation = true
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Conversation mode explicitly requested - continuous conversation will be activated after audio", esn))
	}

	// Audio processing is now synchronous - handled directly in LLM handler when receiving Opus frames
	// Setup DoNewRequest trigger after TTS stops (in a separate goroutine to avoid blocking)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] PANIC in DoNewRequest goroutine (recovered): %v", r))
			}
		}()
		// IMPORTANT: KHÔNG cancel audio context hoặc stop audio queue khi timeout
		// Chỉ cancel/stop khi TTS thực sự dừng (nhận được TTS stop event)
		// Điều này đảm bảo audio playback không bị interrupt khi TTS dài
		ttsStopped := false
		defer func() {
			// Chỉ cancel audio context và stop audio queue khi TTS đã dừng
			// Không cancel nếu timeout (TTS vẫn đang tiếp tục)
			if ttsStopped {
				// TTS đã dừng - stop audio queue but DON'T cancel audio context (keep pipeline warm)
				// Audio client is in pool and will be reused for next TTS
				StopAudio_Queue(esn)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stopped - audio queue stopped (audio client kept in pool for reuse)", esn))
			} else {
				// Timeout - TTS vẫn đang tiếp tục, KHÔNG cancel
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout - TTS may still be in progress, NOT canceling audio context to avoid interrupting playback", esn))
			}
		}()

		// IMPORTANT: Đợi TTS stop event - KHÔNG timeout và tiếp tục nếu TTS vẫn đang tiếp tục
		// Chỉ gọi DoNewRequest khi TTS thực sự dừng (nhận được TTS stop event từ server)
		// Điều này đảm bảo robot không bị dừng audio playback giữa chừng
		select {
		case <-ttsStopChan:
			ttsStopped = true
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received (robot dừng nói), waiting for AudioStreamComplete...", esn))
		case <-time.After(24 * time.Hour):
			// Timeout rất dài (24 giờ) - thực tế không bao giờ timeout
			// Chỉ để tránh goroutine chạy mãi mãi nếu có lỗi
			// Log warning nhưng KHÔNG cancel audio context để audio tiếp tục phát
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Very long timeout reached (24h), TTS/nhạc may still be in progress. NOT calling DoNewRequest to avoid interrupting audio.", esn))
			// KHÔNG tiếp tục - return để tránh interrupt audio playback
			// KHÔNG cancel audio context - để audio tiếp tục phát
			// ttsStopped vẫn là false, defer sẽ không cancel audio context
			return
		}

		// IMPORTANT: Đợi AudioStreamComplete được gửi TRƯỚC khi gọi DoNewRequest
		// Điều này đảm bảo vclient không bị đóng khi DoNewRequest được gọi
		// Flow: TTS stop → Wait for AudioStreamComplete → Deactivate LLM → Activate STT → DoNewRequest
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for AudioStreamComplete to be sent...", esn))
		select {
		case <-llmHandler.audioStreamCompleteChan:
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamComplete confirmed sent, safe to call DoNewRequest", esn))
		case <-time.After(10 * time.Second):
			// Timeout dài hơn (10s) để đợi AudioStreamComplete
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Timeout waiting for AudioStreamComplete (10s), proceeding anyway (may cause issues)", esn))
		}
		if shouldContinueConversation && connFromSTT && deviceID != "" {
			// Deactivate LLM handler NGAY LẬP TỨC - connection manager's reader will route messages to STT handler
			// (giống botkct.py - không cần "release connection", chỉ cần deactivate handler)
			llmHandler.SetActive(false)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))

			// Note: ReleaseConnection is now no-op (giống botkct.py - connection luôn available)
			// Connection will be reused automatically when STT handler is active
			xiaozhi.ReleaseConnection(deviceID) // No-op, kept for clarity
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection ready for STT handler (connection always available, routing handles it)", esn))
		}

		// Debug: Log conditions for DoNewRequest
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🔍 DoNewRequest conditions: shouldContinueConversation=%v, robot!=nil=%v", esn, shouldContinueConversation, robot != nil))
		if !shouldContinueConversation {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  DoNewRequest skipped: shouldContinueConversation=false (SaveChat=%v, isConversationMode=%v)", esn, vars.APIConfig.Knowledge.SaveChat, isConversationMode))
		}
		if robot == nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  DoNewRequest skipped: robot is nil", esn))
		}

		// Trigger DoNewRequest if continuous conversation is enabled
		if shouldContinueConversation && robot != nil {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 🎤 Starting continuous listening trigger...", esn))

			// Activate STT handler BEFORE calling DoNewRequest
			if vars.APIConfig.Knowledge.Provider == "xiaozhi" && deviceID != "" {
				if activated := xiaozhi.ActivateSTTHandler(deviceID); activated {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ STT handler activated", esn))
				}
				// Send listen start message to xiaozhi server
				listenStart := map[string]interface{}{
					"type":  "listen",
					"state": "start",
					"mode":  "auto",
				}
				if err := xiaozhi.WriteJSON(deviceID, listenStart); err == nil {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Listen start message sent to xiaozhi server", esn))
				}
			}

			// Call DoNewRequest 3 times with shorter delay between attempts
			maxAttempts := 3
			attemptInterval := 500 * time.Millisecond
			timeout := 10 * time.Second
			timeoutChan := time.After(timeout)

			for attempt := 1; attempt <= maxAttempts; attempt++ {
				// Check timeout
				select {
				case <-timeoutChan:
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏱️  Timeout reached (%v), stopping DoNewRequest attempts", esn, timeout))
					return
				default:
				}

				// Check if robot is already listening
				if vars.APIConfig.Knowledge.Provider == "xiaozhi" && deviceID != "" {
					if xiaozhi.IsRobotListening(deviceID) {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot is already listening, skipping DoNewRequest (attempt %d/%d)", esn, attempt, maxAttempts))
						return
					}
				}

				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | 📞 Attempt %d/%d: Calling DoNewRequest()...", esn, attempt, maxAttempts))
				DoNewRequest(robot)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ DoNewRequest() completed (attempt %d/%d) - checking if robot is listening...", esn, attempt, maxAttempts))

				// NOTE: Removed SayText('a') suppression due to intermittent errors on some robots.
				// Just wait a bit after DoNewRequest before checking listening state.
				time.Sleep(200 * time.Millisecond)

				// Wait and check if robot is listening
				time.Sleep(500 * time.Millisecond)
				if vars.APIConfig.Knowledge.Provider == "xiaozhi" && deviceID != "" {
					if xiaozhi.IsRobotListening(deviceID) {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot is now listening (after attempt %d/%d), stopping retry loop - SUCCESS!", esn, attempt, maxAttempts))
						return
					}
					if xiaozhi.IsSTTHandlerActive(deviceID) {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ STT handler is active but robot not yet listening, waiting 1s more...", esn))
						time.Sleep(1 * time.Second)
						if xiaozhi.IsRobotListening(deviceID) {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Robot is now listening after additional wait (after attempt %d/%d), stopping retry loop - SUCCESS!", esn, attempt, maxAttempts))
							return
						} else {
							logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  STT handler active but robot still not listening after wait, will retry if attempts remaining", esn))
						}
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Robot not yet listening after attempt %d/%d, will retry if attempts remaining", esn, attempt, maxAttempts))
					}
				}

				// If not last attempt, wait before next attempt
				if attempt < maxAttempts {
					select {
					case <-timeoutChan:
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏱️  Timeout reached, stopping attempts", esn))
						return
					case <-time.After(attemptInterval):
						// Continue to next attempt
					}
				}
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Completed all %d DoNewRequest attempts", esn, maxAttempts))
		}
	}()

	// Wait for completion
	// Ensure esn is not empty to avoid nil pointer issues
	if esn == "" {
		esn = "unknown"
	}
	// Use recover to prevent panic from logger
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Log to stderr directly to avoid logger issues
				fmt.Fprintf(os.Stderr, "[Xiaozhi KG] PANIC in logger (recovered): %v\n", r)
			}
		}()
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for upstream reply (text or tts/audio, timeout: %v; ESP32-style)...", esn, getKGLLMResponseTimeout()))
	}()
	var text string
	select {
	case text = <-textResponse:
		if text == "" {
			text = "(empty response)"
		}
	case <-llmHandler.replyStarted:
		// xiaozhi-esp32: server may send tts start or Opus before any llm text — do not treat as failure.
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Reply pipeline started (tts/audio before text) — waiting up to %v for optional text...", esn, getKGTextAfterAudioTimeout()))
		select {
		case text = <-textResponse:
			if text == "" {
				text = "(empty response)"
			}
		case <-time.After(getKGTextAfterAudioTimeout()):
			text = ""
		}
		if strings.TrimSpace(text) == "" {
			text = "(streaming)"
		}
	case <-time.After(getKGLLMResponseTimeout()):
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ TIMEOUT - No upstream reply (text or tts/audio) after %v (increase XIAOZHI_KG_LLM_TIMEOUT_SEC if needed)", esn, getKGLLMResponseTimeout()))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Debug info: textResponse len=%d, errChan len=%d", esn, len(textResponse), len(errChan)))

		if deviceID != "" {
			if connFromSTT {
				llmHandler.SetActive(false)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (timeout case)", esn))
			}
			xiaozhi.ReleaseConnection(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (timeout case - STT can reuse)", esn))
		}

		llmHandler.mu.Lock()
		audioQueueStarted := llmHandler.audioQueueStarted
		llmHandler.mu.Unlock()
		if audioQueueStarted && esn != "" {
			StopAudio_Queue(esn)
		}

		return "", fmt.Errorf("timeout waiting for response")
	case err := <-errChan:
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Error received from errChan: %v", esn, err))

		if deviceID != "" {
			if connFromSTT {
				llmHandler.SetActive(false)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (error case)", esn))
			}
			xiaozhi.ReleaseConnection(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (error case - STT can reuse)", esn))
		}

		llmHandler.mu.Lock()
		audioQueueStarted := llmHandler.audioQueueStarted
		llmHandler.mu.Unlock()
		if audioQueueStarted && esn != "" {
			StopAudio_Queue(esn)
		}

		return "", err
	}

	// Common success path: received text and/or ESP32-style streaming started.
	// Use recover to prevent panic from logger
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[Xiaozhi KG] PANIC in logger (recovered): %v, esn: %s, text: %s\n", r, esn, text)
			}
		}()
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== LLM / upstream RESPONSE OK ==========", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Response text for caller: '%s'", esn, text))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text length: %d bytes", esn, len(text)))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ==============================================", esn))
	}()

	// Wait for TTS stop event before releasing connection
	// Audio processing is now synchronous (handled directly in LLM handler), so we just wait for TTS stop
	// Keep WebSocket reader goroutine running continuously (like xiaozhi-esp32-main)
	if connFromSTT {
		// If continuous conversation is enabled, connection will be released in DoNewRequest goroutine
		// Otherwise, release it here after TTS stops
		if !shouldContinueConversation {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for TTS stop event before releasing connection...", esn))
			select {
			case <-ttsStopChan:
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received, audio processing completed (synchronous)", esn))
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting a bit more for server to send any remaining messages...", esn))
				time.Sleep(2 * time.Second)
				if deviceID != "" {
					llmHandler.SetActive(false)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
				}
				if deviceID != "" {
					xiaozhi.ReleaseConnection(deviceID)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
				}
			case <-time.After(24 * time.Hour):
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Very long timeout reached (24h), releasing connection anyway", esn))
				if deviceID != "" {
					llmHandler.SetActive(false)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
				}
				if deviceID != "" {
					xiaozhi.ReleaseConnection(deviceID)
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
				}
			}
		} else {
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Continuous conversation enabled - connection release handled by DoNewRequest goroutine (released immediately after TTS stop)", esn))
		}
	} else {
		select {
		case <-ttsStopChan:
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received (synchronous audio processing)", esn))
		case <-time.After(10 * time.Second):
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for TTS stop (10s), proceeding anyway", esn))
		}
	}

	return text, nil
}
