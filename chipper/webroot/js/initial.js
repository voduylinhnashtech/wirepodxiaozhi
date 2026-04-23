function checkLanguage() {
  fetch("/api/get_stt_info")
    .then((response) => response.json())
    .then((parsed) => {
      const sectionLanguage = document.getElementById("section-language");
      const languageSelection = document.getElementById("languageSelection");
      if (!sectionLanguage || !languageSelection) return;

      if (parsed.provider !== "vosk" && parsed.provider !== "whisper.cpp") {
        sectionLanguage.style.display = "none";
        languageSelection.value = "en-US";
      } else {
        sectionLanguage.style.display = "block";
        languageSelection.value = "en-US";
      }
    });
}

function updateSetupStatus(statusString) {
  const setupStatus = document.getElementById("setup-status");
  if (setupStatus) setupStatus.innerHTML = `<p>${statusString}</p>`;
}

// Vosk/whisper: set language (may download model) then set EP/IP.
// Xiaozhi & others: bỏ bước set_stt — gọi thẳng use_ep / use_ip (server tạo server_config).
function sendSetupInfo() {
  const configOptions = document.getElementById("config-options");
  if (configOptions) configOptions.style.display = "none";
  updateSetupStatus("Đang bắt đầu thiết lập...");

  fetch("/api/get_stt_info")
    .then((response) => response.json())
    .then((parsed) => {
      if (parsed.provider === "vosk" || parsed.provider === "whisper.cpp") {
        return sendSetupInfoVoskWhisper();
      }
      return Promise.resolve("skip_stt");
    })
    .then((step) => {
      if (step === "skip_stt" || step === "done_stt") {
        setConn();
      }
    })
    .catch(() => {
      if (configOptions) configOptions.style.display = "block";
      updateSetupStatus("Lỗi mạng hoặc API. Thử lại.");
    });
}

function sendSetupInfoVoskWhisper() {
  const language = document.getElementById("languageSelection").value;
  const langData = { language };
  const languageSelectionDiv = document.getElementById("languageSelectionDiv");
  if (languageSelectionDiv) languageSelectionDiv.style.display = "none";

  return fetch("/api/set_stt_info", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(langData),
  })
    .then((response) => response.text())
    .then((response) => {
      if (response.includes("success")) {
        updateSetupStatus("Đã đặt ngôn ngữ STT.");
        return "done_stt";
      }
      if (response.includes("downloading")) {
        updateSetupStatus("Đang tải mô hình ngôn ngữ...");
        return new Promise((resolve) => {
          const interval = setInterval(() => {
            fetch("/api/get_download_status")
              .then((r) => r.text())
              .then((statusText) => {
                updateSetupStatus(statusText);
                if (statusText.includes("success")) {
                  updateSetupStatus("Đã đặt ngôn ngữ STT.");
                  clearInterval(interval);
                  resolve("done_stt");
                } else if (statusText.includes("error")) {
                  const configOptions = document.getElementById("config-options");
                  if (configOptions) configOptions.style.display = "block";
                  clearInterval(interval);
                  resolve("error");
                } else if (statusText.includes("not downloading")) {
                  updateSetupStatus("Đang bắt đầu tải mô hình...");
                }
              });
          }, 500);
        });
      }
      if (response.includes("vosk")) {
        return "done_stt";
      }
      if (response.includes("error")) {
        updateSetupStatus(response);
        const configOptions = document.getElementById("config-options");
        if (configOptions) configOptions.style.display = "block";
        return "error";
      }
      return "done_stt";
    });
}

function checkConn() {
  const connValue = document.getElementById("connSelection").value;
  const portViz = document.getElementById("portViz");
  if (portViz) portViz.style.display = connValue === "ip" ? "block" : "none";
}

function setConn() {
  updateSetupStatus("Đang đặt Escape Pod hoặc IP...");
  const connValue = document.getElementById("connSelection").value;
  let port = document.getElementById("portInput").value;
  port = port ? port : "443";
  const url = connValue === "ep" ? "/api-chipper/use_ep" : `/api-chipper/use_ip?port=${port}`;

  fetch(url)
    .then((response) => response.text())
    .then((response) => {
      if (response) {
        updateSetupStatus("Xong! Đang chuyển về trang chính...");
        setTimeout(() => (window.location.href = "/"), 3000);
      } else {
        updateSetupStatus("Lỗi thiết lập; xem log wire-pod.");
        const configOptions = document.getElementById("config-options");
        if (configOptions) configOptions.style.display = "block";
      }
    });
}

function directToIndex() {
  window.location.href = "/index.html";
}
