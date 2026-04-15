const intentsJson = JSON.parse(
  '["intent_greeting_hello", "intent_names_ask", "intent_imperative_eyecolor", "intent_character_age", "intent_explore_start", "intent_system_charger", "intent_system_sleep", "intent_greeting_goodmorning", "intent_greeting_goodnight", "intent_greeting_goodbye", "intent_seasonal_happynewyear", "intent_seasonal_happyholidays", "intent_amazon_signin", "intent_imperative_forward", "intent_imperative_turnaround", "intent_imperative_turnleft", "intent_imperative_turnright", "intent_play_rollcube", "intent_play_popawheelie", "intent_play_fistbump", "intent_play_blackjack", "intent_imperative_affirmative", "intent_imperative_negative", "intent_photo_take_extend", "intent_imperative_praise", "intent_imperative_abuse", "intent_weather_extend", "intent_imperative_apologize", "intent_imperative_backup", "intent_imperative_volumedown", "intent_imperative_volumeup", "intent_imperative_lookatme", "intent_imperative_volumelevel_extend", "intent_imperative_shutup", "intent_names_username_extend", "intent_imperative_come", "intent_imperative_love", "intent_knowledge_promptquestion", "intent_clock_checktimer", "intent_global_stop_extend", "intent_clock_settimer_extend", "intent_clock_time", "intent_imperative_quiet", "intent_imperative_dance", "intent_play_pickupcube", "intent_imperative_fetchcube", "intent_imperative_findcube", "intent_play_anytrick", "intent_message_recordmessage_extend", "intent_message_playmessage_extend", "intent_blackjack_hit", "intent_blackjack_stand", "intent_play_keepaway"]'
);

var GetLog = false;

const getE = (element) => document.getElementById(element);

function updateIntentSelection(element) {
  fetch("/api/get_custom_intents_json")
    .then((response) => response.json())
    .then((listResponse) => {
      const container = getE(element);
      container.innerHTML = "";
      if (listResponse && listResponse.length > 0) {
        const select = document.createElement("select");
        select.name = `${element}intents`;
        select.id = `${element}intents`;
        listResponse.forEach((intent) => {
          if (!intent.issystem) {
            const option = document.createElement("option");
            option.value = intent.name;
            option.text = intent.name;
            select.appendChild(option);
          }
        });
        const label = document.createElement("label");
        label.innerHTML = "Choose the intent: ";
        label.htmlFor = `${element}intents`;
        container.appendChild(label).appendChild(select);

        select.addEventListener("change", hideEditIntents);
      } else {
        const error = document.createElement("p");
        error.innerHTML = "No intents found, you must add one first";
        container.appendChild(error);
      }
    }).catch(() => {
      // Do nothing
    });
}

function checkInited() {
  fetch("/api/is_api_v3").then((response) => {
    if (!response.ok) {
      alert(
        "This webroot does not match with the wire-pod binary. Some functionality will be broken. There was either an error during the last update, or you did not precisely follow the update guide. https://github.com/kercre123/wire-pod/wiki/Things-to-Know#updating-wire-pod"
      );
    }
  });

}

function createIntentSelect(element) {
  const select = document.createElement("select");
  select.name = `${element}intents`;
  select.id = `${element}intents`;
  intentsJson.forEach((intent) => {
    const option = document.createElement("option");
    option.value = intent;
    option.text = intent;
    select.appendChild(option);
  });
  const label = document.createElement("label");
  label.innerHTML = "Intent to send to robot after script executed:";
  label.htmlFor = `${element}intents`;
  getE(element).innerHTML = "";
  getE(element).appendChild(label).appendChild(select);
}

function editFormCreate() {
  const intentNumber = getE("editSelectintents").selectedIndex;

  fetch("/api/get_custom_intents_json")
    .then((response) => response.json())
    .then((intents) => {
      const intent = intents[intentNumber];
      if (intent) {
        const form = document.createElement("form");
        form.id = "editIntentForm";
        form.name = "editIntentForm";
        form.innerHTML = `
          <label for="name">Name:<br><input type="text" id="name" value="${intent.name}"></label><br>
          <label for="description">Description:<br><input type="text" id="description" value="${intent.description}"></label><br>
          <label for="utterances">Utterances:<br><input type="text" id="utterances" value="${intent.utterances.join(",")}"></label><br>
          <label for="intent">Intent:<br><select id="intent">${intentsJson
            .map(
              (name) =>
                `<option value="${name}" ${name === intent.intent ? "selected" : ""
                }>${name}</option>`
            )
            .join("")}</select></label><br>
          <label for="paramname">Param Name:<br><input type="text" id="paramname" value="${intent.params.paramname}"></label><br>
          <label for="paramvalue">Param Value:<br><input type="text" id="paramvalue" value="${intent.params.paramvalue}"></label><br>
          <label for="exec">Exec:<br><input type="text" id="exec" value="${intent.exec}"></label><br>
          <label for="execargs">Exec Args:<br><input type="text" id="execargs" value="${intent.execargs.join(",")}"></label><br>
          <label for="luascript">Lua code to run:</label><br><textarea id="luascript">${intent.luascript}</textarea>
          <button onclick="editIntent(${intentNumber})">Submit</button>
        `;
        //form.querySelector("#submit").onclick = () => editIntent(intentNumber);
        getE("editIntentForm").innerHTML = "";
        getE("editIntentForm").appendChild(form);
        showEditIntents();
      } else {
        displayError("editIntentForm", "No intents found, you must add one first");
      }
    }).catch((error) => {
      console.error(error);
      displayError("editIntentForm", "Error fetching intents");
    })
}

function editIntent(intentNumber) {
  const data = {
    number: intentNumber + 1,
    name: getE("name").value,
    description: getE("description").value,
    utterances: getE("utterances").value.split(","),
    intent: getE("intent").value,
    params: {
      paramname: getE("paramname").value,
      paramvalue: getE("paramvalue").value,
    },
    exec: getE("exec").value,
    execargs: getE("execargs").value.split(","),
    luascript: getE("luascript").value,
  };

  fetch("/api/edit_custom_intent", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(data),
  })
    .then((response) => response.text())
    .then((response) => {
      displayMessage("editIntentStatus", response);
      alert(response)
      updateIntentSelection("editSelect");
      updateIntentSelection("deleteSelect");
    });
}

function deleteSelectedIntent() {
  const intentNumber = getE("editSelectintents").selectedIndex + 1;

  fetch("/api/remove_custom_intent", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ number: intentNumber }),
  })
    .then((response) => response.text())
    .then((response) => {
      hideEditIntents();
      alert(response)
      updateIntentSelection("editSelect");
      updateIntentSelection("deleteSelect");
    });
}

function sendIntentAdd() {
  const form = getE("intentAddForm");
  const data = {
    name: form.elements["nameAdd"].value,
    description: form.elements["descriptionAdd"].value,
    utterances: form.elements["utterancesAdd"].value.split(","),
    intent: form.elements["intentAddSelectintents"].value,
    params: {
      paramname: form.elements["paramnameAdd"].value,
      paramvalue: form.elements["paramvalueAdd"].value,
    },
    exec: form.elements["execAdd"].value,
    execargs: form.elements["execAddArgs"].value.split(","),
    luascript: form.elements["luaAdd"].value,
  };
  if (!data.name || !data.description || !data.utterances) {
    displayMessage("addIntentStatus", "A required input is missing. You need a name, description, and utterances.");
    alert("A required input is missing. You need a name, description, and utterances.")
    return
  }

  displayMessage("addIntentStatus", "Adding...");

  fetch("/api/add_custom_intent", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(data),
  })
    .then((response) => response.text())
    .then((response) => {
      displayMessage("addIntentStatus", response);
      alert(response)
      updateIntentSelection("editSelect");
      updateIntentSelection("deleteSelect");
    });
}

function checkWeather() {
  const p = getE("weatherProvider").value;
  getE("apiKeySpan").style.display = p && p !== "none" ? "block" : "none";
}

function sendWeatherAPIKey() {
  const data = {
    provider: getE("weatherProvider").value,
    key: getE("apiKey").value,
  };

  displayMessage("addWeatherProviderAPIStatus", "Saving...");

  fetch("/api/set_weather_api", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(data),
  })
    .then((response) => response.text())
    .then((response) => {
      displayMessage("addWeatherProviderAPIStatus", response);
    });
}

const DEFAULT_WEATHER_PROVIDER = "openweathermap.org";
const DEFAULT_WEATHER_API_KEY = "403a79621d25f8fdee7c468bbd16b820";

function updateWeatherAPI() {
  fetch("/api/get_weather_api")
    .then((response) => response.json())
    .then((data) => {
      let provider = data.provider || "";
      const key = data.key || "";
      if (provider === "none") {
        getE("weatherProvider").value = "none";
        getE("apiKey").value = "";
        checkWeather();
        return;
      }
      if (provider === "" && key === "") {
        provider = DEFAULT_WEATHER_PROVIDER;
      }
      getE("weatherProvider").value = provider || DEFAULT_WEATHER_PROVIDER;
      getE("apiKey").value =
        key || (getE("weatherProvider").value === DEFAULT_WEATHER_PROVIDER ? DEFAULT_WEATHER_API_KEY : "");
      checkWeather();
    });
}

// Load local MAC address to input field
function loadLocalMACToInput() {
  const input = document.getElementById("deviceIdInput");
  const statusDiv = document.getElementById("xiaozhiPairingStatus");
  
  if (!input) {
    console.error("[Pairing] deviceIdInput not found");
    return;
  }
  
  if (statusDiv) {
    statusDiv.innerHTML = "Đang lấy MAC address...";
    statusDiv.style.color = "#666";
    statusDiv.style.display = "block";
  }
  
  fetch("/api/xiaozhi_get_local_mac")
    .then(response => response.json())
    .then(data => {
      const macAddress = data.primary_mac || "";
      if (macAddress) {
        input.value = macAddress;
        if (statusDiv) {
          statusDiv.innerHTML = `✅ Đã tự động điền MAC address: <code style="background: #e8f5e9; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${macAddress}</code>`;
          statusDiv.style.color = "#0f9d58";
        }
        console.log("[Pairing] MAC address loaded:", macAddress);
      } else {
        if (statusDiv) {
          statusDiv.innerHTML = "⚠️ Không tìm thấy MAC address. Vui lòng nhập thủ công.";
          statusDiv.style.color = "#ff9800";
        }
        console.warn("[Pairing] Không tìm thấy MAC address");
      }
    })
    .catch(error => {
      console.error("[Pairing] Lỗi khi lấy MAC address:", error);
      if (statusDiv) {
        statusDiv.innerHTML = `❌ Lỗi khi lấy MAC address: ${error.message}. Vui lòng nhập thủ công.`;
        statusDiv.style.color = "#db4437";
      }
    });
}

// Generate UUID and fill to input field
function generateUUIDToInput() {
  const input = document.getElementById("clientIdInput");
  const statusDiv = document.getElementById("xiaozhiPairingStatus");
  
  if (!input) {
    console.error("[Pairing] clientIdInput not found");
    return;
  }
  
  // Generate UUID v4
  const uuid = 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function(c) {
    const r = Math.random() * 16 | 0;
    const v = c === 'x' ? r : (r & 0x3 | 0x8);
    return v.toString(16);
  });
  
  input.value = uuid;
  
  if (statusDiv) {
    statusDiv.innerHTML = `✅ Đã tự động tạo UUID: <code style="background: #f3e5f5; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${uuid}</code>`;
    statusDiv.style.color = "#9c27b0";
    statusDiv.style.display = "block";
  }
  
  console.log("[Pairing] UUID generated:", uuid);
}

// Generate pairing code from input fields
function generatePairingCodeFromInput() {
  const deviceIdInput = document.getElementById("deviceIdInput");
  const clientIdInput = document.getElementById("clientIdInput");
  const statusDiv = document.getElementById("xiaozhiPairingStatus");
  const generateBtn = document.getElementById("generatePairingCodeBtn");
  
  if (!deviceIdInput) {
    console.error("[Pairing] deviceIdInput not found");
    return;
  }
  
  const deviceId = deviceIdInput.value.trim();
  const clientId = clientIdInput ? clientIdInput.value.trim() : "";
  
  if (!deviceId) {
    if (statusDiv) {
      statusDiv.innerHTML = "❌ Vui lòng nhập Device-Id hoặc MAC Address, hoặc click 'Lấy MAC tự động' để tự động điền.";
      statusDiv.style.color = "#db4437";
      statusDiv.style.display = "block";
    }
    return;
  }
  
  // Disable button while generating
  if (generateBtn) {
    generateBtn.disabled = true;
    generateBtn.style.backgroundColor = "#9e9e9e";
    generateBtn.style.cursor = "not-allowed";
    generateBtn.textContent = "⏳ Đang tạo mã...";
  }
  
  if (statusDiv) {
    let statusText = `Đang tạo pairing code cho: <code style="background: #f5f5f5; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${deviceId}</code>`;
    if (clientId) {
      statusText += ` với Client-Id: <code style="background: #f3e5f5; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${clientId}</code>`;
    } else {
      statusText += ` (Client-Id sẽ được tự động tạo)`;
    }
    statusText += `...`;
    statusDiv.innerHTML = statusText;
    statusDiv.style.color = "#666";
    statusDiv.style.display = "block";
  }
  
  // Generate pairing code with both deviceId and clientId
  generatePairingCodeWithMAC(deviceId, clientId);
  
  // Re-enable button after a delay (will be re-enabled when generation completes)
  setTimeout(() => {
    if (generateBtn) {
      generateBtn.disabled = false;
      generateBtn.style.backgroundColor = "#0f9d58";
      generateBtn.style.cursor = "pointer";
      generateBtn.textContent = "🔑 Tạo mã Pairing";
    }
  }, 2000);
}

// Loaded from /api/get_kg_api so Apply does not overwrite openai_voice when the KG UI has no voice control.
let kgStoredOpenaiVoice = "fable";

function checkKG() {
  getE("xiaozhiInput").style.display = "block";
  getE("intentGraphInput").style.display = "block";
  getE("saveChatInput").style.display = "block";
  getE("xiaozhiDisableIntentInput").style.display = "block";
  getE("llmCommandInput").style.display = "block";
}

function sendKGAPIKey() {
  const data = {
    enable: true,
    provider: "xiaozhi",
    key: "",
    model: "",
    id: "",
    intentgraph: false,
    robotName: "",
    openai_prompt: "",
    openai_voice: kgStoredOpenaiVoice,
    openai_voice_with_english: false,
    save_chat: false,
    commands_enable: false,
    endpoint: "",
    xiaozhi_tts_volume: "normal",
  };
  data.endpoint = getE("xiaozhiBaseURL").value;
  const deviceIDInput = getE("xiaozhiDeviceID");
  if (deviceIDInput) {
    data.device_id = deviceIDInput.value.trim();
    console.log("[KG API] Saving device_id:", data.device_id);
  } else {
    console.warn("[KG API] xiaozhiDeviceID input not found when saving");
    data.device_id = "";
  }
  const clientIDInput = getE("xiaozhiClientID");
  if (clientIDInput) {
    data.client_id = clientIDInput.value.trim();
    console.log("[KG API] Saving client_id:", data.client_id);
  } else {
    console.warn("[KG API] xiaozhiClientID input not found when saving");
    data.client_id = "";
  }
  const volEl = getE("xiaozhiTTSVolume");
  data.xiaozhi_tts_volume = volEl ? volEl.value : "normal";
  data.intentgraph = getE("intentyes").checked
  data.save_chat = getE("saveChatYes").checked
  data.xiaozhi_disable_intent = getE("xiaozhiDisableIntent").checked
  data.commands_enable = getE("commandYes").checked

  fetch("/api/set_kg_api", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(data),
  })
    .then((response) => response.text())
    .then((response) => {
      displayMessage("addKGProviderAPIStatus", response);
      alert(response);
    });
}

function deleteSavedChats() {
  if (confirm("Are you sure? This will delete all saved chats.")) {
    fetch("/api/delete_chats")
      .then((response) => response.text())
      .then(() => {
        alert("Successfully deleted all saved chats.");
      });
  }
}

function updateKGAPI() {
  fetch("/api/get_kg_api")
    .then((response) => response.json())
    .then((data) => {
      getE("kgProvider").value = "xiaozhi";
      kgStoredOpenaiVoice = data.openai_voice || "fable";
      getE("xiaozhiBaseURL").value = data.endpoint || "";
      const deviceIDInput = getE("xiaozhiDeviceID");
      if (deviceIDInput) {
        deviceIDInput.value = data.device_id || "";
        console.log("[KG API] Loaded device_id:", data.device_id);
      } else {
        console.warn("[KG API] xiaozhiDeviceID input not found");
      }
      const clientIDInput = getE("xiaozhiClientID");
      if (clientIDInput) {
        clientIDInput.value = data.client_id || "";
        console.log("[KG API] Loaded client_id:", data.client_id);
      } else {
        console.warn("[KG API] xiaozhiClientID input not found");
      }
      const volEl = getE("xiaozhiTTSVolume");
      if (volEl) {
        volEl.value = data.xiaozhi_tts_volume || "normal";
      }
      getE("commandYes").checked = data.commands_enable
      getE("intentyes").checked = data.intentgraph
      getE("saveChatYes").checked = data.save_chat
      getE("xiaozhiDisableIntent").checked = data.xiaozhi_disable_intent || false
      checkKG();
    });
}

function checkSTTProvider() {
  const provider = getE("sttProvider").value;
  const desc = getE("sttProviderDesc");
  const languageLabel = getE("languageSelectionDiv").querySelector("label");
  
  if (provider === "xiaozhi") {
    desc.innerHTML = "Xiaozhi cloud STT provider. Language is automatically detected from audio input. Language selection below is optional.";
    getE("languageSelectionDiv").style.display = "block";
    if (languageLabel) {
      languageLabel.innerHTML = "Language (optional - auto-detected):";
    }
  } else if (provider === "houndify") {
    desc.innerHTML = "Houndify cloud STT provider. Language selection may be limited or automatic.";
    getE("languageSelectionDiv").style.display = "block";
    if (languageLabel) {
      languageLabel.innerHTML = "Language:";
    }
  } else if (provider === "vosk" || provider === "whisper.cpp") {
    desc.innerHTML = "Local STT provider. Select language to download model if needed.";
    getE("languageSelectionDiv").style.display = "block";
    if (languageLabel) {
      languageLabel.innerHTML = "Language:";
    }
  }
}

function setSTTLanguage() {
  const provider = getE("sttProvider").value;
  const language = getE("languageSelection").value;
  const intentMatchingMode = getE("intentMatchingMode").value;
  const data = { 
    provider: provider,
    language: language,
    intent_matching_mode: intentMatchingMode
  };

  displayMessage("languageStatus", "Setting...");

  fetch("/api/set_stt_info", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(data),
  })
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("downloading")) {
        displayMessage("languageStatus", "Downloading model...");
        updateSTTLanguageDownload();
      } else {
        displayMessage("languageStatus", response);
        getE("languageSelectionDiv").style.display = response.includes("success") ? "block" : "none";
      }
    });
}

function updateSTTLanguageDownload() {

  const interval = setInterval(() => {
    fetch("/api/get_download_status")
      .then((response) => response.text())
      .then((response) => {
        displayMessage("languageStatus", response.includes("not downloading") ? "Initiating download..." : response)
        if (response.includes("success") || response.includes("error")) {
          displayMessage("languageStatus", response);
          getE("languageSelectionDiv").style.display = "block";
          clearInterval(interval);
        }
      });
  }, 500);
}

function sendRestart() {
  fetch("/api/reset")
    .then((response) => response.text())
    .then((response) => {
      displayMessage("restartStatus", response);
    });
}

function hideEditIntents() {
  getE("editIntentForm").style.display = "none";
  getE("editIntentStatus").innerHTML = "";
}

function showEditIntents() {
  getE("editIntentForm").style.display = "block";
}

function displayMessage(elementId, message) {
  const element = getE(elementId);
  element.innerHTML = "";
  const p = document.createElement("p");
  p.textContent = message;
  element.appendChild(p);
}

function displayError(elementId, message) {
  const element = getE(elementId);
  element.innerHTML = "";
  const error = document.createElement("p");
  error.innerHTML = message;
  element.appendChild(error);
}

function toggleSection(sectionToToggle, sectionToClose, foldableID) {
  const toggleSect = getE(sectionToToggle);
  const closeSect = getE(sectionToClose);

  if (toggleSect.style.display === "block") {
    closeSection(toggleSect, foldableID);
  } else {
    openSection(toggleSect, foldableID);
    closeSection(closeSect, foldableID);
  }
}

function openSection(sectionID) {
  sectionID.style.display = "block";
}

function closeSection(sectionID) {
  sectionID.style.display = "none";
}

function updateColor(id) {
  const l_id = id.replace("section", "icon");
  const elements = document.getElementsByName("icon");

  elements.forEach((element) => {
    element.classList.remove("selectedicon");
    element.classList.add("nowselectedicon");
  });

  const targetElement = document.getElementById(l_id);
  if (!targetElement) {
    return;
  }
  targetElement.classList.remove("notselectedicon");
  targetElement.classList.add("selectedicon");
}


function showLog() {
  toggleVisibility(["section-log", "section-botauth", "section-uicustomizer", "section-intentguide"], "section-log", "icon-Logs");
  logDivArea = getE("botTranscriptedTextArea");
  getE("logscrollbottom").checked = true;
  logP = document.createElement("p");
  GetLog = true
  const interval = setInterval(() => {
    if (!GetLog) {
      clearInterval(interval);
      return;
    }
    const url = getE("logdebug").checked ? "/api/get_debug_logs" : "/api/get_logs";
    fetch(url)
      .then((response) => response.text())
      .then((logs) => {
        logDivArea.innerHTML = logs || "No logs yet, you must say a command to Vector. (this updates automatically)";
        if (getE("logscrollbottom").checked) {
          logDivArea.scrollTop = logDivArea.scrollHeight;
        }
      });
  }, 500);
}

function showLanguage() {
  toggleVisibility(["section-weather", "section-restart", "section-kg", "section-language"], "section-language", "icon-Language");
  updateSTTInfo();
}

function updateSTTInfo() {
  fetch("/api/get_stt_info")
    .then((response) => response.json())
    .then((parsed) => {
      // Set provider
      if (parsed.provider) {
        getE("sttProvider").value = parsed.provider;
      } else {
        // Default to vosk if not set
        getE("sttProvider").value = "vosk";
      }
      
      // Set language
      if (parsed.language) {
        getE("languageSelection").value = parsed.language;
      }
      
      // Set intent matching mode
      if (parsed.intent_matching_mode) {
        getE("intentMatchingMode").value = parsed.intent_matching_mode;
      } else {
        // Default to single language
        getE("intentMatchingMode").value = "single";
      }
      
      // Update UI based on provider
      checkSTTProvider();
    })
    .catch((error) => {
      console.error("Error fetching STT info:", error);
    });
}

// Biến global để lưu MAC address đã chọn
let selectedMACAddress = null;

// Function để cập nhật trạng thái các bước pairing
function updateStepStatus(stepNum, status, message) {
  const step = document.getElementById(`step${stepNum}`);
  const stepStatus = document.getElementById(`step${stepNum}Status`);
  const stepNumber = step?.querySelector('.step-number');
  const stepTitle = step?.querySelector('h5');
  
  if (!step || !stepStatus || !stepNumber) return;
  
  // Cập nhật màu border và background
  const colors = {
    'pending': { border: '#e0e0e0', bg: '#e0e0e0', text: '#666', title: '#999' },
    'in-progress': { border: '#ff9800', bg: '#ff9800', text: '#fff', title: '#333' },
    'completed': { border: '#4caf50', bg: '#4caf50', text: '#fff', title: '#333' },
    'error': { border: '#f44336', bg: '#f44336', text: '#fff', title: '#333' }
  };
  
  const color = colors[status] || colors.pending;
  step.style.borderLeftColor = color.border;
  stepNumber.style.backgroundColor = color.bg;
  stepNumber.style.color = color.text;
  if (stepTitle) stepTitle.style.color = color.title;
  
  // Cập nhật status text
  const statusTexts = {
    'pending': '⏳ Đang chờ',
    'in-progress': '🔄 Đang xử lý...',
    'completed': '✅ Hoàn thành',
    'error': '❌ Lỗi'
  };
  
  stepStatus.textContent = message || statusTexts[status] || '⏳ Đang chờ';
  stepStatus.style.backgroundColor = color.bg;
  stepStatus.style.color = color.text;
}

function showXiaozhiPairing() {
  toggleVisibility(["section-log", "section-botauth", "section-uicustomizer", "section-xiaozhi-pairing"], "section-xiaozhi-pairing", "icon-XiaozhiPairing");
  
  // Reset trạng thái các bước
  updateStepStatus(1, 'pending', '⏳ Đang chờ');
  updateStepStatus(2, 'pending', '⏳ Đang chờ');
  updateStepStatus(3, 'pending', '⏳ Đang chờ');
  updateStepStatus(4, 'pending', '⏳ Đang chờ');
  
  // Ẩn container MAC address select ban đầu
  const container = document.getElementById("macAddressSelectContainer");
  if (container) {
    container.style.display = "none";
  }
  
  // Reset nút Load MAC Address
  const loadBtn = document.getElementById("loadMACAddressBtn");
  if (loadBtn) {
    loadBtn.disabled = false;
    loadBtn.style.backgroundColor = "#4285f4";
    loadBtn.textContent = "🔍 Lấy MAC Address";
  }
  
  // Reset selected MAC
  selectedMACAddress = null;
  
  // Tự động load MAC address của máy và danh sách devices khi mở section (cho các section khác)
  loadLocalMACAddress();
  loadConnectedDevices();
  
  // KHÔNG tự động load MAC addresses vào dropdown nữa - user phải click nút
}

function loadMACAddressesToSelect() {
  try {
    console.log("[Pairing] ===== Bắt đầu loadMACAddressesToSelect =====");
    const select = document.getElementById("macAddressSelect");
    const container = document.getElementById("macAddressSelectContainer");
    const loadBtn = document.getElementById("loadMACAddressBtn");
    
    if (!select || !container) {
      console.error("[Pairing] ❌ Không tìm thấy macAddressSelect element hoặc container!");
      // Thử lại sau 500ms
      setTimeout(loadMACAddressesToSelect, 500);
      return;
    }
    
    // Hiển thị container và cập nhật trạng thái
    container.style.display = "block";
    if (loadBtn) {
      loadBtn.disabled = true;
      loadBtn.style.backgroundColor = "#9e9e9e";
      loadBtn.textContent = "🔄 Đang tải MAC addresses...";
    }
    updateStepStatus(1, 'in-progress', '🔄 Đang tải MAC addresses...');
    
    console.log("[Pairing] ✅ Tìm thấy macAddressSelect element");
    select.innerHTML = "<option value=''>Đang tải MAC addresses...</option>";
    
    // Lấy danh sách MAC addresses - đơn giản hóa: chỉ load từ local_mac trước
    console.log("[Pairing] Đang fetch API get_local_mac...");
    fetch("/api/xiaozhi_get_local_mac")
      .then(r => {
        console.log("[Pairing] Response status từ get_local_mac:", r.status);
        if (!r.ok) {
          throw new Error(`HTTP ${r.status}: ${r.statusText}`);
        }
        return r.json();
      })
      .then(macData => {
        console.log("[Pairing] ✅ Nhận được MAC data:", JSON.stringify(macData, null, 2));
        
        // Sau đó mới fetch connected devices
        return fetch("/api/xiaozhi_get_connected_devices")
          .then(r => {
            if (!r.ok) {
              console.warn("[Pairing] Lỗi khi fetch connected devices:", r.status);
              return { devices: [], count: 0 };
            }
            return r.json();
          })
          .then(connectedData => {
            console.log("[Pairing] ✅ Nhận được Connected data:", JSON.stringify(connectedData, null, 2));
            return [macData, connectedData];
          })
          .catch(err => {
            console.warn("[Pairing] Lỗi khi fetch connected devices:", err);
            return [macData, { devices: [], count: 0 }];
          });
      })
      .then(([macData, connectedData]) => {
        if (!macData && !connectedData) {
          throw new Error("Không nhận được data từ cả hai API");
        }
        return processMACAddresses(macData, connectedData, select);
      })
      .catch(err => {
        console.error("[Pairing] ❌ Lỗi trong quá trình fetch:", err);
        const select = document.getElementById("macAddressSelect");
        const container = document.getElementById("macAddressSelectContainer");
        const loadBtn = document.getElementById("loadMACAddressBtn");
        
        if (select) {
          select.innerHTML = `<option value=''>❌ Lỗi: ${err.message}</option>`;
        }
        if (container) {
          container.style.display = "block";
        }
        if (loadBtn) {
          loadBtn.disabled = false;
          loadBtn.style.backgroundColor = "#4285f4";
          loadBtn.textContent = "🔍 Lấy MAC Address";
        }
        updateStepStatus(1, 'error', `❌ Lỗi: ${err.message}`);
        setTimeout(() => loadMACAddressesToSelect(), 2000);
      });
  } catch (error) {
    console.error("[Pairing] ❌ Lỗi trong loadMACAddressesToSelect:", error);
    const select = document.getElementById("macAddressSelect");
    if (select) {
      select.innerHTML = `<option value=''>❌ Lỗi: ${error.message}</option>`;
    }
  }
}

function processMACAddresses(macData, connectedData, select) {
  try {
    console.log("[Pairing] ===== Bắt đầu processMACAddresses =====");
    console.log("[Pairing] MAC data:", JSON.stringify(macData, null, 2));
    console.log("[Pairing] Connected data:", JSON.stringify(connectedData, null, 2));
    
    // Kiểm tra data có hợp lệ không
    if (!macData) {
      console.error("[Pairing] ❌ macData là null hoặc undefined");
      select.innerHTML = "<option value=''>❌ Lỗi: Không nhận được data từ server</option>";
      return;
    }
    
    select.innerHTML = "<option value=''>-- Chọn MAC Address --</option>";
    
    let hasAnyMAC = false;
    let optionCount = 0;
    
    // Thêm option đặc biệt cho PC IP-based MAC address
    const pcIPOption = document.createElement("option");
    pcIPOption.value = "pc_ip_auto";
    pcIPOption.textContent = "💻 PC IP-based (Auto) - Tự động tạo pairing code";
    pcIPOption.style.fontWeight = "bold";
    pcIPOption.style.color = "#1976d2";
    select.appendChild(pcIPOption);
    hasAnyMAC = true;
    optionCount++;
    
    // Thêm MAC addresses đang kết nối (ưu tiên)
    if (connectedData && connectedData.devices && Array.isArray(connectedData.devices) && connectedData.devices.length > 0) {
      console.log("[Pairing] Có", connectedData.devices.length, "devices đang kết nối");
      connectedData.devices.forEach((device, index) => {
        console.log("[Pairing] Thêm device", index + 1, ":", device.device_id);
        const option = document.createElement("option");
        option.value = device.device_id;
        option.textContent = `${device.device_id} (Đang kết nối)`;
        select.appendChild(option);
        hasAnyMAC = true;
        optionCount++;
      });
    } else {
      console.log("[Pairing] Không có devices đang kết nối");
    }
    
    // Thêm Primary MAC
    if (macData.primary_mac) {
      console.log("[Pairing] Thêm Primary MAC:", macData.primary_mac);
      const option = document.createElement("option");
      option.value = macData.primary_mac;
      option.textContent = `${macData.primary_mac} (Primary)`;
      if (!selectedMACAddress) {
        option.selected = true;
        selectedMACAddress = macData.primary_mac;
        console.log("[Pairing] Tự động chọn Primary MAC:", selectedMACAddress);
      }
      select.appendChild(option);
      hasAnyMAC = true;
      optionCount++;
    } else {
      console.log("[Pairing] Không có primary_mac");
    }
    
    // Thêm các MAC addresses khác
    if (macData.all_macs && Array.isArray(macData.all_macs) && macData.all_macs.length > 0) {
      console.log("[Pairing] Có", macData.all_macs.length, "MAC addresses trong all_macs");
      macData.all_macs.forEach((mac, index) => {
        // Bỏ qua MAC đã thêm (primary hoặc đang kết nối)
        if (mac === macData.primary_mac) {
          console.log("[Pairing] Bỏ qua MAC (đã thêm là Primary):", mac);
          return;
        }
        if (connectedData && connectedData.devices && connectedData.devices.some(d => d.device_id === mac)) {
          console.log("[Pairing] Bỏ qua MAC (đang kết nối):", mac);
          return;
        }
        
        console.log("[Pairing] Thêm MAC", index + 1, ":", mac);
        const option = document.createElement("option");
        option.value = mac;
        option.textContent = mac;
        select.appendChild(option);
        hasAnyMAC = true;
        optionCount++;
      });
    } else {
      console.log("[Pairing] Không có all_macs hoặc all_macs rỗng");
    }
    
    console.log("[Pairing] Tổng cộng đã thêm", optionCount, "MAC addresses vào dropdown");
    console.log("[Pairing] Số lượng options trong dropdown:", select.options.length);
    
    if (!hasAnyMAC) {
      select.innerHTML = "<option value=''>⚠️ Không tìm thấy MAC address nào</option>";
      console.warn("[Pairing] ❌ Không tìm thấy MAC address nào!");
      const loadBtn = document.getElementById("loadMACAddressBtn");
      if (loadBtn) {
        loadBtn.disabled = false;
        loadBtn.style.backgroundColor = "#4285f4";
        loadBtn.textContent = "🔍 Lấy MAC Address";
      }
      updateStepStatus(1, 'error', '❌ Không tìm thấy MAC address nào');
      return;
    }
    
    // Đảm bảo có ít nhất một option được chọn
    if (select.options.length <= 1) {
      select.innerHTML = "<option value=''>⚠️ Không có MAC address nào để chọn (options.length = " + select.options.length + ")</option>";
      console.warn("[Pairing] ❌ Dropdown chỉ có", select.options.length, "option (chỉ có placeholder)");
      return;
    }
    
      console.log("[Pairing] ✅ Đã thêm thành công", optionCount, "MAC addresses vào dropdown");
      
      // Cập nhật trạng thái và nút
      const loadBtn = document.getElementById("loadMACAddressBtn");
      if (loadBtn) {
        loadBtn.disabled = false;
        loadBtn.style.backgroundColor = "#4285f4";
        loadBtn.textContent = "🔄 Lấy lại MAC Address";
      }
      updateStepStatus(1, 'pending', '⏳ Đã tải xong, vui lòng chọn MAC address');
      
      // Nếu chưa có MAC nào được chọn và có MAC addresses, chọn MAC đầu tiên
      if (!selectedMACAddress && select.options.length > 1) {
        // Tìm option đầu tiên có value (bỏ qua option "-- Chọn MAC Address --")
        for (let i = 1; i < select.options.length; i++) {
          const option = select.options[i];
          if (option.value) {
            select.value = option.value;
            selectedMACAddress = option.value;
            console.log("[Pairing] Tự động chọn MAC đầu tiên:", selectedMACAddress);
            onMACAddressSelected();
            break;
          }
        }
      } else if (selectedMACAddress) {
        // Nếu đã có MAC được chọn, đảm bảo nó được select trong dropdown
        select.value = selectedMACAddress;
        onMACAddressSelected();
      }
      
      console.log("[Pairing] ===== Hoàn thành processMACAddresses =====");
  } catch (error) {
    console.error("[Pairing] ❌ Lỗi trong processMACAddresses:", error);
    console.error("[Pairing] Error stack:", error.stack);
    select.innerHTML = `<option value=''>❌ Lỗi xử lý: ${error.message}</option>`;
  }
}

function onMACAddressSelected() {
  const select = document.getElementById("macAddressSelect");
  const infoDiv = document.getElementById("selectedMACInfo");
  const generateBtn = document.getElementById("generatePairingCodeBtn");
  
  if (!select || !select.value) {
    selectedMACAddress = null;
    if (infoDiv) infoDiv.style.display = "none";
    if (generateBtn) {
      generateBtn.disabled = true;
      generateBtn.style.backgroundColor = "#9e9e9e";
    }
    return;
  }
  
  selectedMACAddress = select.value;
  console.log("[Pairing] Đã chọn MAC address:", selectedMACAddress);
  
  // Nếu chọn PC IP-based, tự động lấy IP và generate pairing code
  if (selectedMACAddress === "pc_ip_auto") {
    handlePCIPBasedSelection();
    return;
  }
  
  // Hiển thị thông tin MAC đã chọn và Client-Id
  if (infoDiv) {
    const code = infoDiv.querySelector("code");
    if (code) {
      code.textContent = selectedMACAddress;
    }
    
    // Lấy Client-Id từ config để hiển thị
    fetch("/api/get_kg_api")
      .then(response => response.json())
      .then(kgData => {
        const clientIdInfo = document.getElementById("selectedClientIdInfo");
        if (clientIdInfo && kgData.client_id) {
          const clientIdCode = clientIdInfo.querySelector("code");
          if (clientIdCode) {
            clientIdCode.textContent = kgData.client_id;
          }
          clientIdInfo.style.display = "block";
        }
      })
      .catch(err => {
        console.log("[Pairing] Không thể lấy Client-Id từ config:", err);
      });
    
    infoDiv.style.display = "block";
  }
  
  // Enable nút Generate Pairing Code
  if (generateBtn) {
    generateBtn.disabled = false;
    generateBtn.style.backgroundColor = "#4285f4";
    generateBtn.style.cursor = "pointer";
  }
  
  // Cập nhật trạng thái bước 1: completed, bước 2: ready
  updateStepStatus(1, 'completed', `✅ Đã chọn: ${selectedMACAddress}`);
  updateStepStatus(2, 'pending', '⏳ Sẵn sàng generate code');
  
  // Tự động kiểm tra trạng thái pairing của MAC đã chọn
  checkPairingStatusForSelectedMAC(selectedMACAddress);
}

// Xử lý khi chọn PC IP-based option - tự động lấy IP và generate pairing code
function handlePCIPBasedSelection() {
  const infoDiv = document.getElementById("selectedMACInfo");
  const generateBtn = document.getElementById("generatePairingCodeBtn");
  
  // Cập nhật trạng thái
  updateStepStatus(1, 'in-progress', '🔄 Đang lấy IP address...');
  
  // Lấy IP của client
  fetch("/api/xiaozhi_get_client_ip")
    .then(response => response.json())
    .then(data => {
      if (data.device_id) {
        selectedMACAddress = data.device_id;
        console.log("[Pairing] PC IP-based device ID:", selectedMACAddress);
        
        // Hiển thị thông tin (bao gồm Client-Id)
        if (infoDiv) {
          // Lấy Client-Id từ config
          fetch("/api/get_kg_api")
            .then(response => response.json())
            .then(kgData => {
              let clientIdHtml = "";
              if (kgData.client_id) {
                clientIdHtml = `
                  <div style="margin-top: 8px; padding: 8px; background-color: #fff; border-radius: 3px;">
                    <p style="margin: 0; color: #1976d2; font-size: 11px;">
                      🆔 Client-Id (UUID): <code style="background: #e3f2fd; padding: 2px 6px; border-radius: 3px; font-family: monospace; font-weight: bold; color: #1976d2;">${kgData.client_id}</code>
                    </p>
                  </div>
                `;
              }
              
              infoDiv.style.display = "block";
              infoDiv.style.backgroundColor = "#e3f2fd";
              infoDiv.innerHTML = `
                <p style="margin: 0; color: #1976d2; font-size: 12px;">
                  ✅ Đã chọn: <code style="background: white; padding: 2px 6px; border-radius: 3px; font-family: monospace; font-weight: bold;">${selectedMACAddress}</code>
                </p>
                <p style="margin: 5px 0 0 0; color: #666; font-size: 11px;">
                  IP Address: ${data.ip} | Type: PC IP-based
                </p>
                ${clientIdHtml}
              `;
            })
            .catch(err => {
              console.log("[Pairing] Không thể lấy Client-Id:", err);
              infoDiv.style.display = "block";
              infoDiv.style.backgroundColor = "#e3f2fd";
              infoDiv.innerHTML = `
                <p style="margin: 0; color: #1976d2; font-size: 12px;">
                  ✅ Đã chọn: <code style="background: white; padding: 2px 6px; border-radius: 3px; font-family: monospace; font-weight: bold;">${selectedMACAddress}</code>
                </p>
                <p style="margin: 5px 0 0 0; color: #666; font-size: 11px;">
                  IP Address: ${data.ip} | Type: PC IP-based
                </p>
              `;
            });
        }
        
        // Cập nhật trạng thái bước 1: completed
        updateStepStatus(1, 'completed', `✅ Đã chọn: ${selectedMACAddress}`);
        
        // Tự động generate pairing code luôn
        updateStepStatus(2, 'in-progress', '🔄 Đang tạo pairing code...');
        generatePairingCodeWithMAC(selectedMACAddress);
      } else {
        throw new Error("Không nhận được device_id từ server");
      }
    })
    .catch(error => {
      console.error("[Pairing] Lỗi khi lấy IP:", error);
      updateStepStatus(1, 'error', `❌ Lỗi: ${error.message}`);
      
      if (infoDiv) {
        infoDiv.style.display = "block";
        infoDiv.style.backgroundColor = "#ffebee";
        infoDiv.innerHTML = `<p style="margin: 0; color: #c62828; font-size: 12px;">❌ Lỗi: ${error.message}</p>`;
      }
    });
}

function checkPairingStatusForSelectedMAC(macAddress) {
  const statusDiv = document.getElementById("pairingStatusCheck");
  const generateBtn = document.getElementById("generatePairingCodeBtn");
  
  if (!statusDiv) return;
  
  statusDiv.style.display = "block";
  // Activation status check disabled (avoid unnecessary upstream calls/log spam)
  statusDiv.innerHTML = "<p style='color: #666; font-size: 11px;'>ℹ️ Đã tắt kiểm tra trạng thái activate (disabled).</p>";
  // Always allow generating pairing code
  if (generateBtn) {
    generateBtn.disabled = false;
    generateBtn.style.backgroundColor = "#4285f4";
    generateBtn.style.cursor = "pointer";
    generateBtn.title = "";
  }
}

function checkPairingStatusAndUpdateButton() {
  // Hàm này không còn tự động chọn MAC nữa
  // Chỉ kiểm tra khi user chọn MAC từ dropdown (thông qua onMACAddressSelected)
  console.log("[Pairing] checkPairingStatusAndUpdateButton - Chờ user chọn MAC address từ dropdown");
}

function loadLocalMACAddress() {
  const displayDiv = document.getElementById("localMACDisplay");
  displayDiv.innerHTML = "<p style='color: #666;'>Đang lấy MAC address...</p>";
  displayDiv.style.display = "block";
  
  fetch("/api/xiaozhi_get_local_mac")
    .then(response => response.json())
    .then(data => {
      let html = "";
      
      if (data.primary_mac) {
        html += `<div style="border-left: 4px solid #4caf50; padding-left: 15px; margin-bottom: 15px;">`;
        html += `<h5 style="color: #2e7d32; margin-top: 0;">✅ MAC Address chính:</h5>`;
        html += `<p style="font-size: 18px; font-weight: bold; color: #0f9d58; font-family: monospace; margin: 10px 0;">${data.primary_mac}</p>`;
        html += `<button onclick="document.getElementById('checkDeviceIdInput').value='${data.primary_mac}'; checkDeviceStatus();" 
          style="padding: 6px 12px; background-color: #4caf50; color: white; border: none; border-radius: 5px; cursor: pointer; font-size: 12px;">
          🔍 Check Status của MAC này
        </button>`;
        html += `</div>`;
      } else {
        html += `<p style="color: #ff9800;">⚠️ ${data.message || 'Không tìm thấy MAC address'}</p>`;
      }
      
      if (data.all_macs && data.all_macs.length > 1) {
        html += `<div style="margin-top: 15px; padding-top: 15px; border-top: 1px solid #ddd;">`;
        html += `<p style="color: #666; font-size: 12px; margin-bottom: 10px;"><strong>Tất cả MAC addresses:</strong></p>`;
        html += `<ul style="color: #666; font-size: 12px; margin-left: 20px;">`;
        data.all_macs.forEach(mac => {
          const isPrimary = mac === data.primary_mac;
          html += `<li style="margin: 5px 0;">
            <code style="background: #f5f5f5; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${mac}</code>
            ${isPrimary ? '<span style="color: #4caf50; margin-left: 5px;">(Chính)</span>' : ''}
          </li>`;
        });
        html += `</ul></div>`;
      }
      
      displayDiv.innerHTML = html;
    })
    .catch(error => {
      displayDiv.innerHTML = `<p style="color: #db4437;">❌ Lỗi khi lấy MAC address: ${error.message}</p>`;
    });
}

function generateXiaozhiPairingCode() {
  // Luồng: Chọn MAC → Generate Pairing Code với MAC đã chọn
  const statusDiv = document.getElementById("xiaozhiPairingStatus");
  const codeDisplay = document.getElementById("pairingCodeDisplay");
  const codeDiv = document.getElementById("pairingCode");
  const infoDiv = document.getElementById("pairingCodeInfo");
  const select = document.getElementById("macAddressSelect");
  
  // Kiểm tra xem đã chọn MAC address chưa
  if (!selectedMACAddress) {
    // Nếu chưa chọn, lấy từ dropdown
    if (select && select.value) {
      selectedMACAddress = select.value;
      onMACAddressSelected(); // Cập nhật UI
    } else {
      statusDiv.innerHTML = "❌ Vui lòng chọn MAC address trước khi generate pairing code.";
      statusDiv.style.color = "#db4437";
      return;
    }
  }
  
  // Generate pairing code với MAC đã chọn
  console.log("[Pairing] Generate pairing code cho MAC:", selectedMACAddress);
  generatePairingCodeWithMAC(selectedMACAddress);
}

function generatePairingCodeWithMAC(deviceId, clientId = "") {
  const statusDiv = document.getElementById("xiaozhiPairingStatus");
  const codeDisplay = document.getElementById("pairingCodeDisplay");
  const codeDiv = document.getElementById("pairingCode");
  const infoDiv = document.getElementById("pairingCodeInfo");
  const deviceInfoDisplay = document.getElementById("deviceInfoDisplay");
  const displayDeviceId = document.getElementById("displayDeviceId");
  const displayClientId = document.getElementById("displayClientId");
  const generateBtn = document.getElementById("generatePairingCodeBtn");
  
  // Gửi Device-Id và Client-Id đến server để generate pairing code
  // Nếu clientId không được cung cấp, server sẽ tự động tạo UUID
  let url = `/api/xiaozhi_generate_pairing_code?device_id=${encodeURIComponent(deviceId)}`;
  if (clientId && clientId.trim() !== "") {
    url += `&client_id=${encodeURIComponent(clientId.trim())}`;
  }
  
  fetch(url, { method: "POST" })
    .then(response => response.json())
    .then(data => {
      // Re-enable button
      if (generateBtn) {
        generateBtn.disabled = false;
        generateBtn.style.backgroundColor = "#0f9d58";
        generateBtn.style.cursor = "pointer";
        generateBtn.textContent = "🔑 Tạo mã Pairing";
      }
      
      if (data.code) {
        // Display pairing code
        if (codeDiv) {
          codeDiv.textContent = data.code;
        }
        if (codeDisplay) {
          codeDisplay.style.display = "block";
        }
        if (statusDiv) {
          statusDiv.innerHTML = "✅ Pairing code đã được tạo thành công!";
          statusDiv.style.color = "#0f9d58";
        }
        
        // Display device info
        if (deviceInfoDisplay) {
          deviceInfoDisplay.style.display = "block";
        }
        if (displayDeviceId && data.device_id) {
          displayDeviceId.textContent = data.device_id;
        }
        if (displayClientId && data.client_id) {
          displayClientId.textContent = data.client_id;
        }
        
        // Không auto-check activation status nữa (tránh spam upstream / không cần cho luồng pairing)
        
        // Hiển thị thông tin Device-Id và Client-Id
        let infoText = "";
        if (data.device_id) {
          infoText += `<strong>Device-Id / MAC Address:</strong> <code style="background: #f5f5f5; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${data.device_id}</code><br/>`;
        }
        if (data.client_id) {
          infoText += `<strong>Client-Id (UUID):</strong> <code style="background: #e3f2fd; padding: 2px 6px; border-radius: 3px; font-family: monospace; color: #1976d2;">${data.client_id}</code><br/>`;
        }
        if (data.note) {
          infoText += `<span style="color: #ff9800; font-size: 11px;">${data.note}</span><br/>`;
        }
        
        // Update expiry info
        const expiresIn = data.expires_in || 600;
        infoText += `Mã sẽ hết hạn sau: ${Math.floor(expiresIn / 60)} phút ${expiresIn % 60} giây`;
        if (infoDiv) {
          infoDiv.innerHTML = infoText;
        }
        
        // Start countdown
        let remaining = expiresIn;
        const countdownInterval = setInterval(() => {
          remaining--;
          if (remaining > 0) {
            const minutes = Math.floor(remaining / 60);
            const seconds = remaining % 60;
            let countdownText = "";
            if (data.device_id) {
              countdownText += `<strong>Device-Id / MAC Address:</strong> <code style="background: #f5f5f5; padding: 2px 6px; border-radius: 3px; font-family: monospace;">${data.device_id}</code><br/>`;
            }
            if (data.client_id) {
              countdownText += `<strong>Client-Id (UUID):</strong> <code style="background: #e3f2fd; padding: 2px 6px; border-radius: 3px; font-family: monospace; color: #1976d2;">${data.client_id}</code><br/>`;
            }
            countdownText += `Mã sẽ hết hạn sau: ${minutes} phút ${seconds.toString().padStart(2, '0')} giây`;
            if (infoDiv) {
              infoDiv.innerHTML = countdownText;
            }
          } else {
            clearInterval(countdownInterval);
            if (codeDisplay) {
              codeDisplay.style.display = "none";
            }
            if (statusDiv) {
              statusDiv.innerHTML = "Pairing code đã hết hạn. Vui lòng tạo mã mới.";
              statusDiv.style.color = "#db4437";
            }
          }
        }, 1000);
      } else {
        if (statusDiv) {
          statusDiv.innerHTML = "❌ Không thể tạo pairing code. Vui lòng thử lại.";
          statusDiv.style.color = "#db4437";
        }
      }
    })
    .catch(error => {
      // Re-enable button on error
      if (generateBtn) {
        generateBtn.disabled = false;
        generateBtn.style.backgroundColor = "#0f9d58";
        generateBtn.style.cursor = "pointer";
        generateBtn.textContent = "🔑 Tạo mã Pairing";
      }
      
      if (statusDiv) {
        statusDiv.innerHTML = `❌ Lỗi: ${error.message}`;
        statusDiv.style.color = "#db4437";
      }
    });
}

// Polling để kiểm tra activation status
let activationPollingInterval = null;

function startActivationPolling(deviceID) {
  // Disabled by design (avoid unnecessary upstream checks / log spam)
  console.log("[Pairing] startActivationPolling disabled for device:", deviceID);
}

function checkDeviceStatus() {
  const deviceIdInput = document.getElementById("checkDeviceIdInput");
  const clientIdInput = document.getElementById("checkClientIdInput");
  const resultDiv = document.getElementById("deviceStatusResult");
  const deviceId = deviceIdInput ? deviceIdInput.value.trim() : "";
  const clientId = clientIdInput ? clientIdInput.value.trim() : "";
  
  if (!deviceId) {
    resultDiv.innerHTML = `<p style="color: #db4437;">⚠️ Vui lòng nhập Device-Id hoặc MAC address</p>`;
    resultDiv.style.display = "block";
    return;
  }
  
  resultDiv.style.display = "block";
  resultDiv.innerHTML = `<p style="color: #666;">ℹ️ Đã tắt chức năng kiểm tra trạng thái activate (disabled).</p>`;
}

function loadConnectedDevices() {
  const listDiv = document.getElementById("connectedDevicesList");
  listDiv.innerHTML = "<p style='color: #666;'>Đang tải danh sách MAC addresses...</p>";
  
  fetch("/api/xiaozhi_get_connected_devices")
    .then(response => response.json())
    .then(data => {
      if (data.devices && data.devices.length > 0) {
        let html = `<table style="width: 100%; border-collapse: collapse; margin-top: 10px;">
          <thead>
            <tr style="background-color: #4285f4; color: white;">
              <th style="padding: 10px; text-align: left; border: 1px solid #ddd;">Device-Id / MAC Address</th>
              <th style="padding: 10px; text-align: left; border: 1px solid #ddd;">Client ID</th>
              <th style="padding: 10px; text-align: left; border: 1px solid #ddd;">Kết nối lúc</th>
              <th style="padding: 10px; text-align: left; border: 1px solid #ddd;">Hoạt động gần nhất</th>
              <th style="padding: 10px; text-align: left; border: 1px solid #ddd;">Thời gian kết nối</th>
            </tr>
          </thead>
          <tbody>`;
        
        data.devices.forEach(device => {
          const connectedAt = new Date(device.connected_at);
          const lastSeen = new Date(device.last_seen);
          const connectedFor = formatDuration(device.connected_for);
          
          // Phân biệt PC (bắt đầu bằng "pc_") và ESP32 (MAC address thật)
          const isPC = device.device_id.startsWith('pc_');
          const deviceType = isPC ? '🖥️ PC' : '📱 ESP32';
          const deviceColor = isPC ? '#ff9800' : '#0f9d58';
          const deviceNote = isPC ? ' (PC đang chạy code ESP32)' : ' (MAC address thật)';
          
          // Thêm nút check status cho mỗi device
          const checkButton = `<button onclick="checkDeviceStatusById('${device.device_id}')" 
            style="padding: 4px 8px; background-color: #4285f4; color: white; border: none; border-radius: 3px; cursor: pointer; font-size: 11px; margin-top: 5px;">
            🔍 Check Status
          </button>`;
          
          html += `<tr style="background-color: white;">
            <td style="padding: 10px; border: 1px solid #ddd;">
              <span style="font-family: monospace; font-weight: bold; color: ${deviceColor};">
                ${device.device_id}
              </span>
              <br/>
              <span style="font-size: 11px; color: #666;">${deviceType}${deviceNote}</span>
              ${checkButton}
            </td>
            <td style="padding: 10px; border: 1px solid #ddd; font-family: monospace;">${device.client_id || 'N/A'}</td>
            <td style="padding: 10px; border: 1px solid #ddd;">${connectedAt.toLocaleString('vi-VN')}</td>
            <td style="padding: 10px; border: 1px solid #ddd;">${lastSeen.toLocaleString('vi-VN')}</td>
            <td style="padding: 10px; border: 1px solid #ddd;">${connectedFor}</td>
          </tr>`;
        });
        
        html += `</tbody></table>`;
        html += `<p style="margin-top: 10px; font-size: 12px; color: #666;">Tổng cộng: <strong>${data.count}</strong> thiết bị đã kết nối</p>`;
        listDiv.innerHTML = html;
      } else {
        listDiv.innerHTML = `<p style="color: #ff9800; font-style: italic;">Chưa có thiết bị nào kết nối. Hãy kết nối thiết bị ESP32 Xiaozhi để xem MAC address.</p>`;
      }
    })
    .catch(error => {
      listDiv.innerHTML = `<p style="color: #db4437;">Lỗi khi tải danh sách: ${error.message}</p>`;
    });
}

function checkDeviceStatusById(deviceId) {
  // Điền Device-Id vào input và tự động check
  const deviceIdInput = document.getElementById("checkDeviceIdInput");
  if (deviceIdInput) {
    deviceIdInput.value = deviceId;
  }
  checkDeviceStatus();
  // Scroll đến phần check status
  const checkStatusDiv = document.getElementById("deviceStatusResult");
  if (checkStatusDiv) {
    checkStatusDiv.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }
}

function formatDuration(seconds) {
  if (seconds < 60) {
    return `${seconds} giây`;
  } else if (seconds < 3600) {
    const minutes = Math.floor(seconds / 60);
    const secs = seconds % 60;
    return `${minutes} phút ${secs} giây`;
  } else {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    return `${hours} giờ ${minutes} phút`;
  }
}

function showWeather() {
  toggleVisibility(["section-weather", "section-restart", "section-language", "section-kg"], "section-weather", "icon-Weather");
}

function showKG() {
  toggleVisibility(["section-weather", "section-restart", "section-language", "section-kg"], "section-kg", "icon-KG");
  // Tự động load config khi mở section - đợi một chút để đảm bảo DOM đã sẵn sàng
  setTimeout(() => {
    updateKGAPI();
  }, 100);
}

function toggleVisibility(sections, sectionToShow, iconId) {
  if (sectionToShow != "section-log") {
    GetLog = false;
  }
  sections.forEach((section) => {
    const el = document.getElementById(section);
    if (el) {
      el.style.display = "none";
    }
  });
  const showEl = document.getElementById(sectionToShow);
  if (showEl) {
    showEl.style.display = "block";
  }
  if (iconId) {
    updateColor(iconId);
  }
}