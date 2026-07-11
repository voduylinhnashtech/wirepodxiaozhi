package xiaozhi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
)

var (
	otaSyncOnce   sync.Once
	otaSyncResult error
)

// GetOTAHTTPBase converts wss/ws base URL to https://host/ (no /xiaozhi/v1 path).
func GetOTAHTTPBase() string {
	baseURL := GetBaseURL()
	httpURL := strings.Replace(baseURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)
	if strings.Contains(httpURL, "/xiaozhi/v1/") {
		httpURL = strings.TrimSuffix(httpURL, "/xiaozhi/v1/")
	}
	if strings.Contains(httpURL, "/xiaozhi/v1") {
		httpURL = strings.TrimSuffix(httpURL, "/xiaozhi/v1")
	}
	if !strings.HasSuffix(httpURL, "/") {
		httpURL += "/"
	}
	return httpURL
}

// GetUserAgent returns an ESP32-style User-Agent (BOARD_NAME/version).
func GetUserAgent() string {
	v := strings.TrimSpace(vars.CommitSHA)
	if v == "" {
		v = "1.0"
	}
	return "wirepodxiaozhi/" + v
}

// GetAcceptLanguage matches ESP32 Lang::CODE usage on HTTP OTA.
func GetAcceptLanguage() string {
	if vars.APIConfig.STT.Language != "" {
		return vars.APIConfig.STT.Language
	}
	return "vi-VN"
}

// GetProtocolVersion returns websocket protocol version from OTA/config (ESP32 settings "version").
func GetProtocolVersion() int {
	if vars.APIConfig.Knowledge.Provider == "xiaozhi" && vars.APIConfig.Knowledge.XiaozhiProtocolVersion > 0 {
		return vars.APIConfig.Knowledge.XiaozhiProtocolVersion
	}
	return 1
}

// GetWebsocketToken returns Bearer token from OTA response if stored.
func GetWebsocketToken() string {
	if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
		return strings.TrimSpace(vars.APIConfig.Knowledge.XiaozhiToken)
	}
	return ""
}

// BuildSystemInfoJSON builds OTA POST body similar to xiaozhi-esp32 Board::GetSystemInfoJson (simplified).
func BuildSystemInfoJSON(deviceID, clientID string) []byte {
	hostname, _ := os.Hostname()
	lang := GetAcceptLanguage()
	payload := map[string]interface{}{
		"version":                2,
		"language":               lang,
		"mac_address":            deviceID,
		"uuid":                   clientID,
		"platform":               runtime.GOOS,
		"arch":                   runtime.GOARCH,
		"hostname":               hostname,
		"minimum_free_heap_size": 0,
		"application": map[string]interface{}{
			"name":    "wirepodxiaozhi",
			"version": GetUserAgent(),
		},
		"board": map[string]interface{}{
			"type":   "wirepod",
			"name":   "wire-pod",
			"vendor": "wirepodxiaozhi",
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// BuildWebsocketHTTPHeaders returns headers for WSS dial (websocket_protocol.cc OpenAudioChannel).
func BuildWebsocketHTTPHeaders(deviceID, clientID string) http.Header {
	h := http.Header{}
	ver := GetProtocolVersion()
	h.Set("Protocol-Version", fmt.Sprintf("%d", ver))
	if deviceID != "" {
		h.Set("Device-Id", deviceID)
	}
	if clientID != "" {
		h.Set("Client-Id", clientID)
	}
	if token := GetWebsocketToken(); token != "" {
		if !strings.Contains(token, " ") {
			token = "Bearer " + token
		}
		h.Set("Authorization", token)
	}
	return h
}

// BuildOTAHTTPHeaders returns headers for POST /xiaozhi/ota/ (ota.cc SetupHttp).
func BuildOTAHTTPHeaders(deviceID, clientID string) http.Header {
	h := http.Header{}
	h.Set("Activation-Version", "1")
	if sn := strings.TrimSpace(os.Getenv("XIAOZHI_SERIAL_NUMBER")); sn != "" {
		h.Set("Activation-Version", "2")
		h.Set("Serial-Number", sn)
	}
	if deviceID != "" {
		h.Set("Device-Id", deviceID)
	}
	if clientID != "" {
		h.Set("Client-Id", clientID)
	}
	h.Set("User-Agent", GetUserAgent())
	h.Set("Accept-Language", GetAcceptLanguage())
	h.Set("Content-Type", "application/json")
	return h
}

// BuildHelloEvent returns hello JSON like ESP32 GetHelloMessage (no "language" field).
func BuildHelloEvent() map[string]interface{} {
	ver := GetProtocolVersion()
	return map[string]interface{}{
		"type":      "hello",
		"version":   ver,
		"transport": "websocket",
		"features": map[string]interface{}{
			"mcp": true,
			"aec": false,
		},
		"audio_params": map[string]interface{}{
			"format":         "opus",
			"sample_rate":    16000,
			"channels":       1,
			"frame_duration": 60,
		},
	}
}

// applyOtaResponse applies websocket/url/token/version from upstream OTA JSON (ESP32 CheckVersion).
func applyOtaResponse(result map[string]interface{}) {
	if wsAny, ok := result["websocket"]; ok {
		if ws, ok := wsAny.(map[string]interface{}); ok {
			if u, ok := ws["url"].(string); ok && strings.TrimSpace(u) != "" {
				vars.APIConfig.Knowledge.Endpoint = strings.TrimSpace(u)
				logger.Println("Xiaozhi OTA: applied websocket.url -> endpoint " + vars.APIConfig.Knowledge.Endpoint)
			}
			if t, ok := ws["token"].(string); ok && strings.TrimSpace(t) != "" {
				vars.APIConfig.Knowledge.XiaozhiToken = strings.TrimSpace(t)
				logger.Println("Xiaozhi OTA: stored websocket token from server")
			}
			if v, ok := ws["version"].(float64); ok && int(v) > 0 {
				vars.APIConfig.Knowledge.XiaozhiProtocolVersion = int(v)
				logger.Println(fmt.Sprintf("Xiaozhi OTA: protocol version %d", int(v)))
			}
		}
	}
}

// OtaCheckSync POSTs system info to .../xiaozhi/ota/ like ESP32 before opening WebSocket.
func OtaCheckSync(deviceID, clientID string) error {
	if deviceID == "" || clientID == "" {
		return fmt.Errorf("deviceID and clientID required for OTA check")
	}
	otaURL := GetOTAHTTPBase() + "xiaozhi/ota/"
	body := BuildSystemInfoJSON(deviceID, clientID)
	req, err := http.NewRequest("POST", otaURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = BuildOTAHTTPHeaders(deviceID, clientID)

	client := &http.Client{Timeout: 30 * time.Second}
	logger.Println(fmt.Sprintf("Xiaozhi OTA: POST %s (body %d bytes, Device-Id=%s)", otaURL, len(body), deviceID))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OTA check status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("OTA JSON parse: %w", err)
	}
	applyOtaResponse(result)
	vars.WriteConfigToDisk()

	if act, ok := result["activation"].(map[string]interface{}); ok {
		if code, _ := act["code"].(string); code != "" {
			logger.Println(fmt.Sprintf("Xiaozhi OTA: device needs activation (code present). Message: %v", act["message"]))
		}
	}
	return nil
}

// EnsureOtaSyncOnce runs OtaCheckSync once per process when Xiaozhi is enabled.
func EnsureOtaSyncOnce() error {
	if vars.APIConfig.Knowledge.Provider != "xiaozhi" {
		return nil
	}
	deviceID := GetDeviceIDFromConfig()
	clientID := GetClientIDFromConfig()
	if deviceID == "" || clientID == "" {
		return fmt.Errorf("missing device_id or client_id in config")
	}
	otaSyncOnce.Do(func() {
		otaSyncResult = OtaCheckSync(deviceID, clientID)
		if otaSyncResult != nil {
			logger.Println("Xiaozhi OTA: " + otaSyncResult.Error() + " (continuing with configured endpoint)")
			otaSyncResult = nil
		} else {
			logger.Println("Xiaozhi OTA: check completed (ESP32-style POST)")
		}
	})
	return otaSyncResult
}
