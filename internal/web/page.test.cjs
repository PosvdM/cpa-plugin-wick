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
  get firstChild() { return this.children[0]; }
  appendChild(child) { this.children.push(child); }
  removeChild(child) { this.children.splice(this.children.indexOf(child), 1); }
  setAttribute(name, value) { this.attributes[name] = value; }
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
    ...environment,
  };
  runInNewContext(source.slice(0, startup) + `
    globalThis.pageTest = {
      centerKey: centerKey,
      load: function (range, language) {
        lang = language;
        savingsRange = range;
        return loadSavings();
      }
    };
  })();`, context, { filename: "page.js" });
  return {
    get,
    centerKey: context.pageTest.centerKey,
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

test("headline, dollar amounts, breakdowns and statistics stay unchanged", async () => {
  const ui = page("zh");
  await ui.load(response(cases[0].compare));
  assert.equal(ui.get("cost-headline").textContent, "挑灯省下 $0.11");
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
