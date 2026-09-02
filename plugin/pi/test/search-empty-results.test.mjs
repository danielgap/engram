import assert from "node:assert/strict";
import { test } from "node:test";
import {
  importPluginFromSandbox,
  PLUGIN_ROOT,
  withPluginSandbox,
} from "./plugin-sandbox.mjs";

// Issue #672: servers before #862 answer zero-hit /search with HTTP 200 and a
// literal null body. engramFetch collapses that null into the same value it
// uses for transport failure, so executeMemoryTool reports a healthy empty
// search as "could not reach the Engram HTTP server". These tests pin both
// sides of the contract: null body must surface as an empty result, and a
// real transport failure must still surface as unreachable.

const ROOT = PLUGIN_ROOT;

async function loadPluginHarness(sandbox) {
  const registeredTools = new Map();
  const registerEngram = await importPluginFromSandbox(sandbox);
  registerEngram({
    registerTool(tool) {
      registeredTools.set(tool.name, tool);
    },
    on() {},
  });
  return { registeredTools };
}

function runtimeContext(sessionId) {
  return {
    cwd: ROOT,
    sessionManager: { getSessionId: () => sessionId },
    ui: { setStatus() {} },
  };
}

// Like the shared recordingFetch, but honors an explicit null body: routes
// that carry `body: null` must produce a literal `null` JSON payload, the
// exact wire shape old servers emit for zero-hit searches.
function nullAwareRecordingFetch(routes) {
  const calls = [];
  const fetchStub = async (url, init = {}) => {
    const method = init.method ?? "GET";
    const path = new URL(url).pathname + new URL(url).search;
    calls.push({ method, path });
    const route = routes.find(
      (candidate) =>
        candidate.method === method && path.startsWith(candidate.path),
    );
    if (route?.transportFailure)
      throw new Error("connect ECONNREFUSED 127.0.0.1:7437");
    const payload = route && "body" in route ? route.body : {};
    return new Response(JSON.stringify(payload), {
      status: route?.status ?? 200,
      headers: { "Content-Type": "application/json" },
    });
  };
  return { calls, fetchStub };
}

test("mem_search maps a 200 null body to an empty result, not 'could not reach' (#672)", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";

  const { fetchStub } = nullAwareRecordingFetch([
    { method: "GET", path: "/health", body: { status: "ok" } },
    { method: "GET", path: "/project/current", body: { project: "paidosdep" } },
    { method: "GET", path: "/search", body: null },
  ]);
  globalThis.fetch = fetchStub;

  try {
    await withPluginSandbox("engram-pi-search-null-", async ({ sandbox }) => {
      const { registeredTools } = await loadPluginHarness(sandbox);
      const memSearch = registeredTools.get("mem_search");
      assert.ok(memSearch, "mem_search tool should be registered");

      const result = await memSearch.execute(
        "tool-call-search-null",
        { query: "snake_case_var", project: "paidosdep" },
        undefined,
        undefined,
        runtimeContext("test-session"),
      );

      assert.notEqual(
        result.isError,
        true,
        "an empty search result must not surface as a tool error",
      );
      const text = result.content?.[0]?.text ?? "";
      assert.ok(
        !text.includes("could not reach"),
        `empty search must not claim the server is unreachable: ${text}`,
      );
      assert.deepEqual(
        result.details?.data,
        [],
        "a null body from an old server must normalize to []",
      );
    });
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
  }
});

test("mem_search still reports unreachable when the transport itself fails (#672 regression guard)", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";

  const { fetchStub } = nullAwareRecordingFetch([
    { method: "GET", path: "/health", body: { status: "ok" } },
    { method: "GET", path: "/project/current", body: { project: "paidosdep" } },
    { method: "GET", path: "/search", transportFailure: true },
  ]);
  globalThis.fetch = fetchStub;

  try {
    await withPluginSandbox(
      "engram-pi-search-transport-",
      async ({ sandbox }) => {
        const { registeredTools } = await loadPluginHarness(sandbox);
        const memSearch = registeredTools.get("mem_search");
        assert.ok(memSearch, "mem_search tool should be registered");

        const result = await memSearch.execute(
          "tool-call-search-transport",
          { query: "snake_case_var", project: "paidosdep" },
          undefined,
          undefined,
          runtimeContext("test-session"),
        );

        assert.equal(
          result.isError,
          true,
          "a real transport failure must surface as a tool error",
        );
        const text = result.content?.[0]?.text ?? "";
        assert.ok(
          text.includes("could not reach"),
          `transport failure must keep the unreachable message: ${text}`,
        );
      },
    );
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
  }
});
