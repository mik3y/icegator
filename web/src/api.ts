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
  /** The shows this DJ hosts, when there is a schedule. */
  shows?: Show[];
}

export interface Show {
  id: string;
  name: string;
}

export interface Slot {
  id: string;
  show: Show;
  start: string;
  end: string;
  hosts: string[] | null;
  /** Display names for `hosts`, in the same order, when known. */
  host_names?: string[];
}

export interface Info {
  station: string;
  /** Where the DJs come from. */
  provider: "static" | "streamlover";
  /** Whether the provider supplies a schedule. */
  scheduled: boolean;
  /** Whether a DJ with a claim to the air takes it without an operator. */
  policy: "auto" | "manual";
  /** With a schedule, "none", "strict" or a duration such as "10m". */
  overrun?: string;
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
  /** The session a handoff would put on air now, or "". */
  handoff_to: string;
  // The rest is set only when there is a schedule.
  show?: Show;
  /** An operator put the DJ on air off-schedule. */
  forced?: boolean;
  /** The end of the on-air DJ's show, once it has passed. */
  overrun_since?: string;
  /** When the overrunning DJ will be taken off, if they will be. */
  cut_at?: string;
  current?: Slot;
  next?: Slot;
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

const real = {
  me: () => call<{ username: string; info: Info }>("GET", "/api/me"),
  login: (username: string, password: string) =>
    call<{ username: string }>("POST", "/api/login", { username, password }),
  logout: () => call("POST", "/api/logout"),
  handoff: () => call("POST", "/api/handoff"),
  air: (id: string, showId?: string) =>
    call(
      "POST",
      `/api/sessions/${id}/air`,
      showId ? { show_id: showId } : undefined,
    ),
  pull: (id: string) => call("POST", `/api/sessions/${id}/pull`),
  kick: (id: string) => call("POST", `/api/sessions/${id}/kick`),
};

type Api = typeof real;

// In sim mode (`bun run sim`) the calls go to a station simulated in the
// browser. The comparison is spelled out here, and not shared, so that the
// real build can see it is false and leave the simulator out.
const simulated = (): Api =>
  Object.fromEntries(
    Object.keys(real).map((name) => [
      name,
      async (...args: unknown[]) => {
        const sim = await import("./sim/sim");
        return (sim.api as Record<string, (...a: unknown[]) => unknown>)[name](
          ...args,
        );
      },
    ]),
  ) as unknown as Api;

export const api: Api = import.meta.env.MODE === "sim" ? simulated() : real;
