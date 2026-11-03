const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const { test } = require("node:test");
const { runInNewContext } = require("node:vm");

const source = readFileSync(join(__dirname, "page.js"), "utf8");
const startup = source.indexOf("  // ---- Startup ----");
assert.notEqual(startup, -1);

// Run the page's rendering and request code without startup listeners or timers.
class Element {
  children = [];
  textContent = "";
  hidden = false;
  attributes = {};
  parentNode = { hidden: false };
  get firstChild() { return this.children[0]; }
  appendChild(child) { this.children.push(child); }
  removeChild(child) { this.children.splice(this.children.indexOf(child), 1); }
  setAttribute(name, value) { this.attributes[name] = value; }
  getAttribute(name) { return this.attributes[name]; }
  addEventListener() {}
  contains() { return false; }
}

// The text of node and its descendants.
function text(node) {
  return node.textContent + node.children.map(text).join("");
}

// The descendants of node whose class list includes name.
function byClass(node, name) {
  const found = [];
  for (const child of node.children) {
    if ((child.className || "").split(" ").includes(name)) found.push(child);
    found.push(...byClass(child, name));
  }
  return found;
}

function page(language, environment = {}) {
  const elements = new Map();
  const get = (id) => {
    if (!elements.has(id)) elements.set(id, new Element());
    return elements.get(id);
  };
  const context = {
    window: {},
    document: {
      getElementById: get,
      querySelectorAll: () => [],
      createElement: () => new Element(),
      createTextNode: (text) => Object.assign(new Element(), { textContent: text }),
    },
    setTimeout: () => 0,
    clearTimeout: () => {},
    ...environment,
  };
  runInNewContext(source.slice(0, startup) + `
    globalThis.pageTest = {
      centerKey: centerKey,
      load: function (range, language) {
        lang = language;
        savingsRange = range;
        return loadSavings();
      },
      history: function (data, language, open) {
        lang = language;
        historyData = data;
        (open || []).forEach(function (id) { expanded[id] = true; });
        renderEvents();
      }
    };
  })();`, context, { filename: "page.js" });
  return {
    get,
    centerKey: context.pageTest.centerKey,
    history: (data, open) => context.pageTest.history(data, language, open),
    async load(data) {
      let requested;
      context.fetch = async (path) => {
        requested = path;
        return { ok: true, json: async () => data };
      };
      await context.pageTest.load(data.range, language);
      assert.equal(requested, "/v0/management/wick/savings?range=" + data.range);
    },
  };
}

const authHost = "localhost:8317";
const authAgent = "Wick test browser/1.0";
const authKey = "test-management-key-中文🔑";

function obfuscatedAuth(version, value) {
  const mask = Buffer.from("cli-proxy-api-webui::secure-storage|" + authHost +
    (version === "v1" ? "|" + authAgent : ""), "utf8");
  const bytes = Buffer.from(JSON.stringify(value), "utf8");
  for (let i = 0; i < bytes.length; i++) bytes[i] ^= mask[i % mask.length];
  return `enc::${version}::${bytes.toString("base64")}`;
}

function rememberedKey(raw, overrides = {}) {
  return page("en", {
    localStorage: { getItem: (name) => {
      assert.equal(name, "cli-proxy-auth");
      return raw;
    } },
    location: { host: authHost },
    navigator: { userAgent: authAgent },
    atob, TextEncoder, TextDecoder,
    ...overrides,
  }).centerKey();
}

for (const [shape, value] of [
  ["persisted state", { state: { managementKey: authKey, rememberPassword: true }, version: 0 }],
  ["plain object", { managementKey: authKey }],
]) {
  for (const format of ["v1", "v2", "plaintext"]) {
    test(`remembered key: ${format} ${shape}`, () => {
      const raw = format === "plaintext" ? JSON.stringify(value) : obfuscatedAuth(format, value);
      assert.equal(rememberedKey(raw), authKey);
    });
  }
}

test("remembered key: v2 does not read the user agent", () => {
  const navigator = { get userAgent() { throw new Error("user agent unavailable"); } };
  assert.equal(rememberedKey(obfuscatedAuth("v2", { state: { managementKey: authKey } }), { navigator }), authKey);
});

test("remembered key: v1 requires the matching user agent", () => {
  const raw = obfuscatedAuth("v1", { state: { managementKey: authKey } });
  assert.equal(rememberedKey(raw, { navigator: { userAgent: "other browser/2.0" } }), "");
});

for (const raw of [null, "", "not json", "enc::v2::!", "enc::v1::!", "enc::v3::e30=",
  "null", "{}", '{"state":{}}', '{"state":{"managementKey":123}}']) {
  test(`remembered key: invalid or missing storage ${JSON.stringify(raw)}`, () => {
    assert.equal(rememberedKey(raw), "");
  });
}

test("remembered key: unavailable storage falls back to manual entry", () => {
  const localStorage = { getItem() { throw new Error("storage unavailable"); } };
  assert.equal(rememberedKey(null, { localStorage }), "");
});

const stamp = "2026-10-24T00:00:00Z";
function comparison(wick, hour, none) {
  return {
    since: stamp, wick, hour, none,
    difference: none - wick,
    percent: none > 0 ? (none - wick) / none * 100 : undefined,
    hour_difference: hour - wick,
    cache: 0.05, pings: wick - 0.05, saved: none - 0.05,
    ping_count: 6, rounds: 3, results: { saved: 1, wasted: 1, pending: 1 },
  };
}
function response(compare, range = "24h") {
  return { range, from: stamp, prices: { fetched: stamp }, totals: {}, compare };
}

const cases = [
  {
    name: "both cheaper, using unrounded amounts",
    compare: comparison(0.16772, 0.0588, 0.28),
    zh: "相对不挑灯：挑灯便宜 40.1% · 1 小时缓存便宜 79.0%",
    en: "vs no Wick: Wick 40.1% cheaper · 1-hour cache 79.0% cheaper",
  },
  {
    name: "Wick more expensive",
    compare: comparison(0.35, 0.07, 0.28),
    zh: "相对不挑灯：挑灯贵 25.0% · 1 小时缓存便宜 75.0%",
    en: "vs no Wick: Wick 25.0% more expensive · 1-hour cache 75.0% cheaper",
  },
  {
    name: "1-hour cache more expensive",
    compare: comparison(0.14, 0.35, 0.28),
    zh: "相对不挑灯：挑灯便宜 50.0% · 1 小时缓存贵 25.0%",
    en: "vs no Wick: Wick 50.0% cheaper · 1-hour cache 25.0% more expensive",
  },
  {
    name: "both more expensive",
    compare: comparison(0.35, 0.42, 0.28),
    zh: "相对不挑灯：挑灯贵 25.0% · 1 小时缓存贵 50.0%",
    en: "vs no Wick: Wick 25.0% more expensive · 1-hour cache 50.0% more expensive",
  },
  {
    name: "all equal",
    compare: comparison(0.28, 0.28, 0.28),
    zh: "相对不挑灯：挑灯持平 · 1 小时缓存持平",
    en: "vs no Wick: Wick costs the same · 1-hour cache costs the same",
  },
  {
    name: "Wick equal",
    compare: comparison(0.28, 0.14, 0.28),
    zh: "相对不挑灯：挑灯持平 · 1 小时缓存便宜 50.0%",
    en: "vs no Wick: Wick costs the same · 1-hour cache 50.0% cheaper",
  },
  {
    name: "1-hour cache equal",
    compare: comparison(0.14, 0.28, 0.28),
    zh: "相对不挑灯：挑灯便宜 50.0% · 1 小时缓存持平",
    en: "vs no Wick: Wick 50.0% cheaper · 1-hour cache costs the same",
  },
  {
    name: "zero baseline with ping cost",
    compare: comparison(0.1, 0, 0),
    zh: "相对不挑灯：基准花费为 $0，无法计算百分比",
    en: "vs no Wick: baseline cost is $0; percentages unavailable",
  },
  {
    name: "all zero",
    compare: comparison(0, 0, 0),
    zh: "相对不挑灯：基准花费为 $0，无法计算百分比",
    en: "vs no Wick: baseline cost is $0; percentages unavailable",
  },
  {
    name: "missing baseline",
    compare: { ...comparison(0.14, 0.07, 0.28), none: undefined },
    zh: "相对不挑灯：暂无可比较的花费数据",
    en: "vs no Wick: no comparable cost data",
  },
  {
    name: "missing Wick percentage",
    compare: { ...comparison(0.14, 0.07, 0.28), percent: undefined },
    zh: "相对不挑灯：挑灯暂无对比数据 · 1 小时缓存便宜 75.0%",
    en: "vs no Wick: Wick comparison unavailable · 1-hour cache 75.0% cheaper",
  },
  {
    name: "missing 1-hour cost",
    compare: { ...comparison(0.14, 0.07, 0.28), hour: null },
    zh: "相对不挑灯：挑灯便宜 50.0% · 1 小时缓存暂无对比数据",
    en: "vs no Wick: Wick 50.0% cheaper · 1-hour cache comparison unavailable",
  },
  {
    name: "non-finite comparison data",
    compare: { ...comparison(0.14, 0.07, 0.28), percent: NaN, hour: Infinity },
    zh: "相对不挑灯：挑灯暂无对比数据 · 1 小时缓存暂无对比数据",
    en: "vs no Wick: Wick comparison unavailable · 1-hour cache comparison unavailable",
  },
];

// Use whole-dollar differences at a $10,000 baseline to test the 0.05% tie
// without subtraction noise. Also exercise values immediately around that tie.
for (const [percent, rounded] of [[0.04, "0.0"], [0.049999, "0.0"], [0.05, "0.1"], [0.050001, "0.1"], [0.06, "0.1"]]) {
  for (const sign of [1, -1]) {
    const cost = 10000 - sign * percent * 100;
    const equal = rounded === "0.0";
    const zh = equal ? "持平" : (sign > 0 ? "便宜 " : "贵 ") + rounded + "%";
    const en = equal ? "costs the same" : rounded + (sign > 0 ? "% cheaper" : "% more expensive");
    cases.push({
      name: `rounding ${sign * percent}% to ${rounded}%`,
      compare: comparison(cost, cost, 10000),
      zh: `相对不挑灯：挑灯${zh} · 1 小时缓存${zh}`,
      en: `vs no Wick: Wick ${en} · 1-hour cache ${en}`,
    });
  }
}

for (const [name, compare] of [
  ["small saving at a $100 baseline", comparison(99.96, 99.96, 100)],
  ["small extra cost at a $100 baseline", comparison(100.04, 100.04, 100)],
  ["floating-point extra cost", comparison(0.1 + 0.2, 0.1 + 0.2, 0.3)],
  ["floating-point saving", comparison(0.3, 0.3, 0.1 + 0.2)],
]) {
  cases.push({
    name, compare,
    zh: "相对不挑灯：挑灯持平 · 1 小时缓存持平",
    en: "vs no Wick: Wick costs the same · 1-hour cache costs the same",
  });
}

for (const language of ["zh", "en"]) {
  for (const range of ["24h", "7d", "1m", "all"]) {
    for (const scenario of cases) {
      test(`${language}/${range}: ${scenario.name}`, async () => {
        const ui = page(language);
        const data = response(scenario.compare, range);
        const before = structuredClone(data);
        await ui.load(data);
        assert.equal(ui.get("compare-wrap").hidden, false);
        const subtitle = ui.get("cost-deltas").textContent;
        assert.equal(subtitle, scenario[language]);
        assert.equal(subtitle.split(language === "zh" ? "不挑灯" : "vs no Wick").length - 1, 1);
        assert.doesNotMatch(subtitle, /(?:便宜|贵) 0\.0%| 0\.0% (?:cheaper|more expensive)/);
        assert.deepEqual(data, before, "rendering must not mutate statistics");
      });
    }
    test(`${language}/${range}: missing usage or prices falls back to totals`, async () => {
      const ui = page(language);
      for (const patch of [{ compare: null }, { compare: { since: null } }, { prices: {} }]) {
        await ui.load(response(cases[0].compare, range));
        await ui.load({ ...response(cases[0].compare, range), ...patch });
        assert.equal(ui.get("compare-wrap").hidden, true);
        assert.equal(ui.get("cost-bars").children.length, 0);
        assert.equal(ui.get("totals").hidden, false);
      }
    });
  }
}

test("headline, dollar amounts, breakdowns and statistics", async () => {
  const ui = page("zh");
  await ui.load(response(cases[0].compare));
  assert.equal(ui.get("cost-headline").textContent, "挑灯净省 $0.11");
  assert.equal(ui.get("cost-headline").className, "cost-success");
  assert.deepEqual(ui.get("cost-bars").children.map((row) =>
    [row.children[0].textContent, row.children[2].textContent, row.children[3].textContent]), [
    ["挑灯（当前）", "$0.17", "缓存 $0.05 + 挑灯 $0.12"],
    ["1 小时缓存", "$0.06", "缓存按 1 小时写入价和读取价计算，不挑灯"],
    ["不挑灯", "$0.28", "缓存 $0.05 + 过期重写 $0.23"],
  ]);
  assert.equal(ui.get("cost-stats").textContent, "挑灯 6 次 · 3 轮 · 1 轮等到你回来 · 1 轮白挑 · 1 轮挑灯中");
  await ui.load(response(cases[1].compare));
  assert.equal(ui.get("cost-headline").textContent, "挑灯多花 $0.07");
  assert.equal(ui.get("cost-headline").className, "cost-warning");
});

test("savings card: net headline, saved minus cost, ping cost by result and round counts", async () => {
  const compare = {
    ...comparison(1.39, 0.9, 6.37),
    difference: 4.98, cache: 0.05, pings: 1.34, saved: 6.32,
    ping_count: 40, rounds: 11, results: { saved: 3, ended: 4, wasted: 2, replaced: 1, pending: 1 },
    costs: { saved: 0.1, ended: 0.8, wasted: 0.3, replaced: 0, pending: 0.14 },
  };
  const expected = {
    zh: ["挑灯净省 $4.98", "省下重写 $6.32 − 挑灯花费 $1.34（其中省下轮 $0.10 · 收尾 $0.80 · 白挑 $0.30 · 挑灯中 $0.14）",
      "挑灯 40 次 · 11 轮 · 3 轮等到你回来 · 4 轮收尾 · 2 轮白挑 · 1 轮换键 · 1 轮挑灯中"],
    en: ["Wick saves $4.98 net", "Rewrites saved $6.32 − ping cost $1.34 (of which saved rounds $0.10 · ended $0.80 · wasted $0.30 · lit $0.14)",
      "40 pings · 11 rounds · 3 rounds you returned to · 4 ended rounds · 2 wasted rounds · 1 key-changed round · 1 round still lit"],
  };
  for (const language of ["zh", "en"]) {
    const ui = page(language);
    await ui.load(response(compare));
    assert.deepEqual([ui.get("cost-headline").textContent, ui.get("cost-net").textContent, ui.get("cost-stats").textContent], expected[language]);
    await ui.load(response({ ...compare, costs: undefined }));
    assert.equal(ui.get("cost-net").textContent, expected[language][1].replace(/（.*|\s\(.*/, ""));
  }
});

test("totals line: net includes every priced round and reads as extra cost when negative", async () => {
  const totals = { pings: 9, rounds: 3, cost: 0.05, saved: 0.03, net: -0.02, results: { saved: 1, ended: 1, pending: 1 } };
  for (const [language, line] of [
    ["zh", "近 24 小时挑灯 9 次（3 轮），挑灯花费 $0.05；1 轮等到你回来，省下重写 $0.03；多花 $0.02。"],
    ["en", "Last 24 hours: 9 pings in 3 rounds, ping cost $0.05; you came back in time for 1 round, saving $0.03 in rewrites; net cost $0.02."],
  ]) {
    const ui = page(language);
    await ui.load({ range: "24h", from: stamp, prices: { fetched: stamp }, totals, compare: null });
    assert.equal(ui.get("totals").hidden, false);
    assert.equal(ui.get("totals").textContent, line);
  }
});

function historyData() {
  const round = (result, extra = {}) => ({
    start: stamp, last: stamp, model: "claude-opus-5-5", pings: 3, failed: 0, context: 1000,
    cache_read_tokens: 3000, result, cost: 0.01, ...extra,
  });
  return {
    range: "24h", count: 1, prices: { fetched: stamp },
    conversations: [{
      key: "k1", session: "s1", model: "claude-opus-5-5", first: stamp, last: stamp, context: 1000,
      pings: 21, rounds: 7, cost: 0.07, saved: 0.05, net: -0.02, unpriced: 0,
      results: { saved: 1, pending: 1, ended: 3, replaced: 1, wasted: 1 },
      detail: [
        round("pending"),
        round("ended", { end: "manual" }),
        round("ended", { end: "until" }),
        round("ended"),
        round("replaced", { end: "replaced" }),
        round("wasted", { return: stamp, idle: 120 }),
        round("saved", { return: stamp, idle: 600, saved: 0.05 }),
      ],
    }],
  };
}

test("history row: ping cost, rewrites saved and a negative net", () => {
  const ui = page("zh");
  ui.history(historyData());
  const row = ui.get("events").children[0];
  assert.equal(row.children.length, 9);
  assert.deepEqual(row.children.slice(5, 8).map(text), ["$0.07", "$0.05", "−$0.02"]);
  assert.match(row.children[7].className, /\bnet-negative\b/);

  const data = historyData();
  Object.assign(data.conversations[0], { net: 0.03 });
  ui.history(data);
  const cell = ui.get("events").children[0].children[7];
  assert.equal(text(cell), "$0.03");
  assert.doesNotMatch(cell.className, /net-negative/);
});

test("history columns are named and explained in page.html", () => {
  const html = readFileSync(join(__dirname, "page.html"), "utf8");
  for (const [name, title, label] of [["col_cost", "col_cost_hint", "挑灯花费"], ["col_saved", "col_saved_hint", "省下重写"], ["col_net", "col_net_hint", "净省"]]) {
    assert.match(html, new RegExp(`data-i18n="${name}" data-i18n-title="${title}">${label}</th>`));
  }
});

for (const [language, badges, hints] of [
  ["zh", ["省下", "挑灯中", "收尾 3", "换键", "白挑"], {
    "收尾 3": "对话最后一轮，挑灯后你没再回来。",
    "换键": "工具、系统提示或模型变了，对话换了新键继续，旧键停止。",
    "白挑": "挑灯后你很快就回来了（距上个请求不到 5 分钟），不挑缓存也还在，这几次挑灯白花了。",
  }],
  ["en", ["Saved", "Lit", "Ended 3", "Key changed", "Wasted"], {
    "Ended 3": "The last round of the conversation; you did not come back after the pings.",
    "Key changed": "The tools, system prompt or model changed; the conversation continued under a new key and the old key stopped.",
    "Wasted": "You came back soon after the pings (less than 5 minutes after the previous request); the cache was still there without them, so these pings were wasted.",
  }],
]) {
  test(`${language}: result badges and their hover text`, () => {
    const ui = page(language);
    ui.history(historyData());
    const found = byClass(ui.get("events").children[0].children[8], "badge");
    assert.deepEqual(found.map(text), badges);
    assert.deepEqual(found.map((b) => b.className.split(" ")[1]),
      ["badge-success", "badge-muted", "badge-muted", "badge-muted", "badge-warning"]);
    for (const b of found) {
      if (hints[text(b)]) assert.equal(b.attributes.title, hints[text(b)]);
      else assert.ok(b.attributes.title);
    }
  });
}

test("rounds: ended rounds say why the pings stopped", () => {
  for (const [language, ended] of [
    ["zh", [["手动熄灭", "对话最后一轮，挑灯后你没再回来。\n手动熄灭。"],
      ["挑满时长后停下", "对话最后一轮，挑灯后你没再回来。\n挑满时长后停下。"],
      ["缓存过期前没回来", "对话最后一轮，挑灯后你没再回来。\n缓存过期前没回来。"]]],
    ["en", [["Snuffed out by hand", "The last round of the conversation; you did not come back after the pings.\nSnuffed out by hand."],
      ["Stopped after the time kept lit", "The last round of the conversation; you did not come back after the pings.\nStopped after the time kept lit."],
      ["You did not come back before the cache expired", "The last round of the conversation; you did not come back after the pings.\nYou did not come back before the cache expired."]]],
  ]) {
    const ui = page(language);
    ui.history(historyData(), ["k1"]);
    const detail = ui.get("events").children[1];
    assert.equal(detail.children[0].attributes.colspan, "9");
    const results = byClass(detail, "result");
    assert.equal(results.length, 7);
    assert.deepEqual(results.slice(1, 4).map((cell) => [text(byClass(cell, "result-note")[0]), byClass(cell, "badge")[0].attributes.title]), ended);
    assert.equal(byClass(results[4], "badge")[0].className, "badge badge-muted");
    assert.equal(byClass(results[4], "result-note").length, 0);
    const header = byClass(detail, "rounds")[0].children[0].children[0].children[4];
    assert.equal(header.textContent, language === "zh" ? "挑灯花费" : "Ping cost");
    assert.ok(header.attributes.title);
  }
});

test("rounds: superseded and evicted ends", () => {
  const data = historyData();
  data.conversations[0].detail = [
    { ...data.conversations[0].detail[1], end: "superseded" },
    { ...data.conversations[0].detail[1], end: "evicted" },
  ];
  const ui = page("zh");
  ui.history(data, ["k1"]);
  const notes = byClass(ui.get("events").children[1], "result-note").map(text);
  assert.deepEqual(notes, ["随主对话熄灭", "跟踪的对话太多，被挤出"]);
});

test("savings card: the ping cost by result adds up to the total", async () => {
  const costs = { saved: 0.1, ended: 0.8, wasted: 0.3, replaced: 0.05, pending: 0.14, miss: 0.02, failed: 0.01 };
  const pings = Object.values(costs).reduce((a, b) => a + b, 0);
  const ui = page("zh");
  await ui.load(response({ ...comparison(1.42, 0.9, 6.37), pings, saved: 6.32, costs }));
  assert.equal(ui.get("cost-net").textContent,
    "省下重写 $6.32 − 挑灯花费 $1.42（其中省下轮 $0.10 · 收尾 $0.80 · 白挑 $0.30 · 换键 $0.05 · 挑灯中 $0.14 · 未命中 $0.02 · 失败 $0.01）");
  const listed = ui.get("cost-net").textContent.match(/\$\d+\.\d+/g).slice(2).map((v) => Number(v.slice(1)));
  assert.equal(listed.reduce((a, b) => a + b, 0).toFixed(2), pings.toFixed(2));
});

test("savings card: English counts use the singular for one", async () => {
  const ui = page("en");
  const results = (n) => ({ saved: n, ended: n, wasted: n, replaced: n, pending: n });
  await ui.load(response({ ...comparison(0.14, 0.07, 0.28), ping_count: 1, rounds: 1, results: results(1) }));
  assert.equal(ui.get("cost-stats").textContent,
    "1 ping · 1 round · 1 round you returned to · 1 ended round · 1 wasted round · 1 key-changed round · 1 round still lit");
  await ui.load(response({ ...comparison(0.14, 0.07, 0.28), ping_count: 2, rounds: 2, results: results(2) }));
  assert.equal(ui.get("cost-stats").textContent,
    "2 pings · 2 rounds · 2 rounds you returned to · 2 ended rounds · 2 wasted rounds · 2 key-changed rounds · 2 rounds still lit");
});
