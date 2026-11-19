import { request } from "node:http";
import { homedir } from "node:os";
import { join } from "node:path";
import type { RpcInput, RpcOutput } from "@getpaseo/plugin";
import { z } from "zod";
import { agentDocument, type changeAgent, type readAgent } from "../shared/quotas";

const errorBody = z.object({ error: z.string() });
const compaction = z.object({ state: z.string() });

function socketPath(): string {
  const state = process.env.XDG_STATE_HOME || join(homedir(), ".local", "state");
  const directory = process.env.AGENT_QUOTAS_STATE_DIR || join(state, "agent-quotas");
  return process.env.AGENT_QUOTAS_SOCKET || join(directory, "agent-quotas.sock");
}

function call(agentId: string, action = "", body?: object): Promise<unknown> {
  const home = process.env.PASEO_HOME || join(homedir(), ".paseo");
  const path = `/v1/agents/${encodeURIComponent(agentId)}${action}?home=${encodeURIComponent(home)}`;
  const data = body === undefined ? undefined : JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const req = request({
      socketPath: socketPath(),
      path,
      method: action === "" ? "GET" : "POST",
      headers: data === undefined ? undefined : {
        "Content-Type": "application/json",
        "Content-Length": Buffer.byteLength(data),
      },
      timeout: 75_000,
    }, (res) => {
      let text = "";
      res.setEncoding("utf8");
      res.on("data", (chunk: string) => {
        text += chunk;
        if (text.length > 1024 * 1024) req.destroy(new Error("agent-quotas response is too large."));
      });
      res.on("error", reject);
      res.on("end", () => {
        try {
          const value: unknown = JSON.parse(text);
          if (res.statusCode === 200) {
            resolve(value);
            return;
          }
          const result = errorBody.safeParse(value);
          reject(new Error(result.success ? result.data.error : `agent-quotas returned HTTP ${res.statusCode}.`));
        } catch {
          reject(new Error("agent-quotas returned invalid JSON."));
        }
      });
    });
    req.on("timeout", () => req.destroy(new Error("Timed out waiting for agent-quotas.")));
    req.on("error", (err: NodeJS.ErrnoException) => {
      if (err.code === "ENOENT" || err.code === "ECONNREFUSED") {
        reject(new Error("agent-quotas is not running on this host."));

      } else {
        reject(err);
      }
    });
    req.end(data);
  });
}

export async function read({ agentId }: RpcInput<typeof readAgent>): Promise<RpcOutput<typeof readAgent>> {
  return agentDocument.parse(await call(agentId));
}

export async function change({ agentId, action }: RpcInput<typeof changeAgent>): Promise<RpcOutput<typeof changeAgent>> {
  if (action.kind === "compact") {
    const { state } = compaction.parse(await call(agentId, "/compact"));
    return { kind: "compaction", state };
  }
  const body = action.kind === "auto-continue" ? { enabled: action.enabled }
    : action.kind === "policy" ? { cache: action.cache } : {};
  const document = agentDocument.parse(await call(agentId, "/" + action.kind, body));
  return { kind: "settings", document };
}
