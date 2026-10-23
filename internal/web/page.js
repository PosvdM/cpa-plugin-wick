(function () {
  "use strict";

  var API = "/v0/management/wick";
  var CONFIG_API = "/v0/management/plugins/wick/config";
  var KEY_STORE = "wick-management-key";
  // The duration presets and the max_pings each one saves.
  var PRESETS = ["0", "2", "6", "12"];
  // The prompt cache keeps an entry for 300 seconds after the last read.
  var CACHE_TTL = 300;
  var EVENTS_SHOWN = 20;
  var HISTORY_STEP = 100;

  var key = "";
  var status = null;
  var rawConfig = {};
  // The plugin's clock minus the browser's, so countdowns match the plugin.
  var clockOffset = 0;
  var customMode = false;
  var historyRange = "24h";
  var historyLimit = EVENTS_SHOWN;
  var historyData = null;
  // last_ping of the status the history was loaded for.
  var lastPingSeen = null;
  var refreshTimer = null;
  var statusTimer = null;

  function $(id) { return document.getElementById(id); }

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (name) {
        var value = attrs[name];
        if (value === undefined || value === null || value === false) return;
        if (name === "text") node.textContent = value;
        else if (name === "class") node.className = value;
        else if (name.indexOf("on") === 0) node.addEventListener(name.slice(2), value);
        else node.setAttribute(name, value === true ? "" : value);
      });
    }
    (children || []).forEach(function (child) {
      if (child === null || child === undefined) return;
      node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
    });
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  // The page runs in a same-origin iframe of the Management Center, so it
  // reads the theme from the parent document: data-theme is "dark", "white"
  // or absent for the default light theme. Opened on its own, it falls back
  // to the theme the Management Center stored in localStorage.
  function parentRoot() {
    try {
      if (window.parent !== window) return window.parent.document.documentElement;
    } catch (e) { /* cross-origin */ }
    return null;
  }

  function applyTheme() {
    var theme = "light";
    var root = parentRoot();
    if (root) {
      var value = root.getAttribute("data-theme");
      if (value === "dark" || value === "white") theme = value;
    } else {
      var stored = "auto";
      try {
        var parsed = JSON.parse(localStorage.getItem("cli-proxy-theme") || "null");
        stored = (parsed && parsed.state && parsed.state.theme) || (parsed && parsed.theme) || stored;
      } catch (e) { /* use auto */ }
      if (stored === "dark" || stored === "white") theme = stored;
      if (stored === "auto" && window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches) theme = "dark";
    }
    if (theme === "light") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", theme);
  }

  function watchParent() {
    var root = parentRoot();
    if (!root || !window.MutationObserver) return;
    new MutationObserver(function () { applyTheme(); applyLanguage(); })
      .observe(root, { attributes: true, attributeFilter: ["data-theme", "lang"] });
  }

  // ---- Language ----

  // Every visible text, per language. Keys are used by data-i18n attributes
  // in page.html and by translate() below; {0}, {1} are arguments.
  var I18N = {
    zh: {
      app_sub: "挑灯", repo_hint: "在 GitHub 上查看",
      key_title: "需要管理密钥",
      key_hint: "在管理中心登录时勾选“记住密码”，此页面会直接使用该密钥；否则请在这里输入，密钥只保存在当前标签页。",
      key_placeholder: "CPA 管理密钥", key_submit: "确定", key_invalid: "管理密钥无效，请重新输入。",
      state_running: "运行中", state_disabled: "插件未启用", state_off: "挑灯已关闭",
      config_error: "配置无效，插件使用默认值：{0}",
      settings: "设置",
      save_hint: "修改后自动保存到 CPA 的 config.yaml，插件几秒内应用。",
      duration: "默认挑灯时长",
      duration_off: "关闭", duration_15: "15 分钟", duration_30: "30 分钟", duration_60: "1 小时", duration_custom: "自定义",
      max_pings: "最多挑灯", times: "次",
      duration_hint: "对话空闲后每隔 {0}到 {1}挑灯一次，最多 {2} 次，缓存约保持 {3} 分钟。单个对话可在下方另设时长。",
      duration_hint_off: "挑灯已关闭，对话空闲 5 分钟后缓存过期。",
      advanced: "高级设置", advanced_scope: "对所有对话生效",
      interval_min: "最短间隔（秒）", interval_max: "最长间隔（秒）", min_body_kb: "最小请求体（KiB）", history_retention_days: "记录保留（天）",
      advanced_hint: "每次挑灯的时刻在最短和最长间隔之间随机选取，最长间隔不超过 299 秒。请求体小于最小请求体的请求不挑灯，如生成标题等辅助请求。",
      interval_invalid: "间隔须满足 1 ≤ 最短间隔 ≤ 最长间隔 ≤ 299",
      number_invalid: "请填写不小于 0 的整数",
      retention_invalid: "记录保留天数须在 1 到 3650 之间",
      saved: "已保存", save_failed: "保存失败：{0}", load_failed: "加载失败：{0}",
      conversations: "正在挑灯的对话",
      col_conversation: "对话", col_model: "模型", col_model_account: "模型 / 账号", col_context: "缓存", col_pings: "已挑灯",
      col_idle: "空闲", col_next: "下次挑灯", col_duration: "时长",
      no_conversations: "没有正在挑灯的对话。Claude Code 发出请求后，对话显示在这里。",
      subagent: "子代理", no_prompt: "（没有文字）",
      session_hint: "会话 {0}", key_hint_short: "对话键 {0}", directory_hint: "目录 {0}",
      context_pending: "首次挑灯后显示",
      pinging: "挑灯中",
      duration_default: "默认", duration_hours: "{0} 小时", duration_custom_option: "自定义…",
      duration_until: "到 {0}", duration_input: "小时数",
      duration_set: "已设置，挑灯到 {0}", duration_reset: "已恢复默认时长", duration_failed: "设置失败：{0}",
      duration_invalid: "请填写 0 到 24 之间的小时数",
      stop: "熄灭", stopped_one: "已熄灭", stop_failed: "熄灭失败：{0}",
      events: "挑灯记录", range: "范围", range_24h: "24 小时", range_7d: "7 天", range_all: "全部",
      totals_24h: "近 24 小时共挑灯 {0} 次，失败 {1} 次，读取缓存 {2} token，写入缓存 {3} token。",
      totals_7d: "近 7 天共挑灯 {0} 次，失败 {1} 次，读取缓存 {2} token，写入缓存 {3} token。",
      totals_all: "保留的 {4} 天内共挑灯 {0} 次，失败 {1} 次，读取缓存 {2} token，写入缓存 {3} token。",
      col_time: "时间", col_ping: "次数", col_read: "缓存读取", col_write: "缓存写入", col_result: "结果",
      no_events: "所选范围内没有挑灯记录。",
      history_hint: "记录和正在挑灯的对话保存在 {0}，记录保留 {1} 天。",
      result_hit: "命中", result_miss: "未命中", result_error: "失败",
      stop_max_pings: "达到次数上限，已熄灭",
      stop_until: "到达单独设置的时长，已熄灭",
      stop_error: "已熄灭",
      stop_cache_miss: "缓存已过期，已熄灭",
      events_more: "再显示 {0} 条",
      seconds: "{0} 秒", minutes_seconds: "{0} 分 {1} 秒", minutes: "{0} 分钟", hours_minutes: "{0} 小时 {1} 分"
    },
    en: {
      app_sub: "", repo_hint: "View on GitHub",
      key_title: "Management key required",
      key_hint: "If you checked \"Remember password\" when signing in to the Management Center, this page uses that key. Otherwise enter it here; it is kept in this tab only.",
      key_placeholder: "CPA management key", key_submit: "OK", key_invalid: "Invalid management key. Enter it again.",
      state_running: "Running", state_disabled: "Plugin disabled", state_off: "Wick off",
      config_error: "Invalid config, the plugin uses the defaults: {0}",
      settings: "Settings",
      save_hint: "Changes are saved to CPA's config.yaml and applied by the plugin within a few seconds.",
      duration: "Default time kept lit",
      duration_off: "Off", duration_15: "15 min", duration_30: "30 min", duration_60: "1 hour", duration_custom: "Custom",
      max_pings: "Up to", times: "pings",
      duration_hint: "While a conversation is idle, Wick pings every {0} to {1}, up to {2} times, keeping the cache for about {3} minutes. A conversation can be kept lit longer below.",
      duration_hint_off: "Wick is off. The cache expires after 5 idle minutes.",
      advanced: "Advanced", advanced_scope: "Applies to every conversation",
      interval_min: "Shortest interval (s)", interval_max: "Longest interval (s)", min_body_kb: "Minimum body (KiB)", history_retention_days: "Keep history (days)",
      advanced_hint: "Each ping is sent at a random time between the shortest and longest interval; the longest is at most 299 seconds. Requests with a smaller body than the minimum, such as title generation, are not kept lit.",
      interval_invalid: "Intervals must satisfy 1 ≤ shortest ≤ longest ≤ 299",
      number_invalid: "Enter a whole number of at least 0",
      retention_invalid: "Keep history for 1 to 3650 days",
      saved: "Saved", save_failed: "Save failed: {0}", load_failed: "Load failed: {0}",
      conversations: "Conversations kept lit",
      col_conversation: "Conversation", col_model: "Model", col_model_account: "Model / Account", col_context: "Cache", col_pings: "Pings",
      col_idle: "Idle", col_next: "Next ping", col_duration: "Keep lit",
      no_conversations: "No conversations are kept lit. Conversations appear here after Claude Code sends a request.",
      subagent: "Subagent", no_prompt: "(no text)",
      session_hint: "Session {0}", key_hint_short: "Conversation key {0}", directory_hint: "Directory {0}",
      context_pending: "Shown after the first ping",
      pinging: "Pinging",
      duration_default: "Default", duration_hours: "{0} h", duration_custom_option: "Custom…",
      duration_until: "Until {0}", duration_input: "Hours",
      duration_set: "Kept lit until {0}", duration_reset: "Back to the default time", duration_failed: "Could not set: {0}",
      duration_invalid: "Enter a number of hours from 0 to 24",
      stop: "Snuff out", stopped_one: "Snuffed out", stop_failed: "Could not snuff out: {0}",
      events: "Ping history", range: "Range", range_24h: "24 hours", range_7d: "7 days", range_all: "All",
      totals_24h: "Last 24 hours: {0} pings, {1} failed, {2} cached tokens read, {3} written.",
      totals_7d: "Last 7 days: {0} pings, {1} failed, {2} cached tokens read, {3} written.",
      totals_all: "All {4} days kept: {0} pings, {1} failed, {2} cached tokens read, {3} written.",
      col_time: "Time", col_ping: "Ping", col_read: "Cache read", col_write: "Cache write", col_result: "Result",
      no_events: "No pings in this range.",
      history_hint: "The history and the conversations kept lit are saved in {0}; the history is kept for {1} days.",
      result_hit: "Hit", result_miss: "Miss", result_error: "Failed",
      stop_max_pings: "Reached the ping limit, snuffed out",
      stop_until: "Reached the time set for this conversation, snuffed out",
      stop_error: "Snuffed out",
      stop_cache_miss: "Cache had expired, snuffed out",
      events_more: "Show {0} more",
      seconds: "{0} s", minutes_seconds: "{0} min {1} s", minutes: "{0} min", hours_minutes: "{0} h {1} min"
    }
  };

  var lang = "zh";

  function translate(name) {
    var text = (I18N[lang] && I18N[lang][name] !== undefined) ? I18N[lang][name] : (I18N.zh[name] !== undefined ? I18N.zh[name] : name);
    for (var i = 1; i < arguments.length; i++) text = text.split("{" + (i - 1) + "}").join(String(arguments[i]));
    return text;
  }

  // The language follows the Management Center: the lang attribute of the
  // parent page, which it updates on every switch, or the language it stored
  // when the page is opened on its own. Chinese variants show Chinese and
  // every other language English.
  function centerLanguage() {
    var root = parentRoot();
    var value = root ? root.getAttribute("lang") || "" : "";
    if (!value) {
      try {
        var raw = localStorage.getItem("cli-proxy-language") || "";
        try {
          var parsed = JSON.parse(raw);
          value = (parsed && parsed.state && parsed.state.language) || (parsed && parsed.language) || (typeof parsed === "string" ? parsed : "");
        } catch (e) { value = raw; }
      } catch (e) { /* no storage */ }
    }
    if (!value) value = navigator.language || "";
    return /^zh/i.test(value) ? "zh" : "en";
  }

  function applyStaticText() {
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
    [["data-i18n", null], ["data-i18n-placeholder", "placeholder"], ["data-i18n-title", "title"], ["data-i18n-aria-label", "aria-label"]].forEach(function (pair) {
      Array.prototype.forEach.call(document.querySelectorAll("[" + pair[0] + "]"), function (node) {
        var text = translate(node.getAttribute(pair[0]));
        if (pair[1]) node.setAttribute(pair[1], text);
        else node.textContent = text;
      });
    });
  }

  var languageApplied = false;
  function applyLanguage() {
    var next = centerLanguage();
    if (languageApplied && next === lang) return;
    lang = next;
    languageApplied = true;
    applyStaticText();
    if (status) {
      renderStatus();
      renderDurationHint();
    }
  }

  // ---- Management API ----

  // The Management Center keeps the key in localStorage, XOR-obfuscated with
  // a key derived from the host and user agent, when "remember password" is on.
  function centerKey() {
    try {
      var raw = localStorage.getItem("cli-proxy-auth");
      if (!raw) return "";
      var prefix = "enc::v1::";
      if (raw.indexOf(prefix) === 0) {
        var data = atob(raw.slice(prefix.length));
        var mask = new TextEncoder().encode("cli-proxy-api-webui::secure-storage|" + location.host + "|" + navigator.userAgent);
        var bytes = new Uint8Array(data.length);
        for (var i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) ^ mask[i % mask.length];
        raw = new TextDecoder().decode(bytes);
      }
      var parsed = JSON.parse(raw);
      var stored = parsed && parsed.state ? parsed.state : parsed;
      return stored && typeof stored.managementKey === "string" ? stored.managementKey : "";
    } catch (e) {
      return "";
    }
  }

  function api(method, path, body) {
    var opts = { method: method, headers: { "Authorization": "Bearer " + key, "Accept": "application/json" } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (res.status === 401) {
          askForKey(translate("key_invalid"));
          throw new Error("unauthorized");
        }
        if (!res.ok) throw new Error(data.message || data.error || ("HTTP " + res.status));
        return data;
      });
    });
  }

  function askForKey(message) {
    key = "";
    sessionStorage.removeItem(KEY_STORE);
    clearInterval(refreshTimer);
    clearTimeout(statusTimer);
    $("app").hidden = true;
    $("key-section").hidden = false;
    $("key-error").hidden = !message;
    $("key-error").textContent = message || "";
  }

  var toastTimer = null;
  function toast(message) {
    var node = $("toast");
    node.textContent = message;
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { node.hidden = true; }, 5000);
  }

  // ---- Formatting ----

  function parseTime(value) {
    if (!value) return null;
    var t = new Date(value);
    if (isNaN(t.getTime()) || t.getUTCFullYear() < 2000) return null;
    return t;
  }

  function now() { return Date.now() + clockOffset; }

  function pad(n) { return (n < 10 ? "0" : "") + n; }

  // Times are shown in the browser's time zone, with the date when it is
  // not today.
  function fmtTime(t) {
    var text = pad(t.getHours()) + ":" + pad(t.getMinutes()) + ":" + pad(t.getSeconds());
    var today = new Date();
    if (t.toDateString() !== today.toDateString()) text = pad(t.getMonth() + 1) + "/" + pad(t.getDate()) + " " + text;
    return text;
  }

  function fmtSeconds(total) {
    total = Math.max(0, Math.round(total));
    var minutes = Math.floor(total / 60), seconds = total % 60;
    if (minutes >= 60) return translate("hours_minutes", Math.floor(minutes / 60), minutes % 60);
    if (minutes === 0) return translate("seconds", seconds);
    if (seconds === 0) return translate("minutes", minutes);
    return translate("minutes_seconds", minutes, seconds);
  }

  // A time of day without seconds, with the date when it is not today.
  function fmtClock(t) {
    var text = pad(t.getHours()) + ":" + pad(t.getMinutes());
    if (t.toDateString() !== new Date().toDateString()) text = pad(t.getMonth() + 1) + "/" + pad(t.getDate()) + " " + text;
    return text;
  }

  // A length of time as m:ss, or h:mm:ss from an hour on, for the idle time
  // and the countdown.
  function fmtClockSpan(total) {
    total = Math.max(0, total);
    var h = Math.floor(total / 3600), m = Math.floor(total / 60) % 60, s = total % 60;
    return h ? h + ":" + pad(m) + ":" + pad(s) : m + ":" + pad(s);
  }

  function fmtCountdown(ms) {
    return fmtClockSpan(Math.ceil(ms / 1000));
  }

  function fmtNum(n) {
    return Number(n || 0).toLocaleString(lang === "zh" ? "zh-CN" : "en-US");
  }

  function badge(kind, text) {
    return el("span", { class: "badge badge-" + kind }, [el("span", { class: "badge-dot" }), document.createTextNode(text)]);
  }

  // ---- Status ----

  function renderHeader() {
    var state = $("state");
    clear(state);
    if (!status.enabled) state.appendChild(badge("muted", translate("state_disabled")));
    else if (!status.config || !status.config.max_pings) state.appendChild(badge("muted", translate("state_off")));
    else state.appendChild(badge("success", translate("state_running")));
    $("meta").textContent = "v" + status.version;
  }

  function renderNotices() {
    var box = $("notices");
    clear(box);
    if (status.config_error) {
      box.appendChild(el("div", { class: "notice" }, [
        el("span", { class: "notice-icon", text: "!" }),
        el("span", { text: translate("config_error", status.config_error) })
      ]));
    }
  }

  // The idle time and countdown cells carry their times and are updated
  // every second by tick() without rebuilding the table.
  function tick() {
    var t = now();
    Array.prototype.forEach.call(document.querySelectorAll("[data-since]"), function (node) {
      node.textContent = fmtClockSpan(Math.floor((t - Number(node.getAttribute("data-since"))) / 1000));
    });
    Array.prototype.forEach.call(document.querySelectorAll("[data-next]"), function (node) {
      var left = Number(node.getAttribute("data-next")) - t;
      node.textContent = left > 0 ? fmtCountdown(left) : translate("pinging");
    });
  }

  // Names of the Claude Code entrypoints in the User-Agent; others are shown
  // as sent.
  var CLIENTS = {
    "cli": "Claude Code", "claude-desktop": "Claude Desktop", "claude-desktop-3p": "Claude Desktop",
    "claude-vscode": "VS Code", "claude-code-jetbrains-plugin": "JetBrains", "claude-jetbrains": "JetBrains",
    "sdk-ts": "Agent SDK", "sdk-py": "Agent SDK", "sdk-cli": "Agent SDK"
  };

  function clientName(c) {
    if (!c.client) return "Claude Code";
    return CLIENTS[c.client] || c.client;
  }

  // A conversation is shown by its client and first prompt; the full prompt,
  // directory and IDs are in the tooltip.
  function conversationCell(c) {
    var tip = [];
    if (c.prompt) tip.push(c.prompt);
    if (c.cwd) tip.push(translate("directory_hint", c.cwd));
    tip.push(translate("session_hint", c.session));
    if (c.key) tip.push(translate("key_hint_short", c.key));
    return el("td", { class: "conv", title: tip.join("\n") }, [
      el("div", { class: "conv-title" }, [
        el("span", { class: "conv-client", text: clientName(c) }),
        c.agent ? el("span", { class: "tag", text: translate("subagent") }) : null
      ]),
      el("div", { class: "cell-note conv-prompt", text: c.prompt || translate("no_prompt") })
    ]);
  }

  var DURATION_PRESETS = [1, 2, 4, 8];
  // A refresh leaves the conversation table alone for this long after the
  // user last touched a duration menu, so that an open menu is not closed.
  var MENU_HOLD = 15000;
  var menuTouched = 0;
  // The server returns at most this many history entries.
  var HISTORY_MAX = 5000;

  // The duration of one conversation: a menu of presets, with a number field
  // for other values.
  function durationCell(c) {
    var cell = el("td", { class: "duration" });
    var select = el("select", { class: "duration-select", "aria-label": translate("col_duration") });
    function option(value, text) { select.appendChild(el("option", { value: value, text: text })); }
    option("0", translate("duration_default"));
    DURATION_PRESETS.forEach(function (h) { option(String(h), translate("duration_hours", h)); });
    if (c.hours && DURATION_PRESETS.indexOf(c.hours) < 0) option(String(c.hours), translate("duration_hours", c.hours));
    option("custom", translate("duration_custom_option"));
    var initial = String(c.hours || 0);
    select.value = initial;
    // Arrow keys on a closed menu change the value one step at a time, so a
    // change made with the keyboard is committed on Enter or when the menu
    // loses focus; a choice from the open menu is committed at once.
    var keyboard = false;
    function commit() {
      keyboard = false;
      if (select.value === initial) return;
      if (select.value === "custom") showDurationInput(cell, c);
      else setDuration(select, c.key, Number(select.value));
    }
    function touch() { menuTouched = Date.now(); }
    ["focus", "mousedown"].forEach(function (name) { select.addEventListener(name, touch); });
    select.addEventListener("keydown", function (e) {
      touch();
      if (e.key === "Enter") { e.preventDefault(); commit(); }
      else if (/^(Arrow|Page|Home|End)/.test(e.key)) keyboard = true;
    });
    select.addEventListener("change", function () {
      touch();
      if (!keyboard) commit();
    });
    select.addEventListener("blur", function () {
      menuTouched = 0;
      if (keyboard) commit();
    });
    cell.appendChild(select);
    var until = parseTime(c.until);
    if (until) cell.appendChild(el("div", { class: "cell-note", text: translate("duration_until", fmtClock(until)) }));
    return cell;
  }

  function showDurationInput(cell, c) {
    clear(cell);
    var input = el("input", { type: "number", min: "0", max: "24", step: "0.5", class: "duration-input", placeholder: translate("duration_input"), "aria-label": translate("duration_input") });
    if (c.hours) input.value = c.hours;
    var done = false;
    function finish(commit) {
      if (done) return;
      done = true;
      var text = input.value.trim();
      if (!commit || text === "") { input.blur(); renderConversations(); return; }
      var hours = Number(text);
      if (!isFinite(hours) || hours < 0 || hours > 24) {
        toast(translate("duration_invalid"));
        input.blur();
        renderConversations();
        return;
      }
      setDuration(input, c.key, hours);
    }
    input.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      else if (e.key === "Escape") { e.preventDefault(); finish(false); }
    });
    input.addEventListener("blur", function () { finish(true); });
    cell.appendChild(input);
    input.focus();
  }

  function setDuration(control, convKey, hours) {
    control.disabled = true;
    api("POST", API + "/duration", { key: convKey, hours: hours }).then(function (data) {
      control.blur();
      showStatus(data);
      var c = (data.conversations || []).filter(function (x) { return x.key === convKey; })[0];
      var until = c ? parseTime(c.until) : null;
      toast(until ? translate("duration_set", fmtClock(until)) : translate("duration_reset"));
    }).catch(function (err) {
      control.disabled = false;
      control.blur();
      if (err.message !== "unauthorized") toast(translate("duration_failed", err.message));
      loadStatus().catch(function () {});
    });
  }

  function renderConversations() {
    var convs = status.conversations || [];
    var maxPings = status.config ? status.config.max_pings : 0;
    var body = $("conversations");
    $("conversation-count").textContent = convs.length ? String(convs.length) : "";
    // Rebuilding the table would close a menu or field the user is editing.
    var active = document.activeElement;
    var editing = active && body.contains(active) &&
      (active.tagName === "INPUT" || (active.tagName === "SELECT" && Date.now() - menuTouched < MENU_HOLD));
    if (editing) {
      tick();
      return;
    }
    clear(body);
    $("conversations-empty").hidden = convs.length > 0;
    body.parentNode.hidden = convs.length === 0;
    convs.forEach(function (c) {
      var last = parseTime(c.last_request), next = parseTime(c.next_ping);
      var usage = c.last_usage;
      body.appendChild(el("tr", null, [
        conversationCell(c),
        el("td", { title: c.auth_id }, [
          el("div", { text: c.model }),
          el("div", { class: "cell-note truncate", text: c.auth_id })
        ]),
        usage
          ? el("td", { class: "num", text: fmtNum(usage.cache_read_input_tokens) })
          : el("td", { class: "num muted", title: translate("context_pending"), text: "—" }),
        el("td", { class: "num", text: c.until ? String(c.pings) : c.pings + " / " + maxPings }),
        el("td", { class: "mono", "data-since": last ? last.getTime() : null, text: "—" }),
        el("td", { class: "mono", "data-next": next ? next.getTime() : null, text: "—" }),
        durationCell(c),
        el("td", { class: "actions" }, [
          el("button", { class: "btn btn-ghost btn-sm", type: "button", text: translate("stop"), onclick: function (e) { stopConversation(e.currentTarget, c.key); } })
        ])
      ]));
    });
    tick();
  }

  function eventResult(ev) {
    var parts = [];
    if (ev.error) parts.push(badge("failure", translate("result_error")));
    else if (ev.usage && ev.usage.cache_creation_input_tokens > 0) parts.push(badge("warning", translate("result_miss")));
    else parts.push(badge("success", translate("result_hit")));
    var notes = [];
    if (ev.error) notes.push(ev.error);
    if (ev.stopped) notes.push(translate("stop_" + ev.stopped));
    if (notes.length) parts.push(el("span", { class: "result-note", text: notes.join(lang === "zh" ? "；" : "; ") }));
    return parts;
  }

  function renderEvents() {
    var data = historyData;
    if (!data) return;
    var retention = status && status.config ? status.config.history_retention_days : "";
    var totals = data.totals || {};
    $("totals").textContent = translate("totals_" + data.range, fmtNum(totals.pings), fmtNum(totals.failed), fmtNum(totals.cache_read_tokens), fmtNum(totals.cache_creation_tokens), retention);
    var events = data.events || [];
    var body = $("events");
    clear(body);
    $("events-empty").hidden = events.length > 0;
    body.parentNode.hidden = events.length === 0;
    events.forEach(function (ev) {
      var t = parseTime(ev.time);
      var usage = ev.usage || {};
      body.appendChild(el("tr", null, [
        el("td", { class: "mono", text: t ? fmtTime(t) : "—" }),
        conversationCell(ev),
        el("td", { text: ev.model }),
        el("td", { class: "num", text: String(ev.ping) }),
        el("td", { class: "num", text: ev.usage ? fmtNum(usage.cache_read_input_tokens) : "—" }),
        el("td", { class: "num", text: ev.usage ? fmtNum(usage.cache_creation_input_tokens) : "—" }),
        el("td", { class: "result" }, eventResult(ev))
      ]));
    });
    var rest = (data.count || 0) - events.length;
    var more = $("events-more");
    more.hidden = rest <= 0 || events.length >= HISTORY_MAX;
    more.textContent = translate("events_more", Math.min(HISTORY_STEP, rest));
    $("history-hint").textContent = status && status.data_dir ? translate("history_hint", status.data_dir, retention) : "";
  }

  function renderStatus() {
    renderHeader();
    renderNotices();
    renderConversations();
    renderEvents();
  }

  function showStatus(data) {
    status = data;
    var server = parseTime(status.now);
    clockOffset = server ? server.getTime() - Date.now() : 0;
    renderStatus();
    renderDurationHint();
    // The history changes only with a new ping.
    if (status.last_ping !== lastPingSeen) {
      lastPingSeen = status.last_ping;
      loadHistory().catch(function () {});
    }
  }

  function loadStatus() {
    return api("GET", API + "/status").then(showStatus);
  }

  function historyPath() {
    return API + "/history?range=" + encodeURIComponent(historyRange) + "&limit=" + historyLimit;
  }

  function loadHistory() {
    var path = historyPath();
    return api("GET", path).then(function (data) {
      // A response for a range that is no longer selected is dropped.
      if (path !== historyPath()) return;
      historyData = data;
      renderEvents();
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("load_failed", err.message));
      throw err;
    });
  }

  function selectRange(value) {
    historyRange = value;
    historyLimit = EVENTS_SHOWN;
    Array.prototype.forEach.call($("range-buttons").querySelectorAll("button"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-value") === value ? "true" : "false");
    });
    loadHistory().catch(function () {});
  }

  function stopConversation(button, convKey) {
    button.disabled = true;
    api("POST", API + "/stop", { key: convKey }).then(function (data) {
      showStatus(data);
      toast(translate("stopped_one"));
    }).catch(function (err) {
      button.disabled = false;
      if (err.message !== "unauthorized") toast(translate("stop_failed", err.message));
      loadStatus().catch(function () {});
    });
  }

  // ---- Settings ----

  var FIELDS = ["max_pings", "interval_min", "interval_max", "min_body_kb", "history_retention_days"];

  // A field shows the value saved in config.yaml, or the value in effect
  // when the key is not set.
  function configValue(name) {
    var value = rawConfig[name];
    if (typeof value === "number") return value;
    return status && status.config ? status.config[name] : undefined;
  }

  function loadConfig() {
    return api("GET", CONFIG_API).then(function (data) { rawConfig = data || {}; });
  }

  function pressDuration(value) {
    Array.prototype.forEach.call($("duration-buttons").querySelectorAll("button"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-value") === value ? "true" : "false");
    });
    $("custom-pings").hidden = value !== "custom";
  }

  function form() { return $("settings"); }

  function fillSettings() {
    FIELDS.forEach(function (name) {
      var value = configValue(name);
      var input = form().elements[name];
      if (document.activeElement !== input) input.value = value === undefined ? "" : value;
    });
    // A saved count that matches a preset selects the preset, also when it
    // was entered under Custom.
    var pings = String(configValue("max_pings"));
    customMode = PRESETS.indexOf(pings) < 0;
    pressDuration(customMode ? "custom" : pings);
    renderDurationHint();
  }

  // The cache lasts until 300 seconds after the last ping; pings come on
  // average every (interval_min + interval_max) / 2 seconds.
  function renderDurationHint() {
    var pings = Number(configValue("max_pings")), min = Number(configValue("interval_min")), max = Number(configValue("interval_max"));
    if (!isFinite(pings) || !isFinite(min) || !isFinite(max)) { $("duration-hint").textContent = ""; return; }
    if (pings === 0) { $("duration-hint").textContent = translate("duration_hint_off"); return; }
    var minutes = Math.floor((pings * (min + max) / 2 + CACHE_TTL) / 60);
    $("duration-hint").textContent = translate("duration_hint", fmtSeconds(min), fmtSeconds(max), pings, minutes);
  }

  // Saves are queued so that a later change is not overwritten by an
  // earlier one that finishes after it.
  var saveQueue = Promise.resolve();
  function save(patch) {
    saveQueue = saveQueue.then(function () {
      return api("PATCH", CONFIG_API, patch);
    }).then(function () {
      toast(translate("saved"));
      return loadConfig();
    }).then(function () {
      fillSettings();
      // CPA applies the saved config to the plugin a moment later.
      clearTimeout(statusTimer);
      statusTimer = setTimeout(function () { loadStatus().catch(function () {}); }, 3000);
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("save_failed", err.message));
      fillSettings();
    });
    return saveQueue;
  }

  function wholeNumber(text) {
    if (!/^\s*\d+\s*$/.test(text)) return null;
    return parseInt(text, 10);
  }

  function saveField(event) {
    var input = event.target;
    var name = input.name;
    if (FIELDS.indexOf(name) < 0) return;
    var value = wholeNumber(input.value);
    if (value === null) {
      toast(translate("number_invalid"));
      fillSettings();
      return;
    }
    var patch = {};
    if (name === "interval_min" || name === "interval_max") {
      // Both intervals are checked as typed and saved together, so a pair
      // that is only valid after changing both fields can be entered one
      // field at a time; an invalid value stays in its field until then.
      var min = wholeNumber(form().elements["interval_min"].value);
      var max = wholeNumber(form().elements["interval_max"].value);
      if (min === null || max === null || !(min >= 1 && min <= max && max <= 299)) {
        toast(translate("interval_invalid"));
        return;
      }
      if (min === configValue("interval_min") && max === configValue("interval_max")) return;
      patch.interval_min = min;
      patch.interval_max = max;
    } else {
      if (name === "history_retention_days" && !(value >= 1 && value <= 3650)) {
        toast(translate("retention_invalid"));
        fillSettings();
        return;
      }
      if (value === configValue(name)) return;
      patch[name] = value;
    }
    save(patch);
  }

  // ---- Startup ----

  function start() {
    $("key-section").hidden = true;
    $("app").hidden = false;
    Promise.all([loadStatus(), loadConfig()]).then(fillSettings).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("load_failed", err.message));
    });
    clearInterval(refreshTimer);
    refreshTimer = setInterval(function () {
      if (!document.hidden) loadStatus().catch(function () {});
    }, 5000);
  }

  $("key-form").addEventListener("submit", function (e) {
    e.preventDefault();
    key = $("key-input").value.trim();
    if (!key) return;
    sessionStorage.setItem(KEY_STORE, key);
    start();
  });
  Array.prototype.forEach.call($("duration-buttons").querySelectorAll("button"), function (button) {
    button.addEventListener("click", function () {
      var value = button.getAttribute("data-value");
      customMode = value === "custom";
      pressDuration(value);
      if (customMode) {
        $("settings").elements["max_pings"].focus();
        return;
      }
      if (Number(value) !== configValue("max_pings")) save({ max_pings: Number(value) });
    });
  });
  $("settings").addEventListener("change", saveField);
  $("settings").addEventListener("submit", function (e) { e.preventDefault(); });
  // Enter commits a number field; leaving it fires the change that saves it.
  $("settings").addEventListener("keydown", function (e) {
    if (e.key === "Enter" && e.target.tagName === "INPUT") {
      e.preventDefault();
      e.target.blur();
    }
  });
  $("events-more").addEventListener("click", function () {
    historyLimit += HISTORY_STEP;
    loadHistory().catch(function () {});
  });
  Array.prototype.forEach.call($("range-buttons").querySelectorAll("button"), function (button) {
    button.addEventListener("click", function () { selectRange(button.getAttribute("data-value")); });
  });
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && key && status) loadStatus().catch(function () {});
  });
  setInterval(tick, 1000);
  window.addEventListener("storage", function (e) {
    if (e.key === "cli-proxy-theme") applyTheme();
    if (e.key === "cli-proxy-language") applyLanguage();
  });
  if (window.matchMedia) {
    var media = window.matchMedia("(prefers-color-scheme: dark)");
    if (media.addEventListener) media.addEventListener("change", applyTheme);
  }

  // Checked without reading the parent, so the layout also clears the
  // Management Center's toolbar when it is served from another origin.
  if (window.self !== window.top) document.documentElement.classList.add("embedded");
  applyTheme();
  applyLanguage();
  watchParent();
  key = sessionStorage.getItem(KEY_STORE) || centerKey();
  if (key) start(); else askForKey("");
})();
