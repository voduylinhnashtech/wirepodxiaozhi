package vars

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
)

// a way to create a JSON configuration for wire-pod, rather than the use of env vars

var ApiConfigPath = "./apiConfig.json"

// Default OpenWeatherMap preset (setup.html shows the same; users may replace the key).
const (
	DefaultWeatherProvider = "openweathermap.org"
	DefaultWeatherAPIKey   = "403a79621d25f8fdee7c468bbd16b820"
)

// WeatherProviderNone is stored when the user explicitly disables weather in the UI ("None").
const WeatherProviderNone = "none"

var APIConfig apiConfig

type apiConfig struct {
	Weather struct {
		Enable   bool   `json:"enable"`
		Provider string `json:"provider"`
		Key      string `json:"key"`
		Unit     string `json:"unit"`
	} `json:"weather"`
	Knowledge struct {
		Enable                 bool    `json:"enable"`
		Provider               string  `json:"provider"`
		Key                    string  `json:"key"`
		ID                     string  `json:"id"`
		Model                  string  `json:"model"`
		IntentGraph            bool    `json:"intentgraph"`
		RobotName              string  `json:"robotName"`
		OpenAIPrompt           string  `json:"openai_prompt"`
		OpenAIVoice            string  `json:"openai_voice"`
		OpenAIVoiceWithEnglish bool    `json:"openai_voice_with_english"`
		SaveChat               bool    `json:"save_chat"`
		CommandsEnable         bool    `json:"commands_enable"`
		Endpoint               string  `json:"endpoint"`
		DeviceID               string  `json:"device_id"`               // MAC address cho xiaozhi
		ClientID               string  `json:"client_id"`               // Client ID (UUID) cho xiaozhi
		XiaozhiTTSVolume       string  `json:"xiaozhi_tts_volume"`      // normal|medium|high (maps to 1x|2x|4x)
		XiaozhiDisableIntent   bool    `json:"xiaozhi_disable_intent"`  // Disable local intent matching for Xiaozhi
		TopP                   float32 `json:"top_p"`
		Temperature            float32 `json:"temp"`
	} `json:"knowledge"`
	STT struct {
		Service            string `json:"provider"`
		Language           string `json:"language"`
		IntentMatchingMode string `json:"intent_matching_mode"` // "single" or "multilingual"
	} `json:"STT"`
	Server struct {
		// false for ip, true for escape pod
		EPConfig bool   `json:"epconfig"`
		Port     string `json:"port"`
	} `json:"server"`
	HasReadFromEnv   bool `json:"hasreadfromenv"`
	PastInitialSetup bool `json:"pastinitialsetup"`
}

func WriteConfigToDisk() {
	logger.Println("Configuration changed, writing to disk")
	writeBytes, _ := json.Marshal(APIConfig)
	os.WriteFile(ApiConfigPath, writeBytes, 0644)
}

func CreateConfigFromEnv() {
	// if no config exists, create it
	if os.Getenv("WEATHERAPI_ENABLED") == "true" {
		APIConfig.Weather.Enable = true
		APIConfig.Weather.Provider = os.Getenv("WEATHERAPI_PROVIDER")
		APIConfig.Weather.Key = os.Getenv("WEATHERAPI_KEY")
		APIConfig.Weather.Unit = os.Getenv("WEATHERAPI_UNIT")
	} else {
		APIConfig.Weather.Enable = false
	}
	if os.Getenv("KNOWLEDGE_ENABLED") == "true" {
		APIConfig.Knowledge.Enable = true
		APIConfig.Knowledge.Provider = os.Getenv("KNOWLEDGE_PROVIDER")
		if os.Getenv("KNOWLEDGE_PROVIDER") == "houndify" {
			APIConfig.Knowledge.ID = os.Getenv("KNOWLEDGE_ID")
		}
		APIConfig.Knowledge.Key = os.Getenv("KNOWLEDGE_KEY")
	} else {
		APIConfig.Knowledge.Enable = false
	}
	WriteSTT()
	APIConfig.HasReadFromEnv = true
	writeBytes, _ := json.Marshal(APIConfig)
	os.WriteFile(ApiConfigPath, writeBytes, 0644)
}

func WriteSTT() {
	// was not part of the original code, so this is its own function
	// launched if stt not found in config
	APIConfig.STT.Service = os.Getenv("STT_SERVICE")
	if os.Getenv("STT_SERVICE") == "vosk" || os.Getenv("STT_SERVICE") == "whisper.cpp" {
		APIConfig.STT.Language = os.Getenv("STT_LANGUAGE")
	}
}

// applyWirepodProductionDefaults matches the old initial.html flow (Escape Pod + Submit):
// Escape Pod (epconfig), port 443, Xiaozhi STT; initwirepod writes server_config.json after vars.Init.
func applyWirepodProductionDefaults() {
	logger.Println("🔧 Applying default production settings (Xiaozhi STT + Escape Pod + Knowledge Graph = Xiaozhi)")
	APIConfig.Server.EPConfig = true
	APIConfig.Server.Port = "443"
	APIConfig.STT.Service = "xiaozhi"
	APIConfig.STT.Language = "vi-VN"
	APIConfig.STT.IntentMatchingMode = "multilingual"
	APIConfig.Knowledge.Enable = true
	APIConfig.Knowledge.Provider = "xiaozhi"
	APIConfig.PastInitialSetup = true
	APIConfig.HasReadFromEnv = true
	logger.Println("✅ Defaults applied: EPConfig=true, Port=443, STT=xiaozhi, KG provider=xiaozhi")
	applyDefaultOpenWeatherMap()
}

// applyDefaultOpenWeatherMap sets bundled OpenWeatherMap defaults when weather was never configured.
// Skips if the user saved "None" (provider WeatherProviderNone).
func applyDefaultOpenWeatherMap() {
	if strings.TrimSpace(APIConfig.Weather.Provider) == WeatherProviderNone {
		return
	}
	if APIConfig.Weather.Provider != "" || APIConfig.Weather.Key != "" {
		return
	}
	APIConfig.Weather.Provider = DefaultWeatherProvider
	APIConfig.Weather.Key = DefaultWeatherAPIKey
	APIConfig.Weather.Enable = true
	if strings.TrimSpace(APIConfig.Weather.Unit) == "" {
		APIConfig.Weather.Unit = "C"
	}
	logger.Println("🌤 Default weather: OpenWeatherMap key preset (user may change in setup.html)")
}

func ReadConfig() {
	if _, err := os.Stat(ApiConfigPath); err != nil {
		CreateConfigFromEnv()
		applyDefaultOpenWeatherMap()
		if !APIConfig.PastInitialSetup {
			applyWirepodProductionDefaults()
		}
		writeBytes, _ := json.Marshal(APIConfig)
		os.WriteFile(ApiConfigPath, writeBytes, 0644)
		logger.Println("API config JSON created")
		return
	} else {
		// read config
		configBytes, err := os.ReadFile(ApiConfigPath)
		if err != nil {
			APIConfig.Knowledge.Enable = false
			APIConfig.Weather.Enable = false
			logger.Println("Failed to read API config file")
			logger.Println(err)
			return
		}
		err = json.Unmarshal(configBytes, &APIConfig)
		if err != nil {
			APIConfig.Knowledge.Enable = false
			APIConfig.Weather.Enable = false
			logger.Println("Failed to unmarshal API config JSON")
			logger.Println(err)
			return
		}
		// stt service is the only thing controlled by shell
		if APIConfig.STT.Service != os.Getenv("STT_SERVICE") {
			WriteSTT()
		}
		if !APIConfig.HasReadFromEnv {
			if APIConfig.Server.Port != os.Getenv("DDL_RPC_PORT") {
				APIConfig.HasReadFromEnv = true
				APIConfig.PastInitialSetup = true
			}
		}

		if APIConfig.Knowledge.Model == "meta-llama/Llama-2-70b-chat-hf" {
			logger.Println("Setting Together model to Llama3")
			APIConfig.Knowledge.Model = "meta-llama/Llama-3-70b-chat-hf"
		}

		// First-time installs: same as former initial.html + use_ep (no wizard page).
		if !APIConfig.PastInitialSetup {
			applyWirepodProductionDefaults()
			logger.Println("ℹ️  Skipping initial.html — defaults saved; open main UI to adjust if needed")
		}

		applyDefaultOpenWeatherMap()

		writeBytes, _ := json.Marshal(APIConfig)
		os.WriteFile(ApiConfigPath, writeBytes, 0644)
		logger.Println("API config successfully read")
	}
}
