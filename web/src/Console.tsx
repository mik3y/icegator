import { useEffect, useState } from "react";
import { api, type Dj, type Status } from "./api";
import { elapsed, host, streamFormat } from "./format";
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
        <h1>{dj.name}</h1>
        {/* With no title from the DJ, the station shows their name, which
            is already the heading. */}
        {status.title && status.title !== dj.name && (
          <p className="title">{status.title}</p>
        )}
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

function NobodyOnAir({ anyoneConnected }: { anyoneConnected: boolean }) {
  return (
    <section className="strip" aria-label="On air">
      <p className="tally soft">
        <span className="lamp" aria-hidden />
        Nobody is on air
      </p>
      <p className="empty">
        Icegator is not connected to the station, so the station is playing its
        own programming.
        {anyoneConnected
          ? " Put one of the DJs below on air when you are ready."
          : " The first DJ to connect goes on air once their audio is flowing."}
      </p>
    </section>
  );
}

function Row({
  dj,
  place,
  airLabel,
  act,
}: {
  dj: Dj;
  place?: number;
  airLabel: string;
  act: Act;
}) {
  const connecting = dj.state === "connecting";
  return (
    <li className="row">
      <span className="place">{place ?? ""}</span>
      <div>
        <span className="name">{dj.name}</span>
        {!dj.verified && (
          <span
            className="flag"
            title="The station has not confirmed this login yet."
          >
            Login not confirmed
          </span>
        )}
        <p className="small soft">
          {connecting
            ? "Waiting for audio"
            : dj.title || streamFormat(dj.stats)}
        </p>
      </div>
      <div className="level">
        <Meter sessionId={dj.session_id} />
        <LevelVerdict sessionId={dj.session_id} />
      </div>
      <div className="actions">
        <button
          type="button"
          disabled={connecting}
          onClick={() => act(() => api.air(dj.session_id))}
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

  const onAir = status.djs.find((d) => d.state === "on_air");
  const waiting = status.djs.filter(
    (d) => d.state === "queued" || d.state === "connecting",
  );
  const held = status.djs.filter((d) => d.state === "held");
  const next = waiting.find((d) => d.state === "queued");
  const airLabel = onAir ? "Put on air now" : "Put on air";
  let place = 0;

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
          <NobodyOnAir anyoneConnected={status.djs.length > 0} />
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
              on.
            </p>
          </div>
        )}

        <section className="group">
          <h2>Waiting</h2>
          {waiting.length === 0 ? (
            <p className="empty soft">
              Nobody is waiting. A DJ who connects while someone is on air
              appears here
              {status.info.policy === "auto"
                ? " briefly, then takes over."
                : " until you hand off to them."}
            </p>
          ) : (
            <ol className="rows">
              {waiting.map((dj) => {
                if (dj.state === "queued") place += 1;
                return (
                  <Row
                    key={dj.session_id}
                    dj={dj}
                    place={dj.state === "queued" ? place : undefined}
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

      {message && (
        <p className="toast" role="alert">
          {message}
        </p>
      )}
    </div>
  );
}
