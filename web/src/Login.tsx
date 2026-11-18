import { type FormEvent, useState } from "react";
import { api } from "./api";

export default function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.login(username, password);
      onSignedIn();
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };

  return (
    <main className="signin">
      <form onSubmit={submit}>
        <div>
          <h1>Icegator</h1>
          <p className="soft">Sign in to run the gate.</p>
        </div>
        {error && (
          <p className="notice error" role="alert">
            {error}
          </p>
        )}
        <label>
          Username
          <input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            // biome-ignore lint/a11y/noAutofocus: the form is the whole page
            autoFocus
            required
          />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </label>
        <button type="submit" className="primary large" disabled={busy}>
          Sign in
        </button>
      </form>
    </main>
  );
}
