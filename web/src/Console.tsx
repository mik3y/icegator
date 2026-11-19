import { useEffect, useState } from "react";
import { api, type Dj, type Slot, type Status } from "./api";
import { clock, elapsed, host, span, streamFormat } from "./format";
import { useLive, useNow } from "./live";
import Meter, { LevelVerdict } from "./Meter";

type Act = (action: () => Promise<unknown>) => void;

function Facts({
  dj,
  now,
  liveSince,
  recording,
}: {
  dj: Dj;
  now: number;
  liveSince?: string;
  recording?: boolean;
}) {
  return (
    <dl className="facts">
      {liveSince && (
        <div>
          <dt>On air for</dt>
          <dd>{elapsed(liveSince, now)}</dd>
        </div>
      )}
      <div>
        <dt>Connected for</dt>
        <dd>{elapsed(dj.connected_at, now)}</dd>
      </div>
      <div>
        <dt>Stream</dt>
        <dd>{streamFormat(dj.stats)}</dd>
      </div>
      <div>
        <dt>From</dt>
        <dd>{host(dj.remote)}</dd>
      </div>
      {recording && (
        <div>
          <dt>Recording</dt>
          <dd>On</dd>
        </div>
      )}
    </dl>
  );
}

/** What the schedule has to say about the DJ on air, if anything. */
function Standing({ status }: { status: Status }) {
  if (status.forced) {
    return (
      <p className="standing">
        Put on air by an operator, off the schedule.
        {!status.show &&
          " With no show named, this is not being recorded or logged."}
      </p>
    );
  }
  if (!status.overrun_since) return null;
  return (
    <p className="standing warn">
      Their show ended at {clock(status.overrun_since)}.{" "}
      {status.cut_at
        ? `They will be taken off at ${clock(status.cut_at)} unless the next DJ is ready first.`
        : "They stay on until the next show's DJ is ready."}
    </p>
  );
}

function OnAir({
  status,
  dj,
  now,
  act,
}: {
  status: Status;
  dj: Dj;
  now: number;
  act: Act;
}) {
  const live = status.link === "live";
  let lamp = "On air";
  if (status.link === "connecting") lamp = "Connecting to the station";
  if (status.link === "retrying") lamp = "Station connection lost. Retrying.";
  // With no title from the DJ, the station shows a stand-in built from
  // the names already on this strip.
  const standIn =
    status.title === dj.name ||
    (status.show && status.title === `${status.show.name} with ${dj.name}`);
  return (
    <section
      className={`strip ${live ? "live" : "trouble"}`}
      aria-label="On air"
    >
      <div>
        <p className="tally">
          <span className="lamp" aria-hidden />
          {lamp}
        </p>
        {status.link === "retrying" && status.link_error && (
          <p className="small soft">{status.link_error}</p>
        )}
      </div>
      <div>
        {status.show && <p className="show">{status.show.name}</p>}
        <h1>{dj.name}</h1>
        {status.title && !standIn && <p className="title">{status.title}</p>}
        <Standing status={status} />
      </div>
      <div>
        <Meter sessionId={dj.session_id} size="large" />
        <LevelVerdict sessionId={dj.session_id} />
      </div>
      <div className="strip-foot">
        <Facts
          dj={dj}
          now={now}
          liveSince={live ? status.live_since : undefined}
          recording={status.recording}
        />
        <div className="actions">
          <button
            type="button"
            onClick={() => act(() => api.pull(dj.session_id))}
          >
            Take off air
          </button>
          <button
            type="button"
            className="quiet"
            onClick={() => act(() => api.kick(dj.session_id))}
          >
            Disconnect
          </button>
        </div>
      </div>
    </section>
  );
}

function NobodyOnAir({ status }: { status: Status }) {
  const scheduled = status.info.scheduled;
  let detail: string;
  if (!scheduled) {
    detail =
      status.djs.length > 0
        ? "Put one of the DJs below on air when you are ready."
        : "The first DJ to connect goes on air once their audio is flowing.";
  } else if (status.current) {
    detail = `${status.current.show.name} is scheduled now, and none of its DJs is connected and ready.`;
  } else if (status.next) {
    detail = `Nothing is scheduled right now. ${status.next.show.name} starts at ${clock(status.next.start)}.`;
  } else {
    detail = "Nothing is scheduled.";
  }
  return (
    <section className="strip" aria-label="On air">
      <p className="tally soft">
        <span className="lamp" aria-hidden />
        Nobody is on air
      </p>
      <p className="empty">
        Icegator is not connected to the station, so the station is playing its
        own programming. {detail}
      </p>
    </section>
  );
}

function ScheduleLine({
  label,
  slot,
  names,
}: {
  label: string;
  slot?: Slot;
  names: Map<string, string>;
}) {
  if (!slot) {
    return (
      <div>
        <dt>{label}</dt>
        <dd className="soft">Nothing scheduled</dd>
      </div>
    );
  }
  const hosts = slot.hosts ?? [];
  return (
    <div>
      <dt>{label}</dt>
      <dd>
        <b>{slot.show.name}</b>, {span(slot.start, slot.end)}
        <span className="soft">
          {hosts.length > 0
            ? `, with ${hosts.map((h, i) => slot.host_names?.[i] ?? names.get(h) ?? h).join(" and ")}`
            : ", with no DJ assigned"}
        </span>
      </dd>
    </div>
  );
}

/** What a DJ who is not on air is here for, when there is a schedule. */
function claim(dj: Dj, status: Status): string {
  if (status.current?.hosts?.includes(dj.username))
    return `Here for ${status.current.show.name}, which is on now`;
  if (status.next?.hosts?.includes(dj.username))
    return `Here for ${status.next.show.name} at ${clock(status.next.start)}`;
  return "Nothing scheduled soon";
}

function Row({
  dj,
  status,
  place,
  airLabel,
  act,
}: {
  dj: Dj;
  status: Status;
  place?: number;
  airLabel: string;
  act: Act;
}) {
  const scheduled = status.info.scheduled;
  const connecting = dj.state === "connecting";
  // Putting a DJ on air against the schedule can name one of their shows,
  // which is what gets it recorded and logged.
  const [showId, setShowId] = useState("");
  const shows = dj.shows ?? [];
  let detail = dj.title || streamFormat(dj.stats);
  if (connecting) detail = "Waiting for audio";
  else if (scheduled) detail = claim(dj, status);
  return (
    <li className="row">
      <span className="place">{place ?? ""}</span>
      <div>
        <span className="name">{dj.name}</span>
        <p className="small soft">{detail}</p>
      </div>
      <div className="level">
        <Meter sessionId={dj.session_id} />
        <LevelVerdict sessionId={dj.session_id} />
      </div>
      <div className="actions">
        {scheduled && shows.length > 0 && (
          <select
            aria-label={`Show for ${dj.name} to go on air as`}
            value={showId}
            onChange={(e) => setShowId(e.target.value)}
            disabled={connecting}
          >
            <option value="">No show, off the record</option>
            {shows.map((show) => (
              <option key={show.id} value={show.id}>
                As {show.name}
              </option>
            ))}
          </select>
        )}
        <button
          type="button"
          disabled={connecting}
          onClick={() => act(() => api.air(dj.session_id, showId))}
        >
          {airLabel}
        </button>
        <button
          type="button"
          className="quiet"
          onClick={() => act(() => api.kick(dj.session_id))}
        >
          Disconnect
        </button>
      </div>
    </li>
  );
}

export default function Console({
  username,
  onSignedOut,
}: {
  username: string;
  onSignedOut: () => void;
}) {
  const { status, connection } = useLive();
  const now = useNow();
  const [message, setMessage] = useState("");

  useEffect(() => {
    if (connection === "signed-out") onSignedOut();
  }, [connection]);

  useEffect(() => {
    if (!message) return;
    const t = setTimeout(() => setMessage(""), 6000);
    return () => clearTimeout(t);
  }, [message]);

  const act: Act = (action) => {
    action().catch((err: Error) => setMessage(err.message));
  };

  const signOut = () => api.logout().finally(onSignedOut);

  if (!status) {
    return (
      <div className="page">
        <p className="soft">
          {connection === "lost"
            ? "Can't reach Icegator. Retrying."
            : "Loading"}
        </p>
      </div>
    );
  }

  const scheduled = status.info.scheduled;
  const onAir = status.djs.find((d) => d.state === "on_air");
  const waiting = status.djs.filter(
    (d) => d.state === "queued" || d.state === "connecting",
  );
  const held = status.djs.filter((d) => d.state === "held");
  const next = status.djs.find((d) => d.session_id === status.handoff_to);
  const names = new Map(status.djs.map((d) => [d.username, d.name]));
  const airLabel = onAir ? "Put on air now" : "Put on air";
  let place = 0;

  let waitingNote: string;
  if (scheduled) {
    waitingNote =
      "Nobody else is connected. A DJ can connect ahead of their show and wait here until it starts.";
  } else if (status.info.policy === "auto") {
    waitingNote =
      "Nobody is waiting. A DJ who connects while someone is on air appears here briefly, then takes over.";
  } else {
    waitingNote =
      "Nobody is waiting. A DJ who connects while someone is on air appears here until you hand off to them.";
  }

  return (
    <div className="page">
      <header className="masthead">
        <h2>{status.info.station || "Icegator"}</h2>
        <div className="who small">
          <span className="soft">Signed in as {username}</span>
          <button type="button" className="quiet" onClick={signOut}>
            Sign out
          </button>
        </div>
      </header>

      {connection === "lost" && (
        <p className="notice" role="status">
          Lost contact with Icegator, so this page may be out of date.
          Reconnecting.
        </p>
      )}

      <main>
        {onAir ? (
          <OnAir status={status} dj={onAir} now={now} act={act} />
        ) : (
          <NobodyOnAir status={status} />
        )}

        {onAir && next && (
          <div className="handoff">
            <button
              type="button"
              className="primary large"
              onClick={() => act(api.handoff)}
            >
              Hand off to {next.name}
            </button>
            <p className="small soft">
              {onAir.name} comes off air and stays connected. {next.name} goes
              on
              {scheduled && status.current
                ? ` with ${status.current.show.name}.`
                : "."}
            </p>
          </div>
        )}

        {scheduled && (
          <section className="group">
            <h2>Schedule</h2>
            <dl className="schedule">
              <ScheduleLine label="Now" slot={status.current} names={names} />
              <ScheduleLine label="Next" slot={status.next} names={names} />
            </dl>
          </section>
        )}

        <section className="group">
          <h2>{scheduled ? "Connected" : "Waiting"}</h2>
          {waiting.length === 0 ? (
            <p className="empty soft">{waitingNote}</p>
          ) : (
            <ol className="rows">
              {waiting.map((dj) => {
                // A queue has an order. A schedule does not.
                if (!scheduled && dj.state === "queued") place += 1;
                return (
                  <Row
                    key={dj.session_id}
                    dj={dj}
                    status={status}
                    place={
                      !scheduled && dj.state === "queued" ? place : undefined
                    }
                    airLabel={airLabel}
                    act={act}
                  />
                );
              })}
            </ol>
          )}
        </section>

        {held.length > 0 && (
          <section className="group">
            <h2>Held off air</h2>
            <ul className="rows">
              {held.map((dj) => (
                <Row
                  key={dj.session_id}
                  dj={dj}
                  status={status}
                  airLabel={airLabel}
                  act={act}
                />
              ))}
            </ul>
            <p className="small soft empty">
              Held DJs stay connected but only go back on air when you put them
              there.
            </p>
          </section>
        )}
      </main>

      {status.info.version && (
        <footer className="colophon">Icegator {status.info.version}</footer>
      )}

      {message && (
        <p className="toast" role="alert">
          {message}
        </p>
      )}
    </div>
  );
}
