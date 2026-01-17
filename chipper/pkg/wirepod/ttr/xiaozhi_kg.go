package wirepod_ttr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
)

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
	Ctx    context.Context
	Cancel context.CancelFunc
	Robot  *vector.Vector // Keep robot reference to recreate if needed
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
	lastFrameTime           time.Time // Track when last audio frame was received
	esn                     string    // Robot ESN for checking first audio playback
	mu                      sync.RWMutex
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
			if text, ok := event["text"].(string); ok && text != "" {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM text: '%s'", text))
				select {
				case h.textResponse <- text:
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ LLM text sent to channel: '%s'", text))
				default:
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  textResponse channel is full, dropping text"))
				}
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
					h.mu.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS started, ready to receive Opus frames"))
				} else if state == "sentence_start" {
					// TTS sentence_start contains the full text response (priority over LLM event)
					if text, ok := event["text"].(string); ok && text != "" {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS sentence_start text: '%s'", text))
						select {
						case h.textResponse <- text:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS sentence_start text sent to channel: '%s'", text))
						default:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  textResponse channel is full, dropping text"))
						}
					}
				} else if state == "stop" {
					// TTS stopped - send final buffer and AudioStreamComplete (synchronous processing)
					h.mu.Lock()
					h.ttsStopped = true
					vclient := h.vclient
					accumulatedBuffer := h.accumulatedBuffer
					chunkCount := h.chunkCount
					flushTimerStop := h.flushTimerStop
					h.mu.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔊 TTS stopped, sending final buffer and completion"))

					// IMPORTANT: Stop flush timer FIRST and wait for it to fully stop
					// This ensures all pending chunks are sent before AudioStreamComplete
					if flushTimerStop != nil {
						select {
						case flushTimerStop <- true:
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Flush timer stop signal sent"))
						default:
						}
						// Wait for flush timer to fully stop (give it time to finish current flush cycle)
						time.Sleep(200 * time.Millisecond)
					}

					// Signal TTS stop
					select {
					case h.ttsStopChan <- true:
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ TTS stop signal sent"))
					default:
					}

					// Wait longer for any remaining frames (1 second) to ensure all audio is received
					time.Sleep(1 * time.Second)

					// Re-check buffer after wait (in case new frames arrived)
					h.mu.Lock()
					accumulatedBuffer = h.accumulatedBuffer
					h.mu.Unlock()

					// Send final buffer and AudioStreamComplete
					if vclient != nil {
						// Send final buffer if any
						if len(accumulatedBuffer) > 0 {
							// Pad to Vector-friendly chunk boundary, then send in 1024-byte chunks paced like /api-sdk/play_sound
							sentFinal := 0
							finalBuf := padToMultiple(accumulatedBuffer, vectorAudioChunkBytes)
							for len(finalBuf) >= vectorAudioChunkBytes {
								chunk := finalBuf[:vectorAudioChunkBytes]
								finalBuf = finalBuf[vectorAudioChunkBytes:]
								err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
									AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
										AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
											AudioChunkSizeBytes: uint32(len(chunk)),
											AudioChunkSamples:   chunk,
										},
									},
								})
								if err != nil {
									logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send final padded audio chunk: %v", err))
									break
								}
								sentFinal++
								time.Sleep(time.Millisecond * 60)
							}
							// Clear buffer after sending final padded chunks
							h.mu.Lock()
							h.accumulatedBuffer = []byte{}
							h.mu.Unlock()
							if sentFinal > 0 {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent %d final padded chunk(s) before AudioStreamComplete", sentFinal))
								time.Sleep(200 * time.Millisecond)
							}
						}

						// IMPORTANT: Double-check buffer is empty before sending AudioStreamComplete
						// This ensures all chunks have been sent
						h.mu.Lock()
						if len(h.accumulatedBuffer) > 0 {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  WARNING - Buffer still has %d bytes, sending as final chunk before AudioStreamComplete", len(h.accumulatedBuffer)))
							finalChunk := padToMultiple(h.accumulatedBuffer, vectorAudioChunkBytes)
							h.accumulatedBuffer = []byte{}
							h.mu.Unlock()
							// Send remaining buffer with retry logic (like OpenAI TTS), chunked to 1024 bytes
							maxRetries := 3
							retryDelay := 10 * time.Millisecond
							var err error
							for len(finalChunk) >= vectorAudioChunkBytes {
								chunk := finalChunk[:vectorAudioChunkBytes]
								finalChunk = finalChunk[vectorAudioChunkBytes:]
								for retry := 0; retry < maxRetries; retry++ {
									err = vclient.Send(&vectorpb.ExternalAudioStreamRequest{
										AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
											AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
												AudioChunkSizeBytes: uint32(len(chunk)),
												AudioChunkSamples:   chunk,
											},
										},
									})
									if err == nil {
										time.Sleep(time.Millisecond * 60)
										break
									}
									if retry < maxRetries-1 {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Retry %d/%d sending final buffer chunk: %v", retry+1, maxRetries, err))
										time.Sleep(retryDelay)
										retryDelay *= 2
									}
								}
								if err != nil {
									break
								}
							}
							if err != nil {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Failed to send remaining buffer after %d retries: %v", maxRetries, err))
							}
						} else {
							h.mu.Unlock()
						}

						// Send AudioStreamComplete - NOW all chunks should be sent
						// Add retry logic to ensure complete is always sent (like OpenAI TTS)
						maxRetries := 3
						retryDelay := 10 * time.Millisecond
						var err error
						for retry := 0; retry < maxRetries; retry++ {
							err = vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
									AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
								},
							})
							if err == nil {
								break
							}
							if retry < maxRetries-1 {
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Retry %d/%d sending AudioStreamComplete: %v", retry+1, maxRetries, err))
								time.Sleep(retryDelay)
								retryDelay *= 2
							}
						}
						if err != nil {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send AudioStreamComplete after %d retries: %v", maxRetries, err))
						} else {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ AudioStreamComplete sent (total chunks sent: %d)", chunkCount))
							// Clear buffer
							h.mu.Lock()
							h.accumulatedBuffer = []byte{}
							h.mu.Unlock()
							// IMPORTANT: Signal that AudioStreamComplete has been sent
							// This allows DoNewRequest to proceed safely (vclient won't be closed)
							select {
							case h.audioStreamCompleteChan <- true:
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ AudioStreamComplete signal sent"))
							default:
							}
							// IMPORTANT: Wait longer after AudioStreamComplete to ensure robot starts playing audio
							// Robot needs time to process all chunks and start playback
							time.Sleep(1 * time.Second)
						}
					}
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
			// Signal TTS stop if not already signaled (this will trigger final buffer send)
			select {
			case h.ttsStopChan <- true:
			default:
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

		// Resample 24kHz → 8kHz
		downsampledChunks := resample24kTo8kSimple(framePCMBytes)
		if len(downsampledChunks) == 0 {
			if count <= 5 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Resample returned 0 chunks (frame #%d, PCM size: %d bytes) - input may be too small", count, len(framePCMBytes)))
			}
			return nil
		}

		// Log resample success for first few frames
		if count <= 5 {
			totalDownsampled := 0
			for _, chunk := range downsampledChunks {
				totalDownsampled += len(chunk)
			}
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Resampled: frame #%d → %d chunks, %d bytes total (24kHz→8kHz)", count, len(downsampledChunks), totalDownsampled))
		}

		// Accumulate into buffer
		for _, c := range downsampledChunks {
			accumulatedBuffer = append(accumulatedBuffer, c...)
		}

		// Log buffer status for first few frames
		if count <= 5 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 📦 Buffer after frame #%d: %d bytes accumulated", count, len(accumulatedBuffer)))
		}

		// Send audio chunks using the same pattern as Play Audio (Vector control beta)
		// Play Audio logic (from /api-sdk/play_sound):
		// 1. Chia file PCM thành chunks 1024 bytes
		// 2. Gửi từng chunk với delay 60ms giữa các chunks
		// 3. Format: AudioFrameRate: 8000, AudioVolume: 100
		//
		// Code hiện tại áp dụng logic tương tự:
		// - Chunk size ưu tiên: 1024 bytes (giống Play Audio)
		// - Delay giữa chunks: 60ms (giống Play Audio)
		// - AudioFrameRate: 8000 (giống Play Audio)
		// - AudioVolume: 100 (giống Play Audio)
		// - Gửi AudioStreamComplete khi xong (giống Play Audio)
		//
		// Khác biệt: Play Audio chia file trước, code này accumulate buffer real-time
		// nhưng vẫn ưu tiên gửi chunk 1024 bytes khi buffer đủ lớn
		chunksSentInFrame := 0
		bufferSizeBeforeSend := len(accumulatedBuffer)
		if count <= 5 {
			logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔄 Attempting to send chunks: buffer size=%d bytes, vclient=%v", bufferSizeBeforeSend, vclient != nil))
		}
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
			if count <= 5 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 📤 Sending chunk: size=%d bytes, vclient=%v", len(chunkToSend), vclient != nil))
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

				// Log before Send to track if it blocks
				if count <= 5 || retry == 0 {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔵 Calling vclient.Send() (retry %d/%d)...", retry+1, maxRetries))
				}

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

				// Log after Send to track completion
				if count <= 5 || retry == 0 {
					if err == nil {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🟢 vclient.Send() completed successfully"))
					} else {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🔴 vclient.Send() returned error: %v", err))
					}
				}

				if err == nil {
					// Success - break retry loop
					if count <= 5 {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🟡 vclient.Send() succeeded, updating counters..."))
					}
					chunksSentInFrame++
					// IMPORTANT: Update chunkCount when sending chunks directly (not via flush timer)
					// Lock is already released above, so we can lock again safely
					h.mu.Lock()
					if count <= 5 {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🟡 Lock acquired, updating chunkCount..."))
					}
					h.chunkCount++
					chunkCount := h.chunkCount
					h.mu.Unlock()
					if count <= 5 {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🟡 Lock released, chunkCount=%d", chunkCount))
					}
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent audio chunk #%d (%d bytes) to robot (from frame #%d, total chunks: %d)", chunksSentInFrame, len(chunkToSend), count, chunkCount))
					if count <= 5 {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] 🟡 Breaking retry loop..."))
					}
					// Re-lock to update buffer for next iteration (don't break yet, continue loop)
					h.mu.Lock()
					h.accumulatedBuffer = accumulatedBuffer
					// Check if there's more data to send
					accumulatedBuffer = h.accumulatedBuffer
					h.mu.Unlock()
					// Break retry loop, continue to next chunk if buffer >= vectorAudioChunkBytes
					break
				} else {
					// Always log errors for first few frames
					if count <= 5 || retry == 0 {
						logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Error sending chunk (retry %d/%d): %v", retry+1, maxRetries, err))
					}
				}
				// If error is EOF/closed, don't retry
				if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  ERROR - Failed to send audio chunk (stream closed): %v", err))
					// Lock is already released, so we can lock again safely
					h.mu.Lock()
					h.vclient = nil
					h.mu.Unlock()
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
			if chunkCount == 1 || chunkCount%50 == 0 {
				logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ✅ Sent audio chunk #%d (%d bytes) to robot", chunkCount, len(chunkToSend)))
			}
			// CRITICAL: Longer delay for first chunk ONLY on first audio playback
			// This fixes "first TTS doesn't play, second TTS works" issue
			// Robot needs extra time after receiving first chunk to start audio playback (only on first use)
			// After first playback, robot is already warm, so normal delay is sufficient
			// After first chunk of each session, use normal 60ms delay (giống Play Audio - /api-sdk/play_sound)
			if chunkCount == 1 {
				h.mu.RLock()
				esn := h.esn
				h.mu.RUnlock()
				// Check if this is first audio playback for this robot
				// If client was reused from pool, pipeline is already warm, so shorter delay
				// If this is new client, need longer delay
				isFirstAudio := !hasRobotPlayedAudio(esn)
				if isFirstAudio {
					time.Sleep(time.Millisecond * 500) // Extra delay for first chunk on FIRST audio playback (new client, robot warm-up)
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⏱️  First chunk sent (FIRST audio), waiting 500ms for robot audio pipeline warm-up"))
				} else {
					// Robot has played before, but check if client was reused or new
					// If client was reused, delay is already handled in StreamingXiaozhiKG (150ms)
					// Here we just need normal delay for first chunk
					time.Sleep(time.Millisecond * 60) // Normal delay for first chunk (robot already warm or client reused)
					logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⏱️  First chunk sent (subsequent audio or reused client), normal 60ms delay"))
				}
			} else {
				time.Sleep(time.Millisecond * 60) // Normal delay for subsequent chunks
			}
			// Re-lock for next iteration
			h.mu.Lock()
			accumulatedBuffer = h.accumulatedBuffer
		}
		// Final update of buffer
		h.accumulatedBuffer = accumulatedBuffer
		h.mu.Unlock()

		// Log first few frames and then every 10th frame to track audio processing
		if count <= 5 || count%10 == 0 {
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
			// Reuse existing audio client (pipeline already warm!)
			vclient = poolEntry.Client
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ REUSING persistent audio client (pipeline already warm)", esn))

			// CRITICAL: Must send AudioStreamPrepare again for each TTS session
			// Vector SDK requires Prepare before each audio playback (even with same client)
			err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
				AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamPrepare{
					AudioStreamPrepare: &vectorpb.ExternalAudioStreamPrepare{
						AudioFrameRate: 8000,
						AudioVolume:    100,
					},
				},
			})
			if err != nil {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to send AudioStreamPrepare on reused client: %v. Will try to recreate.", esn, err))
				// Remove invalid client from pool and create new one
				audioClientPoolMutex.Lock()
				delete(audioClientPool, esn)
				audioClientPoolMutex.Unlock()
				// Fall through to create new client
				vclient = nil
				audioPrepareSent = false
			} else {
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent on reused client (8kHz, volume 100)", esn))
				audioPrepareSent = true
				// Delay for reused client (need time for robot to process Prepare even if pipeline is warm)
				// Increased from 50ms to 150ms to ensure robot is ready
				time.Sleep(time.Millisecond * 150)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (150ms for reused client)", esn))
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
				err = vclient.Send(&vectorpb.ExternalAudioStreamRequest{
					AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamPrepare{
						AudioStreamPrepare: &vectorpb.ExternalAudioStreamPrepare{
							AudioFrameRate: 8000,
							AudioVolume:    100,
						},
					},
				})
				if err != nil {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  WARNING - Failed to send AudioStreamPrepare: %v. Disabling audio playback.", esn, err))
					vclient = nil
					audioPrepareSent = false
				} else {
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ AudioStreamPrepare sent successfully (8kHz, volume 100)", esn))
					audioPrepareSent = true

					// Delay only needed for FIRST time creating client (robot warm-up)
					// If client is reused from pool, pipeline is already warm (handled above)
					// This is a NEW client creation, so check if robot has played audio before
					isFirstAudio := !hasRobotPlayedAudio(esn)
					if isFirstAudio {
						time.Sleep(time.Millisecond * 700) // Longer delay for first audio (robot warm-up) - increased from 500ms
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (700ms for FIRST audio - robot warm-up)", esn))
					} else {
						// Robot has played audio before, but this is a NEW client (pool was empty or old client failed)
						// Still need some delay, but less than first time
						time.Sleep(time.Millisecond * 200) // Medium delay for new client but robot has played before
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Delay after AudioStreamPrepare completed (200ms for new client, robot has played before)", esn))
					}

					// Store in pool for reuse (keep pipeline warm)
					audioClientPoolMutex.Lock()
					audioClientPool[esn] = &AudioClientEntry{
						Client: audioClient,
						Ctx:    persistentAudioCtx,
						Cancel: persistentAudioCancel,
						Robot:  robot,
					}
					audioClientPoolMutex.Unlock()
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio client stored in pool (will be reused for next TTS, pipeline stays warm)", esn))
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
			llmHandler.mu.Unlock()
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Audio setup complete BEFORE text query - vclient and opusDecoder ready", esn))

			// Start flush timer goroutine
			go func() {
				defer func() {
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
						chunkCount := llmHandler.chunkCount
						audioChunkCount := llmHandler.audioChunkCount
						llmHandler.mu.Unlock()

						// Check if we should auto-send AudioStreamComplete
						// If no new frames for 2 seconds and we have sent at least 1 chunk, send complete
						// Also check if lastFrameTime is not zero (at least one frame was received)
						timeSinceLastFrame := time.Since(lastFrameTime)
						// Log debug info every 500ms to track timer activity
						if timeSinceLastFrame > 500*time.Millisecond && !lastFrameTime.IsZero() && chunkCount > 0 && !ttsStopped {
							if int(timeSinceLastFrame.Seconds()*2)%2 == 0 { // Log every ~500ms
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⏱️  Flush timer active: lastFrame=%v ago, chunkCount=%d, ttsStopped=%v, vclient=%v", timeSinceLastFrame, chunkCount, ttsStopped, vclient != nil))
							}
						}
						if !ttsStopped && vclient != nil && chunkCount > 0 && !lastFrameTime.IsZero() && timeSinceLastFrame > 2*time.Second {
							logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⏰ No new audio frames for 2s (last frame: %v ago, chunkCount: %d, audioFrames: %d), auto-sending AudioStreamComplete", timeSinceLastFrame, chunkCount, audioChunkCount))
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
									err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
										AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
											AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
												AudioChunkSizeBytes: uint32(len(chunk)),
												AudioChunkSamples:   chunk,
											},
										},
									})
									if err != nil {
										logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Failed to send final buffer chunk before auto-complete: %v", err))
										break
									}
									sentFinal++
									time.Sleep(time.Millisecond * 60)
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
							err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamComplete{
									AudioStreamComplete: &vectorpb.ExternalAudioStreamComplete{},
								},
							})
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

						if len(accumulatedBuffer) >= vectorAudioChunkBytes && vclient != nil && time.Since(lastSendTime) > 50*time.Millisecond {
							chunkToSend := accumulatedBuffer[:vectorAudioChunkBytes]
							remaining := accumulatedBuffer[vectorAudioChunkBytes:]
							err := vclient.Send(&vectorpb.ExternalAudioStreamRequest{
								AudioRequestType: &vectorpb.ExternalAudioStreamRequest_AudioStreamChunk{
									AudioStreamChunk: &vectorpb.ExternalAudioStreamChunk{
										AudioChunkSizeBytes: uint32(len(chunkToSend)),
										AudioChunkSamples:   chunkToSend,
									},
								},
							})
							if err != nil {
								if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
									logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  vclient stream closed (EOF/closed)"))
									llmHandler.mu.Lock()
									llmHandler.vclient = nil
									llmHandler.mu.Unlock()
									continue
								}
								logger.Println(fmt.Sprintf("[Xiaozhi KG Handler] ⚠️  Error sending audio chunk: %v", err))
							} else {
								llmHandler.mu.Lock()
								llmHandler.chunkCount++
								llmHandler.accumulatedBuffer = remaining
								llmHandler.lastSendTime = time.Now()
								llmHandler.mu.Unlock()
								time.Sleep(time.Millisecond * 60)
							}
						}
					case <-llmHandler.flushTimerStop:
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

	// CRITICAL: Register LLM handler BEFORE sending text query
	// Server may send llm/tts messages immediately after receiving text query
	// Handler must be ready to receive these messages
	if deviceID != "" {
		// Register handler with audio already setup
		llmHandler.SetActive(true)
		xiaozhi.SetLLMHandler(deviceID, llmHandler)
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler registered BEFORE text query (audio ready: %v)", esn, vclient != nil && audioPrepareSent))
	}

	// Step 3: Send text query (giống botkct.py line 789 - gửi trực tiếp sau khi nhận STT, KHÔNG gửi listen start)
	// Nếu dùng connection từ STT, sessionID đã có sẵn
	// Nếu tạo connection mới, sessionID đã được extract từ hello response ở trên
	// botkct.py (line 634-638) sử dụng format: {"session_id": "...", "type": "text", "text": "..."}
	// botkct.py KHÔNG gửi listen start trước text message, nó gửi text message trực tiếp trên cùng connection
	textMessage := map[string]interface{}{
		"type": "text",
		"text": transcribedText,
	}
	// Extract session_id from hello response if available (giống botkct.py)
	if sessionID != "" {
		textMessage["session_id"] = sessionID
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Using session_id from hello for text query: %s", esn, sessionID))
	}
	// KHÔNG thêm device_id hay client_id vào message body (theo botkct.py)

	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== SENDING TEXT QUERY ==========", esn))
	logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Sending text query: %s", esn, transcribedText))
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
	logger.Println("Xiaozhi KG: Sending query: " + transcribedText)

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
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting for LLM response (timeout: 30s)...", esn))
	}()
	select {
	case text := <-textResponse:
		// Ensure text is not nil/empty to avoid issues
		if text == "" {
			text = "(empty response)"
		}
		// Use recover to prevent panic from logger
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Log to stderr directly to avoid logger issues
					fmt.Fprintf(os.Stderr, "[Xiaozhi KG] PANIC in logger (recovered): %v, esn: %s, text: %s\n", r, esn, text)
				}
			}()
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ========== LLM TEXT RESPONSE RECEIVED ==========", esn))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM response text: '%s'", esn, text))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text length: %d bytes", esn, len(text)))
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Text will be returned to caller", esn))
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
				// Wait for TTS stop event (audio processing is synchronous, so it's already done when TTS stops)
				select {
				case <-ttsStopChan:
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received, audio processing completed (synchronous)", esn))
					// Wait a bit for WebSocket reader goroutine to process remaining messages
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⏳ Waiting a bit more for server to send any remaining messages...", esn))
					time.Sleep(2 * time.Second) // Reduced wait time
					// Deactivate LLM handler - connection manager's reader will route messages to STT handler
					if deviceID != "" {
						llmHandler.SetActive(false)
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
					}
					// Release connection (don't close it) so STT can reuse for next request
					if deviceID != "" {
						xiaozhi.ReleaseConnection(deviceID)
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
					}
				case <-time.After(24 * time.Hour):
					// Timeout rất dài (24 giờ) - thực tế không bao giờ timeout
					// Chỉ để tránh goroutine chạy mãi mãi nếu có lỗi
					logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Very long timeout reached (24h), releasing connection anyway", esn))
					// Deactivate LLM handler - connection manager's reader will route messages to STT handler
					if deviceID != "" {
						llmHandler.SetActive(false)
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (connection manager will route to STT handler)", esn))
					}
					// Release connection (don't close it) so STT can reuse for next request
					if deviceID != "" {
						xiaozhi.ReleaseConnection(deviceID)
						logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (STT handler will handle next request)", esn))
					}
				}
			} else {
				// Continuous conversation enabled - connection is released IMMEDIATELY after TTS stop in goroutine
				// Don't wait here because ttsStopChan is already consumed by DoNewRequest goroutine
				// The goroutine handles: TTS stop → Release connection → Activate STT → DoNewRequest
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Continuous conversation enabled - connection release handled by DoNewRequest goroutine (released immediately after TTS stop)", esn))
			}
		} else {
			// For new connections, wait for TTS stop event
			select {
			case <-ttsStopChan:
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ TTS stop event received (synchronous audio processing)", esn))
			case <-time.After(10 * time.Second):
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ⚠️  Timeout waiting for TTS stop (10s), proceeding anyway", esn))
			}
		}

		// NOTE: Continuous conversation flow (in separate goroutine):
		// 1. TTS stop event received
		// 2. Release connection IMMEDIATELY (so STT can use it)
		// 3. Deactivate LLM handler (connection manager routes to STT handler)
		// 4. Activate STT handler
		// 5. Send listen start message to server
		// 6. Call DoNewRequest to open robot mic
		// 7. Robot speaks → STT handler uses released connection to send audio
		// This allows continuous conversation without needing "hey vector" each time

		// Don't cancel audioCtx here - let audio processing goroutine cancel it when done
		// This prevents vclient stream from closing while audio is still being sent
		return text, nil
	case <-time.After(30 * time.Second):
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ TIMEOUT - No LLM response received after 30 seconds", esn))
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | Debug info: textResponse channel length=%d, errChan length=%d", esn, len(textResponse), len(errChan)))

		// IMPORTANT: Deactivate LLM handler and release connection on timeout
		// This allows STT to reuse the connection for next request
		if deviceID != "" {
			// Deactivate LLM handler
			if connFromSTT {
				llmHandler.SetActive(false)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (timeout case)", esn))
			}
			// Release connection so STT can reuse it
			xiaozhi.ReleaseConnection(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (timeout case - STT can reuse)", esn))
		}

		// Stop audio queue if started
		llmHandler.mu.Lock()
		audioQueueStarted := llmHandler.audioQueueStarted
		llmHandler.mu.Unlock()
		if audioQueueStarted && esn != "" {
			StopAudio_Queue(esn)
		}

		// DON'T cancel audioCtx - audio client is in pool and will be reused
		// audioCancelSafe() // REMOVED - keep pipeline warm
		return "", fmt.Errorf("timeout waiting for response")
	case err := <-errChan:
		logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ❌ Error received from errChan: %v", esn, err))

		// IMPORTANT: Deactivate LLM handler and release connection on error
		// This allows STT to reuse the connection for next request
		if deviceID != "" {
			// Deactivate LLM handler
			if connFromSTT {
				llmHandler.SetActive(false)
				logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ LLM handler deactivated (error case)", esn))
			}
			// Release connection so STT can reuse it
			xiaozhi.ReleaseConnection(deviceID)
			logger.Println(fmt.Sprintf("[Xiaozhi KG] Device: %s | ✅ Connection released (error case - STT can reuse)", esn))
		}

		// Stop audio queue if started
		llmHandler.mu.Lock()
		audioQueueStarted := llmHandler.audioQueueStarted
		llmHandler.mu.Unlock()
		if audioQueueStarted && esn != "" {
			StopAudio_Queue(esn)
		}

		// DON'T cancel audioCtx - audio client is in pool and will be reused
		// audioCancelSafe() // REMOVED - keep pipeline warm
		return "", err
	}
}
