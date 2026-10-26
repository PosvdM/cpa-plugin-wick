(function () {
  "use strict";

  var API = "/v0/management/wick";
  var CONFIG_API = "/v0/management/plugins/wick/config";
  var KEY_STORE = "wick-management-key";
  // The duration presets of the settings, in minutes.
  var PRESETS = ["0", "15", "30", "60"];
  // The prompt cache keeps an entry for 300 seconds after the last read.
  var CACHE_TTL = 300;
  var EVENTS_SHOWN = 20;
  var HISTORY_STEP = 100;

  var key = "";
  var status = null;
  var rawConfig = {};
  // The plugin's clock minus the browser's, so countdowns match the plugin.
  var clockOffset = 0;
  var historyRange = "24h";
  var historyLimit = EVENTS_SHOWN;
  var historyData = null;
  var savingsRange = "24h";
  var savingsData = null;
  // When the savings were last loaded; they are reloaded once a minute,
  // since requests add to the comparison without a new event.
  var savingsLoaded = 0;
  var SAVINGS_REFRESH = 60000;
  // last_event of the status the history was loaded for.
  var lastEventSeen = null;
  // Reloads the history when its earliest pending round gets a result.
  var pendingTimer = null;
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
      minutes_unit: "分钟",
      duration_hint: "对话空闲后每隔 {0}到 {1}挑灯一次，缓存保持到空闲 {2}。单个对话可在“正在挑灯的对话”中另设时长。",
      duration_hint_off: "挑灯已关闭，对话空闲 5 分钟后缓存过期。",
      subagent_duration: "子代理挑灯时长",
      subagent_hint: "子代理的缓存保持到空闲 {0}。主对话收到下一个请求时，子代理随之熄灭。",
      subagent_hint_off: "子代理不挑灯。", subagent_hint_wick_off: "默认挑灯时长为“关闭”时，子代理也不挑灯。",
      advanced: "高级设置", advanced_scope: "对所有对话生效",
      interval_min: "最短间隔（秒）", interval_max: "最长间隔（秒）", min_body_kb: "最小请求体（KiB）", history_retention_days: "记录保留（天）",
      advanced_hint: "每次挑灯的时刻在最短和最长间隔之间随机选取，最长间隔不超过 299 秒。请求体小于最小请求体的请求不挑灯，如生成标题等辅助请求。",
      interval_invalid: "间隔须满足 1 ≤ 最短间隔 ≤ 最长间隔 ≤ 299",
      number_invalid: "请填写不小于 0 的整数", minutes_invalid: "请填写 0，或 6 到 1440 之间的分钟数",
      retention_invalid: "记录保留天数须在 1 到 3650 之间",
      saved: "已保存", save_failed: "保存失败：{0}", load_failed: "加载失败：{0}",
      conversations: "正在挑灯的对话",
      col_conversation: "对话", col_model: "模型", col_model_account: "模型 / 账号", col_context: "上下文", col_pings: "本轮挑灯",
      col_idle: "空闲", col_next: "下次挑灯", col_duration: "时长",
      no_conversations: "没有正在挑灯的对话。客户端发出符合条件的请求后，对话显示在这里。",
      subagent: "子代理", no_prompt: "（没有文字）",
      session_hint: "会话 {0}", key_hint_short: "对话键 {0}", directory_hint: "目录 {0}",
      context_pending: "首次挑灯后显示",
      pings_total: "累计 {0}", pings_total_hint: "共挑灯 {0} 次，本轮 {1} 次。对话收到新请求后开始新的一轮。",
      pinging: "挑灯中",
      duration_default: "{0}（默认）", duration_hours: "{0} 小时",
      duration_custom_option: "自定义…", duration_until: "到 {0}", duration_input: "分钟数",
      duration_set: "已设置，挑灯到 {0}", duration_reset: "已恢复默认时长", duration_failed: "设置失败：{0}",
      duration_invalid: "请填写 1 到 1440 之间的分钟数", duration_too_short: "时长须超过 {0} 分钟", duration_passed: "这个时长已经过了",
      stop: "熄灭", stopped_one: "已熄灭", stop_failed: "熄灭失败：{0}",
      events: "挑灯记录", range: "范围", range_24h: "24 小时", range_7d: "7 天", range_1m: "1 个月", range_all: "全部",
      range_label_24h: "近 24 小时", range_label_7d: "近 7 天", range_label_1m: "近 1 个月", range_label_all: "保留的 {0} 天内",
      savings: "花费对比",
      totals_priced: "{0}挑灯 {1}（{2}），花费 {3}；{4}等到你回来，省下 {5}；{6}。",
      totals_plain: "{0}挑灯 {1}（{2}），{3}等到你回来。",
      net_saved: "净省 {0}", net_cost: "净耗 {0}",
      totals_failed: "其中 {0}失败。",
      totals_unpriced: "{0}的模型在定价页上找不到价格，不计入金额。",
      plan_none: "不挑灯", plan_wick: "挑灯（当前）", plan_hour: "1 小时缓存",
      cost_saves: "挑灯省下 {0}", cost_costs: "挑灯多花 {0}",
      percent_less: "比不挑灯便宜 {0}%", percent_more: "比不挑灯贵 {0}%",
      hour_less: "比 1 小时缓存便宜 {0}", hour_more: "比 1 小时缓存贵 {0}",
      wick_breakdown: "缓存 {0} + 挑灯 {1}", none_breakdown: "缓存 {0} + 过期重写 {1}",
      hour_breakdown: "缓存按 1 小时写入价和读取价计算，不挑灯",
      cost_pings: "挑灯 {0} 次", cost_rounds: "{0} 轮", cost_saved: "{0} 轮等到你回来", cost_wasted: "{0} 轮白挑", cost_pending: "{0} 轮挑灯中",
      pending_note: "待定轮次的挑灯花费已计入；缓存过期或请求回来后确定结果。",
      coverage_note: "实际统计自 {0} 起。请求用量从 v0.6.0 起记录，较早数据无法用于完整花费对比。",
      compare_unpriced: "{0} 个请求或轮次没有模型价格，不计入金额。",
      other_clients: "其他客户端", other_clients_hint: "默认关闭。仅为带显式会话标识且符合缓存条件的 pi 等客户端使用 5 分钟缓存；严格伪装等处理仍可能影响缓存。开启后请确认挑灯读取量大于 0、写入量为 0。关闭后停止挑灯，后续请求原样交给 CPA。",
      n_pings: "{0} 次", n_ping: "{0} 次", n_rounds: "{0} 轮", n_round: "{0} 轮",
      col_time: "最后挑灯", col_rounds: "挑灯", col_cost: "花费", col_saved: "省下", col_result: "结果",
      col_round_time: "时间", col_round_pings: "本轮次数", col_read: "缓存读取",
      rounds_pings: "{0} · {1}", expand_hint: "点击查看每一轮",
      first_ping_hint: "范围内首次挑灯 {0}", context_hint: "最后一次挑灯的输入 token 数",
      no_events: "所选范围内没有挑灯记录。",
      history_hint: "记录和正在挑灯的对话保存在 {0}，记录保留 {1} 天。",
      price_hint: "金额按 Anthropic 官方定价页的 API 价格计算，价格更新于 {0}。订阅（如 Pro）的额度与 API 价格不完全对应，金额只能作为比例参考。",
      price_stale: "最近一次更新价格失败：{0}",
      price_none: "尚未取得 API 价格，暂不显示金额。", price_error: "获取失败：{0}",
      result_saved: "省下", result_saved_amount: "省下 {0}", result_wasted: "白挑", result_pending: "挑灯中",
      result_miss: "未命中", result_failed: "失败", result_count: "{0} {1}",
      round_back: "空闲 {0} 后回来",
      events_more: "再显示 {0} 个对话",
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
      minutes_unit: "min",
      duration_hint: "While a conversation is idle, Wick pings it every {0} to {1}, keeping the cache until it has been idle for {2}. Each conversation can be given its own time under Conversations kept lit.",
      duration_hint_off: "Wick is off. The cache expires after 5 idle minutes.",
      subagent_duration: "Time kept lit for subagents",
      subagent_hint: "A subagent's cache is kept until it has been idle for {0}. Subagents are snuffed out when the main conversation sends its next request.",
      subagent_hint_off: "Subagents are not kept lit.", subagent_hint_wick_off: "With the default time set to Off, subagents are not kept lit either.",
      advanced: "Advanced", advanced_scope: "Applies to every conversation",
      interval_min: "Shortest interval (s)", interval_max: "Longest interval (s)", min_body_kb: "Minimum body (KiB)", history_retention_days: "Keep history (days)",
      advanced_hint: "Each ping is sent at a random time between the shortest and longest interval; the longest is at most 299 seconds. Requests with a smaller body than the minimum, such as title generation, are not kept lit.",
      interval_invalid: "Intervals must satisfy 1 ≤ shortest ≤ longest ≤ 299",
      number_invalid: "Enter a whole number of at least 0", minutes_invalid: "Enter 0, or 6 to 1440 minutes",
      retention_invalid: "Keep history for 1 to 3650 days",
      saved: "Saved", save_failed: "Save failed: {0}", load_failed: "Load failed: {0}",
      conversations: "Conversations kept lit",
      col_conversation: "Conversation", col_model: "Model", col_model_account: "Model / Account", col_context: "Context", col_pings: "This round",
      col_idle: "Idle", col_next: "Next ping", col_duration: "Keep lit",
      no_conversations: "No conversations are kept lit. Conversations appear here after a client sends an eligible request.",
      subagent: "Subagent", no_prompt: "(no text)",
      session_hint: "Session {0}", key_hint_short: "Conversation key {0}", directory_hint: "Directory {0}",
      context_pending: "Shown after the first ping",
      pings_total: "{0} in all", pings_total_hint: "{0} pings in all, {1} in this round. A new request starts a new round.",
      pinging: "Pinging",
      duration_default: "{0} (default)", duration_hours: "{0} h",
      duration_custom_option: "Custom…", duration_until: "Until {0}", duration_input: "Minutes",
      duration_set: "Kept lit until {0}", duration_reset: "Back to the default time", duration_failed: "Could not set: {0}",
      duration_invalid: "Enter 1 to 1440 minutes", duration_too_short: "Enter more than {0} minutes", duration_passed: "That time has already passed",
      stop: "Snuff out", stopped_one: "Snuffed out", stop_failed: "Could not snuff out: {0}",
      events: "Ping history", range: "Range", range_24h: "24 hours", range_7d: "7 days", range_1m: "1 month", range_all: "All",
      range_label_24h: "Last 24 hours", range_label_7d: "Last 7 days", range_label_1m: "Last month", range_label_all: "All {0} days kept",
      savings: "Cost comparison",
      totals_priced: "{0}: {1} in {2}, costing {3}; you came back in time for {4}, saving {5}; {6}.",
      totals_plain: "{0}: {1} in {2}; you came back in time for {3}.",
      net_saved: "net saving {0}", net_cost: "net cost {0}",
      totals_failed: "{0} failed.",
      totals_unpriced: "{0} with a model missing from the pricing page are shown without amounts.",
      plan_none: "No Wick", plan_wick: "Wick · Current", plan_hour: "1-hour cache",
      cost_saves: "Wick saves {0} vs no Wick", cost_costs: "Wick costs {0} more vs no Wick",
      percent_less: "{0}% cheaper vs no Wick", percent_more: "{0}% more vs no Wick",
      hour_less: "{0} cheaper vs 1-hour cache", hour_more: "{0} more vs 1-hour cache",
      wick_breakdown: "Cache {0} + pings {1}", none_breakdown: "Cache {0} + expired-cache rewrites {1}",
      hour_breakdown: "Cache at 1-hour write and read prices, without pings",
      cost_pings: "{0} pings", cost_rounds: "{0} rounds", cost_saved: "{0} rounds you returned to", cost_wasted: "{0} wasted rounds", cost_pending: "{0} rounds still lit",
      pending_note: "Pending rounds include their ping costs; results settle when the cache expires or a request returns.",
      coverage_note: "Actual coverage starts {0}. Request usage is recorded from v0.6.0; older data cannot support the full cost comparison.",
      compare_unpriced: "{0} requests or rounds have no model price and are excluded from amounts.",
      other_clients: "Other clients", other_clients_hint: "Off by default. Uses a 5-minute cache for pi and other clients only when requests have an explicit session identifier and meet the cache requirements. Strict cloaking and similar processing can still affect caching. After enabling, verify that pings read cache tokens and write none. Turning this off stops their pings and passes subsequent requests unchanged to CPA.",
      n_pings: "{0} pings", n_ping: "{0} ping", n_rounds: "{0} rounds", n_round: "{0} round",
      col_time: "Last ping", col_rounds: "Pings", col_cost: "Cost", col_saved: "Saved", col_result: "Result",
      col_round_time: "Time", col_round_pings: "Pings", col_read: "Cache read",
      rounds_pings: "{0} · {1}", expand_hint: "Click to show each round",
      first_ping_hint: "First ping in the range {0}", context_hint: "Input tokens of the last ping",
      no_events: "No pings in this range.",
      history_hint: "The history and the conversations kept lit are saved in {0}; the history is kept for {1} days.",
      price_hint: "Amounts use the API prices on Anthropic's pricing page, updated {0}. Subscription usage, such as Pro, does not map exactly to API prices, so take the amounts as proportions.",
      price_stale: "The last price update failed: {0}",
      price_none: "API prices are not available yet, so no amounts are shown.", price_error: "Fetching failed: {0}",
      result_saved: "Saved", result_saved_amount: "Saved {0}", result_wasted: "Wasted", result_pending: "Lit",
      result_miss: "Miss", result_failed: "Failed", result_count: "{0} {1}",
      round_back: "back after {0} idle",
      events_more: "Show {0} more conversations",
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
    renderSavings();
    renderEvents();
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

  function count(n, one, many) {
    return translate(n === 1 ? one : many, fmtNum(n));
  }

  // Dollar amounts at API prices, with two decimals.
  function fmtMoney(v) {
    if (v === null || v === undefined) return "—";
    if (v !== 0 && Math.abs(v) < 0.005) return "<$0.01";
    return "$" + Math.abs(v).toFixed(2);
  }

  function badge(kind, text) {
    return el("span", { class: "badge badge-" + kind }, [el("span", { class: "badge-dot" }), document.createTextNode(text)]);
  }

  // ---- Status ----

  function renderHeader() {
    var state = $("state");
    clear(state);
    if (!status.enabled) state.appendChild(badge("muted", translate("state_disabled")));
    else if (!status.config || !status.config.duration_minutes) state.appendChild(badge("muted", translate("state_off")));
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

  // The durations offered for one conversation, in minutes.
  var DURATION_PRESETS = [15, 30, 60, 120, 240, 480];
  // A refresh leaves the conversation table alone for this long after the
  // user last touched a duration menu, so that an open menu is not closed.
  var MENU_HOLD = 15000;
  var menuTouched = 0;
  // The server returns at most this many conversations of the history.
  var HISTORY_MAX = 5000;

  function fmtDuration(minutes) {
    if (minutes < 60) return translate("minutes", minutes);
    if (minutes % 60 === 0) return translate("duration_hours", minutes / 60);
    return translate("hours_minutes", Math.floor(minutes / 60), minutes % 60);
  }

  // The minutes after its last request that the cache of c already lasts.
  // A duration up to this long would not change anything.
  function minutesCovered(c) {
    var last = parseTime(c.last_request), read = parseTime(c.last_read);
    if (!last || !read) return CACHE_TTL / 60;
    return (read.getTime() - last.getTime()) / 60000 + CACHE_TTL / 60;
  }

  // The duration of one conversation: a menu of presets in order of length,
  // the default among them, with a number field for other values. Durations
  // the cache already lasts are disabled, since choosing one would snuff
  // out the conversation.
  function durationCell(c) {
    var cell = el("td", { class: "duration" });
    var select = el("select", { class: "duration-select", "aria-label": translate("col_duration") });
    var def = c.default_minutes, covered = minutesCovered(c);
    var initial = c.minutes && c.minutes !== def ? String(c.minutes) : "0";
    var choices = DURATION_PRESETS.slice();
    if (c.minutes && choices.indexOf(c.minutes) < 0) choices.push(c.minutes);
    if (choices.indexOf(def) < 0) choices.push(def);
    choices.sort(function (a, b) { return a - b; });
    choices.forEach(function (m) {
      var value = m === def ? "0" : String(m);
      select.appendChild(el("option", {
        value: value,
        text: m === def ? translate("duration_default", fmtDuration(m)) : fmtDuration(m),
        disabled: m <= covered && value !== initial
      }));
    });
    select.appendChild(el("option", { value: "custom", text: translate("duration_custom_option") }));
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
    var input = el("input", { type: "number", min: "1", max: "1440", step: "1", class: "duration-input", placeholder: translate("duration_input"), "aria-label": translate("duration_input") });
    if (c.minutes) input.value = c.minutes;
    var done = false;
    function finish(commit) {
      if (done) return;
      done = true;
      var text = input.value.trim();
      if (!commit || text === "") { input.blur(); renderConversations(); return; }
      var minutes = Number(text);
      var covered = minutesCovered(c);
      if (!/^\d+$/.test(text) || minutes < 1 || minutes > 1440 || minutes <= covered) {
        toast(minutes <= covered && minutes >= 1 ? translate("duration_too_short", Math.floor(covered)) : translate("duration_invalid"));
        input.blur();
        renderConversations();
        return;
      }
      setDuration(input, c.key, minutes);
    }
    input.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      else if (e.key === "Escape") { e.preventDefault(); finish(false); }
    });
    input.addEventListener("blur", function () { finish(true); });
    cell.appendChild(input);
    input.focus();
  }

  function setDuration(control, convKey, minutes) {
    control.disabled = true;
    api("POST", API + "/duration", { key: convKey, minutes: minutes }).then(function (data) {
      control.blur();
      showStatus(data);
      var c = (data.conversations || []).filter(function (x) { return x.key === convKey; })[0];
      var until = c ? parseTime(c.until) : null;
      toast(minutes && until ? translate("duration_set", fmtClock(until)) : translate("duration_reset"));
    }).catch(function (err) {
      control.disabled = false;
      control.blur();
      if (err.message === "the duration has already passed") toast(translate("duration_passed"));
      else if (err.message !== "unauthorized") toast(translate("duration_failed", err.message));
      loadStatus().catch(function () {});
    });
  }

  function renderConversations() {
    var convs = status.conversations || [];
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
          ? el("td", { class: "num", text: fmtNum(contextTokens(usage)) })
          : el("td", { class: "num muted", title: translate("context_pending"), text: "—" }),
        el("td", { class: "num", title: translate("pings_total_hint", c.total_pings || 0, c.pings) }, [
          el("div", { text: String(c.pings) }),
          el("div", { class: "cell-note", text: translate("pings_total", c.total_pings || 0) })
        ]),
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

  // The input of a request: cached, written and uncached tokens.
  function contextTokens(usage) {
    return (usage.cache_read_input_tokens || 0) + (usage.cache_creation_input_tokens || 0) + (usage.input_tokens || 0);
  }

  // Round results, in the order their badges are shown, with the badge
  // style of each. Rounds recorded before returns were recorded have the
  // result "unknown" and no badge.
  var RESULTS = [["saved", "success"], ["pending", "muted"], ["wasted", "warning"], ["miss", "warning"], ["failed", "failure"]];
  var RESULT_KIND = {};
  RESULTS.forEach(function (r) { RESULT_KIND[r[0]] = r[1]; });

  // Conversations of the history whose rounds are shown, by key.
  var expanded = {};

  function historyId(c) { return c.key || "session:" + c.session; }

  // Amounts are shown once prices were fetched, for conversations with a
  // round that has a result and a price.
  function pricesReady(data) {
    data = data || historyData;
    return !!(data && data.prices && parseTime(data.prices.fetched));
  }

  function pricedRounds(t) {
    var results = t.results || {};
    return (t.rounds || 0) - (results.unknown || 0) - (t.unpriced || 0);
  }

  // The rounds of a conversation by result: a badge for each result, with
  // the count when there is more than one round of it, and the error of a
  // last failed ping.
  function historyResult(c) {
    var results = c.results || {};
    var parts = RESULTS.filter(function (r) { return results[r[0]] > 0; }).map(function (r) {
      var label = translate("result_" + r[0]);
      return badge(r[1], results[r[0]] > 1 ? translate("result_count", label, results[r[0]]) : label);
    });
    if (c.last_error && c.detail && c.detail.length && c.detail[0].result === "failed") {
      parts.push(el("span", { class: "result-note", text: c.last_error }));
    }
    return parts;
  }

  function roundResult(r) {
    if (!RESULT_KIND[r.result]) return [];
    var text = r.result === "saved" && r.saved !== undefined && r.saved !== null
      ? translate("result_saved_amount", fmtMoney(r.saved))
      : translate("result_" + r.result);
    var parts = [badge(RESULT_KIND[r.result], text)];
    if (r.return) parts.push(el("span", { class: "result-note", text: translate("round_back", fmtClockSpan(r.idle || 0)) }));
    return parts;
  }

  // The rounds of one conversation, newest first.
  function roundsTable(c) {
    var priced = pricesReady();
    var body = el("tbody");
    (c.detail || []).forEach(function (r) {
      var start = parseTime(r.start), last = parseTime(r.last);
      var time = start ? fmtTime(start) : "—";
      if (start && last && last.getTime() !== start.getTime()) time += " – " + pad(last.getHours()) + ":" + pad(last.getMinutes()) + ":" + pad(last.getSeconds());
      body.appendChild(el("tr", null, [
        el("td", { class: "mono", text: time }),
        r.context ? el("td", { class: "num", text: fmtNum(r.context) }) : el("td", { class: "num muted", text: "—" }),
        el("td", { class: "num", text: String(r.pings) }),
        el("td", { class: "num", text: fmtNum(r.cache_read_tokens) }),
        el("td", { class: "num", text: priced && r.cost !== undefined && r.cost !== null ? fmtMoney(r.cost) : "—" }),
        el("td", { class: "result" }, roundResult(r))
      ]));
    });
    return el("table", { class: "rounds" }, [
      el("thead", null, [el("tr", null, [
        el("th", { text: translate("col_round_time") }),
        el("th", { class: "num", text: translate("col_context") }),
        el("th", { class: "num", text: translate("col_round_pings") }),
        el("th", { class: "num", text: translate("col_read") }),
        el("th", { class: "num", text: translate("col_cost") }),
        el("th", { text: translate("col_result") })
      ])]),
      body
    ]);
  }

  // The savings card: the totals of the range in one line and the cache
  // cost of the requests with Wick, without it and with a 1-hour cache.
  function renderSavings() {
    var data = savingsData;
    if (!data) return;
    var t = data.totals || {};
    var results = t.results || {};
    var cmp = data.compare || {};
    var priced = pricesReady(data);
    var compared = priced && !!parseTime(cmp.since);
    var retention = status && status.config ? status.config.history_retention_days : "";
    var range = translate("range_label_" + data.range, retention);
    var pings = count(t.pings || 0, "n_ping", "n_pings");
    var rounds = count(t.rounds || 0, "n_round", "n_rounds");
    var saved = count(results.saved || 0, "n_round", "n_rounds");
    $("totals").hidden = compared;
    if (priced) {
      $("totals").textContent = translate("totals_priced", range, pings, rounds, fmtMoney(t.cost || 0), saved, fmtMoney(t.saved || 0),
        translate((t.net || 0) >= 0 ? "net_saved" : "net_cost", fmtMoney(t.net || 0)));
    } else $("totals").textContent = translate("totals_plain", range, pings, rounds, saved);
    var notes = [];
    var scoped = compared ? cmp : t;
    if (scoped.failed) notes.push(translate("totals_failed", count(scoped.failed, "n_ping", "n_pings")));
    if (scoped.unpriced && priced) notes.push(compared ? translate("compare_unpriced", scoped.unpriced) : translate("totals_unpriced", count(scoped.unpriced, "n_round", "n_rounds")));
    if (compared && (cmp.results || {}).pending) notes.push(translate("pending_note"));
    var since = parseTime(cmp.since), from = parseTime(data.from);
    if (compared && since && from && since > from) notes.push(translate("coverage_note", since.toLocaleString(lang === "zh" ? "zh-CN" : "en-US")));
    $("totals-notes").textContent = notes.join(lang === "zh" ? "" : " ");
    renderCompare(compared ? cmp : null);
    renderPriceHint(data);
  }

  function renderCompare(cmp) {
    $("compare-wrap").hidden = !cmp;
    var body = $("cost-bars");
    clear(body);
    if (!cmp) return;
    var diff = cmp.difference || 0, hourDiff = cmp.hour_difference || 0;
    $("cost-headline").className = diff >= 0 ? "cost-success" : "cost-warning";
    $("cost-headline").textContent = translate(diff >= 0 ? "cost_saves" : "cost_costs", fmtMoney(diff));
    var deltas = [];
    if (typeof cmp.percent === "number") deltas.push(translate(cmp.percent >= 0 ? "percent_less" : "percent_more", Math.abs(cmp.percent).toFixed(1)));
    deltas.push(translate(hourDiff >= 0 ? "hour_less" : "hour_more", fmtMoney(hourDiff)));
    $("cost-deltas").textContent = deltas.join(" · ");
    var max = Math.max(cmp.wick || 0, cmp.hour || 0, cmp.none || 0);
    function bar(label, cost, detail, primary) {
      body.appendChild(el("div", { class: "cost-row" + (primary ? " cost-current" : "") }, [
        el("span", { class: "cost-label", text: translate(label) }),
        el("div", { class: "cost-track", "aria-hidden": "true" }, [el("div", { class: "cost-fill", style: "width:" + (max > 0 ? cost / max * 100 : 0) + "%" })]),
        el("strong", { class: "cost-amount", text: fmtMoney(cost) }),
        el("p", { class: "cost-detail", text: detail })
      ]));
    }
    bar("plan_wick", cmp.wick || 0, translate("wick_breakdown", fmtMoney(cmp.cache || 0), fmtMoney(cmp.pings || 0)), true);
    bar("plan_hour", cmp.hour || 0, translate("hour_breakdown"));
    bar("plan_none", cmp.none || 0, translate("none_breakdown", fmtMoney(cmp.cache || 0), fmtMoney(cmp.saved || 0)));
    var stats = [], results = cmp.results || {};
    [["cost_pings", cmp.ping_count], ["cost_rounds", cmp.rounds], ["cost_saved", results.saved], ["cost_wasted", results.wasted], ["cost_pending", results.pending]].forEach(function (item) {
      if (item[1]) stats.push(translate(item[0], item[1]));
    });
    $("cost-stats").textContent = stats.join(" · ");
  }

  function renderPriceHint(data) {
    var p = data.prices || {};
    var fetched = parseTime(p.fetched);
    var text = fetched ? translate("price_hint", fmtTime(fetched)) : translate("price_none");
    if (p.error) text += (lang === "zh" ? "" : " ") + translate(fetched ? "price_stale" : "price_error", p.error) + (lang === "zh" ? "。" : ".");
    $("price-hint").textContent = text;
  }

  function renderEvents() {
    var data = historyData;
    if (!data) return;
    var priced = pricesReady();
    var convs = data.conversations || [];
    var body = $("events");
    // Rows are rebuilt on every refresh; the focused one keeps the focus.
    var active = document.activeElement;
    var focused = active && body.contains(active) && active.getAttribute("data-id");
    clear(body);
    $("events-empty").hidden = convs.length > 0;
    body.parentNode.hidden = convs.length === 0;
    convs.forEach(function (c) {
      var id = historyId(c);
      var open = !!expanded[id];
      var last = parseTime(c.last), first = parseTime(c.first);
      var detail = open ? el("tr", { class: "detail-row" }, [el("td", { colspan: "8" }, [roundsTable(c)])]) : null;
      function toggle() {
        if (expanded[id]) delete expanded[id]; else expanded[id] = true;
        renderEvents();
      }
      // A click that ends a text selection does not toggle the row.
      function click() {
        if (window.getSelection && String(window.getSelection())) return;
        toggle();
      }
      var hasAmounts = priced && pricedRounds(c) > 0;
      body.appendChild(el("tr", {
        class: "conv-row", tabindex: "0", "data-id": id, "aria-expanded": open ? "true" : "false", title: translate("expand_hint"),
        onclick: click,
        onkeydown: function (e) {
          if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); }
        }
      }, [
        el("td", { class: "mono", title: first ? translate("first_ping_hint", fmtTime(first)) : null }, [
          el("span", { class: "chevron", "aria-hidden": "true", text: "›" }),
          last ? fmtTime(last) : "—"
        ]),
        conversationCell(c),
        el("td", { text: c.model }),
        el("td", { class: "num words", text: translate("rounds_pings", count(c.rounds || 0, "n_round", "n_rounds"), count(c.pings, "n_ping", "n_pings")) }),
        c.context
          ? el("td", { class: "num", title: translate("context_hint"), text: fmtNum(c.context) })
          : el("td", { class: "num muted", text: "—" }),
        el("td", { class: "num", text: hasAmounts ? fmtMoney(c.cost || 0) : "—" }),
        el("td", { class: "num", text: hasAmounts && c.saved > 0 ? fmtMoney(c.saved) : "—" }),
        el("td", { class: "result" }, historyResult(c))
      ]));
      if (detail) body.appendChild(detail);
    });
    if (focused) {
      Array.prototype.forEach.call(body.querySelectorAll(".conv-row"), function (row) {
        if (row.getAttribute("data-id") === focused) row.focus({ preventScroll: true });
      });
    }
    var rest = (data.count || 0) - convs.length;
    var more = $("events-more");
    more.hidden = rest <= 0 || convs.length >= HISTORY_MAX;
    more.textContent = translate("events_more", Math.min(HISTORY_STEP, rest));
    renderHistoryHint();
    schedulePendingReload(convs);
  }

  function renderHistoryHint() {
    var retention = status && status.config ? status.config.history_retention_days : "";
    $("history-hint").textContent = status && status.data_dir ? translate("history_hint", status.data_dir, retention) : "";
  }

  // A pending round gets its result on the server 300 seconds after its
  // last ping, with no new event, so the history is reloaded then.
  function schedulePendingReload(convs) {
    clearTimeout(pendingTimer);
    var next = Infinity;
    convs.forEach(function (c) {
      (c.detail || []).forEach(function (r) {
        var last = r.result === "pending" ? parseTime(r.last) : null;
        if (last) next = Math.min(next, last.getTime() + CACHE_TTL * 1000);
      });
    });
    if (next === Infinity) return;
    pendingTimer = setTimeout(function () {
      if (!key) return;
      loadHistory().catch(function () {});
      loadSavings().catch(function () {});
    }, Math.max(1000, next - now() + 2000));
  }

  // The history table is rebuilt only when the history is loaded, so that
  // the status refresh every few seconds keeps its focus, selection and
  // scroll position.
  function renderStatus() {
    renderHeader();
    renderNotices();
    renderConversations();
    renderHistoryHint();
  }

  function showStatus(data) {
    status = data;
    var server = parseTime(status.now);
    clockOffset = server ? server.getTime() - Date.now() : 0;
    renderStatus();
    renderDurationHint();
    // The history changes only with a new ping or return.
    if (status.last_event !== lastEventSeen) {
      lastEventSeen = status.last_event;
      loadHistory().catch(function () {});
      loadSavings().catch(function () {});
    } else if (Date.now() - savingsLoaded >= SAVINGS_REFRESH) {
      loadSavings(true).catch(function () {});
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

  function savingsPath() {
    return API + "/savings?range=" + encodeURIComponent(savingsRange);
  }

  // Responses are numbered, so that a slow one cannot replace a newer one.
  // A background refresh fails without a message.
  var savingsSeq = 0;
  function loadSavings(background) {
    var path = savingsPath();
    var seq = ++savingsSeq;
    savingsLoaded = Date.now();
    return api("GET", path).then(function (data) {
      if (seq !== savingsSeq || path !== savingsPath()) return;
      savingsData = data;
      renderSavings();
    }).catch(function (err) {
      if (!background && err.message !== "unauthorized") toast(translate("load_failed", err.message));
      throw err;
    });
  }

  function selectSavingsRange(value) {
    savingsRange = value;
    press($("savings-range-buttons"), value, true);
    loadSavings().catch(function () {});
  }

  function selectRange(value) {
    historyRange = value;
    historyLimit = EVENTS_SHOWN;
    press($("range-buttons"), value, true);
    loadHistory().catch(function () {});
  }

  // ---- Segmented buttons ----

  // Presses the button of box with value and moves the thumb under it.
  function press(box, value, bounce) {
    Array.prototype.forEach.call(box.querySelectorAll(".seg"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-value") === value ? "true" : "false");
    });
    placeThumb(box, bounce);
  }

  // Places the thumb of box under its pressed button, measured because the
  // buttons differ in width and may wrap. With bounce it slides there and
  // squashes on arrival, for a click; otherwise it moves at once, for the
  // first layout, a language switch or a resize. A placement that changes
  // nothing is skipped, so that it cannot cut a slide short.
  function placeThumb(box, bounce) {
    var thumb = box.querySelector(".seg-thumb");
    if (!thumb) return;
    var b = box.querySelector('.seg[aria-pressed="true"]');
    var spot = b ? [b.offsetLeft, b.offsetTop, b.offsetWidth, b.offsetHeight] : [0, 0, 0, 0];
    var key = spot.join(",");
    if (!bounce && box.getAttribute("data-thumb") === key) return;
    box.setAttribute("data-thumb", key);
    if (!bounce) box.classList.add("placing");
    box.style.setProperty("--thumb-x", spot[0] + "px");
    box.style.setProperty("--thumb-y", spot[1] + "px");
    box.style.setProperty("--thumb-w", spot[2] + "px");
    box.style.setProperty("--thumb-h", spot[3] + "px");
    void thumb.offsetWidth; // apply the position before the transition returns
    if (!bounce) {
      box.classList.remove("placing");
      return;
    }
    thumb.classList.remove("bounce");
    void thumb.offsetWidth; // restart the animation
    thumb.classList.add("bounce");
  }

  // The size of a segmented control or its buttons changes when the page
  // is first shown, the language changes or the window is resized.
  var thumbObserver = window.ResizeObserver ? new ResizeObserver(function (entries) {
    entries.forEach(function (entry) {
      var box = entry.target.closest(".segmented");
      if (box) placeThumb(box, false);
    });
  }) : null;
  Array.prototype.forEach.call(document.querySelectorAll(".segmented"), function (box) {
    box.insertBefore(el("span", { class: "seg-thumb", "aria-hidden": "true" }), box.firstChild);
    if (!thumbObserver) return;
    thumbObserver.observe(box);
    Array.prototype.forEach.call(box.querySelectorAll(".seg"), function (b) { thumbObserver.observe(b); });
  });

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

  var FIELDS = ["duration_minutes", "subagent_duration_minutes", "interval_min", "interval_max", "min_body_kb", "history_retention_days"];

  // The two duration choices: the default for conversations and the one
  // for subagents, each a row of presets with a field for other minutes.
  var DURATIONS = [
    { field: "duration_minutes", buttons: "duration-buttons", custom: "custom-minutes" },
    { field: "subagent_duration_minutes", buttons: "subagent-buttons", custom: "custom-subagent-minutes" }
  ];

  // A field shows the value saved in config.yaml, or the value in effect
  // when the key is not set.
  function configValue(name) {
    var value = rawConfig[name];
    if (typeof value === "number" || typeof value === "boolean") return value;
    return status && status.config ? status.config[name] : undefined;
  }

  function loadConfig() {
    return api("GET", CONFIG_API).then(function (data) { rawConfig = data || {}; });
  }

  function pressDuration(d, value, bounce) {
    press($(d.buttons), value, bounce);
    $(d.custom).hidden = value !== "custom";
  }

  function form() { return $("settings"); }

  function fillSettings() {
    form().elements.other_clients.checked = configValue("other_clients") === true;
    FIELDS.forEach(function (name) {
      var value = configValue(name);
      var input = form().elements[name];
      if (document.activeElement !== input) input.value = value === undefined ? "" : value;
    });
    // A saved duration that matches a preset selects the preset, also when
    // it was entered under Custom.
    DURATIONS.forEach(function (d) {
      var minutes = String(configValue(d.field));
      pressDuration(d, PRESETS.indexOf(minutes) < 0 ? "custom" : minutes, false);
    });
    renderDurationHint();
  }

  function renderDurationHint() {
    var minutes = Number(configValue("duration_minutes")), min = Number(configValue("interval_min")), max = Number(configValue("interval_max"));
    if (!isFinite(minutes) || !isFinite(min) || !isFinite(max)) { $("duration-hint").textContent = $("subagent-hint").textContent = ""; return; }
    if (minutes === 0) {
      $("duration-hint").textContent = translate("duration_hint_off");
      $("subagent-hint").textContent = translate("subagent_hint_wick_off");
      return;
    }
    $("duration-hint").textContent = translate("duration_hint", fmtSeconds(min), fmtSeconds(max), fmtDuration(minutes));
    var sub = Number(configValue("subagent_duration_minutes"));
    if (!isFinite(sub)) $("subagent-hint").textContent = "";
    else if (sub === 0) $("subagent-hint").textContent = translate("subagent_hint_off");
    else $("subagent-hint").textContent = translate("subagent_hint", fmtDuration(sub));
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
    if (name === "other_clients") {
      if (input.checked !== configValue(name)) save({ other_clients: input.checked });
      return;
    }
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
      // The cache of a request lasts 5 minutes, so 1 to 5 would keep
      // nothing lit.
      if ((name === "duration_minutes" || name === "subagent_duration_minutes") && (value > 1440 || (value >= 1 && value <= 5))) {
        toast(translate("minutes_invalid"));
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
  DURATIONS.forEach(function (d) {
    Array.prototype.forEach.call($(d.buttons).querySelectorAll("button"), function (button) {
      button.addEventListener("click", function () {
        var value = button.getAttribute("data-value");
        pressDuration(d, value, true);
        if (value === "custom") {
          $("settings").elements[d.field].focus();
          return;
        }
        if (Number(value) !== configValue(d.field)) {
          var patch = {};
          patch[d.field] = Number(value);
          save(patch);
        }
      });
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
  Array.prototype.forEach.call($("savings-range-buttons").querySelectorAll("button"), function (button) {
    button.addEventListener("click", function () { selectSavingsRange(button.getAttribute("data-value")); });
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
