// Paseo helper (paseohelper.go): one long-lived connection to the Paseo daemon that serves the
// factory's inspect, agent update, send and archive calls without starting a CLI per call.
//
// Usage: node paseo-helper.mjs <@getpaseo/cli package dir>
//
// It connects with the installed CLI's own client code (its bundled @getpaseo/client, daemon
// selection, credential and client id) and serves only when the daemon reports the CLI's version.
// The first stdout line is {"ready":true,"cli":v,"daemon":v} or {"ready":false,"error":msg}.
// Then each stdin line is one request, {id, op, agent, json, name, labels, text, images, force},
// answered on stdout by {id, ok, out} or {id, ok:false, error, unsent, lost}, in any order:
//   - out is what the CLI prints on stdout with --json; error is what it prints on stderr;
//   - unsent: the request never reached the daemon (a send can safely go through the CLI);
//   - lost: the connection dropped, so the answer is not the daemon's.
// Each operation is the CLI command's logic (dist/commands/agent/*.js) on the shared client.
// The helper exits when the daemon connection drops, and when stdin closes once it has answered
// the requests it read.
import { readFile } from "node:fs/promises";
import { extname, join } from "node:path";
import { pathToFileURL } from "node:url";
import { createInterface } from "node:readline";

const cliDir = process.argv[2];
const write = (msg) => process.stdout.write(JSON.stringify(msg) + "\n");

const PARENT_AGENT_ID_LABEL = "paseo.parent-agent-id";

function normalizeModelId(value) {
  if (typeof value !== "string") return null;
  const normalized = value.trim();
  if (!normalized || normalized.toLowerCase() === "default") return null;
  return normalized;
}

function buildLastUsage(s) {
  if (!s.lastUsage) return null;
  return {
    InputTokens: s.lastUsage.inputTokens ?? 0,
    OutputTokens: s.lastUsage.outputTokens ?? 0,
    CachedTokens: s.lastUsage.cachedInputTokens ?? 0,
    CostUsd: s.lastUsage.totalCostUsd ?? 0,
  };
}

function buildCapabilities(s) {
  if (!s.capabilities) return null;
  return {
    Streaming: s.capabilities.supportsStreaming ?? false,
    Persistence: s.capabilities.supportsSessionPersistence ?? false,
    DynamicModes: s.capabilities.supportsDynamicModes ?? false,
    McpServers: s.capabilities.supportsMcpServers ?? false,
  };
}

// toInspectData is the CLI's `paseo inspect --json` shape.
export function toInspectData(s) {
  return {
    Id: s.id,
    Name: s.title ?? "-",
    Provider: s.provider,
    Model: normalizeModelId(s.runtimeInfo?.model) ?? normalizeModelId(s.model) ?? "-",
    Thinking: s.effectiveThinkingOptionId ?? "auto",
    Status: s.status,
    Archived: s.archivedAt != null,
    ArchivedAt: s.archivedAt ?? null,
    Mode: s.currentModeId ?? "default",
    Cwd: s.cwd,
    CreatedAt: s.createdAt,
    UpdatedAt: s.updatedAt,
    LastUsage: buildLastUsage(s),
    Capabilities: buildCapabilities(s),
    AvailableModes: s.availableModes ? s.availableModes.map((m) => ({ id: m.id, label: m.label })) : null,
    PendingPermissions: (s.pendingPermissions ?? []).map((p) => ({ id: p.id, tool: p.name ?? "unknown" })),
    Worktree: s.labels?.["paseo.worktree"] ?? null,
    ParentAgentId: s.labels?.[PARENT_AGENT_ID_LABEL] ?? null,
  };
}

// parseLabelOptions is the CLI's --label parser: comma-separated key=value pairs.
export function parseLabelOptions(labels) {
  const parsed = {};
  for (const rawLabel of labels ?? []) {
    for (const segment of rawLabel.split(",")) {
      const label = segment.trim();
      if (!label) continue;
      const eqIndex = label.indexOf("=");
      if (eqIndex === -1) {
        throw { code: "INVALID_LABEL", message: `Invalid label format: ${label}`, details: "Labels must be in key=value format" };
      }
      const key = label.slice(0, eqIndex).trim();
      if (!key) {
        throw {
          code: "INVALID_LABEL",
          message: `Invalid label format: ${label}`,
          details: "Labels must include a non-empty key in key=value format",
        };
      }
      parsed[key] = label.slice(eqIndex + 1);
    }
  }
  return parsed;
}

function formatLabels(labels) {
  const entries = Object.entries(labels ?? {});
  return entries.length === 0 ? "-" : entries.map(([k, v]) => `${k}=${v}`).join(",");
}

const notFound = (agent) => ({
  code: "AGENT_NOT_FOUND",
  message: `Agent not found: ${agent}`,
  details: 'Use "paseo ls" to list available agents',
});

const mimeTypes = { ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp" };

async function readImageFiles(paths) {
  return Promise.all(
    paths.map(async (path) => {
      try {
        const buffer = await readFile(path);
        return { data: buffer.toString("base64"), mimeType: mimeTypes[extname(path).toLowerCase()] ?? "image/jpeg" };
      } catch (err) {
        if (err && typeof err === "object" && "code" in err) throw err;
        throw { code: "IMAGE_READ_ERROR", message: `Failed to read image file: ${path}`, details: err instanceof Error ? err.message : String(err) };
      }
    }),
  );
}

async function inspect(client, req) {
  const res = await client.fetchAgent({ agentId: req.agent });
  if (!res) throw notFound(req.agent);
  return toInspectData(res.agent);
}

async function update(client, req) {
  const name = req.name?.trim();
  if (req.name != null && !name) {
    throw { code: "INVALID_NAME", message: "Name cannot be empty", details: "Use --name <name> with a non-empty value" };
  }
  const labels = parseLabelOptions(req.labels);
  if (!name && Object.keys(labels).length === 0) {
    throw {
      code: "NO_CHANGES_PROVIDED",
      message: "Nothing to update",
      details: "Provide at least one of: --name <name>, --label <key=value>, --thinking <id>",
    };
  }
  const res = await client.fetchAgent({ agentId: req.agent });
  if (!res) throw notFound(req.agent);
  const agentId = res.agent.id;
  await client.updateAgent(agentId, { ...(name ? { name } : {}), ...(Object.keys(labels).length > 0 ? { labels } : {}) });
  const after = await client.fetchAgent({ agentId });
  if (!after) throw new Error(`Agent not found after update: ${agentId}`);
  return {
    agentId: after.agent.id,
    name: after.agent.title,
    labels: formatLabels(after.agent.labels),
    thinkingOptionId: after.agent.effectiveThinkingOptionId ?? null,
    noticeType: null,
    notice: null,
  };
}

async function send(client, req) {
  if (!req.text?.trim()) {
    throw {
      code: "MISSING_PROMPT",
      message: "A prompt is required",
      details: "Usage: paseo agent send [options] <id> [prompt] | --prompt <text> | --prompt-file <path>",
    };
  }
  const images = req.images?.length > 0 ? await readImageFiles(req.images) : undefined;
  await client.sendAgentMessage(req.agent, req.text, { images });
  return { agentId: req.agent, status: "sent", message: "Message sent, not waiting for completion" };
}

async function archive(client, req) {
  const res = await client.fetchAgent({ agentId: req.agent });
  if (!res) throw notFound(req.agent);
  const agent = res.agent;
  if (agent.archivedAt) {
    throw {
      code: "AGENT_ALREADY_ARCHIVED",
      message: `Agent ${agent.id.slice(0, 7)} is already archived`,
      details: `Archived at: ${agent.archivedAt}`,
    };
  }
  if (agent.status === "running" && !req.force) {
    throw {
      code: "AGENT_RUNNING",
      message: `Agent ${agent.id.slice(0, 7)} is currently running`,
      details:
        "Use --force to archive a running agent (it will interrupt the active run), or stop it first with: paseo agent stop. Use paseo agent delete to hard-delete it.",
    };
  }
  const result = await client.archiveAgent(agent.id);
  return { agentId: agent.id, status: "archived", archivedAt: result.archivedAt };
}

const ops = { inspect, update, send, archive };
const failed = { inspect: "INSPECT_FAILED", update: "UPDATE_FAILED", send: "SEND_FAILED", archive: "ARCHIVE_FAILED" };
const failedText = { inspect: "inspect agent", update: "update agent", send: "send message", archive: "archive agent" };

// commandError is the CLI's error for err, which the command wrapped the same way.
function commandError(op, err) {
  if (err && typeof err === "object" && "code" in err && typeof err.code === "string" && typeof err.message === "string") {
    return { code: err.code, message: err.message, details: err.details };
  }
  const message = err instanceof Error ? err.message : String(err);
  return { code: failed[op], message: `Failed to ${failedText[op]}: ${message}` };
}

// renderError is what the CLI prints on stderr (no colour: the factory reads it from a pipe).
export function renderError(error, json) {
  if (json) return JSON.stringify({ error }, null, 2);
  return typeof error.details === "string" && error.details ? `Error: ${error.message}\n${error.details}` : `Error: ${error.message}`;
}

async function main() {
  const load = (rel) => import(pathToFileURL(join(cliDir, rel)).href);
  let client;
  try {
    const pkg = JSON.parse(await readFile(join(cliDir, "package.json"), "utf8"));
    if (pkg.name !== "@getpaseo/cli") throw new Error(`${cliDir} is not @getpaseo/cli`);
    const { connectToDaemon } = await load("dist/utils/client.js");
    const { selectDaemonTarget } = await load("dist/utils/daemon-target.js");
    client = await connectToDaemon({ target: selectDaemonTarget({}) });
    const daemon = client.getLastServerInfoMessage()?.version;
    if (daemon !== pkg.version) {
      await client.close().catch(() => {});
      write({ ready: false, error: `daemon ${daemon ?? "unknown"} does not match CLI ${pkg.version}` });
      process.exit(1);
    }
    write({ ready: true, cli: pkg.version, daemon });
  } catch (err) {
    write({ ready: false, error: err?.message ?? String(err) });
    process.exit(1);
  }

  let exiting = false;
  const exit = async () => {
    if (exiting) return;
    exiting = true;
    await client.close().catch(() => {});
    process.exit(0);
  };
  client.subscribeConnectionStatus((state) => {
    if (state.status !== "connected") exit();
  });
  let inflight = 0;
  let inputClosed = false;
  const lines = createInterface({ input: process.stdin });
  lines.on("line", async (line) => {
    if (!line.trim()) return;
    let req;
    try {
      req = JSON.parse(line);
    } catch {
      return;
    }
    if (!client.isConnected) {
      write({ id: req.id, ok: false, unsent: true, lost: true, error: "not connected" });
      return;
    }
    const op = ops[req.op];
    if (!op) {
      write({ id: req.id, ok: false, unsent: true, error: `unknown op ${req.op}` });
      return;
    }
    inflight++;
    try {
      const data = await op(client, req);
      write({ id: req.id, ok: true, out: JSON.stringify(data, null, 2) + "\n" });
    } catch (err) {
      write({ id: req.id, ok: false, lost: !client.isConnected, error: renderError(commandError(req.op, err), req.json) });
    }
    inflight--;
    if (inputClosed && inflight === 0) exit();
  });
  // input closed: answer the requests already read, then exit
  lines.on("close", () => {
    inputClosed = true;
    if (inflight === 0) exit();
  });
}

main();
