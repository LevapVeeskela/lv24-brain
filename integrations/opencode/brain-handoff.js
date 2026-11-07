/**
 * LV24 Brain OpenCode handoff hook template.
 *
 * OpenCode loads the default export and calls `server(ctx)`. The returned
 * object's `event` hook runs `brain session end --hook opencode` when a session
 * goes idle or closes. The engine writes a handoff only when something was
 * recorded since the latest one and no handoff was written in the last 30
 * minutes, so idle checkpoints no longer pile up placeholder handoffs.
 * Every failure is swallowed so the brain CLI can never break an OpenCode
 * session.
 *
 * Placeholders (replaced by installers/windows/install.ps1):
 *   __BRAIN_BIN__       absolute path to brain.exe (or `brain` on PATH)
 *   __BRAIN_CONFIG__    path to brain.yaml
 *
 * Environment overrides are supported: BRAIN_BIN, BRAIN_CONFIG.
 *
 * Hook evidence (OpenCode 2.1.4):
 *   - default export shape: { id, server(ctx) }
 *   - server(ctx) returns { event: async ({ event }) => ... }
 *   - event.type values include session.idle, session.deleted, global.disposed.
 *   - There is no session.end / session.ended event in this build.
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

function isConfigured() {
  return Boolean(BRAIN_BIN) && !BRAIN_BIN.startsWith("__");
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

function handoffArgs(directory) {
  const args = ["session", "end", "--hook", "opencode", "--cwd", directory];
  if (BRAIN_CONFIG && !BRAIN_CONFIG.startsWith("__")) {
    args.push("--config", BRAIN_CONFIG);
  }
  return args;
}

function runBrainHandoff(directory) {
  return new Promise((resolvePromise) => {
    let child;
    try {
      child = spawn(BRAIN_BIN, handoffArgs(directory), {
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
      try {
        child.kill();
      } catch {
        // Best effort only.
      }
      finish();
    }, HANDOFF_TIMEOUT_MS);

    child.once("error", finish);
    child.once("exit", finish);
  });
}

function createBrainHandoffServer(ctx) {
  let disposed = false;
  let lastHandoffAt = 0;

  return {
    event: async ({ event } = {}) => {
      if (disposed || !isConfigured()) return;
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
      await runBrainHandoff(projectDirectory);
    },

    dispose: () => {
      disposed = true;
    },
  };
}

export default {
  id: "brain-handoff",
  server: createBrainHandoffServer,
};
