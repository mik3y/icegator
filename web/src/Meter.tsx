import type { Level, Verdict } from "./api";
import { useLevel } from "./live";

// The scale runs from FLOOR dBFS at the left edge to full scale at the right.
const FLOOR = -54;
const MARKS = [-48, -36, -24, -12, -6, 0];

const position = (db: number) =>
  Math.min(100, Math.max(0, ((db - FLOOR) / -FLOOR) * 100));

/** Whole decibels with a true minus sign. */
const decibels = (db: number) => `${Math.round(db)} dB`.replace("-", "\u2212");

const VERDICT_TEXT: Record<Verdict, string> = {
  silent: "Silent",
  low: "Too quiet",
  good: "Good level",
  high: "Too hot",
};

function Channel({
  label,
  rms,
  peak,
}: {
  label: string;
  rms: number;
  peak: number;
}) {
  return (
    <div className="channel">
      <span aria-hidden>{label}</span>
      <div className="bar">
        <div
          className="fill"
          style={{ clipPath: `inset(0 ${100 - position(rms)}% 0 0)` }}
        />
        {peak > FLOOR && (
          <div
            className="peak"
            style={{ left: `calc(${position(peak)}% - 2px)` }}
          />
        )}
      </div>
    </div>
  );
}

function describe(level: Level | undefined): string {
  if (!level) return "no audio measured yet";
  return `${VERDICT_TEXT[level.verdict]}, ${decibels(level.loudness)}`;
}

/**
 * A stereo level meter for one DJ. The large size adds the dB scale and is
 * meant for the on-air strip.
 */
export default function Meter({
  sessionId,
  size = "small",
}: {
  sessionId: string;
  size?: "small" | "large";
}) {
  const level = useLevel(sessionId);
  const rms = level?.rms ?? [-120, -120];
  const peak = level?.peak ?? [-120, -120];
  return (
    <div
      className={`meter ${size}`}
      role="img"
      aria-label={`Level, ${describe(level)}`}
    >
      <Channel label="L" rms={rms[0]} peak={peak[0]} />
      <Channel label="R" rms={rms[1]} peak={peak[1]} />
      {size === "large" && (
        <div className="scale" aria-hidden>
          {MARKS.map((db) => (
            <span key={db} style={{ left: `${position(db)}%` }}>
              {String(db).replace("-", "\u2212")}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

/** The verdict in words, with the measured loudness beside it. */
export function LevelVerdict({ sessionId }: { sessionId: string }) {
  const level = useLevel(sessionId);
  if (!level) return <p className="verdict soft">Measuring…</p>;
  return (
    <p className="verdict">
      <b className={level.verdict}>{VERDICT_TEXT[level.verdict]}</b>{" "}
      <span className="soft">{decibels(level.loudness)}</span>
    </p>
  );
}
