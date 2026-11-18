/** Elapsed time as a clock reading, such as 4:07 or 1:12:30. */
export function elapsed(fromIso: string, now: number): string {
  const total = Math.max(
    0,
    Math.floor((now - new Date(fromIso).getTime()) / 1000),
  );
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

export function streamFormat(stats: {
  bitrate: number;
  sample_rate: number;
  channels: number;
}): string {
  if (!stats.bitrate) return "No audio yet";
  const khz = (stats.sample_rate / 1000).toFixed(
    stats.sample_rate % 1000 ? 1 : 0,
  );
  return `${Math.round(stats.bitrate / 1000)} kbps, ${khz} kHz ${stats.channels === 1 ? "mono" : "stereo"}`;
}

/** The address without its port. */
export function host(remote: string): string {
  const i = remote.lastIndexOf(":");
  return i > 0 ? remote.slice(0, i) : remote;
}

/** A time of day in the viewer's own zone, such as 9:00 PM. */
export function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString([], {
    hour: "numeric",
    minute: "2-digit",
  });
}

/** A slot's span, such as 8:00 PM to 9:00 PM. */
export function span(start: string, end: string): string {
  return `${clock(start)} to ${clock(end)}`;
}
