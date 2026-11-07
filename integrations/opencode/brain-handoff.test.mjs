import { test } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";

import plugin, { createServer } from "./brain-handoff.js";

const DIRECTORY = "/work/proj";
const CONFIG = "/brain/brain.yaml";
const BASE_DEPS = { bin: "brain", config: CONFIG };

/** Fake ctx whose session.hook records the "context" handler. */
function fakeCtx({ withHook = true } = {}) {
  const handlers = {};
  const state = { registrationDisposed: false };
  const ctx = { location: { directory: DIRECTORY } };
  if (withHook) {
    ctx.session = {
      hook: (name, fn) => {
        handlers[name] = fn;
        return Promise.resolve({
          dispose: () => {
            state.registrationDisposed = true;
          },
        });
      },
    };
  }
  return { ctx, handlers, state };
}

/**
 * Fake spawn. `script(child, args)` decides how the child behaves; every
 * call is recorded in `calls` with its args and options.
 */
function fakeSpawn(script) {
  const calls = [];
  const spawn = (bin, args, options) => {
    const child = new EventEmitter();
    child.stdout = new EventEmitter();
    child.killed = false;
    child.kill = () => {
      child.killed = true;
      return true;
    };
    calls.push({ bin, args, options, child });
    script(child, args);
    return child;
  };
  return { spawn, calls };
}

/** Child that writes `text` to stdout and exits with `code`. */
const exitsWith = (text, code = 0) => (child) =>
  setImmediate(() => {
    if (text) child.stdout.emit("data", Buffer.from(text, "utf8"));
    child.emit("exit", code);
    child.emit("close", code);
  });

const failsToStart = () => (child) =>
  setImmediate(() => child.emit("error", new Error("ENOENT")));

const startCalls = (calls) => calls.filter((c) => c.args[1] === "start");

const contextInput = (sessionID, system) => ({ sessionID, system });

function baseSystem() {
  return [{ type: "text", text: "base" }];
}

test("start context runs once per session and is appended on every call", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith("Brain: project context"));
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn });

  const first = baseSystem();
  const second = baseSystem();
  await handlers.context(contextInput("s1", first));
  await handlers.context(contextInput("s1", second));

  assert.equal(startCalls(calls).length, 1);
  assert.deepEqual(startCalls(calls)[0].args, [
    "session",
    "start",
    "--hook",
    "opencode",
    "--cwd",
    DIRECTORY,
    "--config",
    CONFIG,
  ]);
  assert.equal(first[0].text, "base\n\nBrain: project context");
  assert.equal(second[0].text, "base\n\nBrain: project context");
});

test("each session gets its own start run", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith("ctx"));
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn });

  await handlers.context(contextInput("s1", baseSystem()));
  await handlers.context(contextInput("s2", baseSystem()));

  assert.equal(startCalls(calls).length, 2);
});

test("empty start output appends nothing and does not re-run", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith("", 0));
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn });

  const system = baseSystem();
  await handlers.context(contextInput("s1", system));
  await handlers.context(contextInput("s1", system));

  assert.deepEqual(system, [{ type: "text", text: "base" }]);
  assert.equal(startCalls(calls).length, 1);
});

test("non-zero exit resolves to empty and appends nothing", async () => {
  const { spawn } = fakeSpawn(exitsWith("should be ignored", 1));
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn });

  const system = baseSystem();
  await handlers.context(contextInput("s1", system));

  assert.deepEqual(system, [{ type: "text", text: "base" }]);
});

test("spawn error resolves to empty without throwing", async () => {
  const { spawn } = fakeSpawn(failsToStart());
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn });

  const system = baseSystem();
  await assert.doesNotReject(handlers.context(contextInput("s1", system)));

  assert.deepEqual(system, [{ type: "text", text: "base" }]);
});

test("timeout kills the child and resolves to empty", async () => {
  const { spawn, calls } = fakeSpawn(() => {});
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn, startTimeoutMs: 20 });

  const system = baseSystem();
  await handlers.context(contextInput("s1", system));

  assert.deepEqual(system, [{ type: "text", text: "base" }]);
  assert.equal(calls[0].child.killed, true);
});

test("missing ctx.session.hook skips injection and keeps the end handoff", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith(""));
  const { ctx } = fakeCtx({ withHook: false });
  const server = createServer(ctx, { ...BASE_DEPS, spawn });

  assert.equal(typeof server.event, "function");
  await server.event({ event: { type: "session.idle" } });

  assert.equal(startCalls(calls).length, 0);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].args[1], "end");
  assert.equal(calls[0].options.stdio, "ignore");
});

test("non-array system or missing sessionID spawns nothing", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith("ctx"));
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { ...BASE_DEPS, spawn });

  await handlers.context(contextInput("s1", "not-an-array"));
  await handlers.context({ system: baseSystem() });

  assert.equal(calls.length, 0);
});

test("unconfigured binary registers no hook and spawns nothing", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith("ctx"));
  const { ctx, handlers } = fakeCtx();
  createServer(ctx, { bin: "__BRAIN_BIN__", config: CONFIG, spawn });

  assert.equal(handlers.context, undefined);
  assert.equal(calls.length, 0);
});

test("session.deleted drops the cached start context", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith("ctx"));
  const { ctx, handlers } = fakeCtx();
  const server = createServer(ctx, { ...BASE_DEPS, spawn });

  await handlers.context(contextInput("s1", baseSystem()));
  await server.event({
    event: { type: "session.deleted", properties: { sessionID: "s1" } },
  });
  await handlers.context(contextInput("s1", baseSystem()));

  assert.equal(startCalls(calls).length, 2);
});

test("session.idle still runs the end handoff with the expected args", async () => {
  const { spawn, calls } = fakeSpawn(exitsWith(""));
  const { ctx } = fakeCtx();
  const server = createServer(ctx, { ...BASE_DEPS, spawn });

  await server.event({ event: { type: "session.idle" } });

  assert.deepEqual(calls[0].args, [
    "session",
    "end",
    "--hook",
    "opencode",
    "--cwd",
    DIRECTORY,
    "--config",
    CONFIG,
  ]);
  assert.equal(calls[0].options.cwd, DIRECTORY);
});

test("dispose disposes the context hook registration", async () => {
  const { spawn } = fakeSpawn(exitsWith(""));
  const { ctx, state } = fakeCtx();
  const server = createServer(ctx, { ...BASE_DEPS, spawn });

  server.dispose();
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(state.registrationDisposed, true);
});

test("default export keeps the { id, server } shape", () => {
  assert.equal(plugin.id, "brain-handoff");
  assert.equal(typeof plugin.server, "function");
});
