/**
 * Loads en-US + vi-VN intent JSON from the chipper API and renders the Intent Guide.
 */
(function () {
  let _loaded = false;
  let _loading = false;

  function escapeHtml(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function mergeByName(list) {
    const m = {};
    if (!Array.isArray(list)) return m;
    for (const row of list) {
      if (!row || !row.name) continue;
      if (!m[row.name]) {
        m[row.name] = {
          name: row.name,
          keyphrases: [],
          requiresexact: !!row.requiresexact,
        };
      }
      m[row.name].keyphrases.push(...(row.keyphrases || []));
      m[row.name].requiresexact = m[row.name].requiresexact || !!row.requiresexact;
    }
    return m;
  }

  function uniqueKeyphrases(kp) {
    const uniq = [];
    const seen = new Set();
    if (!kp || !kp.length) return uniq;
    for (const p of kp) {
      const t = String(p).trim();
      if (!t || seen.has(t)) continue;
      seen.add(t);
      uniq.push(t);
    }
    return uniq;
  }

  /** Mỗi keyphrase một dòng — dễ đọc trên nền tối. */
  function formatKeyphrasesList(kp) {
    const uniq = uniqueKeyphrases(kp);
    if (!uniq.length) return '<span class="ig-kp-empty">—</span>';
    return `<ul class="ig-kp-list">${uniq.map((t) => `<li>${escapeHtml(t)}</li>`).join("")}</ul>`;
  }

  /** Icon cho từng intent (Font Awesome 6 solid — tránh icon quá hiếm). */
  function iconForIntent(name) {
    const byName = {
      intent_play_blackjack: "fa-solid fa-diamond",
      intent_blackjack_hit: "fa-solid fa-plus",
      intent_blackjack_stand: "fa-solid fa-hand",
      intent_names_username_extend: "fa-solid fa-signature",
      intent_weather_extend: "fa-solid fa-cloud-sun",
      intent_names_ask: "fa-solid fa-circle-question",
      intent_imperative_eyecolor: "fa-solid fa-eye",
      intent_character_age: "fa-solid fa-cake-candles",
      intent_explore_start: "fa-solid fa-person-walking",
      intent_system_charger: "fa-solid fa-house",
      intent_system_sleep: "fa-solid fa-moon",
      intent_system_noaudio: "fa-solid fa-volume-xmark",
      intent_greeting_goodmorning: "fa-solid fa-sun",
      intent_greeting_goodnight: "fa-solid fa-moon",
      intent_greeting_goodbye: "fa-solid fa-door-open",
      intent_seasonal_happynewyear: "fa-solid fa-star",
      intent_seasonal_happyholidays: "fa-solid fa-gift",
      intent_amazon_signin: "fa-solid fa-microphone-lines",
      intent_imperative_forward: "fa-solid fa-arrow-up",
      intent_imperative_turnaround: "fa-solid fa-arrows-rotate",
      intent_imperative_turnleft: "fa-solid fa-arrow-left",
      intent_imperative_turnright: "fa-solid fa-arrow-right",
      intent_play_rollcube: "fa-solid fa-cube",
      intent_play_popawheelie: "fa-solid fa-angles-up",
      intent_play_fistbump: "fa-solid fa-hand-fist",
      intent_imperative_affirmative: "fa-solid fa-circle-check",
      intent_imperative_negative: "fa-solid fa-circle-xmark",
      intent_photo_take_extend: "fa-solid fa-camera",
      intent_imperative_praise: "fa-solid fa-thumbs-up",
      intent_imperative_abuse: "fa-solid fa-face-angry",
      intent_imperative_apologize: "fa-solid fa-hands-praying",
      intent_imperative_backup: "fa-solid fa-arrow-left-long",
      intent_imperative_volumedown: "fa-solid fa-volume-low",
      intent_imperative_volumeup: "fa-solid fa-volume-high",
      intent_imperative_lookatme: "fa-solid fa-eye",
      intent_imperative_volumelevel_extend: "fa-solid fa-sliders",
      intent_imperative_shutup: "fa-solid fa-hand",
      intent_greeting_hello: "fa-solid fa-hand-wave",
      intent_imperative_come: "fa-solid fa-person-walking-arrow-right",
      intent_imperative_love: "fa-solid fa-heart",
      intent_knowledge_promptquestion: "fa-solid fa-comments",
      intent_clock_checktimer: "fa-solid fa-stopwatch",
      intent_global_stop_extend: "fa-solid fa-ban",
      intent_clock_settimer_extend: "fa-solid fa-hourglass-start",
      intent_clock_time: "fa-solid fa-clock",
      intent_imperative_quiet: "fa-solid fa-volume-off",
      intent_imperative_dance: "fa-solid fa-music",
      intent_play_pickupcube: "fa-solid fa-box-open",
      intent_imperative_fetchcube: "fa-solid fa-arrow-right",
      intent_imperative_findcube: "fa-solid fa-magnifying-glass",
      intent_play_anytrick: "fa-solid fa-wand-magic-sparkles",
      intent_message_recordmessage_extend: "fa-solid fa-microphone",
      intent_message_playmessage_extend: "fa-solid fa-circle-play",
      intent_play_keepaway: "fa-solid fa-person-running",
    };
    if (byName[name]) return byName[name];
    if (name.includes("blackjack")) return "fa-solid fa-spade";
    if (name.includes("greeting") || name.includes("hello")) return "fa-solid fa-hand-wave";
    if (name.includes("weather")) return "fa-solid fa-cloud-sun";
    if (name.includes("clock") || name.includes("timer")) return "fa-solid fa-clock";
    if (name.includes("volume")) return "fa-solid fa-volume-high";
    if (name.includes("photo")) return "fa-solid fa-camera";
    if (name.includes("message")) return "fa-solid fa-envelope";
    if (name.includes("cube") || name.includes("pickup") || name.includes("fetch") || name.includes("findcube"))
      return "fa-solid fa-cube";
    if (name.includes("dance")) return "fa-solid fa-music";
    if (name.includes("names_")) return "fa-solid fa-id-card";
    if (name.includes("sleep")) return "fa-solid fa-moon";
    if (name.includes("charger")) return "fa-solid fa-house";
    if (name.includes("love") || name.includes("praise")) return "fa-solid fa-heart";
    if (name.includes("abuse")) return "fa-solid fa-face-meh";
    if (name.includes("affirmative")) return "fa-solid fa-circle-check";
    if (name.includes("negative")) return "fa-solid fa-circle-xmark";
    if (name.includes("explore")) return "fa-solid fa-person-walking";
    if (name.includes("imperative_forward") || name.includes("turn")) return "fa-solid fa-arrows-up-down-left-right";
    return "fa-solid fa-tag";
  }

  function pickMerged(map, name) {
    const row = map[name];
    if (!row) return { keyphrases: [], requiresexact: false };
    return row;
  }

  const TIME_GROUP = {
    icon: "fa-solid fa-clock",
    titleVi: "Mấy giờ, timer & báo thức",
    titleEn: "What time · set / check / cancel timers",
    intents: [
      "intent_clock_time",
      "intent_clock_checktimer",
      "intent_clock_settimer_extend",
      "intent_global_stop_extend",
    ],
  };

  const WEATHER_GROUP = {
    icon: "fa-solid fa-cloud-sun",
    titleVi: "Thời tiết & dự báo",
    titleEn: "Weather forecast",
    intents: ["intent_weather_extend"],
  };

  const SPOTLIGHT_GROUPS = [
    {
      icon: "fa-solid fa-cube",
      titleVi: "Chơi với khối & cube",
      titleEn: "Cube & block games",
      intents: [
        "intent_play_rollcube",
        "intent_play_popawheelie",
        "intent_play_fistbump",
        "intent_play_pickupcube",
        "intent_imperative_fetchcube",
        "intent_imperative_findcube",
        "intent_play_keepaway",
      ],
    },
    {
      icon: "fa-solid fa-house",
      titleVi: "Về nhà / đế sạc",
      titleEn: "Go home & charger",
      intents: ["intent_system_charger"],
    },
    {
      icon: "fa-solid fa-camera",
      titleVi: "Chụp ảnh",
      titleEn: "Take a photo",
      intents: ["intent_photo_take_extend"],
    },
    {
      icon: "fa-solid fa-id-card",
      titleVi: "Tên tôi là gì / tên của tôi",
      titleEn: "What’s my name / my name is…",
      intents: ["intent_names_ask", "intent_names_username_extend"],
    },
  ];

  /** Một nhóm spotlight — g.titleVi đã escapeHtml. */
  function renderSpotlightGroupHtml(g, enMap, viMap) {
    let html = `<div class="ig-spot-group">
        <div class="ig-spot-group-hd"><i class="${g.icon}" aria-hidden="true"></i>
          <div class="ig-spot-titles"><span class="ig-spot-title-main">${g.titleVi}</span><span class="ig-spot-title-sub">${escapeHtml(g.titleEn)}</span></div>
        </div>
        <div class="ig-spot-group-body">`;

    for (const iname of g.intents) {
      const en = pickMerged(enMap, iname);
      const vi = pickMerged(viMap, iname);
      const ic = iconForIntent(iname);
      html += `
          <div class="ig-spot-intent">
            <div class="ig-spot-intent-name"><i class="${ic}" aria-hidden="true"></i><code>${escapeHtml(iname)}</code></div>
            <div class="ig-spot-kp-col"><span class="ig-mini">en-US</span>${formatKeyphrasesList(en.keyphrases)}</div>
            <div class="ig-spot-kp-col"><span class="ig-mini">vi-VN</span>${formatKeyphrasesList(vi.keyphrases)}</div>
          </div>`;
    }

    html += `</div></div>`;
    return html;
  }

  function renderTimeSection(enMap, viMap) {
    const el = document.getElementById("intentGuideTime");
    if (!el) return;
    const intro =
      '<p class="ig-text-muted ig-details-intro"><i class="fa-solid fa-clock" aria-hidden="true"></i> Xem giờ, kiểm tra / đặt / hủy báo thức — mỗi intent một icon. / Time &amp; timer intents.</p>';
    el.innerHTML =
      intro +
      renderSpotlightGroupHtml(
        {
          icon: TIME_GROUP.icon,
          titleVi: escapeHtml(TIME_GROUP.titleVi),
          titleEn: TIME_GROUP.titleEn,
          intents: TIME_GROUP.intents,
        },
        enMap,
        viMap
      );
  }

  function renderWeatherSection(enMap, viMap) {
    const el = document.getElementById("intentGuideWeather");
    if (!el) return;
    const intro =
      '<p class="ig-text-muted ig-details-intro"><i class="fa-solid fa-cloud-sun" aria-hidden="true"></i> Hỏi thời tiết, dự báo, mưa nắng… / Weather phrases.</p>';
    el.innerHTML =
      intro +
      renderSpotlightGroupHtml(
        {
          icon: WEATHER_GROUP.icon,
          titleVi: escapeHtml(WEATHER_GROUP.titleVi),
          titleEn: WEATHER_GROUP.titleEn,
          intents: WEATHER_GROUP.intents,
        },
        enMap,
        viMap
      );
  }

  function renderSpotlight(enMap, viMap) {
    const el = document.getElementById("intentGuideSpotlight");
    if (!el) return;

    let html = `<p class="ig-text-muted ig-details-intro">Cube · về nhà · chụp ảnh · hỏi tên — mỗi câu một dòng. / Quick picks with icons.</p>`;

    for (const g of SPOTLIGHT_GROUPS) {
      html += renderSpotlightGroupHtml(
        {
          icon: g.icon,
          titleVi: escapeHtml(g.titleVi),
          titleEn: g.titleEn,
          intents: g.intents,
        },
        enMap,
        viMap
      );
    }

    el.innerHTML = html;
  }

  function renderBlackjack(enMap, viMap) {
    const el = document.getElementById("intentGuideBlackjack");
    if (!el) return;

    const cards = [
      {
        icon: "fa-solid fa-spade",
        title: "Bắt đầu ván / Start game",
        intent: "intent_play_blackjack",
        note:
          "Gọi trò chơi. / Say you want to play cards or blackjack.",
      },
      {
        icon: "fa-solid fa-plus",
        title: "Lấy thêm bài (Hit)",
        intent: "intent_blackjack_hit",
        extraIntents: ["intent_imperative_affirmative"],
        note:
          "<strong>intent_blackjack_hit</strong> = rút thêm bài. Trong lúc chơi, <strong>intent_imperative_affirmative</strong> (yes / có / ok…) cũng thường được dùng để đồng ý — tùy ngữ cảnh game. / <strong>Hit</strong> uses hit phrases; <strong>affirmative</strong> may also mean “yes” in context.",
      },
      {
        icon: "fa-solid fa-hand",
        title: "Không lấy thêm bài (Stand)",
        intent: "intent_blackjack_stand",
        extraIntents: ["intent_imperative_negative"],
        note:
          "<strong>intent_blackjack_stand</strong> = dừng rút (stand). <strong>intent_imperative_negative</strong> (no / không…) = không / từ chối — trong ván cũng có thể được hiểu là không lấy thêm bài. / <strong>Stand</strong> stops hitting; <strong>negative</strong> is “no”.",
      },
      {
        icon: "fa-solid fa-rotate",
        title: "Play another round? (Hỏi chơi tiếp)",
        intent: "_round_prompt",
        note:
          "Khi robot hỏi có chơi tiếp ván nữa không: <strong>intent_imperative_affirmative</strong> = muốn chơi tiếp / yes. <strong>intent_imperative_negative</strong> = không chơi nữa / no. / When asked for another round: <strong>affirmative</strong> = play again; <strong>negative</strong> = stop.",
      },
    ];

    let html = `
      <div class="ig-bj-hero">
        <span class="ig-bj-suits" aria-hidden="true"><i class="fa-solid fa-spade"></i><i class="fa-solid fa-heart" style="color:#e85d6c;"></i><i class="fa-solid fa-diamond" style="color:#6ab7ff;"></i><i class="fa-solid fa-star" style="opacity:.9;"></i></span>
        <h3 class="ig-bj-title">Blackjack / Xì dách — luồng lệnh</h3>
        <p class="ig-text-muted ig-bj-lead">Đầy đủ từ khóa lấy từ file JSON server (en-US + vi-VN). / Full keyphrases from server JSON files.</p>
      </div>`;

    for (const c of cards) {
      if (c.intent === "_round_prompt") {
        html += `
          <div class="ig-bj-card">
            <div class="ig-bj-card-head"><i class="${c.icon}" aria-hidden="true"></i><span>${escapeHtml(c.title)}</span></div>
            <p class="ig-bj-note ig-text-muted">${c.note}</p>
            <div class="ig-bj-kp-grid">
              <div><span class="ig-lang-label"><i class="fa-solid fa-circle-check" style="color:var(--fg-color);"></i> intent_imperative_affirmative</span>
                <div class="ig-kp-block"><span class="ig-mini">en-US</span><div>${formatKeyphrasesList(pickMerged(enMap, "intent_imperative_affirmative").keyphrases)}</div></div>
                <div class="ig-kp-block"><span class="ig-mini">vi-VN</span><div>${formatKeyphrasesList(pickMerged(viMap, "intent_imperative_affirmative").keyphrases)}</div></div>
              </div>
              <div><span class="ig-lang-label"><i class="fa-solid fa-circle-xmark" style="opacity:.85;"></i> intent_imperative_negative</span>
                <div class="ig-kp-block"><span class="ig-mini">en-US</span><div>${formatKeyphrasesList(pickMerged(enMap, "intent_imperative_negative").keyphrases)}</div></div>
                <div class="ig-kp-block"><span class="ig-mini">vi-VN</span><div>${formatKeyphrasesList(pickMerged(viMap, "intent_imperative_negative").keyphrases)}</div></div>
              </div>
            </div>
          </div>`;
        continue;
      }

      const en = pickMerged(enMap, c.intent);
      const vi = pickMerged(viMap, c.intent);
      const extraRows = (c.extraIntents || [])
        .map(
          (iname) => `
        <div class="ig-bj-extra">
          <span class="ig-code"><i class="fa-solid fa-link"></i> ${escapeHtml(iname)}</span>
          <div class="ig-kp-block"><span class="ig-mini">en-US</span><div>${formatKeyphrasesList(pickMerged(enMap, iname).keyphrases)}</div></div>
          <div class="ig-kp-block"><span class="ig-mini">vi-VN</span><div>${formatKeyphrasesList(pickMerged(viMap, iname).keyphrases)}</div></div>
        </div>`
        )
        .join("");

      html += `
        <div class="ig-bj-card">
          <div class="ig-bj-card-head"><i class="${c.icon}" aria-hidden="true"></i><span>${escapeHtml(c.title)}</span>
            <span class="ig-code">${escapeHtml(c.intent)}</span>
          </div>
          <p class="ig-bj-note ig-text-muted">${c.note}</p>
          <div class="ig-kp-block"><span class="ig-mini">en-US</span><div>${formatKeyphrasesList(en.keyphrases)}</div></div>
          <div class="ig-kp-block"><span class="ig-mini">vi-VN</span><div>${formatKeyphrasesList(vi.keyphrases)}</div></div>
          ${en.requiresexact || vi.requiresexact ? `<p class="ig-exact"><i class="fa-solid fa-lock"></i> requiresexact</p>` : ""}
          ${extraRows}
        </div>`;
    }

    el.innerHTML = html;
  }

  function renderFullTable(enMap, viMap) {
    const el = document.getElementById("intentGuideFull");
    if (!el) return;

    const names = new Set([...Object.keys(enMap), ...Object.keys(viMap)]);
    const sorted = Array.from(names).sort((a, b) => a.localeCompare(b));

    let rows = "";
    for (const name of sorted) {
      const en = pickMerged(enMap, name);
      const vi = pickMerged(viMap, name);
      const exact = en.requiresexact || vi.requiresexact;
      rows += `<tr>
        <td class="ig-td-icon"><i class="${iconForIntent(name)}" title="${escapeHtml(name)}" aria-hidden="true"></i></td>
        <td class="ig-td-name"><code>${escapeHtml(name)}</code>${exact ? ' <span class="ig-badge">exact</span>' : ""}</td>
        <td class="ig-td-kp"><div class="ig-kp-full">${formatKeyphrasesList(en.keyphrases)}</div></td>
        <td class="ig-td-kp"><div class="ig-kp-full">${formatKeyphrasesList(vi.keyphrases)}</div></td>
      </tr>`;
    }

    el.innerHTML = `
      <p class="ig-text-muted ig-details-intro">Hiển thị đầy đủ (cuộn trang chính). Mỗi intent một icon. / Full height — scroll the page. One icon per intent.</p>
      <div class="ig-table-wrap">
        <table class="ig-table">
          <thead>
            <tr>
              <th class="ig-th-icon" title="Icon"><i class="fa-solid fa-star" aria-hidden="true"></i></th>
              <th><i class="fa-solid fa-tag" aria-hidden="true"></i> Intent <small>(tên)</small></th>
              <th><i class="fa-solid fa-globe" aria-hidden="true"></i> en-US <small>(keyphrases)</small></th>
              <th><i class="fa-solid fa-language" aria-hidden="true"></i> vi-VN <small>(keyphrases)</small></th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
  }

  window.loadIntentReference = function loadIntentReference() {
    const status = document.getElementById("intentGuideStatus");
    if (_loaded || _loading) {
      if (_loaded && status) status.textContent = "";
      return;
    }
    _loading = true;
    if (status) {
      status.innerHTML =
        '<i class="fa-solid fa-spinner fa-spin"></i> Đang tải danh sách intent từ server… / Loading intents from server…';
    }

    fetch("/api/get_intent_reference")
      .then((r) => {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then((data) => {
        const enMap = mergeByName(data.enUS || []);
        const viMap = mergeByName(data.viVN || []);
        renderBlackjack(enMap, viMap);
        renderTimeSection(enMap, viMap);
        renderWeatherSection(enMap, viMap);
        renderSpotlight(enMap, viMap);
        renderFullTable(enMap, viMap);
        _loaded = true;
        if (status) status.textContent = "";
      })
      .catch((e) => {
        console.error(e);
        if (status) {
          status.innerHTML =
            '<span style="color:#e57373;">Không tải được /api/get_intent_reference. Hãy cập nhật chipper và khởi động lại. / Could not load intent list — rebuild & restart chipper.</span>';
        }
      })
      .finally(() => {
        _loading = false;
      });
  };

  function wireIntentGuideToolbar() {
    const root = document.getElementById("section-intentguide");
    if (!root) return;
    const expand = document.getElementById("igBtnExpandAll");
    const collapse = document.getElementById("igBtnCollapseAll");
    const setAll = function (open) {
      root.querySelectorAll("details.ig-details").forEach(function (d) {
        d.open = open;
      });
    };
    if (expand) expand.onclick = function () { setAll(true); };
    if (collapse) collapse.onclick = function () { setAll(false); };
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wireIntentGuideToolbar);
  } else {
    wireIntentGuideToolbar();
  }
})();

window.igExpandAll = function (open) {
  const root = document.getElementById("section-intentguide");
  if (!root) return;
  root.querySelectorAll("details.ig-details").forEach(function (d) {
    d.open = !!open;
  });
};
