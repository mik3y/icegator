// A stand-in for Icegator that runs in the browser, for working on the
// dashboard without a server or any streams. It is loaded only when vite
// runs in "sim" mode (`bun run sim`) and is left out of the real build.
//
// It keeps a made-up station, answers the dashboard's API calls against
// it, and feeds the live stream of statuses and meter readings. The rules
// for who goes on air are a rough copy of the server's, close enough to
// click through. The toolbar in Toolbar.tsx changes the station directly.

import {
  ApiError,
  type Dj,
  type DjState,
  type Level,
  type Show,
  type Slot,
  type Status,
  type Verdict,
} from "../api";

/** How a simulated DJ's audio is doing. */
export type Audio = Verdict | "connecting";

export interface SimDj {
  id: string;
  username: string;
  name: string;
  show: Show;
  state: DjState;
  audio: Audio;
  title: string;
  connectedAt: number;
}

export interface Settings {
  scheduled: boolean;
  policy: "auto" | "manual";
  link: "live" | "connecting" | "retrying";
  recording: boolean;
  /** Whether the DJ on air has run past the end of their show. */
  overrun: "no" | "unlimited" | "limited";
  /** Whether the page has lost contact with the server. */
  lost: boolean;
}

export interface Sim {
  djs: SimDj[];
  settings: Settings;
  signedIn: boolean;
}

const ROSTER = [
  { username: "nightowl", name: "Night Owl", show: "Late Shift" },
  { username: "mina", name: "Mina Park", show: "Morning Glory" },
  { username: "djcool", name: "DJ Cool", show: "Cool Runnings" },
  { username: "rosa", name: "Rosa Luz", show: "Onda Tropical" },
  {
    username: "theo",
    name: "Theo & The Long Name Collective",
    show: "Deep Cuts",
  },
  { username: "sam", name: "Sam", show: "Static" },
];

const TRACKS = [
  "Floating Points - Silhouettes",
  "Alice Coltrane - Journey in Satchidananda",
  "Burial - Archangel",
  "Khruangbin - Maria También",
  "A Very Long Artist Name Featuring Several Guests - A Track Title That Runs On",
  "Untitled",
];

const MINUTE = 60_000;
const started = Date.now();
let serial = 0;

let sim: Sim = {
  djs: [],
  settings: {
    scheduled: false,
    policy: "manual",
    link: "live",
    recording: false,
    overrun: "no",
    lost: false,
  },
  signedIn: true,
};

// What the DJ on air went on with. Kept apart from the DJs because it
// belongs to the turn, as it does on the server.
let turn: { show?: Show; forced: boolean; since: number } = {
  forced: false,
  since: started,
};

const showOf = (i: number): Show => ({
  id: `show_${ROSTER[i].username}`,
  name: ROSTER[i].show,
});

// Whose show the schedule is built around, as a place in the roster. It
// is whoever was on air when the schedule was last set up, so that the
// toolbar's choice about overrunning applies to the DJ on screen.
let anchor = 0;

/** The schedule around now, which runs through the roster in order. */
function slots(): { current: Slot; next: Slot; ended?: number } {
  const slot = (place: number, from: number): Slot => {
    const i = (anchor + place) % ROSTER.length;
    return {
      id: `slot_${i}`,
      show: showOf(i),
      start: new Date(started + from * MINUTE).toISOString(),
      end: new Date(started + (from + 60) * MINUTE).toISOString(),
      hosts: [ROSTER[i].username],
      host_names: [ROSTER[i].name],
    };
  };
  if (sim.settings.overrun === "no") {
    return { current: slot(0, -20), next: slot(1, 40) };
  }
  // The first show is over, and the next one's hour has begun.
  return {
    current: slot(1, -4),
    next: slot(2, 56),
    ended: started - 4 * MINUTE,
  };
}

const onAir = () => sim.djs.find((d) => d.state === "on_air");
const ready = (d: SimDj) => d.state === "queued" && d.audio !== "connecting";

/** The DJ who would take the air now, were someone to let them. */
function claimant(): SimDj | undefined {
  if (!sim.settings.scheduled) return sim.djs.find(ready);
  const hosts = slots().current.hosts ?? [];
  if (hosts.includes(onAir()?.username ?? "")) return undefined;
  return sim.djs.find((d) => ready(d) && hosts.includes(d.username));
}

function putOnAir(dj: SimDj, show?: Show, forced = false) {
  for (const d of sim.djs) {
    if (d.state === "on_air") d.state = "held";
  }
  dj.state = "on_air";
  turn = { show, forced, since: Date.now() };
}

/** Applies the rules after any change. */
function settle() {
  const next = claimant();
  if (next && (!onAir() || sim.settings.policy === "auto")) {
    putOnAir(next, sim.settings.scheduled ? slots().current.show : undefined);
  }
}

function status(): Status {
  const { settings } = sim;
  const air = onAir();
  const schedule = settings.scheduled ? slots() : undefined;
  const overrunning =
    schedule?.ended !== undefined &&
    air !== undefined &&
    !turn.forced &&
    !schedule.current.hosts?.includes(air.username);
  const fallback =
    air && turn.show ? `${turn.show.name} with ${air.name}` : air?.name;
  return {
    info: {
      station: "Radio Simulated",
      provider: settings.scheduled ? "streamlover" : "static",
      scheduled: settings.scheduled,
      policy: settings.policy,
      overrun: settings.scheduled ? "none" : undefined,
      version: "sim",
    },
    djs: sim.djs.map(
      (d): Dj => ({
        session_id: d.id,
        username: d.username,
        name: d.name,
        state: d.audio === "connecting" ? "connecting" : d.state,
        remote: `203.0.113.${(Number.parseInt(d.id, 36) % 200) + 10}:51234`,
        user_agent: "butt 1.45.0",
        connected_at: new Date(d.connectedAt).toISOString(),
        title: d.title,
        stats: {
          format: "MPEG-1 Layer 3",
          bitrate: d.audio === "connecting" ? 0 : 192000,
          avg_bitrate: d.audio === "connecting" ? 0 : 192000,
          sample_rate: 44100,
          channels: 2,
          audio_ns: 0,
          last_data: new Date().toISOString(),
        },
        level: null,
        shows: settings.scheduled ? [d.show] : undefined,
      }),
    ),
    on_air: air?.id ?? "",
    link: air ? settings.link : undefined,
    link_error:
      air && settings.link === "retrying"
        ? "dial tcp 203.0.113.5:8005: connection refused"
        : undefined,
    live_since: air ? new Date(turn.since).toISOString() : undefined,
    title: air ? air.title || fallback : undefined,
    recording:
      settings.recording && !!air && (!settings.scheduled || !!turn.show),
    handoff_to: (air && claimant()?.id) || "",
    show: air ? turn.show : undefined,
    forced: air && settings.scheduled ? turn.forced : undefined,
    overrun_since:
      overrunning && schedule?.ended
        ? new Date(schedule.ended).toISOString()
        : undefined,
    cut_at:
      overrunning && settings.overrun === "limited"
        ? new Date(started + 6 * MINUTE).toISOString()
        : undefined,
    current: schedule?.current,
    next: schedule?.next,
  };
}

// --- Meter readings

const TARGET: Record<Verdict, number> = {
  silent: -90,
  low: -31,
  good: -15,
  high: -3,
};
const loudness = new Map<string, number>();

function levels(): Record<string, Level> {
  const out: Record<string, Level> = {};
  const jitter = (by: number) => (Math.random() - 0.5) * 2 * by;
  for (const d of sim.djs) {
    if (d.audio === "connecting") continue;
    const target = TARGET[d.audio];
    const loud =
      (loudness.get(d.id) ?? target) * 0.7 + (target + jitter(3)) * 0.3;
    loudness.set(d.id, loud);
    const still = d.audio === "silent";
    const rms: [number, number] = still
      ? [-90, -90]
      : [loud + jitter(5), loud + jitter(5)];
    out[d.id] = {
      rms,
      peak: [
        Math.min(0, rms[0] + 4 + Math.random() * 5),
        Math.min(0, rms[1] + 4 + Math.random() * 5),
      ],
      loudness: loud,
      verdict: d.audio,
    };
  }
  return out;
}

// --- The two ends the dashboard talks to

interface Feed {
  status: (s: Status) => void;
  levels: (l: Record<string, Level>) => void;
  connection: (c: "open" | "lost") => void;
}

const feeds = new Set<Feed>();
const watchers = new Set<() => void>();

function publish() {
  sim = { ...sim, djs: [...sim.djs] };
  for (const fn of watchers) fn();
  for (const feed of feeds) {
    feed.connection(sim.settings.lost ? "lost" : "open");
    // A page that has lost contact goes on showing what it last heard.
    if (!sim.settings.lost) feed.status(status());
  }
}

setInterval(() => {
  if (sim.settings.lost) return;
  const reading = levels();
  for (const feed of feeds) feed.levels(reading);
}, 250);

/** Stands in for the live event stream. Returns how to disconnect. */
export function connect(feed: Feed): () => void {
  feeds.add(feed);
  feed.connection(sim.settings.lost ? "lost" : "open");
  feed.status(status());
  return () => feeds.delete(feed);
}

function find(id: string): SimDj {
  const dj = sim.djs.find((d) => d.id === id);
  if (!dj) throw new ApiError(404, "That DJ is no longer connected.");
  return dj;
}

/** Stands in for the API, with the same calls as `api` in ../api. */
export const api = {
  async me() {
    if (!sim.signedIn) throw new ApiError(401, "Not signed in");
    return { username: "operator", info: status().info };
  },
  async login(username: string, password: string) {
    if (!username || !password) {
      throw new ApiError(401, "Wrong username or password.");
    }
    sim.signedIn = true;
    publish();
    return { username };
  },
  async logout() {
    sim.signedIn = false;
    publish();
  },
  async handoff() {
    const next = claimant();
    if (!next) throw new ApiError(409, "Nobody is ready to hand off to.");
    putOnAir(next, sim.settings.scheduled ? slots().current.show : undefined);
    publish();
  },
  async air(id: string, showId?: string) {
    const dj = find(id);
    if (!sim.settings.scheduled) {
      putOnAir(dj);
    } else {
      const current = slots().current;
      const theirs = current.hosts?.includes(dj.username);
      const named = showId === dj.show.id ? dj.show : undefined;
      putOnAir(dj, named ?? (theirs ? current.show : undefined), !theirs);
    }
    publish();
  },
  async pull(id: string) {
    find(id).state = "held";
    settle();
    publish();
  },
  async kick(id: string) {
    actions.drop(id);
  },
};

// --- What the toolbar does

export const actions = {
  /** Connects the next DJ of the roster who is not here yet. */
  add(audio: Audio) {
    const here = new Set(sim.djs.map((d) => d.username));
    const i = ROSTER.findIndex((r) => !here.has(r.username));
    if (i < 0) return;
    serial++;
    sim.djs.push({
      id: (serial * 7919 + 100000).toString(36),
      username: ROSTER[i].username,
      name: ROSTER[i].name,
      show: showOf(i),
      state: "queued",
      audio,
      title: "",
      connectedAt: Date.now(),
    });
    settle();
    publish();
  },
  drop(id: string) {
    sim.djs = sim.djs.filter((d) => d.id !== id);
    loudness.delete(id);
    settle();
    publish();
  },
  clear() {
    sim.djs = [];
    loudness.clear();
    publish();
  },
  audio(id: string, audio: Audio) {
    find(id).audio = audio;
    settle();
    publish();
  },
  /** Has the DJ's software announce another track. */
  track(id: string) {
    const dj = find(id);
    dj.title = TRACKS[(TRACKS.indexOf(dj.title) + 1) % TRACKS.length];
    publish();
  },
  set(change: Partial<Settings>) {
    sim.settings = { ...sim.settings, ...change };
    if (change.scheduled !== undefined || change.overrun !== undefined) {
      // Whoever is on air is taken to be doing their own show, which is
      // the one in progress or the one that has just ended.
      const air = onAir();
      const place = ROSTER.findIndex((r) => r.username === air?.username);
      anchor = Math.max(0, place);
      turn = {
        ...turn,
        show: sim.settings.scheduled ? air?.show : undefined,
        forced: false,
      };
      // Their going over is the thing to look at, so nobody takes it away.
      if (sim.settings.overrun !== "no") return publish();
    }
    settle();
    publish();
  },
};

export const store = {
  subscribe(fn: () => void) {
    watchers.add(fn);
    return () => {
      watchers.delete(fn);
    };
  },
  get: () => sim,
  full: () => sim.djs.length >= ROSTER.length,
};

// Start with something to look at.
actions.add("good");
actions.add("low");
actions.add("connecting");
actions.track(sim.djs[0].id);
