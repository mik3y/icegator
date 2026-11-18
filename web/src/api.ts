// Types and calls for icegator's dashboard API (internal/api in the Go code).

export type DjState = "connecting" | "queued" | "on_air" | "held";
export type Verdict = "silent" | "low" | "good" | "high";

export interface Level {
  rms: [number, number];
  peak: [number, number];
  loudness: number;
  verdict: Verdict;
}

export interface Dj {
  session_id: string;
  username: string;
  name: string;
  state: DjState;
  verified: boolean;
  remote: string;
  user_agent: string;
  connected_at: string;
  title: string;
  stats: {
    format: string;
    bitrate: number;
    avg_bitrate: number;
    sample_rate: number;
    channels: number;
    audio_ns: number;
    last_data: string;
  };
  level: Level | null;
}

export interface Info {
  station: string;
  mode: "standalone" | "streamlover";
  auth: "forward" | "local";
  /** Whether a DJ with a claim to the air takes it without an operator. */
  policy: "auto" | "manual";
}

export interface Status {
  info: Info;
  djs: Dj[];
  on_air: string;
  link?: "connecting" | "live" | "retrying";
  link_error?: string;
  live_since?: string;
  title?: string;
  recording: boolean;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  let resp: Response;
  try {
    resp = await fetch(path, {
      method,
      credentials: "same-origin",
      // The custom header is what lets the server tell this app's requests
      // from a form on another site.
      headers: {
        "X-Icegator": "1",
        ...(body ? { "Content-Type": "application/json" } : {}),
      },
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, "Can't reach Icegator. Check that it is running.");
  }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    throw new ApiError(
      resp.status,
      data.error ?? `Request failed (${resp.status})`,
    );
  }
  return data as T;
}

export const api = {
  me: () => call<{ username: string; info: Info }>("GET", "/api/me"),
  login: (username: string, password: string) =>
    call<{ username: string }>("POST", "/api/login", { username, password }),
  logout: () => call("POST", "/api/logout"),
  handoff: () => call("POST", "/api/handoff"),
  air: (id: string) => call("POST", `/api/sessions/${id}/air`),
  pull: (id: string) => call("POST", `/api/sessions/${id}/pull`),
  kick: (id: string) => call("POST", `/api/sessions/${id}/kick`),
};
