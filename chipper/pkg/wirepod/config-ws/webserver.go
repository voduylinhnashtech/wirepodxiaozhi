package webserver

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/scripting"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/wirepod/localization"
	processreqs "github.com/kercre123/wire-pod/chipper/pkg/wirepod/preqs"
	botsetup "github.com/kercre123/wire-pod/chipper/pkg/wirepod/setup"
	"github.com/kercre123/wire-pod/chipper/pkg/xiaozhi"
	"gopkg.in/ini.v1"
)

var SttInitFunc func() error

func apiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")

	switch strings.TrimPrefix(r.URL.Path, "/api/") {
	case "add_custom_intent":
		handleAddCustomIntent(w, r)
	case "edit_custom_intent":
		handleEditCustomIntent(w, r)
	case "get_custom_intents_json":
		handleGetCustomIntentsJSON(w)
	case "remove_custom_intent":
		handleRemoveCustomIntent(w, r)
	case "set_weather_api":
		handleSetWeatherAPI(w, r)
	case "get_weather_api":
		handleGetWeatherAPI(w)
	case "set_kg_api":
		handleSetKGAPI(w, r)
	case "get_kg_api":
		handleGetKGAPI(w)
	case "set_stt_info":
		handleSetSTTInfo(w, r)
	case "get_download_status":
		handleGetDownloadStatus(w)
	case "get_stt_info":
		handleGetSTTInfo(w)
	case "get_config":
		handleGetConfig(w)
	case "get_logs":
		handleGetLogs(w)
	case "get_debug_logs":
		handleGetDebugLogs(w)
	case "is_running":
		handleIsRunning(w)
	case "delete_chats":
		handleDeleteChats(w)
	case "get_ota":
		handleGetOTA(w, r)
	case "get_version_info":
		handleGetVersionInfo(w)
	case "generate_certs":
		handleGenerateCerts(w)
	case "is_api_v3":
		fmt.Fprintf(w, "it is!")
	case "bot_auth_export":
		handleBotAuthExport(w, r)
	case "bot_auth_import":
		handleBotAuthImport(w, r)
	case "xiaozhi_generate_pairing_code":
		handleXiaozhiGeneratePairingCode(w, r)
	case "xiaozhi_validate_pairing_code":
		handleXiaozhiValidatePairingCode(w, r)
	case "xiaozhi_consume_pairing_code":
		handleXiaozhiConsumePairingCode(w, r)
	case "xiaozhi_activate":
		handleXiaozhiActivate(w, r)
	case "xiaozhi_remove_device":
		handleXiaozhiRemoveDevice(w, r)
	case "xiaozhi_list_devices":
		handleXiaozhiListDevices(w, r)
	case "xiaozhi_get_connected_devices":
		handleXiaozhiGetConnectedDevices(w, r)
	case "xiaozhi_check_device_status":
		handleXiaozhiCheckDeviceStatus(w, r)
	case "xiaozhi_get_local_mac":
		handleXiaozhiGetLocalMAC(w, r)
	case "xiaozhi_get_pairing_status":
		handleXiaozhiGetPairingStatus(w, r)
	case "xiaozhi_check_version":
		handleXiaozhiCheckVersion(w, r)
	case "xiaozhi_get_client_ip":
		handleXiaozhiGetClientIP(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// ---- Bot auth bundle export/import (for migrating a robot between machines) ----

type botSdkInfoJSON struct {
	GlobalGUID string `json:"global_guid"`
	Robots     []struct {
		Esn       string `json:"esn"`
		IPAddress string `json:"ip_address"`
		GUID      string `json:"guid"`
		Activated bool   `json:"activated"`
	} `json:"robots"`
}

type jdocEntryJSON struct {
	Thing string     `json:"thing"`
	Name  string     `json:"name"`
	Jdoc  vars.AJdoc `json:"jdoc"`
}

func normalizeESN(esn string) string {
	return strings.TrimSpace(strings.ToLower(esn))
}

func zipAddBytes(zw *zip.Writer, name string, b []byte) error {
	h := &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: time.Now(),
	}
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func zipAddFile(zw *zip.Writer, name string, srcPath string) error {
	b, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return zipAddBytes(zw, name, b)
}

func handleBotAuthExport(w http.ResponseWriter, r *http.Request) {
	esn := normalizeESN(r.URL.Query().Get("esn"))
	if esn == "" {
		http.Error(w, "missing esn", http.StatusBadRequest)
		return
	}

	type exportManifest struct {
		ESN      string            `json:"esn"`
		Included []string          `json:"included"`
		Missing  map[string]string `json:"missing"`
		Notes    []string          `json:"notes"`
	}
	manifest := exportManifest{
		ESN:     esn,
		Missing: map[string]string{},
		Notes: []string{
			"Some files are optional and will be missing if they do not exist on this machine.",
			"To make GUID/token work on another machine, you typically need botSdkInfo + session-certs + .anki_vector certs (and sometimes jdocs). This export includes all matching session-certs/<*> for the ESN when available.",
		},
	}
	addIncluded := func(name string) {
		manifest.Included = append(manifest.Included, name)
	}
	addMissing := func(name string, why string) {
		manifest.Missing[name] = why
	}

	// Load botSdkInfo.json from disk
	var info botSdkInfoJSON
	botBytes, err := os.ReadFile(vars.BotInfoPath)
	if err != nil {
		http.Error(w, "failed to read botSdkInfo.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.Unmarshal(botBytes, &info); err != nil {
		http.Error(w, "failed to parse botSdkInfo.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter to requested ESN (keep global_guid)
	var filtered botSdkInfoJSON
	filtered.GlobalGUID = info.GlobalGUID
	for _, rob := range info.Robots {
		if normalizeESN(rob.Esn) == esn {
			filtered.Robots = append(filtered.Robots, rob)
		}
	}
	if len(filtered.Robots) == 0 {
		http.Error(w, "esn not found in botSdkInfo.json", http.StatusNotFound)
		return
	}

	filename := fmt.Sprintf("wirepod-bot-auth-%s.zip", esn)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// README
	if err := zipAddBytes(zw, "README.txt", []byte("Wire-Pod bot auth bundle\n\nContents:\n- botSdkInfo.json (filtered)\n- jdocs.json (filtered for vic:<esn>)\n- session-certs/<files matching ESN>\n- .anki_vector/sdk_config.ini and cert(s) for this ESN\n\nImport this zip on the other machine via Bot Setup → Import.\n")); err == nil {
		addIncluded("README.txt")
	}

	// botSdkInfo.json (filtered)
	filteredBytes, _ := json.Marshal(filtered)
	if err := zipAddBytes(zw, "botSdkInfo.json", filteredBytes); err == nil {
		addIncluded("botSdkInfo.json")
	}

	// session-certs (optional): include any file in SessionCertPath that matches this ESN
	if vars.SessionCertPath != "" {
		entries, err := os.ReadDir(vars.SessionCertPath)
		if err == nil {
			foundAny := false
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				n := e.Name()
				// Most common: filename == ESN, but some setups may include additional suffixes.
				if normalizeESN(n) != esn && !strings.Contains(strings.ToLower(n), esn) {
					continue
				}
				foundAny = true
				src := filepath.Join(vars.SessionCertPath, n)
				dst := filepath.ToSlash(filepath.Join("session-certs", n))
				if err := zipAddFile(zw, dst, src); err == nil {
					addIncluded(dst)
				} else {
					addMissing(dst, "read failed: "+err.Error())
				}
			}
			if !foundAny {
				addMissing(filepath.ToSlash(filepath.Join("session-certs", esn)), "no matching files found in "+vars.SessionCertPath)
			}
		} else {
			addMissing("session-certs/", "cannot read dir "+vars.SessionCertPath+": "+err.Error())
		}
	} else {
		addMissing("session-certs/", "vars.SessionCertPath is empty")
	}

	// .anki_vector files (optional)
	if vars.SDKIniPath != "" {
		// sdk_config.ini
		iniPath := filepath.Join(vars.SDKIniPath, "sdk_config.ini")
		if _, err := os.Stat(iniPath); err == nil {
			name := filepath.ToSlash(filepath.Join(".anki_vector", "sdk_config.ini"))
			if err := zipAddFile(zw, name, iniPath); err == nil {
				addIncluded(name)
			} else {
				addMissing(name, "read failed: "+err.Error())
			}
		} else {
			addMissing(filepath.ToSlash(filepath.Join(".anki_vector", "sdk_config.ini")), "not found at "+iniPath)
		}
		// cert(s) matching this ESN
		entries, err := os.ReadDir(vars.SDKIniPath)
		if err == nil {
			var names []string
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				if !strings.HasSuffix(name, ".cert") {
					continue
				}
				if !strings.Contains(strings.ToLower(name), esn) {
					continue
				}
				dst := filepath.ToSlash(filepath.Join(".anki_vector", name))
				if err := zipAddFile(zw, dst, filepath.Join(vars.SDKIniPath, name)); err == nil {
					addIncluded(dst)
				} else {
					addMissing(dst, "read failed: "+err.Error())
				}
			}
			// If no cert matched, note it
			foundCert := false
			for _, inc := range manifest.Included {
				if strings.HasPrefix(inc, ".anki_vector/") && strings.HasSuffix(inc, ".cert") {
					foundCert = true
					break
				}
			}
			if !foundCert {
				addMissing(".anki_vector/<botname>-"+esn+".cert", "no matching *.cert found in "+vars.SDKIniPath)
			}
		} else {
			addMissing(".anki_vector/", "cannot read dir "+vars.SDKIniPath+": "+err.Error())
		}
	} else {
		addMissing(".anki_vector/", "vars.SDKIniPath is empty (not initialized or unsupported on this OS)")
	}

	// jdocs.json filtered (optional)
	if jdocsBytes, err := os.ReadFile(vars.JdocsPath); err == nil {
		var entries []jdocEntryJSON
		if err := json.Unmarshal(jdocsBytes, &entries); err == nil {
			var out []jdocEntryJSON
			wantThing := "vic:" + esn
			for _, e := range entries {
				if e.Thing == wantThing {
					out = append(out, e)
				}
			}
			if len(out) > 0 {
				outBytes, _ := json.Marshal(out)
				if err := zipAddBytes(zw, "jdocs.json", outBytes); err == nil {
					addIncluded("jdocs.json")
				}
			} else {
				addMissing("jdocs.json", "no entries for vic:"+esn+" in "+vars.JdocsPath)
			}
		} else {
			addMissing("jdocs.json", "failed to parse "+vars.JdocsPath+": "+err.Error())
		}
	} else {
		addMissing("jdocs.json", "not found at "+vars.JdocsPath)
	}

	// manifest
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	_ = zipAddBytes(zw, "MANIFEST.json", manifestBytes)
}

func safeZipReadAll(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, rc, limit+1); err != nil && err != io.EOF {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, fmt.Errorf("file too large: %s", f.Name)
	}
	return buf.Bytes(), nil
}

func handleBotAuthImport(w http.ResponseWriter, r *http.Request) {
	// limit upload to 25MB
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		http.Error(w, "invalid multipart upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	zipBytes, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "failed to read upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		http.Error(w, "invalid zip: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Load current botSdkInfo.json
	var current botSdkInfoJSON
	if b, err := os.ReadFile(vars.BotInfoPath); err == nil {
		_ = json.Unmarshal(b, &current)
	}

	// Load current jdocs.json (optional)
	var currentJdocs []jdocEntryJSON
	if b, err := os.ReadFile(vars.JdocsPath); err == nil {
		_ = json.Unmarshal(b, &currentJdocs)
	}
	jdocMap := make(map[string]jdocEntryJSON)
	for _, e := range currentJdocs {
		jdocMap[e.Thing+"|"+e.Name] = e
	}

	importedBots := 0
	importedCerts := 0
	importedJdocs := 0
	importedSDKIniWrites := 0

	// If the zip contains a sdk_config.ini, we will MERGE only the imported ESN sections into local sdk_config.ini.
	var importedSDKIniBytes []byte
	importedESNs := map[string]bool{}

	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		switch {
		case name == "botSdkInfo.json":
			b, err := safeZipReadAll(f, 2<<20)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var imp botSdkInfoJSON
			if err := json.Unmarshal(b, &imp); err != nil {
				http.Error(w, "invalid botSdkInfo.json in zip: "+err.Error(), http.StatusBadRequest)
				return
			}
			// merge global guid (only if current empty)
			if current.GlobalGUID == "" && imp.GlobalGUID != "" {
				current.GlobalGUID = imp.GlobalGUID
			}
			// upsert robots
			for _, rob := range imp.Robots {
				esn := normalizeESN(rob.Esn)
				if esn == "" {
					continue
				}
				importedESNs[esn] = true
				found := false
				for i := range current.Robots {
					if normalizeESN(current.Robots[i].Esn) == esn {
						current.Robots[i] = rob
						found = true
						break
					}
				}
				if !found {
					current.Robots = append(current.Robots, rob)
				}
				importedBots++
			}
		case strings.HasPrefix(name, "session-certs/"):
			base := strings.TrimPrefix(name, "session-certs/")
			base = normalizeESN(base)
			if base == "" || strings.Contains(base, "/") {
				continue
			}
			b, err := safeZipReadAll(f, 5<<20)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = os.MkdirAll(vars.SessionCertPath, 0777)
			if err := os.WriteFile(filepath.Join(vars.SessionCertPath, base), b, 0644); err == nil {
				importedCerts++
			}
		case strings.HasPrefix(name, ".anki_vector/"):
			base := filepath.Base(name)
			if base == "" || base == "." || base == ".." {
				continue
			}
			// only allow sdk_config.ini and *.cert
			if base != "sdk_config.ini" && !strings.HasSuffix(strings.ToLower(base), ".cert") {
				continue
			}
			b, err := safeZipReadAll(f, 5<<20)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if base == "sdk_config.ini" {
				// Defer merge until after we know imported ESNs (from botSdkInfo.json)
				importedSDKIniBytes = b
				continue
			}
			// cert files: write directly
			if vars.SDKIniPath != "" {
				_ = os.MkdirAll(vars.SDKIniPath, 0777)
				if err := os.WriteFile(filepath.Join(vars.SDKIniPath, base), b, 0644); err == nil {
					importedCerts++
				}
			}
		case name == "jdocs.json":
			b, err := safeZipReadAll(f, 10<<20)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var imp []jdocEntryJSON
			if err := json.Unmarshal(b, &imp); err != nil {
				// ignore invalid
				continue
			}
			for _, e := range imp {
				if e.Thing == "" || e.Name == "" {
					continue
				}
				jdocMap[e.Thing+"|"+e.Name] = e
				importedJdocs++
			}
		default:
			// ignore
		}
	}

	// Merge sdk_config.ini sections if present
	if len(importedSDKIniBytes) > 0 && vars.SDKIniPath != "" {
		_ = os.MkdirAll(vars.SDKIniPath, 0777)
		localPath := filepath.Join(vars.SDKIniPath, "sdk_config.ini")
		localIni, _ := ini.LooseLoad(localPath)
		if localIni == nil {
			localIni = ini.Empty()
		}
		importIni, err := ini.LoadSources(ini.LoadOptions{Loose: true, IgnoreInlineComment: true}, importedSDKIniBytes)
		if err == nil && importIni != nil {
			for esn := range importedESNs {
				sec, err := importIni.GetSection(esn)
				if err != nil || sec == nil {
					continue
				}
				// Ensure section exists in local and copy all keys
				localSec, _ := localIni.GetSection(esn)
				if localSec == nil {
					localSec, _ = localIni.NewSection(esn)
				}
				for _, k := range sec.Keys() {
					localSec.Key(k.Name()).SetValue(k.Value())
				}
				importedSDKIniWrites++
			}
			_ = localIni.SaveTo(localPath)
		}
	}

	// write merged botSdkInfo.json
	mergedBytes, _ := json.Marshal(current)
	if err := os.WriteFile(vars.BotInfoPath, mergedBytes, 0644); err != nil {
		http.Error(w, "failed to write botSdkInfo.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// update in-memory too
	vars.BotInfo.GlobalGUID = current.GlobalGUID
	vars.BotInfo.Robots = current.Robots

	// write merged jdocs.json
	if len(jdocMap) > 0 {
		var out []jdocEntryJSON
		for _, v := range jdocMap {
			out = append(out, v)
		}
		// stable-ish order
		sort.Slice(out, func(i, j int) bool {
			if out[i].Thing == out[j].Thing {
				return out[i].Name < out[j].Name
			}
			return out[i].Thing < out[j].Thing
		})
		outBytes, _ := json.Marshal(out)
		_ = os.MkdirAll(filepath.Dir(vars.JdocsPath), 0777)
		_ = os.WriteFile(vars.JdocsPath, outBytes, 0644)
	}

	fmt.Fprintf(w, "import ok (bots: %d, files: %d, sdk_config sections merged: %d, jdocs: %d). You may need to restart wire-pod.", importedBots, importedCerts, importedSDKIniWrites, importedJdocs)
}

func handleAddCustomIntent(w http.ResponseWriter, r *http.Request) {
	var intent vars.CustomIntent
	if err := json.NewDecoder(r.Body).Decode(&intent); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if anyEmpty(intent.Name, intent.Description, intent.Intent) || len(intent.Utterances) == 0 {
		http.Error(w, "missing required field (name, description, utterances, and intent are required)", http.StatusBadRequest)
		return
	}
	intent.LuaScript = strings.TrimSpace(intent.LuaScript)
	if intent.LuaScript != "" {
		if err := scripting.ValidateLuaScript(intent.LuaScript); err != nil {
			http.Error(w, "lua validation error: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	vars.CustomIntentsExist = true
	vars.CustomIntents = append(vars.CustomIntents, intent)
	saveCustomIntents()
	fmt.Fprint(w, "Intent added successfully.")
}

func handleEditCustomIntent(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Number int `json:"number"`
		vars.CustomIntent
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if request.Number < 1 || request.Number > len(vars.CustomIntents) {
		http.Error(w, "invalid intent number", http.StatusBadRequest)
		return
	}
	intent := &vars.CustomIntents[request.Number-1]
	if request.Name != "" {
		intent.Name = request.Name
	}
	if request.Description != "" {
		intent.Description = request.Description
	}
	if len(request.Utterances) != 0 {
		intent.Utterances = request.Utterances
	}
	if request.Intent != "" {
		intent.Intent = request.Intent
	}
	if request.Params.ParamName != "" {
		intent.Params.ParamName = request.Params.ParamName
	}
	if request.Params.ParamValue != "" {
		intent.Params.ParamValue = request.Params.ParamValue
	}
	if request.Exec != "" {
		intent.Exec = request.Exec
	}
	if request.LuaScript != "" {
		intent.LuaScript = request.LuaScript
		if err := scripting.ValidateLuaScript(intent.LuaScript); err != nil {
			http.Error(w, "lua validation error: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if len(request.ExecArgs) != 0 {
		intent.ExecArgs = request.ExecArgs
	}
	intent.IsSystemIntent = false
	saveCustomIntents()
	fmt.Fprint(w, "Intent edited successfully.")
}

func handleGetCustomIntentsJSON(w http.ResponseWriter) {
	if !vars.CustomIntentsExist {
		http.Error(w, "you must create an intent first", http.StatusBadRequest)
		return
	}
	customIntentJSONFile, err := os.ReadFile(vars.CustomIntentsPath)
	if err != nil {
		http.Error(w, "could not read custom intents file", http.StatusInternalServerError)
		logger.Println(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(customIntentJSONFile)
}

func handleRemoveCustomIntent(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Number int `json:"number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if request.Number < 1 || request.Number > len(vars.CustomIntents) {
		http.Error(w, "invalid intent number", http.StatusBadRequest)
		return
	}
	vars.CustomIntents = append(vars.CustomIntents[:request.Number-1], vars.CustomIntents[request.Number:]...)
	saveCustomIntents()
	fmt.Fprint(w, "Intent removed successfully.")
}

func handleSetWeatherAPI(w http.ResponseWriter, r *http.Request) {
	var config struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if config.Provider == "" {
		vars.APIConfig.Weather.Enable = false
	} else {
		vars.APIConfig.Weather.Enable = true
		vars.APIConfig.Weather.Key = strings.TrimSpace(config.Key)
		vars.APIConfig.Weather.Provider = config.Provider
	}
	vars.WriteConfigToDisk()
	fmt.Fprint(w, "Changes successfully applied.")
}

func handleGetWeatherAPI(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vars.APIConfig.Weather)
}

func handleSetKGAPI(w http.ResponseWriter, r *http.Request) {
	if err := json.NewDecoder(r.Body).Decode(&vars.APIConfig.Knowledge); err != nil {
		fmt.Println(err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Log để debug
	logger.Println(fmt.Sprintf("[KG API] Saved config - Provider: %s, Endpoint: %s, DeviceID: %s, ClientID: %s",
		vars.APIConfig.Knowledge.Provider,
		vars.APIConfig.Knowledge.Endpoint,
		vars.APIConfig.Knowledge.DeviceID,
		vars.APIConfig.Knowledge.ClientID))

	// Tự động sync STT provider với Knowledge Graph provider
	// Nếu Knowledge Graph = xiaozhi → STT tự động = xiaozhi cloud
	// Nếu Knowledge Graph khác → giữ nguyên STT provider hiện tại (hoặc set về vosk nếu chưa set)
	if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
		oldSTTService := vars.APIConfig.STT.Service
		vars.APIConfig.STT.Service = "xiaozhi"
		logger.Println(fmt.Sprintf("[KG API] Knowledge Graph = xiaozhi → Auto-set STT.Service = xiaozhi (was: %s)", oldSTTService))
	} else if vars.APIConfig.Knowledge.Provider != "" {
		// Nếu Knowledge Graph provider khác (openai, together, etc.) và STT đang là xiaozhi
		// → chuyển về vosk (vì xiaozhi STT chỉ dùng khi KG = xiaozhi)
		if vars.APIConfig.STT.Service == "xiaozhi" {
			vars.APIConfig.STT.Service = "vosk"
			logger.Println(fmt.Sprintf("[KG API] Knowledge Graph = %s (not xiaozhi) → Auto-set STT.Service = vosk", vars.APIConfig.Knowledge.Provider))
		}
	}

	vars.WriteConfigToDisk()
	processreqs.ReloadVosk()
	fmt.Fprint(w, "Changes successfully applied.")
}

func handleGetKGAPI(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// Log để debug
	logger.Println(fmt.Sprintf("[KG API] Loading config - Provider: %s, Endpoint: %s, DeviceID: %s, ClientID: %s",
		vars.APIConfig.Knowledge.Provider,
		vars.APIConfig.Knowledge.Endpoint,
		vars.APIConfig.Knowledge.DeviceID,
		vars.APIConfig.Knowledge.ClientID))
	json.NewEncoder(w).Encode(vars.APIConfig.Knowledge)
}

func handleSetSTTInfo(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Provider           string `json:"provider"`
		Language           string `json:"language"`
		IntentMatchingMode string `json:"intent_matching_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Set provider if provided
	if request.Provider != "" {
		vars.APIConfig.STT.Service = request.Provider
	}
	
	// Set intent matching mode if provided
	if request.IntentMatchingMode != "" {
		vars.APIConfig.STT.IntentMatchingMode = request.IntentMatchingMode
	} else {
		// Default to single if not provided
		vars.APIConfig.STT.IntentMatchingMode = "single"
	}

	if vars.APIConfig.STT.Service == "vosk" {
		if !isValidLanguage(request.Language, localization.ValidVoskModels) {
			http.Error(w, "language not valid", http.StatusBadRequest)
			return
		}
		if !isDownloadedLanguage(request.Language, vars.DownloadedVoskModels) {
			go localization.DownloadVoskModel(request.Language)
			fmt.Fprint(w, "downloading language model...")
			return
		}
	} else if vars.APIConfig.STT.Service == "whisper.cpp" {
		if !isValidLanguage(request.Language, localization.ValidVoskModels) {
			http.Error(w, "language not valid", http.StatusBadRequest)
			return
		}
	} else if vars.APIConfig.STT.Service == "xiaozhi" || vars.APIConfig.STT.Service == "houndify" {
		// Cloud-based STT providers - language is optional or auto-detected
		if request.Language != "" {
			vars.APIConfig.STT.Language = request.Language
		} else {
			vars.APIConfig.STT.Language = "en-US" // Default
		}
	} else {
		http.Error(w, "service must be vosk, whisper.cpp, xiaozhi, or houndify", http.StatusBadRequest)
		return
	}

	if request.Language != "" {
		vars.APIConfig.STT.Language = request.Language
	}
	vars.APIConfig.PastInitialSetup = true
	vars.WriteConfigToDisk()
	processreqs.ReloadVosk()
	logger.Println("Reloaded voice processor successfully")
	fmt.Fprint(w, "Language switched successfully.")
}

func handleGetDownloadStatus(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(localization.DownloadStatus))
	if localization.DownloadStatus == "success" || strings.Contains(localization.DownloadStatus, "error") {
		localization.DownloadStatus = "not downloading"
	}
}

func handleGetSTTInfo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vars.APIConfig.STT)
}

func handleGetConfig(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vars.APIConfig)
}

func handleGetLogs(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(logger.LogList))
}

func handleGetDebugLogs(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(logger.LogTrayList))
}

func handleIsRunning(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("true"))
}

func handleDeleteChats(w http.ResponseWriter) {
	vars.RememberedChats = []vars.RememberedChat{}
	fmt.Fprint(w, "done")
}

func handleGetOTA(w http.ResponseWriter, r *http.Request) {
	otaName := strings.Split(r.URL.Path, "/")[3]
	targetURL, err := url.Parse("https://archive.org/download/vector-pod-firmware/" + strings.TrimSpace(otaName))
	if err != nil {
		http.Error(w, "failed to parse URL", http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequest(r.Method, targetURL.String(), nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "failed to perform request", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		http.Error(w, "failed to copy response body", http.StatusInternalServerError)
	}
}

func handleGetVersionInfo(w http.ResponseWriter) {
	var installedVer string
	ver, err := os.ReadFile(vars.VersionFile)
	if err == nil {
		installedVer = strings.TrimSpace(string(ver))
	}
	currentVer, err := GetLatestReleaseTag("kercre123", "WirePod")
	if err != nil {
		http.Error(w, "error communicating with github (ver): "+err.Error(), http.StatusInternalServerError)
		return
	}
	currentCommit, err := GetLatestCommitSha()
	if err != nil {
		http.Error(w, "error communicating with github (commit): "+err.Error(), http.StatusInternalServerError)
		return
	}
	type VersionInfo struct {
		FromSource      bool   `json:"fromsource"`
		InstalledVer    string `json:"installedversion"`
		InstalledCommit string `json:"installedcommit"`
		CurrentVer      string `json:"currentversion"`
		CurrentCommit   string `json:"currentcommit"`
		UpdateAvailable bool   `json:"avail"`
	}
	var fromSource bool
	if installedVer == "" {
		fromSource = true
	}
	var uAvail bool
	if fromSource {
		uAvail = vars.CommitSHA != strings.TrimSpace(currentCommit)
	} else {
		uAvail = installedVer != strings.TrimSpace(currentVer)
	}
	verInfo := VersionInfo{
		FromSource:      fromSource,
		InstalledVer:    installedVer,
		InstalledCommit: vars.CommitSHA,
		CurrentVer:      strings.TrimSpace(currentVer),
		CurrentCommit:   strings.TrimSpace(currentCommit),
		UpdateAvailable: uAvail,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(verInfo)
}

func handleGenerateCerts(w http.ResponseWriter) {
	if err := botsetup.CreateCertCombo(); err != nil {
		http.Error(w, "error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, "done")
}

func saveCustomIntents() {
	customIntentJSONFile, _ := json.Marshal(vars.CustomIntents)
	os.WriteFile(vars.CustomIntentsPath, customIntentJSONFile, 0644)
}

func DisableCachingAndSniffing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
		w.Header().Set("pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func StartWebServer() {
	botsetup.RegisterSSHAPI()
	botsetup.RegisterBLEAPI()
	http.HandleFunc("/api/", apiHandler)
	http.HandleFunc("/session-certs/", certHandler)
	// Register xiaozhi WebSocket handler if enabled
	if xiaozhi.IsEnabled() {
		xiaozhiServer := xiaozhi.NewWebSocketServer()
		http.HandleFunc("/xiaozhi/v1/", xiaozhiServer.RealTime)
		logger.Println("Xiaozhi WebSocket handler registered at /xiaozhi/v1/")
	}
	var webRoot http.Handler
	if runtime.GOOS == "darwin" && vars.Packaged {
		appPath, _ := os.Executable()
		webRoot = http.FileServer(http.Dir(filepath.Dir(appPath) + "/../Frameworks/chipper/webroot"))
	} else if runtime.GOOS == "android" || runtime.GOOS == "ios" {
		webRoot = http.FileServer(http.Dir(vars.AndroidPath + "/static/webroot"))
	} else {
		webRoot = http.FileServer(http.Dir("./webroot"))
	}
	http.Handle("/", DisableCachingAndSniffing(webRoot))
	fmt.Printf("Starting webserver at port " + vars.WebPort + " (http://localhost:" + vars.WebPort + ")\n")
	if err := http.ListenAndServe(":"+vars.WebPort, nil); err != nil {
		logger.Println("Error binding to " + vars.WebPort + ": " + err.Error())
		if vars.Packaged {
			logger.ErrMsg("FATAL: Wire-pod was unable to bind to port " + vars.WebPort + ". Another process is likely using it. Exiting.")
		}
		os.Exit(1)
	}
}

func GetLatestCommitSha() (string, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/kercre123/wire-pod/commits", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get commits: %s", resp.Status)
	}
	type Commit struct {
		Sha string `json:"sha"`
	}
	var commits []Commit
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", fmt.Errorf("no commits found")
	}
	return commits[0].Sha[:7], nil
}

func GetLatestReleaseTag(owner, repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	type Release struct {
		TagName string `json:"tag_name"`
	}
	var release Release
	if err := json.Unmarshal(body, &release); err != nil {
		return "", err
	}

	return release.TagName, nil
}

func certHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/session-certs/"):
		split := strings.Split(r.URL.Path, "/")
		if len(split) < 3 {
			http.Error(w, "must request a cert by esn (ex. /session-certs/00e20145)", http.StatusBadRequest)
			return
		}
		esn := split[2]
		fileBytes, err := os.ReadFile(path.Join(vars.SessionCertPath, esn))
		if err != nil {
			http.Error(w, "cert does not exist", http.StatusNotFound)
			return
		}
		w.Write(fileBytes)
	}
}

func anyEmpty(values ...string) bool {
	for _, v := range values {
		if v == "" {
			return true
		}
	}
	return false
}

func isValidLanguage(language string, validLanguages []string) bool {
	for _, lang := range validLanguages {
		if lang == language {
			return true
		}
	}
	return false
}

func handleXiaozhiGeneratePairingCode(w http.ResponseWriter, r *http.Request) {
	// Log request info
	fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Method=%s, URL=%s, Headers=%v\n", r.Method, r.URL.String(), r.Header)

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		// Tự động lấy MAC address của máy đang chạy code
		deviceID = xiaozhi.GetPrimaryMACAddress()
		if deviceID == "" {
			// Nếu không lấy được MAC address, fallback về pending
			deviceID = fmt.Sprintf("pending_%d", time.Now().Unix())
		}
	}
	fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: deviceID=%s (from query or auto-detected)\n", deviceID)

	// Lấy Client-Id từ query parameter (nếu có), nếu không thì lấy từ config hoặc tự động generate
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		// Không có client_id trong query, lấy từ config hoặc generate mới
		clientID = xiaozhi.GetClientIDFromConfig()
		if clientID == "" {
			// Nếu Knowledge provider không phải xiaozhi, vẫn generate để hiển thị
			clientID = xiaozhi.GenerateClientID()
			fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Generated new Client-Id=%s (not from query)\n", clientID)
			// Lưu vào config nếu chưa có
			if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
				vars.APIConfig.Knowledge.ClientID = clientID
				vars.WriteConfigToDisk()
				fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Saved Client-Id to config\n")
			}
		} else {
			fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Using existing Client-Id=%s from config\n", clientID)
		}
	} else {
		fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Using Client-Id=%s from query parameter\n", clientID)
		// Nếu client_id được cung cấp từ query, lưu vào config nếu Knowledge provider là xiaozhi
		if vars.APIConfig.Knowledge.Provider == "xiaozhi" {
			vars.APIConfig.Knowledge.ClientID = clientID
			vars.WriteConfigToDisk()
			fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Saved Client-Id from query to config\n")
		}
	}

	// Mode selection:
	// - default: if Knowledge provider is xiaozhi and upstream looks like api.tenclass.net, fetch activation code from upstream OTA endpoint
	// - mode=local: force old behavior (generate locally)
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	forceLocal := mode == "local"

	// If user configured upstream Xiaozhi (default), we must return upstream-issued activation code;
	// otherwise the 6-digit code will be rejected by upstream pairing page.
	upstreamLooksLikeTenclass := strings.Contains(xiaozhi.GetBaseURL(), "api.tenclass.net")
	useUpstream := !forceLocal && vars.APIConfig.Knowledge.Provider == "xiaozhi" && upstreamLooksLikeTenclass

	if useUpstream {
		fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Fetching activation code from upstream OTA endpoint (Device-Id=%s, Client-Id=%s)\n", deviceID, clientID)
		// Activation-Version/Serial-Number:
		// - default to version 1 (no serial required)
		// - allow override via query params if user has burned serial and needs v2 flow
		activationVersion := strings.TrimSpace(r.URL.Query().Get("activation_version"))
		serialNumber := strings.TrimSpace(r.URL.Query().Get("serial_number"))
		act, err := xiaozhi.FetchUpstreamActivationFromOTACheck(deviceID, clientID, activationVersion, serialNumber)
		if err != nil {
			fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Upstream fetch failed: %v\n", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error":     "Không lấy được pairing/activation code từ upstream Xiaozhi (api.tenclass.net). Hãy kiểm tra mạng/DNS hoặc firewall của server.",
				"detail":    err.Error(),
				"device_id": deviceID,
				"client_id": clientID,
				"mode":      "upstream",
			})
			return
		}

		expiresIn := 600
		if act.TimeoutMS > 0 {
			expiresIn = act.TimeoutMS / 1000
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code":               act.Code,
			"challenge":          act.Challenge,
			"device_id":          deviceID,
			"client_id":          clientID,
			"expires_in":         expiresIn,
			"note":               "Mã này được lấy từ upstream Xiaozhi (endpoint /xiaozhi/ota/). Hãy nhập đúng mã này trên trang pairing/activation của upstream.",
			"message":            act.Message,
			"mode":               "upstream",
			"activation_version": activationVersion,
		})
		return
	}

	// Fallback/explicit local mode
	fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Generating pairing code locally (mode=%s)\n", mode)
	code, challenge, err := xiaozhi.GeneratePairingCode(deviceID, clientID)
	if err != nil {
		fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Failed to generate pairing code: %v\n", err)
		http.Error(w, fmt.Sprintf("Failed to generate pairing code: %v", err), http.StatusInternalServerError)
		return
	}
	fmt.Printf("[DEBUG] handleXiaozhiGeneratePairingCode: Successfully generated local code='%s', challenge='%s' (first 20 chars)\n", code, challenge[:20])

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code":           code,
		"challenge":      challenge,
		"device_id":      deviceID,
		"client_id":      clientID,
		"expires_in":     600,
		"note":           "Mã này được tạo LOCAL trên wirepodxiaozhi (mode=local). Nó sẽ KHÔNG hợp lệ nếu bạn nhập lên upstream api.tenclass.net.",
		"client_id_note": fmt.Sprintf("Client-Id (UUID): %s", clientID),
		"mode":           "local",
	})
}

func handleXiaozhiValidatePairingCode(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "code parameter is required", http.StatusBadRequest)
		return
	}

	// Lấy device_id từ query (nếu có) - có thể là MAC address từ device kết nối
	deviceIDFromQuery := r.URL.Query().Get("device_id")

	// Lấy MAC address của máy đang chạy code để validate
	localMAC := xiaozhi.GetPrimaryMACAddress()

	// Mặc định consume code (one-time use) để tránh tái sử dụng
	// Chỉ khi có check_only=true thì mới chỉ kiểm tra mà không consume
	checkOnly := r.URL.Query().Get("check_only") == "true"

	var valid bool
	var deviceID string
	var challenge string
	if checkOnly {
		// Chỉ validate, không xóa mã (dùng để kiểm tra trạng thái)
		valid, deviceID = xiaozhi.ValidatePairingCode(code)
		if valid {
			// Kiểm tra xem DeviceID trong pairing code có khớp với MAC address của máy không
			if localMAC != "" && deviceID != localMAC && !strings.HasPrefix(deviceID, "pending_") {
				response := map[string]interface{}{
					"valid":     false,
					"error":     fmt.Sprintf("Pairing code không khớp với máy này. Code dành cho MAC: %s, nhưng máy này có MAC: %s", deviceID, localMAC),
					"device_id": deviceID,
					"local_mac": localMAC,
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden) // 403 Forbidden
				json.NewEncoder(w).Encode(response)
				return
			}

			// Lấy challenge để device tính HMAC
			challenge, _ = xiaozhi.GetChallengeFromCode(code)

			// Kiểm tra xem device đã được activate chưa (nếu có device_id từ query)
			if deviceIDFromQuery != "" {
				if xiaozhi.IsDeviceActivated(deviceIDFromQuery) {
					device, _ := xiaozhi.GetActivatedDevice(deviceIDFromQuery)
					response := map[string]interface{}{
						"valid":             false,
						"device_id":         deviceID,
						"error":             fmt.Sprintf("Device %s has already been activated at %s", deviceIDFromQuery, device.ActivatedAt.Format(time.RFC3339)),
						"already_activated": true,
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict) // 409 Conflict
					json.NewEncoder(w).Encode(response)
					return
				}
			}
		}
	} else {
		// Mặc định: consume code sau khi validate (one-time use)
		valid, deviceID = xiaozhi.ConsumePairingCode(code)

		if valid {
			// Kiểm tra xem DeviceID trong pairing code có khớp với MAC address của máy không
			if localMAC != "" && deviceID != localMAC && !strings.HasPrefix(deviceID, "pending_") {
				response := map[string]interface{}{
					"valid":     false,
					"error":     fmt.Sprintf("Pairing code không khớp với máy này. Code dành cho MAC: %s, nhưng máy này có MAC: %s", deviceID, localMAC),
					"device_id": deviceID,
					"local_mac": localMAC,
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden) // 403 Forbidden
				json.NewEncoder(w).Encode(response)
				return
			}

			// Kiểm tra xem device đã được activate chưa (nếu có device_id từ query)
			if deviceIDFromQuery != "" {
				if xiaozhi.IsDeviceActivated(deviceIDFromQuery) {
					device, _ := xiaozhi.GetActivatedDevice(deviceIDFromQuery)
					response := map[string]interface{}{
						"valid":             false,
						"device_id":         deviceID,
						"error":             fmt.Sprintf("Device %s has already been activated at %s", deviceIDFromQuery, device.ActivatedAt.Format(time.RFC3339)),
						"already_activated": true,
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict) // 409 Conflict
					json.NewEncoder(w).Encode(response)
					return
				}
			}
		}
	}

	response := map[string]interface{}{
		"valid":     valid,
		"device_id": deviceID,
	}
	if challenge != "" {
		response["challenge"] = challenge
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiConsumePairingCode validate và xóa mã sau khi sử dụng (one-time use)
// Endpoint này đảm bảo mã chỉ được sử dụng một lần
func handleXiaozhiConsumePairingCode(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "code parameter is required", http.StatusBadRequest)
		return
	}

	valid, deviceID := xiaozhi.ConsumePairingCode(code)
	response := map[string]interface{}{
		"valid":     valid,
		"device_id": deviceID,
		"consumed":  valid, // Mã đã được sử dụng và xóa
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiActivate xử lý activation request từ device
// Device gửi HMAC của challenge để xác thực và nhận token
func handleXiaozhiActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req xiaozhi.ActivationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// Validate request
	if req.Algorithm != "hmac-sha256" {
		http.Error(w, "Unsupported algorithm", http.StatusBadRequest)
		return
	}

	// Lấy các headers từ request (giống ESP32)
	deviceIDFromHeader := r.Header.Get("Device-Id")
	clientIDFromHeader := r.Header.Get("Client-Id")
	activationVersion := r.Header.Get("Activation-Version")
	serialNumberFromHeader := r.Header.Get("Serial-Number")
	userAgent := r.Header.Get("User-Agent")
	acceptLanguage := r.Header.Get("Accept-Language")

	// Debug: log các headers nhận được
	fmt.Printf("[DEBUG] handleXiaozhiActivate: Device-Id=%s, Client-Id=%s, Activation-Version=%s, Serial-Number=%s, User-Agent=%s, Accept-Language=%s\n",
		deviceIDFromHeader, clientIDFromHeader, activationVersion, serialNumberFromHeader, userAgent, acceptLanguage)

	// Validate required fields (giống ESP32)
	if req.Challenge == "" || req.HMAC == "" {
		http.Error(w, "Missing required fields: challenge and hmac are required", http.StatusBadRequest)
		return
	}

	// Device-Id từ header là bắt buộc (giống ESP32)
	if deviceIDFromHeader == "" {
		http.Error(w, "Missing Device-Id header", http.StatusBadRequest)
		return
	}

	// Kiểm tra xem device đã được activate chưa (dựa trên Device-Id từ header)
	if xiaozhi.IsDeviceActivated(deviceIDFromHeader) {
		device, _ := xiaozhi.GetActivatedDevice(deviceIDFromHeader)
		response := xiaozhi.ActivationResponse{
			Success: false,
			Message: fmt.Sprintf("Device %s has already been activated at %s", deviceIDFromHeader, device.ActivatedAt.Format(time.RFC3339)),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict) // 409 Conflict
		json.NewEncoder(w).Encode(response)
		return
	}

	// TODO: Lấy secret key từ config hoặc device-specific key
	// Hiện tại dùng một shared secret (nên thay bằng device-specific key)
	// ESP32 dùng HMAC_KEY0 từ efuse, nhưng trong wirepodxiaozhi ta dùng shared secret
	secretKey := []byte("xiaozhi-pairing-secret-key") // TODO: Thay bằng key từ config

	// Validate activation request - tìm pairing code từ challenge
	valid, validatedDeviceID := xiaozhi.ValidateActivationRequest(&req, secretKey)
	if !valid {
		fmt.Printf("[DEBUG] handleXiaozhiActivate: HMAC validation failed: %s\n", validatedDeviceID)
		response := xiaozhi.ActivationResponse{
			Success: false,
			Message: validatedDeviceID, // validatedDeviceID chứa error message trong trường hợp này
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Kiểm tra Device-Id từ header có khớp với Device-Id trong pairing code không
	if validatedDeviceID != deviceIDFromHeader {
		fmt.Printf("[DEBUG] handleXiaozhiActivate: Device-Id mismatch: header=%s, pairing_code=%s\n", deviceIDFromHeader, validatedDeviceID)
		response := xiaozhi.ActivationResponse{
			Success: false,
			Message: fmt.Sprintf("Device-Id mismatch: header (%s) does not match pairing code device (%s)", deviceIDFromHeader, validatedDeviceID),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Generate token cho device (có thể là JWT hoặc random token)
	// TODO: Implement proper token generation
	token := fmt.Sprintf("token_%s_%d", deviceIDFromHeader, time.Now().Unix())

	// Lấy Client-Id từ request header hoặc config (giống ESP32)
	// Ưu tiên từ header, sau đó từ config
	clientID := clientIDFromHeader
	if clientID == "" {
		clientID = xiaozhi.GetClientIDFromConfig()
	}

	// Đăng ký device đã được activate
	// Sử dụng Device-Id từ header (MAC address) - giống ESP32
	// Serial-Number chỉ là optional field, Device-Id là bắt buộc
	xiaozhi.RegisterActivatedDevice(deviceIDFromHeader, clientID, token)
	fmt.Printf("[DEBUG] handleXiaozhiActivate: Device activated successfully - Device-Id=%s, Client-Id=%s, Serial-Number=%s\n", deviceIDFromHeader, clientID, serialNumberFromHeader)

	response := xiaozhi.ActivationResponse{
		Success: true,
		Token:   token,
		Message: "Activation successful",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiRemoveDevice xóa device khỏi danh sách activated (để có thể pair lại)
func handleXiaozhiRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "DELETE" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		// Nếu không có trong query, thử lấy từ body
		var req struct {
			DeviceID string `json:"device_id"`
		}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&req)
			deviceID = req.DeviceID
		}
	}

	if deviceID == "" {
		http.Error(w, "device_id parameter is required", http.StatusBadRequest)
		return
	}

	removed := xiaozhi.RemoveActivatedDevice(deviceID)
	response := map[string]interface{}{
		"success": removed,
		"message": "",
	}

	if removed {
		response["message"] = fmt.Sprintf("Device %s has been removed and can be paired again", deviceID)
	} else {
		response["message"] = fmt.Sprintf("Device %s not found in activated devices", deviceID)
		w.WriteHeader(http.StatusNotFound)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiListDevices liệt kê tất cả devices đã activate
func handleXiaozhiListDevices(w http.ResponseWriter, r *http.Request) {
	devices := xiaozhi.GetAllActivatedDevices()

	response := map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiGetConnectedDevices lấy danh sách devices đã kết nối WebSocket (MAC addresses)
func handleXiaozhiGetConnectedDevices(w http.ResponseWriter, r *http.Request) {
	devices := xiaozhi.GetAllConnectedDevices()

	// Format response để dễ đọc
	deviceList := make([]map[string]interface{}, 0, len(devices))
	for _, device := range devices {
		deviceList = append(deviceList, map[string]interface{}{
			"device_id":     device.DeviceID,
			"client_id":     device.ClientID,
			"connected_at":  device.ConnectedAt.Format(time.RFC3339),
			"last_seen":     device.LastSeen.Format(time.RFC3339),
			"connected_for": int(time.Since(device.ConnectedAt).Seconds()),
		})
	}

	response := map[string]interface{}{
		"count":   len(devices),
		"devices": deviceList,
		"note":    "These are MAC addresses of devices that have connected via WebSocket",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiCheckDeviceStatus kiểm tra xem device đã được activate/pair hay chưa
func handleXiaozhiCheckDeviceStatus(w http.ResponseWriter, r *http.Request) {
	// Disabled: avoid unnecessary upstream activation checks and log spam
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"disabled": true,
		"message":  "xiaozhi_check_device_status is disabled",
	})
	return

	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		// Nếu không có trong query, thử lấy từ body
		var req struct {
			DeviceID string `json:"device_id"`
		}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&req)
			deviceID = req.DeviceID
		}
	}

	if deviceID == "" {
		fmt.Printf("[DEBUG] handleXiaozhiCheckDeviceStatus: Missing device_id parameter\n")
		http.Error(w, "device_id parameter is required", http.StatusBadRequest)
		return
	}

	// Lấy Client-Id từ query parameter (ưu tiên), sau đó từ request header, cuối cùng từ config
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		clientID = r.Header.Get("Client-Id")
	}
	if clientID == "" {
		clientID = xiaozhi.GetClientIDFromConfig()
	}

	// Log request parameters
	fmt.Printf("[DEBUG] handleXiaozhiCheckDeviceStatus: deviceID=%s, clientID=%s\n", deviceID, clientID)

	// Kiểm tra trạng thái activation từ upstream server (CHỈ từ server, KHÔNG dùng local storage)
	isActivated, serverMessage, err := xiaozhi.CheckDeviceActivationFromServer(deviceID, clientID)

	// Log result
	fmt.Printf("[DEBUG] handleXiaozhiCheckDeviceStatus: Result - isActivated=%v, serverMessage=%s, error=%v\n", isActivated, serverMessage, err)

	// Đảm bảo có Client-Id để hiển thị trong response
	if clientID == "" {
		clientID = xiaozhi.GetClientIDFromConfig()
	}

	response := map[string]interface{}{
		"device_id":    deviceID,
		"client_id":    clientID, // Thêm Client-Id vào response
		"is_activated": false,    // Mặc định false, chỉ set true khi server confirm rõ ràng
		"status":       "unknown",
		"source":       "upstream_server_only",
		"note":         "Trạng thái được kiểm tra TRỰC TIẾP từ upstream server (api.tenclass.net), KHÔNG dùng local storage/cache",
	}

	if err != nil {
		// Nếu không thể kết nối đến server, return error - KHÔNG fallback về local storage
		response["error"] = err.Error()
		response["source"] = "server_error"
		response["is_activated"] = false
		response["status"] = "error"
		response["message"] = fmt.Sprintf("Không thể kết nối đến upstream server: %v", err)
		response["note"] = "Không thể kết nối đến upstream server. Vui lòng thử lại sau."
	} else {
		// Kiểm tra thành công từ upstream server
		if isActivated {
			// Chỉ set true khi server confirm rõ ràng
			response["is_activated"] = true
			response["status"] = "activated"
			response["message"] = serverMessage
			// KHÔNG lưu vào local storage - chỉ trả về kết quả từ server
		} else {
			// Server nói chưa activate
			response["is_activated"] = false
			response["status"] = "not_activated"
			response["message"] = serverMessage
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiGetLocalMAC lấy MAC address của máy đang chạy code
func handleXiaozhiGetLocalMAC(w http.ResponseWriter, r *http.Request) {
	primaryMAC := xiaozhi.GetPrimaryMACAddress()
	allMACs := xiaozhi.GetLocalMACAddresses()

	response := map[string]interface{}{
		"primary_mac": primaryMAC,
		"all_macs":    allMACs,
		"count":       len(allMACs),
		"note":        "MAC addresses của máy đang chạy wirepodxiaozhi server",
	}

	if primaryMAC == "" {
		response["message"] = "Không tìm thấy MAC address. Có thể máy không có network interface hoặc đang chạy trong môi trường không hỗ trợ."
	} else {
		response["message"] = fmt.Sprintf("MAC address chính: %s", primaryMAC)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleXiaozhiGetPairingStatus(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "code parameter is required", http.StatusBadRequest)
		return
	}

	pairingCode, exists := xiaozhi.GetPairingCode(code)
	if !exists {
		response := map[string]interface{}{
			"exists": false,
			"valid":  false,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	response := map[string]interface{}{
		"exists":     true,
		"valid":      true,
		"code":       pairingCode.Code,
		"device_id":  pairingCode.DeviceID,
		"challenge":  pairingCode.Challenge,
		"expires_at": pairingCode.ExpiresAt.Format(time.RFC3339),
		"expires_in": int(time.Until(pairingCode.ExpiresAt).Seconds()),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiCheckVersion xử lý request check version từ ESP32
// ESP32 gọi endpoint này để check version và nhận activation code
// Response format giống như ESP32 mong đợi: { "activation": { "code": "...", "challenge": "...", "message": "...", "timeout_ms": 600000 } }
func handleXiaozhiCheckVersion(w http.ResponseWriter, r *http.Request) {
	// Lấy Device-Id từ header (MAC address từ ESP32) - giống ESP32 SetupHttp()
	deviceID := r.Header.Get("Device-Id")
	if deviceID == "" {
		// Nếu không có Device-Id, thử lấy từ query
		deviceID = r.URL.Query().Get("device_id")
		if deviceID == "" {
			// Fallback: dùng MAC address của máy đang chạy code
			deviceID = xiaozhi.GetPrimaryMACAddress()
			if deviceID == "" {
				deviceID = fmt.Sprintf("pending_%d", time.Now().Unix())
			}
		}
	}

	// Lấy Client-Id từ header (giống ESP32) - ưu tiên từ header, sau đó từ config
	clientID := r.Header.Get("Client-Id")
	if clientID == "" {
		clientID = xiaozhi.GetClientIDFromConfig()
		if clientID == "" {
			clientID = xiaozhi.GenerateClientID()
		}
	}

	// Lấy các headers khác (giống ESP32)
	activationVersion := r.Header.Get("Activation-Version")
	serialNumber := r.Header.Get("Serial-Number")
	userAgent := r.Header.Get("User-Agent")
	acceptLanguage := r.Header.Get("Accept-Language")

	// Debug: log các headers nhận được
	fmt.Printf("[DEBUG] handleXiaozhiCheckVersion: Device-Id=%s, Client-Id=%s, Activation-Version=%s, Serial-Number=%s, User-Agent=%s, Accept-Language=%s\n",
		deviceID, clientID, activationVersion, serialNumber, userAgent, acceptLanguage)

	// Kiểm tra xem device đã được activate chưa
	isActivated := xiaozhi.IsDeviceActivated(deviceID)

	// Tìm pairing code hiện tại cho device này (nếu có)
	var activationCode string
	var activationChallenge string
	var activationMessage string

	// Tìm pairing code đang active cho device này
	allCodes := xiaozhi.GetAllActiveCodes()
	for code, pairingCode := range allCodes {
		if pairingCode.DeviceID == deviceID {
			activationCode = code
			activationChallenge = pairingCode.Challenge
			activationMessage = fmt.Sprintf("Device %s needs pairing. Please enter code: %s", deviceID, code)
			break
		}
	}

	// Nếu chưa có pairing code và device chưa được activate, tạo mới
	if activationCode == "" && !isActivated {
		// Generate pairing code với Client-Id từ header/config
		code, challenge, err := xiaozhi.GeneratePairingCode(deviceID, clientID)
		if err == nil {
			activationCode = code
			activationChallenge = challenge
			activationMessage = fmt.Sprintf("Device %s needs pairing. Please enter code: %s", deviceID, code)
			fmt.Printf("[DEBUG] handleXiaozhiCheckVersion: Generated new pairing code='%s' for deviceID='%s', clientID='%s'\n", code, deviceID, clientID)
		} else {
			fmt.Printf("[DEBUG] handleXiaozhiCheckVersion: Failed to generate pairing code: %v\n", err)
		}
	}

	// Tạo response giống như ESP32 mong đợi
	response := map[string]interface{}{
		"firmware": map[string]interface{}{
			"version": "1.0.0", // Có thể lấy từ config
			"url":     "",      // OTA URL nếu có
		},
	}

	// Thêm activation object nếu có code hoặc device chưa được activate
	if activationCode != "" || !isActivated {
		activationObj := map[string]interface{}{}

		if isActivated {
			device, _ := xiaozhi.GetActivatedDevice(deviceID)
			activationObj["message"] = fmt.Sprintf("Device %s is already activated at %s", deviceID, device.ActivatedAt.Format(time.RFC3339))
		} else if activationCode != "" {
			activationObj["code"] = activationCode
			activationObj["challenge"] = activationChallenge
			activationObj["message"] = activationMessage
			activationObj["timeout_ms"] = 600000 // 10 minutes
		} else {
			activationObj["message"] = fmt.Sprintf("Device %s needs pairing. Please generate pairing code from web UI.", deviceID)
		}

		response["activation"] = activationObj
	}

	// Thêm các headers giống ESP32 response
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Device-Id", deviceID) // Echo back Device-Id

	// Lấy Client-Id từ header request hoặc config
	clientIDFromRequest := r.Header.Get("Client-Id")
	if clientIDFromRequest != "" {
		w.Header().Set("Client-Id", clientIDFromRequest)
	} else {
		clientID := xiaozhi.GetClientIDFromConfig()
		if clientID != "" {
			w.Header().Set("Client-Id", clientID)
		}
	}

	// Thêm Activation-Version header (giống ESP32) - echo back từ request
	if activationVersion != "" {
		w.Header().Set("Activation-Version", activationVersion)
	} else {
		w.Header().Set("Activation-Version", "1") // Default version 1
	}

	json.NewEncoder(w).Encode(response)
}

// handleXiaozhiGetClientIP lấy IP address của client để tạo PC IP-based MAC address
func handleXiaozhiGetClientIP(w http.ResponseWriter, r *http.Request) {
	// Lấy IP từ RemoteAddr (format: "IP:port")
	ip := r.RemoteAddr
	// Xử lý format "IP:port" - chỉ lấy phần IP
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}

	// Nếu có X-Forwarded-For header (khi đi qua proxy), lấy IP đầu tiên
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			ip = strings.TrimSpace(ips[0])
		}
	}

	// Tạo PC IP-based device ID
	pcDeviceID := fmt.Sprintf("pc_%s", ip)

	response := map[string]interface{}{
		"ip":        ip,
		"device_id": pcDeviceID,
		"type":      "pc_ip_based",
		"note":      "PC IP-based device ID. Sử dụng để tạo pairing code cho PC kết nối như ESP32.",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func isDownloadedLanguage(language string, downloadedLanguages []string) bool {
	for _, lang := range downloadedLanguages {
		if lang == language {
			return true
		}
	}
	return false
}
