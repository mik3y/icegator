import { useEffect, useState, useSyncExternalStore } from "react";
import type { Level, Status } from "./api";

// Meter readings arrive four times a second. They are kept outside React
// state so that only the meter for a given DJ re-renders on each one,
// rather than the whole console.
const levels = new Map<string, Level>();
const listeners = new Map<string, Set<() => void>>();

function setLevels(next: Record<string, Level>) {
  for (const [id, level] of Object.entries(next)) {
    levels.set(id, level);
    for (const fn of listeners.get(id) ?? []) fn();
  }
  for (const id of levels.keys()) {
    if (!(id in next)) levels.delete(id);
  }
}

export function useLevel(sessionId: string): Level | undefined {
  return useSyncExternalStore(
    (fn) => {
      let set = listeners.get(sessionId);
      if (!set) {
        set = new Set();
        listeners.set(sessionId, set);
      }
      set.add(fn);
      return () => {
        set.delete(fn);
      };
    },
    () => levels.get(sessionId),
  );
}

export type Connection = "connecting" | "open" | "lost" | "signed-out";

/**
 * Subscribes to the live event stream. The browser reconnects by itself
 * after a drop, so "lost" clears without any action here once the server
 * is back.
 */
export function useLive(): { status: Status | null; connection: Connection } {
  const [status, setStatus] = useState<Status | null>(null);
  const [connection, setConnection] = useState<Connection>("connecting");

  useEffect(() => {
    const source = new EventSource("/api/events");
    source.onopen = () => setConnection("open");
    source.addEventListener("status", (e) => {
      const next: Status = JSON.parse((e as MessageEvent).data);
      // An older server sends null here when nobody is connected.
      setStatus({ ...next, djs: next.djs ?? [] });
    });
    source.addEventListener("levels", (e) =>
      setLevels(JSON.parse((e as MessageEvent).data)),
    );
    source.onerror = () => {
      setConnection("lost");
      // A stream that fails with the session gone will never recover, so
      // find out whether that is what happened.
      fetch("/api/me", { credentials: "same-origin" })
        .then((r) => {
          if (r.status === 401) {
            source.close();
            setConnection("signed-out");
          }
        })
        .catch(() => {});
    };
    return () => source.close();
  }, []);

  return { status, connection };
}

/** Re-renders once a second, for clocks and elapsed times. */
export function useNow(): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  return now;
}
