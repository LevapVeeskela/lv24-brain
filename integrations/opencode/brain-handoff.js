/**
 * LV24 Brain OpenCode hook template.
 *
 * One file for both OpenCode runtimes. V1 loads the default export and calls
 * `server(ctx)`; V2 requires `setup(ctx)` and ignores `server` (V1 does the
 * opposite). Two hooks in both runtimes:
 *
 *   - Session start: the first `context` call of a session runs
 *     `brain session start --hook opencode` once and appends its plain-text
 *     output to the system prompt. Later calls of the same session append the
 *     cached text again (the system prompt is rebuilt every turn), but the
 *     command never runs twice for one session. Empty output, errors and a
 *     10 s timeout inject nothing.
 *   - Session end: the event stream runs `brain session end --hook opencode`
 *     when a session goes idle or closes. The engine writes a handoff only when
 *     something was recorded since the latest one and no handoff was written
 *     in the last 30 minutes, so idle checkpoints no longer pile up placeholder
 *     handoffs.
 *
 * Every failure is swallowed so the brain CLI can never break an OpenCode
 * session. Builds without `ctx.session.hook` skip injection and keep the end
 * handoff; V2 builds without `ctx.event.subscribe` keep the start injection.
 *
 * Placeholders (replaced by installers/windows/install.ps1):
 *   __BRAIN_BIN__       absolute path to brain.exe (or `brain` on PATH)
 *   __BRAIN_CONFIG__    path to brain.yaml
 *
 * Environment overrides are supported: BRAIN_BIN, BRAIN_CONFIG.
 *
 * Hook evidence:
 *   - OpenCode 2.1.4 (V1): default export { id, server(ctx) }; server returns
 *     { event: async ({ event }) => ..., dispose }; event types include
 *     session.idle, session.deleted, global.disposed; ctx.session.hook
 *     ("context", fn) receives { sessionID, system }.
 *   - OpenCode 2.2.x (V2): default export { id, setup(ctx) }; setup registers
 *     hooks on the context and returns a cleanup function; events come from
 *     ctx.event.subscribe({ signal }) as raw { type, data, location } values
 *     (session.idle / session.deleted stay, global.disposed is V1-only);
 *     ctx.session.hook("context", fn) receives { sessionID, system } and
 *     system is an array of { type: "text", text } parts.
 */

import { spawn } from "node:child_process";
import { resolve, sep } from "node:path";

const BRAIN_BIN = process.env.BRAIN_BIN || "__BRAIN_BIN__";
const BRAIN_CONFIG = process.env.BRAIN_CONFIG || "__BRAIN_CONFIG__";

// Idle is a checkpoint; deleted/disposed are final signals.
const HANDOFF_EVENTS = new Set([
  "session.idle",
  "session.deleted",
  "global.disposed",
]);
const FINAL_EVENTS = new Set(["session.deleted", "global.disposed"]);

const IDLE_DEBOUNCE_MS = 10 * 60 * 1000;
const FINAL_DEBOUNCE_MS = 60 * 1000;
const HANDOFF_TIMEOUT_MS = 30 * 1000;
const START_TIMEOUT_MS = 10 * 1000;
const MAX_TRACKED_SESSIONS = 200;

// V2 event streams end or throw when the shared server restarts; reconnect
// with a bounded doubling delay that resets once a real event arrives.
const V2_EVENT_RETRY_MIN_MS = 50;
const V2_EVENT_RETRY_MAX_MS = 5000;

const DEFAULT_DEPS = {
  spawn,
  bin: BRAIN_BIN,
  config: BRAIN_CONFIG,
  startTimeoutMs: START_TIMEOUT_MS,
};

function isConfigured(bin) {
  return Boolean(bin) && !bin.startsWith("__");
}

function normalizePath(value) {
  try {
    return resolve(value).replace(/[\\/]+$/, "").toLowerCase();
  } catch {
    return "";
  }
}

function isRelatedLocation(eventDirectory, projectDirectory) {
  if (!eventDirectory || !projectDirectory) return true;
  const a = normalizePath(eventDirectory);
  const b = normalizePath(projectDirectory);
  if (!a || !b) return true;
  return a === b || a.startsWith(b + sep) || b.startsWith(a + sep);
}

function resolveProjectDirectory(ctx, event) {
  const fromContext = ctx?.location?.directory;
  if (typeof fromContext === "string" && fromContext) return fromContext;
  const fromEvent = event?.location?.directory;
  if (typeof fromEvent === "string" && fromEvent) return fromEvent;
  if (typeof ctx?.directory === "string" && ctx.directory) return ctx.directory;
  return process.cwd();
}

/** Builds `brain session <subcommand> --hook opencode --cwd <dir> [--config cfg]`. */
function brainArgs(subcommand, directory, config) {
  const args = ["session", subcommand, "--hook", "opencode", "--cwd", directory];
  if (config && !config.startsWith("__")) {
    args.push("--config", config);
  }
  return args;
}

function killQuietly(child) {
  try {
    child?.kill?.();
  } catch {
    // Best effort only.
  }
}

function runBrainHandoff(deps, directory) {
  return new Promise((resolvePromise) => {
    let child;
    try {
      child = deps.spawn(deps.bin, brainArgs("end", directory, deps.config), {
        cwd: directory,
        windowsHide: true,
        stdio: "ignore",
      });
    } catch {
      resolvePromise();
      return;
    }

    let settled = false;
    let timer = null;
    const finish = () => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      resolvePromise();
    };

    timer = setTimeout(() => {
      killQuietly(child);
      finish();
    }, HANDOFF_TIMEOUT_MS);

    child.once("error", finish);
    child.once("exit", finish);
  });
}

function decodeStdout(chunks) {
  return Buffer.concat(chunks).toString("utf8").trim();
}

/** Resolves to the trimmed start context, or "" on any failure. Never rejects. */
function runBrainStart(deps, directory) {
  return new Promise((resolvePromise) => {
    const chunks = [];
    let settled = false;
    let timer = null;
    const finish = (text) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      resolvePromise(text);
    };

    let child;
    try {
      child = deps.spawn(deps.bin, brainArgs("start", directory, deps.config), {
        cwd: directory,
        windowsHide: true,
        stdio: ["ignore", "pipe", "ignore"],
      });
    } catch {
      finish("");
      return;
    }

    timer = setTimeout(() => {
      killQuietly(child);
      finish("");
    }, deps.startTimeoutMs);

    child.stdout?.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
    child.once("error", () => finish(""));
    child.once("close", (code) => finish(code === 0 ? decodeStdout(chunks) : ""));
  });
}

/**
 * Append text to the last system text part, or push a new part when the
 * last part is missing or not text. Mirrors the engram plugin: some models
 * accept only a single system block, so text is concatenated onto it.
 */
function appendSystemText(system, text) {
  const last = system[system.length - 1];
  if (last && last.type === "text" && typeof last.text === "string") {
    last.text = last.text + "\n\n" + text;
    return;
  }
  system.push({ type: "text", text });
}

function rememberSession(map, sessionID, value) {
  map.set(sessionID, value);
  while (map.size > MAX_TRACKED_SESSIONS) {
    map.delete(map.keys().next().value);
  }
}

function deletedSessionId(event) {
  if (event?.type !== "session.deleted") return "";
  const id =
    event.properties?.sessionID ?? event.data?.sessionID ?? event.sessionID;
  return typeof id === "string" ? id : "";
}

/**
 * Builds the plugin instance. `deps` is a test seam:
 * { spawn, bin, config, startTimeoutMs }; defaults come from the template.
 */
export function createServer(ctx, deps = {}) {
  const d = { ...DEFAULT_DEPS, ...deps };
  let disposed = false;
  let lastHandoffAt = 0;
  // sessionID -> Promise<string> of the start context (run once per session).
  const startContexts = new Map();
  let contextRegistration = null;

  const getStartContext = (directory, sessionID) => {
    let pending = startContexts.get(sessionID);
    if (!pending) {
      pending = runBrainStart(d, directory);
      rememberSession(startContexts, sessionID, pending);
    }
    return pending;
  };

  const onSystemContext = async (event) => {
    try {
      if (disposed || !isConfigured(d.bin)) return;
      const system = event?.system;
      const sessionID = event?.sessionID;
      if (!Array.isArray(system)) return;
      if (typeof sessionID !== "string" || !sessionID) return;

      const directory = resolveProjectDirectory(ctx, event);
      const text = await getStartContext(directory, sessionID);
      if (text) appendSystemText(system, text);
    } catch {
      // Never let context injection break an OpenCode session.
    }
  };

  if (isConfigured(d.bin) && typeof ctx?.session?.hook === "function") {
    try {
      contextRegistration = Promise.resolve(
        ctx.session.hook("context", onSystemContext)
      ).catch(() => null);
    } catch {
      contextRegistration = null;
    }
  }

  return {
    event: async (input) => {
      try {
        const event = input?.event;
        const forgotten = deletedSessionId(event);
        if (forgotten) startContexts.delete(forgotten);

        if (disposed || !isConfigured(d.bin)) return;
        if (!HANDOFF_EVENTS.has(event?.type)) return;

        const projectDirectory = resolveProjectDirectory(ctx, event);
        if (!projectDirectory) return;

        const eventDirectory = event?.location?.directory;
        if (!isRelatedLocation(eventDirectory, projectDirectory)) return;

        const now = Date.now();
        const debounceMs = FINAL_EVENTS.has(event.type)
          ? FINAL_DEBOUNCE_MS
          : IDLE_DEBOUNCE_MS;
        if (now - lastHandoffAt < debounceMs) return;

        lastHandoffAt = now;
        await runBrainHandoff(d, projectDirectory);
      } catch {
        // Handoff failures are never surfaced to OpenCode.
      }
    },

    dispose: () => {
      disposed = true;
      startContexts.clear();
      contextRegistration
        ?.then((registration) => registration?.dispose?.())
        .catch(() => {});
    },
  };
}

function delayUnlessAborted(ms, signal) {
  return new Promise((resolvePromise) => {
    if (signal.aborted) {
      resolvePromise();
      return;
    }
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolvePromise();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
  });
}

/**
 * OpenCode V2 entrypoint. V2 calls `setup(ctx)` and registers hooks on the
 * context instead of accepting a `{ event }` object, so the same core runs
 * behind an event subscription. Returns the cleanup V2 expects.
 */
export async function setupV2(ctx, deps = {}) {
  const server = createServer(ctx, deps);
  const controller = new AbortController();

  if (typeof ctx?.event?.subscribe !== "function") {
    return () => server.dispose();
  }

  const listening = (async () => {
    let retryMs = V2_EVENT_RETRY_MIN_MS;
    while (!controller.signal.aborted) {
      try {
        for await (const event of ctx.event.subscribe({ signal: controller.signal })) {
          if (controller.signal.aborted) break;
          // Every subscription opens with a handshake; only a real event
          // proves the stream is healthy enough to reset the backoff.
          if (event?.type !== "server.connected") retryMs = V2_EVENT_RETRY_MIN_MS;
          await server.event({ event });
        }
      } catch {
        // The shared server went away; reconnect below.
      }
      if (controller.signal.aborted) break;
      await delayUnlessAborted(retryMs, controller.signal);
      retryMs = Math.min(retryMs * 2, V2_EVENT_RETRY_MAX_MS);
    }
  })();

  return async () => {
    controller.abort();
    await listening;
    server.dispose();
  };
}

export default {
  id: "brain-handoff",
  server: (ctx) => createServer(ctx),
  setup: (ctx) => setupV2(ctx),
};
