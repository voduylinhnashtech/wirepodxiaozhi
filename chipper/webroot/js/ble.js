const vectorEpodSetup = "https://wpsetup.keriganc.com";
let authEl = document.getElementById("botAuth");
let statusP = document.createElement("p");
let OTAUpdating = false;

const externalSetup = document.createElement("a");
externalSetup.href = vectorEpodSetup;
externalSetup.innerHTML = vectorEpodSetup;

// Cache for manual botSdkInfo UI
let _manualBotInfoCache = null; // { global_guid, robots: [...] }
let _manualEsnDropdownInitialized = false;

function showBotAuth() {
  GetLog = false;
  toggleSections("section-botauth", "icon-BotAuth");
  // Load current botSdkInfo into manual inputs (if available)
  try {
    loadManualBotInfo();
  } catch (e) {
    // ignore
  }
  checkBLECapability();
}

function toggleSections(showSection, icon) {
  const sections = ["section-log", "section-botauth", "section-uicustomizer", "section-intentguide"];
  sections.forEach((section) => {
    const el = document.getElementById(section);
    if (el) {
      el.style.display = "none";
    }
  });
  const showEl = document.getElementById(showSection);
  if (showEl) {
    showEl.style.display = "block";
  }
  updateColor(icon);
}

function checkBLECapability() {
  updateAuthel("Checking if wire-pod can use BLE directly...");
  fetch("/api-ble/init")
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("success")) {
        beginBLESetup();
      } else {
        showExternalSetupInstructions();
      }
    });
}

function showExternalSetupInstructions() {
  authEl.innerHTML = `
    <p>Head to the following site on any device with Bluetooth support to set up your Vector.</p>
    <a href="${vectorEpodSetup}" target="_blank">${vectorEpodSetup}</a>
    <br>
    <small class="desc">Note: with OSKR/dev robots, it might give a warning about firmware. This can be ignored.</small>
  `;
}

function _setManualBotInfoStatus(msg, isError) {
  const el = document.getElementById("manualBotInfoStatus");
  if (!el) return;
  el.innerHTML = `<p style="margin:0; color:${isError ? "#b00020" : "#0b6e4f"}">${msg}</p>`;
}

function _setBotAuthTransferStatus(msg, isError) {
  const el = document.getElementById("botAuthTransferStatus");
  if (!el) return;
  el.innerHTML = `<p style="margin:0; color:${isError ? "#b00020" : "#0b6e4f"}">${msg}</p>`;
}

function _normalizeEsn(esn) {
  return (esn || "").trim().toLowerCase();
}

function _normalizeIp(ip) {
  ip = (ip || "").trim();
  // Allow user to paste "x.x.x.x:443" and strip the port
  if (ip.includes(":")) {
    const parts = ip.split(":");
    if (parts.length >= 2 && /^\d+$/.test(parts[parts.length - 1])) {
      ip = parts.slice(0, parts.length - 1).join(":");
    }
  }
  return ip;
}

function _getManualEsnSelectedValue() {
  const sel = document.getElementById("manualEsnSelect");
  return sel ? sel.value : "";
}

function _setManualEsnSelectedValue(value) {
  const sel = document.getElementById("manualEsnSelect");
  if (!sel) return;
  sel.value = value;
}

function _showManualEsnInput(show) {
  const esnInput = document.getElementById("manualEsn");
  if (!esnInput) return;
  esnInput.style.display = show ? "block" : "none";
}

function _renderManualEsnOptions(robots, selectedEsn) {
  const sel = document.getElementById("manualEsnSelect");
  if (!sel) return;
  sel.innerHTML = "";

  const placeholder = document.createElement("option");
  placeholder.value = "";
  placeholder.text = robots && robots.length ? "Select ESN..." : "No robots found";
  sel.appendChild(placeholder);

  if (robots && robots.length) {
    robots.forEach((r) => {
      const opt = document.createElement("option");
      opt.value = _normalizeEsn(r.esn);
      opt.text = r.esn;
      sel.appendChild(opt);
    });
  }

  const addOpt = document.createElement("option");
  addOpt.value = "__new__";
  addOpt.text = "➕ New ESN...";
  sel.appendChild(addOpt);

  if (selectedEsn) {
    sel.value = _normalizeEsn(selectedEsn);
  }
}

function onManualEsnChange() {
  const value = _getManualEsnSelectedValue();
  const esnInput = document.getElementById("manualEsn");
  const ipInput = document.getElementById("manualLanIp");
  const guidInput = document.getElementById("manualGuid");
  if (!ipInput || !guidInput) return;

  if (value === "__new__") {
    _showManualEsnInput(true);
    if (esnInput) esnInput.value = "";
    ipInput.value = "";
    guidInput.value = "";
    _setManualBotInfoStatus("Enter a new ESN, then fill IP/GUID and Save.", false);
    return;
  }

  _showManualEsnInput(false);
  if (!value) return;

  const robots = (_manualBotInfoCache && _manualBotInfoCache.robots) ? _manualBotInfoCache.robots : [];
  const chosen = robots.find((r) => _normalizeEsn(r.esn) === _normalizeEsn(value));
  if (chosen) {
    if (esnInput) esnInput.value = chosen.esn || "";
    ipInput.value = chosen.ip_address || "";
    guidInput.value = chosen.guid || "";
  }
}

async function deleteManualBotInfo() {
  const selVal = _getManualEsnSelectedValue();
  const esnInput = document.getElementById("manualEsn");
  const esn = selVal === "__new__" ? _normalizeEsn(esnInput?.value) : _normalizeEsn(selVal);
  if (!esn) {
    _setManualBotInfoStatus("Please select an ESN to delete.", true);
    return;
  }

  try {
    _setManualBotInfoStatus("Deleting...", false);
    const body = new URLSearchParams();
    body.set("esn", esn);
    body.set("delete", "1");
    const resp = await fetch("/api-sdk/set_bot_info", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });
    const txt = await resp.text();
    if (!resp.ok) {
      _setManualBotInfoStatus(`Delete failed: ${txt}`, true);
      return;
    }
    _setManualBotInfoStatus(txt || "Deleted.", false);
    await loadManualBotInfo();
  } catch (e) {
    _setManualBotInfoStatus(`Error deleting botSdkInfo: ${e}`, true);
  }
}

function openDeleteBotInfoModal() {
  const modal = document.getElementById("deleteBotInfoModal");
  const text = document.getElementById("deleteBotInfoModalText");
  if (!modal || !text) return;
  const selVal = _getManualEsnSelectedValue();
  const esnInput = document.getElementById("manualEsn");
  const esn = selVal === "__new__" ? _normalizeEsn(esnInput?.value) : _normalizeEsn(selVal);
  if (!esn || esn === "__new__") {
    _setManualBotInfoStatus("Please select an ESN to delete.", true);
    return;
  }
  text.textContent = `Delete ESN "${esn}" from botSdkInfo.json?`;
  modal.style.display = "flex";
}

function closeDeleteBotInfoModal(cancelOnly) {
  const modal = document.getElementById("deleteBotInfoModal");
  if (!modal) return;
  modal.style.display = "none";
}

async function confirmDeleteManualBotInfo() {
  closeDeleteBotInfoModal();
  await deleteManualBotInfo();
}

function exportBotAuthBundle() {
  const selVal = _getManualEsnSelectedValue();
  const esnInput = document.getElementById("manualEsn");
  const esn = selVal && selVal !== "__new__" ? _normalizeEsn(selVal) : _normalizeEsn(esnInput?.value);
  if (!esn) {
    _setBotAuthTransferStatus("Please select an ESN to export.", true);
    return;
  }
  _setBotAuthTransferStatus(`Exporting bundle for ${esn}...`, false);
  // Trigger file download
  window.location.href = `/api/bot_auth_export?esn=${encodeURIComponent(esn)}`;
}

function triggerImportBotAuthBundle() {
  const input = document.getElementById("botAuthImportFile");
  if (!input) return;
  input.value = "";
  input.click();
}

async function handleImportBotAuthBundle(evt) {
  const file = evt?.target?.files?.[0];
  if (!file) return;
  try {
    _setBotAuthTransferStatus("Importing bundle...", false);
    const form = new FormData();
    form.append("file", file);
    const resp = await fetch("/api/bot_auth_import", {
      method: "POST",
      body: form,
    });
    const txt = await resp.text();
    if (!resp.ok) {
      _setBotAuthTransferStatus(`Import failed: ${txt}`, true);
      return;
    }
    _setBotAuthTransferStatus(txt || "Import OK.", false);
    await loadManualBotInfo();
  } catch (e) {
    _setBotAuthTransferStatus(`Import error: ${e}`, true);
  }
}

async function loadManualBotInfo() {
  try {
    _setManualBotInfoStatus("Loading botSdkInfo...", false);
    const resp = await fetch("/api-sdk/get_bot_info");
    if (!resp.ok) {
      _setManualBotInfoStatus(`Failed to load botSdkInfo (HTTP ${resp.status})`, true);
      return;
    }
    const info = await resp.json();
    const robots = (info && info.robots) ? info.robots : [];
    const globalGuid = info && info.global_guid ? info.global_guid : "";
    _manualBotInfoCache = info;

    // If an ESN is already typed, prefer that robot entry; else pick first robot
    const esnInput = document.getElementById("manualEsn");
    const esnSelect = document.getElementById("manualEsnSelect");
    const ipInput = document.getElementById("manualLanIp");
    const guidInput = document.getElementById("manualGuid");
    const globalGuidInput = document.getElementById("manualGlobalGuid");
    if (!esnInput || !ipInput || !guidInput) return;

    const selected = esnSelect ? esnSelect.value : "";
    const wanted = selected && selected !== "__new__" ? _normalizeEsn(selected) : _normalizeEsn(esnInput.value);
    let chosen = null;
    if (wanted) {
      chosen = robots.find((r) => _normalizeEsn(r.esn) === wanted);
    }
    if (!chosen && robots.length > 0) {
      chosen = robots[0];
    }

    // Render dropdown options
    _renderManualEsnOptions(robots, chosen ? chosen.esn : "");
    if (chosen) {
      _setManualEsnSelectedValue(_normalizeEsn(chosen.esn));
      _showManualEsnInput(false);
    } else {
      // allow new ESN entry
      _setManualEsnSelectedValue("__new__");
      _showManualEsnInput(true);
    }

    if (chosen) {
      esnInput.value = chosen.esn || "";
      ipInput.value = chosen.ip_address || "";
      guidInput.value = chosen.guid || "";
    }
    // Load AppTokens hash for this ESN (best-effort)
    try {
      const esnForTokens = chosen && chosen.esn ? _normalizeEsn(chosen.esn) : _normalizeEsn(esnInput.value);
      const hashInput = document.getElementById("manualAppTokensHash");
      if (hashInput && esnForTokens) {
        const r2 = await fetch("/api-sdk/get_bot_apptokens?esn=" + encodeURIComponent(esnForTokens));
        if (r2.ok) {
          const j = await r2.json();
          hashInput.value = j.hash || "";
        } else {
          hashInput.value = "";
        }
      }
    } catch (e) {
      // ignore
    }
    if (globalGuidInput) {
      globalGuidInput.value = globalGuid || "";
    }
    _setManualBotInfoStatus("Loaded.", false);
  } catch (e) {
    _setManualBotInfoStatus(`Error loading botSdkInfo: ${e}`, true);
  }
}

async function saveManualAppTokens() {
  const selVal = _getManualEsnSelectedValue();
  const esnInput = document.getElementById("manualEsn");
  const esn = selVal && selVal !== "__new__" ? _normalizeEsn(selVal) : _normalizeEsn(esnInput?.value);
  const hash = (document.getElementById("manualAppTokensHash")?.value || "").trim();
  if (!esn) {
    _setManualBotInfoStatus("ESN is required to save AppTokens.", true);
    return;
  }
  if (!hash) {
    _setManualBotInfoStatus("AppTokens hash is empty.", true);
    return;
  }
  try {
    _setManualBotInfoStatus("Saving AppTokens...", false);
    const body = new URLSearchParams();
    body.set("esn", esn);
    body.set("hash", hash);
    const resp = await fetch("/api-sdk/set_bot_apptokens", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });
    const txt = await resp.text();
    if (!resp.ok) {
      _setManualBotInfoStatus(`Save AppTokens failed: ${txt}`, true);
      return;
    }
    _setManualBotInfoStatus(txt || "Saved AppTokens.", false);
  } catch (e) {
    _setManualBotInfoStatus(`Save AppTokens error: ${e}`, true);
  }
}

async function saveManualBotInfo() {
  const selVal = _getManualEsnSelectedValue();
  const esn = selVal && selVal !== "__new__" ? _normalizeEsn(selVal) : _normalizeEsn(document.getElementById("manualEsn")?.value);
  const ip = _normalizeIp(document.getElementById("manualLanIp")?.value);
  const guid = (document.getElementById("manualGuid")?.value || "").trim();
  const globalGuid = (document.getElementById("manualGlobalGuid")?.value || "").trim();

  if (!esn || !ip) {
    _setManualBotInfoStatus("ESN and LAN IP are required.", true);
    return;
  }
  // GUID can be blank - backend can auto-generate it on save

  try {
    _setManualBotInfoStatus("Saving...", false);
    const body = new URLSearchParams();
    body.set("esn", esn);
    body.set("ip_address", ip);
    body.set("guid", guid);
    body.set("global_guid", globalGuid);
    if (!guid) {
      body.set("auto_generate_guid", "1");
    }

    const resp = await fetch("/api-sdk/set_bot_info", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });
    const txt = await resp.text();
    if (!resp.ok) {
      _setManualBotInfoStatus(`Save failed: ${txt}`, true);
      return;
    }
    _setManualBotInfoStatus(txt || "Saved.", false);
    // Reload to show generated GUID (if any)
    await loadManualBotInfo();
  } catch (e) {
    _setManualBotInfoStatus(`Error saving botSdkInfo: ${e}`, true);
  }
}

function beginBLESetup() {
  authEl.innerHTML = `
    <p>1. Place Vector on the charger.</p>
    <p>2. Double press the button. A key should appear on screen.</p>
    <p>3. Click 'Begin Scanning' and pair with your Vector.</p>
    <button onclick="scanRobots(false)">Begin Scanning</button>
  `;
}

function reInitBLE() {
  fetch("/api-ble/disconnect").then(() => fetch("/api-ble/init").catch(() => {
    showExternalSetupInstructions();
  })).catch(() => fetch("/api-ble/init"));
}

function scanRobots(returning) {
  const disconnectButtonDiv = document.getElementById("disconnectButton");
  disconnectButtonDiv.innerHTML = `
    <button onclick="disconnect()">Disconnect</button>
  `;
  updateAuthel("Scanning...");
  fetch("/api-ble/scan", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" } })
    .then((response) => response.json())
    .then((parsed) => {
      authEl.innerHTML = returning ? "<p>Incorrect PIN was entered, scanning again...</p>" : "";
      authEl.innerHTML += "<small>Scanning...</small>";

      const buttonsDiv = document.createElement("div");
      parsed.forEach((robot) => {
        const button = document.createElement("button");
        button.innerHTML = robot.name;
        button.onclick = () => connectRobot(robot.id);
        buttonsDiv.appendChild(button);
      });

      const rescanButton = document.createElement("button");
      rescanButton.innerHTML = "Re-scan";
      rescanButton.onclick = () => {
        updateAuthel("Reiniting BLE then scanning...");
        reInitBLE().then(() => scanRobots(false));
      };

      updateAuthel("Click on the robot you would like to pair with.");
      authEl.appendChild(rescanButton);
      authEl.appendChild(buttonsDiv);
    }).catch(() => {
      updateAuthel("Error scanning. Reiniting BLE then scanning...");
      reInitBLE().then(() => scanRobots(false));
    });
}

function disconnect() {
  authEl.innerHTML = "Disconnecting...";
  OTAUpdating = false;
  fetch("/api-ble/stop_ota").then(() => fetch("/api-ble/disconnect").then(() => checkBLECapability())).catch(() => checkBLECapability());
}

function connectRobot(id) {
  updateAuthel("Connecting to robot...");
  fetch(`/api-ble/connect?id=${id}`)
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("success")) {
        createPinEntry();
      } else {
        alert("Error connecting. WirePod will restart and this will return to the first screen of setup.");
        updateAuthel("Waiting for WirePod to restart...");
        setTimeout(checkBLECapability, 3000);
      }
    }).catch(() => {
      alert("Error connecting. WirePod will restart and this will return to the first screen of setup.");
      updateAuthel("Waiting for WirePod to restart...");
      setTimeout(checkBLECapability, 3000);
    })
}

function createPinEntry() {
  authEl.innerHTML = `
    <p>Enter the pin shown on Vector's screen.</p>
    <input type="text" id="pinEntry" placeholder="Enter PIN here" maxlength="6">
    <br>
    <button onclick="sendPin()">Send PIN</button>
  `;
}

function sendPin() {
  const pin = document.getElementById("pinEntry").value;
  updateAuthel("Sending PIN...");
  fetch(`/api-ble/send_pin?pin=${pin}`)
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("incorrect pin") || response.includes("length of pin")) {
        updateAuthel("Wrong PIN... Reiniting BLE then scanning...");
        reInitBLE().then(() => scanRobots(true));
      } else {
        wifiCheck();
      }
    }).catch(() => {
      updateAuthel("Error sending PIN... Reiniting BLE then scanning...");
      reInitBLE().then(() => scanRobots(true));
    });
}

function wifiCheck() {
  fetch("/api-ble/get_wifi_status")
    .then((response) => response.text())
    .then((response) => {
      if (response === "1") {
        whatToDo();
      } else {
        scanWifi();
      }
    }).catch(() => {
      updateAuthel("Error checking Wi-Fi status... Reiniting BLE then scanning...");
      reInitBLE().then(() => scanRobots(true));
    });
}

function scanWifi() {
  authEl.innerHTML = "Scanning for Wi-Fi networks...";
  fetch("/api-ble/scan_wifi")
    .then((response) => response.json())
    .then((networks) => {
      authEl.innerHTML = `
        <p>Select a Wi-Fi network to connect Vector to.</p>
        <button onclick="scanWifi()">Scan Again</button>
        <br>
        ${networks
          .map(
            (network) =>
              network.ssid && `<button onclick="createWiFiPassEntry('${network.ssid}', '${network.authtype}')">${network.ssid}</button>`
          )
          .join("")}
      `;
    }).catch(() => {
      updateAuthel("Error scanning Wi-Fi networks... Reiniting BLE then scanning...");
      reInitBLE().then(() => scanRobots(true));
    });
}

function createWiFiPassEntry(ssid, authtype) {
  authEl.innerHTML = `
    <button onclick="scanWifi()">Scan Again</button>
    <p>Enter the password for ${ssid}</p>
    <input type="text" id="passEntry" placeholder="Password">
    <br>
    <button onclick="connectWifi('${ssid}', '${authtype}')">Connect to Wi-Fi</button>
  `;
}

function connectWifi(ssid, authtype) {
  const password = document.getElementById("passEntry").value;
  authEl.innerHTML = "Connecting Vector to Wi-Fi...";
  fetch(`/api-ble/connect_wifi?ssid=${ssid}&password=${password}&authType=${authtype}`)
    .then((response) => response.text())
    .then((response) => {
      if (!response.includes("255")) {
        alert("Error connecting, likely incorrect password");
        createWiFiPassEntry(ssid, authtype);
      } else {
        whatToDo();
      }
    }).catch(() => {
      updateAuthel("Error connecting to Wi-Fi... Reiniting BLE then scanning...");
      reInitBLE().then(() => scanRobots(true));
    });
}

function checkFirmware() {
  fetch("/api-ble/get_firmware")
    .then((response) => response.text())
    .then((response) => {
      const splitFirmware = response.split("-");
      console.log(splitFirmware);
    }).catch(() => {
      updateAuthel("Error getting firmware version... Reiniting BLE then scanning...");
      reInitBLE().then(() => scanRobots(true));
    });
}

function whatToDo() {
  fetch("/api-ble/get_robot_status")
    .then((response) => response.text())
    .then((response) => {
      switch (response) {
        case "in_recovery_prod":
          doOTA("local");
          break;
        case "in_recovery_dev":
          doOTA("http://wpsetup.keriganc.com:81/1.6.0.3331.ota");
          break;
        case "in_firmware_nonep":
          showRecoveryInstructions();
          break;
        case "in_firmware_dev":
          showDevWarning();
          break;
        case "in_firmware_ep":
          showAuthButton();
          break;
      }
    });
}

function showRecoveryInstructions() {
  authEl.innerHTML = `
    <p>1. Place Vector on the charger.</p>
    <p>2. Hold the button for 15 seconds. He will turn off - keep holding it until he turns back on.</p>
    <p>3. Click 'Begin Scanning' and pair with your Vector.</p>
    <button onclick="scanRobots(false)">Begin Scanning</button>
  `;
  alert("Your bot is not on the correct firmware for wire-pod. Follow the directions to put him in recovery mode.");
}

function showDevWarning() {
  alert("Your bot is a dev robot. Make sure you have done the 'Configure an OSKR/dev-unlocked robot' section before authentication. If you did already, you can ignore this warning.");
  showAuthButton();
}

function showAuthButton() {
  authEl.innerHTML = `<button onclick="doAuth()">AUTHENTICATE</button>`;
}

function doOTA(url) {
  updateAuthel("Starting OTA update...");
  fetch(`/api-ble/start_ota?url=${url}`)
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("success")) {
        OTAUpdating = true;
        const interval = setInterval(() => {
          fetch("/api-ble/get_ota_status")
            .then((otaResponse) => otaResponse.text())
            .then((otaResponse) => {
              updateAuthel(otaResponse);
              if (otaResponse.includes("complete")) {
                alert("The OTA update is complete. When the bot reboots, follow the steps to re-pair the bot with wire-pod. wire-pod will then authenticate the robot and setup will be complete.");
                OTAUpdating = false;
                clearInterval(interval);
                checkBLECapability();
              } else if (otaResponse.includes("stopped") || !OTAUpdating) {
                clearInterval(interval);
              }
            });
        }, 2000);
      } else {
        whatToDo();
      }
    });
}

function updateAuthel(update) {
  authEl.innerHTML = `<p>${update}</p>`;
}

function doAuth() {
  updateAuthel("Authenticating your Vector...");
  fetch("/api-ble/do_auth")
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("error")) {
        showAuthError();
      } else {
        showWakeOptions();
      }
    });
}

function showAuthError() {
  updateAuthel("Authentication failure. Try again in ~15 seconds. If it happens again, check the troubleshooting guide:");
  const troubleshootingLink = document.createElement("a");
  troubleshootingLink.href = "https://github.com/kercre123/wire-pod/wiki/Troubleshooting#error-logging-in-the-bot-is-likely-unable-to-communicate-with-your-wire-pod-instance";
  troubleshootingLink.target = "_blank";
  troubleshootingLink.innerText = "https://github.com/kercre123/wire-pod/wiki/Troubleshooting";
  authEl.appendChild(document.createElement("br"));
  authEl.appendChild(troubleshootingLink);
}

function showWakeOptions() {
  updateAuthel("Authentication was successful! How would you like to wake Vector up?");
  authEl.innerHTML += `
    <button onclick="doOnboard(true)">Wake with wake-up animation (recommended)</button>
    <br>
    <button onclick="doOnboard(false)">Wake immediately, without wake-up animation</button>
  `;
}

function doOnboard(withAnim) {
  updateAuthel("Onboarding robot...");
  fetch(`/api-ble/onboard?with_anim=${withAnim}`).then(() => {
    fetch("/api-ble/disconnect");
    updateAuthel("Vector is now fully set up! Use the Bot Settings tab to further configure your bot.");
    const disconnectButtonDiv = document.getElementById("disconnectButton");
    disconnectButtonDiv.innerHTML = `
      <button onclick="checkBLECapability()">Return to pair instructions</button>
    `;
  });
}
