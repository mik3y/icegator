import "./toolbar.css";
import { useState, useSyncExternalStore } from "react";
import { type Audio, actions, type Settings, store } from "./sim";

const AUDIO: [Audio, string][] = [
  ["good", "Good level"],
  ["low", "Too quiet"],
  ["high", "Too hot"],
  ["silent", "Silent"],
  ["connecting", "No audio yet"],
];

function Choice<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: T;
  options: [T, string][];
  onChange: (value: T) => void;
}) {
  return (
    <label>
      <span>{label}</span>
      <select value={value} onChange={(e) => onChange(e.target.value as T)}>
        {options.map(([v, text]) => (
          <option key={v} value={v}>
            {text}
          </option>
        ))}
      </select>
    </label>
  );
}

/**
 * Controls for the simulated station. Shown only when the dashboard runs
 * in sim mode.
 */
export default function Toolbar() {
  const sim = useSyncExternalStore(store.subscribe, store.get);
  // On a narrow screen the controls would cover the page they are for.
  const [open, setOpen] = useState(() => window.innerWidth >= 700);
  const [audio, setAudio] = useState<Audio>("good");
  const { settings } = sim;
  const set = (change: Partial<Settings>) => actions.set(change);

  return (
    <aside className="simbar" aria-label="Simulator">
      <header>
        <strong>Simulator</strong>
        <span>
          No server is running. Everything on this page is made up, and resets
          when the page reloads.
        </span>
        <button type="button" onClick={() => setOpen(!open)}>
          {open ? "Hide controls" : "Show controls"}
        </button>
      </header>
      {open && (
        <div className="simbar-body">
          <fieldset>
            <legend>Station</legend>
            <Choice
              label="DJs take turns by"
              value={settings.scheduled ? "schedule" : "queue"}
              options={[
                ["queue", "Queue"],
                ["schedule", "Schedule"],
              ]}
              onChange={(v) => set({ scheduled: v === "schedule" })}
            />
            <Choice
              label="Handoff policy"
              value={settings.policy}
              options={[
                ["manual", "Manual"],
                ["auto", "Auto"],
              ]}
              onChange={(policy) => set({ policy })}
            />
            <Choice
              label="Station connection"
              value={settings.link}
              options={[
                ["live", "Live"],
                ["connecting", "Connecting"],
                ["retrying", "Lost, retrying"],
              ]}
              onChange={(link) => set({ link })}
            />
            {settings.scheduled && (
              <Choice
                label="Show on air"
                value={settings.overrun}
                options={[
                  ["no", "In its hour"],
                  ["unlimited", "Overrunning, no limit"],
                  ["limited", "Overrunning, cut-off set"],
                ]}
                onChange={(overrun) => set({ overrun })}
              />
            )}
            <label className="check">
              <input
                type="checkbox"
                checked={settings.recording}
                onChange={(e) => set({ recording: e.target.checked })}
              />
              <span>Recording</span>
            </label>
            <label className="check">
              <input
                type="checkbox"
                checked={settings.lost}
                onChange={(e) => set({ lost: e.target.checked })}
              />
              <span>Page has lost contact</span>
            </label>
          </fieldset>

          <fieldset>
            <legend>DJs</legend>
            <Choice
              label="Connect a DJ with"
              value={audio}
              options={AUDIO}
              onChange={setAudio}
            />
            <button
              type="button"
              disabled={store.full()}
              onClick={() => actions.add(audio)}
            >
              Connect a DJ
            </button>
            <button
              type="button"
              disabled={sim.djs.length === 0}
              onClick={actions.clear}
            >
              Disconnect all
            </button>
          </fieldset>

          {sim.djs.length > 0 && (
            <ul>
              {sim.djs.map((dj) => (
                <li key={dj.id}>
                  <b>{dj.name}</b>
                  <select
                    aria-label={`Audio from ${dj.name}`}
                    value={dj.audio}
                    onChange={(e) =>
                      actions.audio(dj.id, e.target.value as Audio)
                    }
                  >
                    {AUDIO.map(([v, text]) => (
                      <option key={v} value={v}>
                        {text}
                      </option>
                    ))}
                  </select>
                  <button type="button" onClick={() => actions.track(dj.id)}>
                    Next track
                  </button>
                  <button type="button" onClick={() => actions.drop(dj.id)}>
                    Drop
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </aside>
  );
}
